package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// C61 step 4: push over inbox entries (INBOX_ENTRIES=read). The same
// scripted traffic runs on stores in off, shadow and read; off and shadow
// must push exactly the same deliveries and wake-up firings (today's
// behaviour), and read the same again but for the differences named here.

// forInboxModes runs a test under INBOX_ENTRIES off and read.
func forInboxModes(t *testing.T, run func(*testing.T, InboxMode)) {
	t.Helper()
	for _, m := range []InboxMode{InboxOff, InboxRead} {
		t.Run(m.String(), func(t *testing.T) { run(t, m) })
	}
}

func withInbox(c Config, m InboxMode) Config {
	c.Features.InboxEntries = m
	return c
}

// otherID is an id the script does not name (dave's room-activity post).
var otherID = regexp.MustCompile(`[0-9a-f]{32}`)

// wakeSeen is each store's wake-up notices already counted by wakeFired.
var wakeSeen sync.Map

// wakeFired runs the wake-up clock once and returns the firings made since
// the previous call, wherever they were made: by the clock, or under
// INBOX_ENTRIES=read as the entries were written. Under off it is what
// wakeWork counts when nothing expires.
func wakeFired(t *testing.T, s *Store) int {
	t.Helper()
	wakeWork(t, s)
	n := sqlCount(t, s, "SELECT count(*) FROM wakeup_notices")
	prev, _ := wakeSeen.Swap(s, n)
	p, _ := prev.(int64)
	return int(n - p)
}

// activeWebhookKinds installs a confirmed subscription that asked for kinds.
func activeWebhookKinds(t *testing.T, s *Store, account, url, kinds string) string {
	t.Helper()
	id, _ := activeWebhook(t, s, account, url)
	if _, err := s.db.Exec("UPDATE webhook_subscriptions SET kinds=? WHERE id=?", kinds, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// pushRun is the scripted traffic played on one store, with every push it
// made written as labelled lines.
type pushRun struct {
	s        *Store
	sc       inboxScript
	label    *strings.Replacer
	webhooks []string // account reason about
	mcp      []string // account name subject
	wakes    []string // account kind event room
}

// runPushScript plays runInboxScript under mode with every account
// subscribed to everything by webhook (the default set) and MCP Events, a
// room.post subscription for dave, and reply, mention and message wake-ups.
func runPushScript(t *testing.T, mode InboxMode) pushRun {
	t.Helper()
	s := openInboxTest(t, mode)
	alice, bob, carol, dave := keyFor(211), keyFor(212), keyFor(213), keyFor(214)
	names := map[string]string{}
	for name, k := range map[string]ed25519.PrivateKey{"alice": alice, "bob": bob, "carol": carol, "dave": dave} {
		names[keyID(k)] = name
		activeWebhook(t, s, keyID(k), "https://hooks.example.org/"+name)
		mcpSubscribe(t, s, keyID(k), "reply", "mention", "conversation.message", "conversation.request", "work.update", "identity.witnessed")
		for _, on := range []string{"reply", "mention", "message"} {
			runBounded(t, s, svcCall(k, "wakeup", "schedule", map[string]any{"key": "p-" + on, "on": on}, 1, "push-"+name+"-"+on))
		}
	}
	if _, err := s.db.Exec(`INSERT INTO mcp_event_subscriptions(id,account,credential,name,arguments,url,secret,via,state,created_at,refreshed_at,refresh_before,confirmed_at)
 VALUES(?,?,'test','room.post','{"room":"lobby"}','https://events.example.org/x','secret','mcp','active',?,?,?,?)`, randomID(), keyID(dave), testTime, testTime, testTime+86400*365, testTime); err != nil {
		t.Fatal(err)
	}
	sc := runInboxScript(t, s, true)
	wakeWork(t, s)
	pairs := []string{}
	for id, name := range names {
		pairs = append(pairs, id, name)
	}
	for name, id := range map[string]string{"root": sc.root, "reply": sc.reply, "edit": sc.edit, "anonReply": sc.anonReply, "selfReply": sc.selfReply,
		"addressed": sc.addressed, "triple": sc.triple, "privateToAlice": sc.privateToAlice, "privateToCarol": sc.privateToCarol, "hello": sc.hello,
		"hi": sc.hi, "convReply": sc.convReply, "work": sc.workID, "dm": sc.dm, "item": sc.receiverItem} {
		pairs = append(pairs, id, name)
	}
	r := pushRun{s: s, sc: sc, label: strings.NewReplacer(pairs...)}
	rows, err := s.db.Query("SELECT s.account,d.body FROM webhook_deliveries d JOIN webhook_subscriptions s ON s.id=d.subscription ORDER BY d.seq")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var account, body string
		if err = rows.Scan(&account, &body); err != nil {
			t.Fatal(err)
		}
		var b struct {
			Reason string `json:"reason"`
			Event  struct {
				ID string `json:"id"`
			} `json:"event"`
			Entry struct {
				Subject string `json:"subject"`
			} `json:"entry"`
		}
		if err = json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		about := b.Event.ID
		if about == "" {
			about = b.Entry.Subject
		}
		r.webhooks = append(r.webhooks, otherID.ReplaceAllString(r.label.Replace(account+" "+b.Reason+" "+about), "other"))
	}
	rows.Close()
	rows, err = s.db.Query("SELECT s.account,s.name,d.body FROM mcp_event_deliveries d JOIN mcp_event_subscriptions s ON s.id=d.subscription ORDER BY d.seq")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var account, name, body string
		if err = rows.Scan(&account, &name, &body); err != nil {
			t.Fatal(err)
		}
		var draft mcpEventDraft
		if err = json.Unmarshal([]byte(body), &draft); err != nil {
			t.Fatal(err)
		}
		r.mcp = append(r.mcp, r.label.Replace(account+" "+name+" "+draft.Subject))
	}
	rows.Close()
	rows, err = s.db.Query("SELECT account,kind,event,room FROM wakeup_notices ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var account, kind, event, room string
		if err = rows.Scan(&account, &kind, &event, &room); err != nil {
			t.Fatal(err)
		}
		r.wakes = append(r.wakes, r.label.Replace(fmt.Sprintf("%s %s %s %s", account, kind, event, room)))
	}
	rows.Close()
	for _, list := range [][]string{r.webhooks, r.mcp, r.wakes} {
		sort.Strings(list)
	}
	return r
}

func samePushes(t *testing.T, what string, a, b []string) {
	t.Helper()
	if !slices.Equal(a, b) {
		t.Errorf("%s differ:\n first  %q\n second %q", what, a, b)
	}
}

// Off and shadow push the same: the shadow log changes no delivery and no
// firing.
func TestInboxPushOffMatchesShadow(t *testing.T) {
	off, shadow := runPushScript(t, InboxOff), runPushScript(t, InboxShadow)
	if len(off.webhooks) < 15 || len(off.mcp) < 10 || len(off.wakes) < 6 {
		t.Fatalf("the script pushed too little to compare: %d webhooks, %d MCP, %d wake-ups", len(off.webhooks), len(off.mcp), len(off.wakes))
	}
	samePushes(t, "webhooks", off.webhooks, shadow.webhooks)
	samePushes(t, "MCP Events", off.mcp, shadow.mcp)
	samePushes(t, "wake-ups", off.wakes, shadow.wakes)
}

// Read pushes from the entries what shadow pushed from the transports' own
// queries, one delivery per entry, but for one designed difference: a
// conversation request is pushed once, when it is made, about the
// conversation (webhook reason request, MCP conversation.request,
// on:"message"), where today each of the asker's first messages is.
func TestInboxPushReadMatchesShadow(t *testing.T) {
	shadow, read := runPushScript(t, InboxShadow), runPushScript(t, InboxRead)
	// The difference, named: shadow's request pushes are about hello, the
	// first message; read's about the conversation itself.
	request := func(list []string) []string {
		out := []string{}
		for _, line := range list {
			line = strings.Replace(line, " request hello", " request dm", 1)
			line = strings.Replace(line, " conversation.request hello", " conversation.request dm", 1)
			line = strings.Replace(line, " message hello dm", " message  dm", 1)
			out = append(out, line)
		}
		sort.Strings(out)
		return slices.Compact(out)
	}
	if !slices.Contains(shadow.webhooks, "bob request hello") || !slices.Contains(read.webhooks, "bob request dm") {
		t.Fatalf("the request pushes: shadow %q, read %q", shadow.webhooks, read.webhooks)
	}
	if !slices.Contains(read.wakes, "bob message  dm") {
		t.Fatalf("bob's on:message wake-up fires on the request: %q", read.wakes)
	}
	samePushes(t, "webhooks", request(shadow.webhooks), read.webhooks)
	samePushes(t, "MCP Events", request(shadow.mcp), read.mcp)
	samePushes(t, "wake-ups", request(shadow.wakes), read.wakes)

	// One delivery per entry: every webhook delivery but room activity is
	// keyed on, and carries, an entry of its subscriber's.
	s := read.s
	rows, err := s.db.Query(`SELECT d.event_id,d.body,s.account,coalesce(e.account,'') FROM webhook_deliveries d JOIN webhook_subscriptions s ON s.id=d.subscription
 LEFT JOIN inbox_entries e ON e.id=d.event_id`)
	if err != nil {
		t.Fatal(err)
	}
	keyed := 0
	for rows.Next() {
		var key, body, account, entryAccount string
		if err = rows.Scan(&key, &body, &account, &entryAccount); err != nil {
			t.Fatal(err)
		}
		var b struct {
			Reason string         `json:"reason"`
			Entry  map[string]any `json:"entry"`
		}
		if err = json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		if b.Reason == "room_activity" {
			if entryAccount != "" || b.Entry != nil {
				t.Errorf("room activity is not an entry: %s", body)
			}
			continue
		}
		keyed++
		if entryAccount != account || b.Entry["id"] != key {
			t.Errorf("a delivery not keyed on its subscriber's entry: %s (%s)", body, key)
		}
	}
	rows.Close()
	if keyed < 8 {
		t.Fatalf("%d entry deliveries", keyed)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM mcp_event_deliveries d LEFT JOIN inbox_entries e ON e.id=d.event_id JOIN mcp_event_subscriptions s ON s.id=d.subscription WHERE s.name<>'room.post' AND e.id IS NULL"); n != 0 {
		t.Fatalf("%d MCP deliveries not keyed on an entry", n)
	}
	// Pushing is part of writing an entry: the backfill, which writes
	// nothing new, pushes nothing.
	before := sqlCount(t, s, "SELECT count(*) FROM webhook_deliveries") + sqlCount(t, s, "SELECT count(*) FROM mcp_event_deliveries")
	s.restartInboxBackfill()
	for pass := 0; pass < 100; pass++ {
		if _, done, err := s.BackfillInboxEntries(testContext); err != nil {
			t.Fatal(err)
		} else if done {
			break
		}
	}
	if after := sqlCount(t, s, "SELECT count(*) FROM webhook_deliveries") + sqlCount(t, s, "SELECT count(*) FROM mcp_event_deliveries"); after != before {
		t.Fatalf("the backfill pushed %d deliveries", after-before)
	}
}

// webhookBodies is one subscription's delivery bodies, decoded.
func webhookBodies(t *testing.T, s *Store, subscription string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, row := range webhookRows(t, s) {
		if row["subscription"] != subscription {
			continue
		}
		var b map[string]any
		if err := json.Unmarshal([]byte(row["body"]), &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func reasonsOf(bodies []map[string]any) []string {
	var out []string
	for _, b := range bodies {
		out = append(out, b["reason"].(string))
	}
	sort.Strings(out)
	return out
}

// A subscription's kinds pick what it gets: the opt-in kinds (work,
// witness, received, wakeup) only when asked for, carrying the entry and no
// message; a narrower list only its reasons, a message that is several
// delivered under the one it asked for.
func TestWebhookKindsFilterEntries(t *testing.T) {
	s := openInboxTest(t, InboxRead)
	alice, bob, carol := keyFor(211), keyFor(212), keyFor(213)
	optIn := activeWebhookKinds(t, s, keyID(alice), "https://hooks.example.org/a-opt", "wakeup,work,witness")
	plain, _ := activeWebhook(t, s, keyID(alice), "https://hooks.example.org/a-default")
	bobs := activeWebhookKinds(t, s, keyID(bob), "https://hooks.example.org/b", "request,received,wakeup")
	mentions := activeWebhookKinds(t, s, keyID(carol), "https://hooks.example.org/c", "mention")
	triple := activeWebhookKinds(t, s, keyID(alice), "https://hooks.example.org/a-mention", "mention")
	sc := runInboxScript(t, s, true)

	got := webhookBodies(t, s, optIn)
	if r := reasonsOf(got); strings.Join(r, ",") != "wakeup,witness,work" {
		t.Fatalf("alice's opt-in subscription: %v", r)
	}
	for _, b := range got {
		entry, _ := b["entry"].(map[string]any)
		if b["event"] != nil || entry["kind"] != b["reason"] || entry["id"] == "" || b["read"] == "" || b["type"] != "event" {
			t.Errorf("an opt-in delivery: %+v", b)
		}
		if b["reason"] == "work" {
			if detail, _ := entry["detail"].(map[string]any); detail["state"] != "claimed" || detail["role"] != "requester" || b["read"] != "/api/work/"+sc.workID {
				t.Errorf("the work delivery: %+v", b)
			}
		}
	}
	for _, reason := range reasonsOf(webhookBodies(t, s, plain)) {
		if reason == "work" || reason == "witness" || reason == "wakeup" {
			t.Fatalf("the default set got an opt-in kind: %s", reason)
		}
	}
	if r := reasonsOf(webhookBodies(t, s, bobs)); strings.Join(r, ",") != "received,request,wakeup" {
		t.Fatalf("bob's subscription: %v", r)
	}
	carols := webhookBodies(t, s, mentions)
	if len(carols) != 1 || carols[0]["reason"] != "mention" || carols[0]["event"].(map[string]any)["id"] != sc.edit {
		t.Fatalf("carol's mention-only subscription: %+v", carols)
	}
	// The triple is a reply, addressed and a mention: delivered once, as the
	// mention it asked for; the addressed-only messages not at all.
	alices := webhookBodies(t, s, triple)
	if len(alices) != 1 || alices[0]["reason"] != "mention" || alices[0]["event"].(map[string]any)["id"] != sc.triple {
		t.Fatalf("alice's mention-only subscription: %+v", alices)
	}
	if strings.Contains(fmt.Sprint(webhookRows(t, s)), "hello bob") || strings.Contains(fmt.Sprint(webhookRows(t, s)), "job") {
		t.Fatal("a delivery carries text")
	}
}

// webhook.create takes kinds only under INBOX_ENTRIES=read (before, the
// field is refused as any unknown one is); webhook.list shows them.
func TestWebhookCreateKinds(t *testing.T) {
	s := openTest(t, withInbox(Config{}, InboxShadow))
	s.webhookInsecure = true
	key := keyFor(93)
	withKinds := `{"schema":1,"url":"https://hooks.example.org/k","kinds":["work","reply"]}`
	fails(t, s, signed(key, Command{Operation: "webhook.create", Data: withKinds}), "invalid_webhook")
	if list := run(t, s, signed(key, Command{Operation: "webhook.list"})); fmt.Sprint(list.Data["subscriptions"]) != "[]" {
		t.Fatalf("list %+v", list.Data)
	}
	setInboxMode(s, InboxRead)
	for _, raw := range []string{
		`{"schema":1,"url":"https://hooks.example.org/k","kinds":[]}`,
		`{"schema":1,"url":"https://hooks.example.org/k","kinds":["reply","reply"]}`,
		`{"schema":1,"url":"https://hooks.example.org/k","kinds":["everything"]}`,
		`{"schema":1,"url":"https://hooks.example.org/k","kinds":"reply"}`,
		`{"schema":1,"url":"https://hooks.example.org/k","kinds":null}`,
		`{"schema":1,"url":"https://hooks.example.org/k","kinds":["reply"],"kinds":["work"]}`,
	} {
		fails(t, s, signed(key, Command{Operation: "webhook.create", Data: raw}), "invalid_webhook")
	}
	created := run(t, s, signed(key, Command{Operation: "webhook.create", Data: withKinds, Timestamp: testTime + 3}))
	if fmt.Sprint(created.Data["kinds"]) != "[reply work]" {
		t.Fatalf("created %+v", created.Data)
	}
	run(t, s, signed(key, Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/d"}`, Timestamp: testTime + 1}))
	list := run(t, s, signed(key, Command{Operation: "webhook.list", Timestamp: testTime + 2}))
	subs := list.Data["subscriptions"].([]map[string]any)
	kinds := map[string]string{}
	for _, sub := range subs {
		kinds[sub["url"].(string)] = fmt.Sprint(sub["kinds"])
	}
	if len(subs) != 2 || kinds["https://hooks.example.org/k"] != "[reply work]" || kinds["https://hooks.example.org/d"] != fmt.Sprint(WebhookDefaultKinds) {
		t.Fatalf("listed %+v", subs)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM webhook_subscriptions WHERE kinds='reply,work'"); n != 1 {
		t.Fatalf("stored kinds: %d", n)
	}
}

// Under read, personal wake-ups fire as the entry is written, in the
// post's transaction, not by the clock: a reply, a mention (an @handle and
// an addressee), a conversation message and a request each wake at once,
// the clock fires none of them again, and room wake-ups stay the clock's.
func TestWakeupsFireOnEntryInsert(t *testing.T) {
	s := openWakeTest(t, "wakeup")
	setInboxMode(s, InboxRead)
	alice := handleKey(t, s, 1, "alice")
	bob := handleKey(t, s, 2, "bob")
	carol := handleKey(t, s, 3, "carol")
	dave := handleKey(t, s, 4, "dave")
	wakeWork(t, s)
	notices := func(key ed25519.PrivateKey) []string {
		t.Helper()
		rows, err := s.db.Query("SELECT kind,event FROM wakeup_notices WHERE account=? ORDER BY seq", keyID(key))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var kind, event string
			if err = rows.Scan(&kind, &event); err != nil {
				t.Fatal(err)
			}
			out = append(out, kind+" "+event)
		}
		return out
	}
	schedule := func(key ed25519.PrivateKey, name string, args map[string]any) {
		t.Helper()
		args["key"] = name
		run(t, s, svcCall(key, "wakeup", "schedule", args, 1, "ins-"+name+keyID(key)[:6]))
	}
	schedule(alice, "r", map[string]any{"on": "reply"})
	schedule(alice, "m", map[string]any{"on": "mention"})
	schedule(bob, "m", map[string]any{"on": "mention"})
	schedule(dave, "msg", map[string]any{"on": "message"})
	root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "root"})).Receipt.ID
	if n := sqlCount(t, s, "SELECT count(*) FROM wakeup_notices"); n != 0 {
		t.Fatalf("alice's own post woke %d", n)
	}
	schedule(carol, "room", map[string]any{"on": "room", "room": "lobby"})
	reply := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "a reply"})).Receipt.ID
	if got := notices(alice); strings.Join(got, ",") != "reply "+reply {
		t.Fatalf("the reply wakes alice before any clock pass: %v", got)
	}
	addressed := run(t, s, signed(carol, Command{Operation: "post", Room: "elsewhere", To: keyID(alice), Text: "for alice and @bob"})).Receipt.ID
	if got := notices(alice); len(got) != 2 || got[1] != "mention "+addressed {
		t.Fatalf("an addressee wakes on mention: %v", got)
	}
	if got := notices(bob); strings.Join(got, ",") != "mention "+addressed {
		t.Fatalf("an @handle wakes on mention: %v", got)
	}
	// The room wake-ups are still the clock's: nothing yet for carol, then
	// one firing on the next pass, and no personal firing repeated.
	if len(notices(carol)) != 0 {
		t.Fatalf("carol's room wake-up fired at insert: %v", notices(carol))
	}
	if n := wakeWork(t, s); n != 1 || len(notices(carol)) != 1 || !strings.HasPrefix(notices(carol)[0], "room ") {
		t.Fatalf("the clock fired %d: carol %v", n, notices(carol))
	}
	if n := wakeWork(t, s); n != 0 {
		t.Fatalf("a second pass fired %d", n)
	}
	// A request to dave (alice and he share nothing, so her DM is one)
	// wakes his on:"message" at once, before any message.
	dm := convRoom("wake-insert")
	runBounded(t, s, openCmd(alice, dm, "dm", dave))
	if n := sqlCount(t, s, "SELECT count(*) FROM conversation_members WHERE room=? AND account=? AND state='requested'", dm, keyID(dave)); n != 1 {
		t.Fatalf("dave is not requested: %d", n)
	}
	got := notices(dave)
	if len(got) != 1 || got[0] != "message " {
		t.Fatalf("the request wakes dave's on:message: %v", got)
	}
	var room string
	if err := s.db.QueryRow("SELECT room FROM wakeup_notices WHERE account=? AND kind='message'", keyID(dave)).Scan(&room); err != nil || room != dm {
		t.Fatalf("the request notice names its conversation: %q %v", room, err)
	}
	// A wake-up registered after the message does not fire on it.
	schedule(bob, "late", map[string]any{"on": "reply"})
	if n := sqlCount(t, s, "SELECT count(*) FROM wakeup_notices WHERE account=? AND kind='reply'", keyID(bob)); n != 0 {
		t.Fatalf("a later wake-up fired on an earlier message: %d", n)
	}
}

// Under read a conversation request reaches MCP Events once, when it is
// made, about the conversation: its room, who asks, never a body; one that
// is answered before it is sent sends nothing.
func TestMCPEventsRequestFromEntry(t *testing.T) {
	s, r := mcpEventStore(t)
	setInboxMode(s, InboxRead)
	alice, aliceKey, _ := hostedPrincipal(t, s, "alice-req")
	if _, err := s.SubscribeMCPEvent(testContext, alice, subscribeRequest(t, "conversation.request", nil, "https://example.com/hook", r.secret)); err != nil {
		t.Fatal(err)
	}
	bob, carol := keyFor(74), keyFor(75)
	registerAll(t, s, bob, carol)
	room := convRoom("mcp-request")
	openConv(t, s, bob, room, "dm", aliceKey)
	for i := 0; i < 3; i++ {
		run(t, s, signed(bob, Command{Operation: "post", Room: room, Text: fmt.Sprintf("the private body %d", i), Visibility: "private", Timestamp: testTime + int64(i)}))
	}
	drain(t, s)
	events := r.events()
	if len(events) != 1 {
		t.Fatalf("want one request event, got %d", len(events))
	}
	data, _ := events[0].body["data"].(map[string]any)
	author, _ := data["author"].(map[string]any)
	if events[0].body["name"] != "conversation.request" || data["room"] != room || data["visibility"] != "conversation" || author["fingerprint"] != keyID(bob) ||
		data["from"] != nil || data["message_id"] != nil || strings.Contains(string(events[0].raw), "private body") || data["untrusted"] != true {
		t.Fatalf("request event %s", events[0].raw)
	}
	// Answered before it went out: dropped at send.
	other := convRoom("mcp-request-2")
	openConv(t, s, carol, other, "dm", aliceKey)
	run(t, s, respond(aliceKey, other, "decline"))
	drain(t, s)
	if n := len(r.events()); n != 1 || mcpQueued(t, s) != 0 {
		t.Fatalf("a declined request was sent: %d events", n)
	}
}

// The duplicate-delivery fix (C60) holds for deliveries queued from
// entries: a receiver that takes seconds to answer gets a real post's
// delivery once.
func TestWebhookSlowReceiverIsDeliveredOnceFromEntries(t *testing.T) {
	s := insecureStore(t)
	setInboxMode(s, InboxRead)
	server, seen := endpoint(t, func(*received) (int, string) {
		time.Sleep(7 * time.Second)
		return 200, "ok"
	})
	subscriber, poster := keyFor(103), keyFor(104)
	registerAll(t, s, subscriber, poster)
	mine := run(t, s, signed(subscriber, Command{Operation: "post", Room: "lobby", Text: "seed"})).Receipt.ID
	id, _ := activeWebhook(t, s, keyID(subscriber), server.URL+"/hook")
	run(t, s, signed(poster, Command{Operation: "post", Room: "lobby", ReplyTo: mine, To: keyID(subscriber), Text: "both"}))
	rows := webhookRows(t, s)
	if len(rows) != 1 || rows[0]["subscription"] != id || sqlCount(t, s, "SELECT count(*) FROM inbox_entries WHERE id=?", rows[0]["event"]) != 1 {
		t.Fatalf("queued %+v", rows)
	}
	if worked, err := s.deliverOnce(testContext); err != nil || !worked {
		t.Fatalf("delivery did not run: %v", err)
	}
	s.now = func() time.Time { return time.Unix(testTime+webhookMaxBackoff+1, 0) }
	if worked, err := s.deliverOnce(testContext); err != nil || worked {
		t.Fatalf("a delivered event was attempted again: worked=%v err=%v", worked, err)
	}
	if n := len(seen()); n != 1 || queueDepth(t, s) != 0 {
		t.Fatalf("slow receiver saw %d requests, %d still queued", n, queueDepth(t, s))
	}
}

// The push path runs in the producer's transaction: no function in
// inbox_push.go names the store's pool (one SQLite connection).
func TestInboxPushNeverUsesThePool(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "inbox_push.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		checked++
		ast.Inspect(fn, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "db" {
				t.Errorf("inbox_push.go: %s uses the pool (.db)", fn.Name.Name)
			}
			return true
		})
	}
	if checked < 10 {
		t.Fatalf("checked %d functions", checked)
	}
}

// Under read work.update follows the work entries: a result submitted for a
// named reviewer's verdict reaches the reviewer too (role reviewer), and the
// requester and worker keep the roles MCP Events always gave them.
func TestMCPWorkUpdateReachesTheReviewer(t *testing.T) {
	s := openInboxTest(t, InboxRead)
	requester, worker, reviewer := keyID(keyFor(221)), keyID(keyFor(222)), keyID(keyFor(223))
	for _, account := range []string{requester, worker, reviewer} {
		mcpSubscribe(t, s, account, "work.update")
	}
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	w := workRow{ID: randomID(), Sequence: 3, State: "submitted", Requester: requester, Reviewer: reviewer, Worker: worker}
	a := actor{id: worker, account: worker, signed: true}
	entries, err := s.recordInbox(testContext, tx, workInboxSource("work.submit", w, "lobby", worker, a, testTime))
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries %+v %v", entries, err)
	}
	if err = s.pushWork(testContext, tx, "work.submit", w, workRoot{Room: "lobby"}, worker, 0, a, entries, testTime); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(testContext, "SELECT s.account,d.body FROM mcp_event_deliveries d JOIN mcp_event_subscriptions s ON s.id=d.subscription")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for rows.Next() {
		var account, body string
		if err = rows.Scan(&account, &body); err != nil {
			t.Fatal(err)
		}
		var draft mcpEventDraft
		if err = json.Unmarshal([]byte(body), &draft); err != nil {
			t.Fatal(err)
		}
		roles[account] = fmt.Sprint(draft.Data["role"], " ", draft.Data["state"])
	}
	rows.Close()
	if len(roles) != 2 || roles[reviewer] != "reviewer submitted" || roles[requester] != "requester submitted" {
		t.Fatalf("work.update roles %v", roles)
	}
}
