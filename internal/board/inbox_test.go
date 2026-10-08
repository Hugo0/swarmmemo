package board

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// C61 step 1: the inbox entry log, written in shadow. These tests hold the
// entries side by side with what updates.get, webhooks and MCP Events
// deliver today, per account, for one scripted mix of activity on the real
// engine. Where the transports already disagree with each other (the RFC's
// drift), the test names the difference rather than failing on it.

// openInboxTest is a store with the inbox log on (or off), wake-ups and
// receivers enabled, spending through the fake ledger.
func openInboxTest(t *testing.T, mode InboxMode) *Store {
	t.Helper()
	c := updatesConfig()
	c.Features = Features{Services: []string{"receiver", "wakeup"}, InboxEntries: mode}
	s := openTest(t, c)
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	return s
}

// runBounded is run under a deadline: a producer that used the pool under
// its own transaction would wait for the one connection forever, and fails
// here instead of hanging the suite.
func runBounded(t *testing.T, s *Store, c Command) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(testContext, 20*time.Second)
	defer cancel()
	r, err := s.Execute(ctx, c, "test-origin")
	if err != nil {
		t.Fatalf("%s: %v", c.Operation, err)
	}
	return r
}

// mcpSubscribe installs live MCP Events subscriptions directly, as
// activeWebhook does for webhooks: the parity test needs the queue, not the
// verification handshake.
func mcpSubscribe(t *testing.T, s *Store, account string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := s.db.Exec(`INSERT INTO mcp_event_subscriptions(id,account,credential,name,arguments,url,secret,via,state,created_at,refreshed_at,refresh_before,confirmed_at)
 VALUES(?,?,'test',?,'{}','https://events.example.org/x','secret','mcp','active',?,?,?,?)`, randomID(), account, name, testTime, testTime, testTime+86400*365, testTime); err != nil {
			t.Fatal(err)
		}
	}
}

// inboxScript is the scripted mix: who did what, by id.
type inboxScript struct {
	alice, bob, carol, dave                                      ed25519.PrivateKey
	root, reply, edit, anonReply, selfReply, addressed, triple   string
	privateToAlice, privateToCarol, hello, hi, convReply, workID string
	dm, receiverItem                                             string
}

// runInboxScript plays the mix. With accept, bob accepts alice's DM request
// and the conversation goes on; without, the request stays pending.
func runInboxScript(t *testing.T, s *Store, accept bool) inboxScript {
	t.Helper()
	sc := inboxScript{alice: keyFor(211), bob: keyFor(212), carol: keyFor(213), dave: keyFor(214)}
	for _, p := range []struct {
		key    ed25519.PrivateKey
		handle string
	}{{sc.alice, "ialice"}, {sc.bob, "ibob"}, {sc.carol, "icarol"}, {sc.dave, "idave"}} {
		runBounded(t, s, signed(p.key, Command{Operation: "agent.register", Handle: p.handle}))
	}
	post := func(key ed25519.PrivateKey, c Command) string {
		t.Helper()
		if key != nil {
			c = signed(key, c)
		}
		return runBounded(t, s, c).Receipt.ID
	}
	// A DM, opened while alice and bob share nothing (bob's inbound policy
	// makes it a request), answered further down.
	sc.dm = convRoom("inbox-c61")
	runBounded(t, s, openCmd(sc.alice, sc.dm, "dm", sc.bob))
	sc.hello = post(sc.alice, Command{Operation: "post", Room: sc.dm, Text: "hello bob", Visibility: "private"})
	sc.root = post(sc.alice, Command{Operation: "post", Room: "lobby", Text: "root by alice"})
	// A reply, then its new version, which adds a mention of carol.
	sc.reply = post(sc.bob, Command{Operation: "post", Room: "lobby", ReplyTo: sc.root, Text: "a reply"})
	sc.edit = post(sc.bob, Command{Operation: "post", Room: "lobby", ReplyTo: sc.root, Text: "a reply, now for @icarol too", Data: dataJSON(`"supersedes":"` + sc.reply + `"`)})
	// An anonymous reply, and alice answering herself: news, and no news.
	sc.anonReply = post(nil, Command{Operation: "post", Room: "lobby", ReplyTo: sc.root, Text: "anonymous reply"})
	sc.selfReply = post(sc.alice, Command{Operation: "post", Room: "lobby", ReplyTo: sc.root, Text: "replying to myself"})
	// Addressed to alice, mentioning bob; and one message that is a reply,
	// addressed and a mention for alice at once.
	sc.addressed = post(sc.carol, Command{Operation: "post", Room: "lobby", To: keyID(sc.alice), Text: "for alice, and @ibob should see this"})
	sc.triple = post(sc.dave, Command{Operation: "post", Room: "lobby", ReplyTo: sc.root, To: keyID(sc.alice), Text: "@ialice all three at once"})
	// Room activity only: concerns nobody's entries.
	post(sc.dave, Command{Operation: "post", Room: "lobby", Text: "just talking"})
	// A private room of alice and bob: carol, outside it, hears nothing.
	runBounded(t, s, signed(sc.alice, Command{Operation: "room.create", Room: "ipriv", Visibility: "private", Members: []string{keyID(sc.bob)}}))
	sc.privateToAlice = post(sc.bob, Command{Operation: "post", Room: "ipriv", To: keyID(sc.alice), Text: "private, for alice"})
	sc.privateToCarol = post(sc.bob, Command{Operation: "post", Room: "ipriv", To: keyID(sc.carol), Text: "private, for @icarol who is not here"})
	// With accept, bob takes alice's request and the conversation goes on.
	if accept {
		runBounded(t, s, respond(sc.bob, sc.dm, "accept"))
		sc.hi = post(sc.bob, Command{Operation: "post", Room: sc.dm, Text: "hi alice", Visibility: "private"})
		sc.convReply = post(sc.alice, Command{Operation: "post", Room: sc.dm, ReplyTo: sc.hi, To: keyID(sc.bob), Text: "answering you, bob", Visibility: "private"})
	}
	// Work: carol claims alice's work, alice cancels it.
	sc.workID = createTestWork(t, s, sc.alice, "lobby", "request", 0)
	runBounded(t, s, workCommand(s, sc.carol, Command{Operation: "work.claim", MessageID: sc.workID, TTL: 600}))
	runBounded(t, s, workCommand(s, sc.alice, Command{Operation: "work.cancel", MessageID: sc.workID, Reason: "no longer needed"}))
	// A witness of alice's claimed page link, by dave.
	page := "https://other.example.org/agents/ialice"
	runBounded(t, s, linkCommand(sc.alice, "identity.link", "url", page))
	runBounded(t, s, witnessCommand(sc.dave, keyID(sc.alice), "url", page, "inbox-witness-nonce-0123456789", "verified"))
	// A receiver item for bob, which fires his on:"received" wake-up.
	runBounded(t, s, svcCall(sc.bob, "wakeup", "schedule", map[string]any{"key": "got", "on": "received"}, 1, "c61-received"))
	created := runBounded(t, s, svcCall(sc.bob, "receiver", "create", map[string]any{"label": "jobs", "screen": false}, 5, "c61-receiver"))
	url, _ := svcField(t, created.Data, "result", "url").(string)
	parts := strings.Split(url[strings.Index(url, "/in/")+4:], "/")
	if len(parts) != 2 {
		t.Fatalf("receive url %q", url)
	}
	receipt, err := s.Receive(testContext, services.Delivery{ID: parts[0], Token: parts[1], ContentType: "application/json", Body: []byte(`{"job":1}`), Source: net.ParseIP("198.51.100.61")})
	if err != nil {
		t.Fatal(err)
	}
	sc.receiverItem = receipt.Item
	// A time wake-up for alice, fired by the clock.
	runBounded(t, s, svcCall(sc.alice, "wakeup", "schedule", map[string]any{"key": "later", "at": testTime + 60}, 1, "c61-time"))
	svcSetNow(s, testTime+60)
	if n := wakeWork(t, s); n < 1 {
		t.Fatalf("the time wake-up fires: %d", n)
	}
	svcSetNow(s, testTime)
	return sc
}

// entrySet is an account's entries by subject.
func entrySet(t *testing.T, s *Store, key ed25519.PrivateKey) map[string]InboxEntry {
	t.Helper()
	list, err := s.InboxShadow(testContext, keyID(key), 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]InboxEntry{}
	for _, e := range list {
		if _, dup := out[e.Subject]; dup {
			t.Fatalf("two entries for %s", e.Subject)
		}
		out[e.Subject] = e
	}
	return out
}

// withReason is the subjects of entries of kind (any when "") listing reason.
func withReason(entries map[string]InboxEntry, kind, reason string) []string {
	var out []string
	for subject, e := range entries {
		if (kind == "" || e.Kind == kind) && slices.Contains(e.Reasons, reason) {
			out = append(out, subject)
		}
	}
	sort.Strings(out)
	return out
}

func inboxSorted(list []string) []string {
	out := append([]string{}, list...)
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func inboxSameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	g, w := inboxSorted(got), inboxSorted(want)
	if !slices.Equal(g, w) {
		t.Errorf("%s:\n entries %v\n today   %v", what, g, w)
	}
}

// ownUpdates is the account's own signed updates.get from the start.
func ownUpdates(t *testing.T, s *Store, key ed25519.PrivateKey) Result {
	t.Helper()
	return runBounded(t, s, signed(key, Command{Operation: "updates.get", Target: keyID(key), Limit: PageMax}))
}

// delivered is one account's queued pushes: webhook (event, reason) and MCP
// (subscription name, event key) pairs.
func delivered(t *testing.T, s *Store, key ed25519.PrivateKey) (webhooks, mcp map[string][]string) {
	t.Helper()
	webhooks, mcp = map[string][]string{}, map[string][]string{}
	rows, err := s.db.Query("SELECT d.body FROM webhook_deliveries d JOIN webhook_subscriptions s ON s.id=d.subscription WHERE s.account=? ORDER BY d.seq", keyID(key))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		var b struct {
			Reason string `json:"reason"`
			Event  struct {
				ID string `json:"id"`
			} `json:"event"`
		}
		if err = json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		webhooks[b.Reason] = append(webhooks[b.Reason], b.Event.ID)
	}
	rows.Close()
	rows, err = s.db.Query("SELECT s.name,d.event_id FROM mcp_event_deliveries d JOIN mcp_event_subscriptions s ON s.id=d.subscription WHERE s.account=? ORDER BY d.seq", keyID(key))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, event string
		if err = rows.Scan(&name, &event); err != nil {
			t.Fatal(err)
		}
		mcp[name] = append(mcp[name], event)
	}
	rows.Close()
	return webhooks, mcp
}

func noticeIDs(t *testing.T, r Result, key, field string) []string {
	t.Helper()
	list, _ := svcField(t, r.Data, key).([]any)
	var out []string
	for _, item := range list {
		out = append(out, fmt.Sprint(item.(map[string]any)[field]))
	}
	return out
}

// TestInboxEntriesParity is the step-1 parity test: for every account, the
// entries equal what each transport delivers today, reason by reason, and
// every difference between transports is listed here as the RFC found it.
func TestInboxEntriesParity(t *testing.T) {
	s := openInboxTest(t, InboxShadow)
	alice, bob, carol, dave := keyFor(211), keyFor(212), keyFor(213), keyFor(214)
	for _, k := range []ed25519.PrivateKey{alice, bob, carol, dave} {
		activeWebhook(t, s, keyID(k), "https://hooks.example.org/"+keyID(k)[:8])
		mcpSubscribe(t, s, keyID(k), "reply", "mention", "conversation.message", "conversation.request", "work.update", "identity.witnessed")
	}
	// bob's pending request, as updates.get shows it before he answers.
	sc := runInboxScript(t, s, true)

	// The expected entries, written out once by hand.
	type want struct {
		kind    string
		reasons string
		needs   bool
	}
	expect := map[string]map[string]want{
		keyID(alice): {
			sc.reply:          {"reply", "reply", false},
			sc.edit:           {"reply", "reply", false},
			sc.anonReply:      {"reply", "reply", false},
			sc.addressed:      {"addressed", "addressed", true},
			sc.triple:         {"reply", "reply,addressed,mention", true},
			sc.privateToAlice: {"addressed", "addressed", true},
			sc.hi:             {"conversation", "conversation", false},
		},
		keyID(bob): {
			sc.addressed: {"mention", "mention", true},
			sc.dm:        {"request", "request", true},
			sc.convReply: {"conversation", "conversation,reply,addressed", true},
		},
		keyID(carol): {
			sc.edit: {"mention", "mention", true},
		},
		keyID(dave): {},
	}
	for _, k := range []ed25519.PrivateKey{alice, bob, carol, dave} {
		got := entrySet(t, s, k)
		for subject, e := range got {
			if e.Kind == inboxWork || e.Kind == inboxWitness || e.Kind == inboxReceived || e.Kind == inboxWakeup {
				continue
			}
			w, ok := expect[keyID(k)][subject]
			if !ok {
				t.Errorf("%s: unexpected entry %+v", keyID(k)[:8], e)
				continue
			}
			if e.Kind != w.kind || strings.Join(e.Reasons, ",") != w.reasons || e.NeedsAnswer != w.needs || e.Stale || e.Disposition != "" || e.EventSeq < 0 || e.Kind != inboxRequest && e.EventSeq == 0 {
				t.Errorf("%s: entry %+v, want %+v", keyID(k)[:8], e, w)
			}
			if strings.Contains(e.Detail, "alice") || strings.Contains(e.Detail, "hello") {
				t.Errorf("an entry carries text: %+v", e)
			}
		}
		for subject := range expect[keyID(k)] {
			if _, ok := got[subject]; !ok {
				t.Errorf("%s: missing entry for %s", keyID(k)[:8], subject)
			}
		}
	}
	if e := entrySet(t, s, alice)[sc.anonReply]; e.Actor != "" {
		t.Errorf("an anonymous reply names no actor: %+v", e)
	}
	if e := entrySet(t, s, alice)[sc.edit]; e.Detail != `{"edit":true}` {
		t.Errorf("a new version says so: %+v", e)
	}

	for _, k := range []ed25519.PrivateKey{alice, bob, carol, dave} {
		name := map[string]string{keyID(alice): "alice", keyID(bob): "bob", keyID(carol): "carol", keyID(dave): "dave"}[keyID(k)]
		entries := entrySet(t, s, k)
		up := ownUpdates(t, s, k)

		// updates.get (own read): replies, addressed and mentions match
		// exactly, received and wake-up firings too.
		inboxSameSet(t, name+" updates.get replies", withReason(entries, "", inboxReply), ids(up, "replies"))
		inboxSameSet(t, name+" updates.get addressed", withReason(entries, "", inboxAddressed), ids(up, "addressed"))
		inboxSameSet(t, name+" updates.get mentions", withReason(entries, "", inboxMention), ids(up, "mentions"))
		inboxSameSet(t, name+" updates.get received", withReason(entries, inboxReceived, inboxReceived), noticeIDs(t, up, "received", "id"))
		var wakes []string
		for _, subject := range withReason(entries, inboxWakeup, inboxWakeup) {
			id, _, _ := strings.Cut(subject, "@")
			wakes = append(wakes, id)
		}
		inboxSameSet(t, name+" updates.get wakeups", wakes, noticeIDs(t, up, "wakeups", "id"))
		// Difference 1 (documented): a conversation's messages from before a
		// member accepted its request. updates.get lists them as the
		// member's conversation messages from then on (it reads the rooms
		// the member is active in now); the log holds the request, one
		// entry for the room, and no entry per earlier message.
		conversations := ids(up, "conversations")
		if name == "bob" {
			if !slices.Contains(conversations, sc.hello) || entries[sc.dm].Kind != inboxRequest {
				t.Fatalf("bob's pre-accept message: updates %v, request entry %+v", conversations, entries[sc.dm])
			}
			conversations = slices.DeleteFunc(slices.Clone(conversations), func(id string) bool { return id == sc.hello })
		}
		inboxSameSet(t, name+" updates.get conversations", withReason(entries, inboxConversation, inboxConversation), conversations)
		// Difference 2 (documented): work updates and witnesses reach pull
		// clients only as current state; updates.get lists neither.
		for _, kind := range []string{inboxWork, inboxWitness} {
			for _, subject := range withReason(entries, kind, kind) {
				if slices.Contains(up.messageIDs(), subject) {
					t.Errorf("%s: %s %s in updates.get", name, kind, subject)
				}
			}
		}

		webhooks, mcp := delivered(t, s, k)
		// Webhooks: one delivery per message, under its first reason, which
		// is the entry's kind (conversation entries: its first of reply,
		// addressed, conversation). A request is one entry per room and a
		// delivery per early message. Room activity is a webhook reason with
		// no entry (difference 3: not materialized, by design).
		var pushed []string
		for reason, events := range webhooks {
			if reason == "room_activity" {
				continue
			}
			for _, id := range events {
				if reason == inboxRequest {
					pushed = append(pushed, sc.dm+"|request")
					continue
				}
				pushed = append(pushed, id+"|"+reason)
			}
		}
		var fromEntries []string
		for subject, e := range entries {
			switch e.Kind {
			case inboxRequest:
				fromEntries = append(fromEntries, subject+"|request")
			case inboxConversation:
				first := inboxConversation
				for _, r := range []string{inboxReply, inboxAddressed} {
					if slices.Contains(e.Reasons, r) {
						first = r
						break
					}
				}
				fromEntries = append(fromEntries, subject+"|"+first)
			case inboxReply, inboxAddressed, inboxMention:
				fromEntries = append(fromEntries, subject+"|"+e.Kind)
			}
		}
		inboxSameSet(t, name+" webhooks", fromEntries, slices.Compact(inboxSorted(pushed)))

		// MCP Events: reply and mention (to, or an @handle) as entries, but a
		// new version notifies only the mentions it adds (difference 4:
		// updates.get, webhooks and the log carry the edited reply, MCP
		// Events does not). conversation.message for conversation entries,
		// conversation.request per early message. work.update and
		// identity.witnessed match the work and witness entries one to one,
		// but for the reviewer's entry, which MCP Events does not send
		// (difference 5: the log adds the reviewer of record). MCP Events
		// has no received or wake-up events (difference 6).
		var replies, mentions, convs, works, witnesses []string
		for subject, e := range entries {
			edit := e.Detail == `{"edit":true}`
			switch e.Kind {
			case inboxReply, inboxAddressed, inboxMention:
				if slices.Contains(e.Reasons, inboxReply) && !edit {
					replies = append(replies, subject)
				}
				if slices.Contains(e.Reasons, inboxMention) || slices.Contains(e.Reasons, inboxAddressed) && !edit {
					mentions = append(mentions, subject)
				}
			case inboxConversation:
				convs = append(convs, subject)
			case inboxWork:
				if !strings.Contains(e.Detail, `"role":"reviewer"`) {
					id, seq, _ := strings.Cut(subject, "@")
					works = append(works, "work:"+id+":"+seq)
				}
			case inboxWitness:
				witnesses = append(witnesses, subject)
			}
		}
		inboxSameSet(t, name+" mcp reply", replies, mcp["reply"])
		inboxSameSet(t, name+" mcp mention", mentions, mcp["mention"])
		inboxSameSet(t, name+" mcp conversation.message", convs, mcp["conversation.message"])
		var requests []string
		for range mcp["conversation.request"] {
			requests = append(requests, sc.dm)
		}
		inboxSameSet(t, name+" mcp conversation.request", withReason(entries, inboxRequest, inboxRequest), slices.Compact(inboxSorted(requests)))
		inboxSameSet(t, name+" mcp work.update", works, mcp["work.update"])
		inboxSameSet(t, name+" mcp identity.witnessed", witnesses, mcp["identity.witnessed"])
	}

	// The comparison above compared real traffic: alice's pushes, counted.
	if w, m := delivered(t, s, alice); len(w["reply"]) != 4 || len(w["addressed"]) != 2 || len(w["conversation"]) != 1 ||
		len(m["reply"]) != 3 || len(m["mention"]) != 3 || len(m["conversation.message"]) != 1 || len(m["work.update"]) != 1 || len(m["identity.witnessed"]) != 1 {
		t.Fatalf("alice's pushes: webhooks %v, MCP %v", w, m)
	}
	// The work and witness entries themselves.
	aliceEntries, carolEntries := entrySet(t, s, alice), entrySet(t, s, carol)
	if got := withReason(aliceEntries, inboxWork, inboxWork); len(got) != 1 || !strings.Contains(aliceEntries[got[0]].Detail, `"state":"claimed"`) || !strings.Contains(aliceEntries[got[0]].Detail, `"role":"requester"`) {
		t.Errorf("alice hears her work claimed: %v", got)
	}
	if got := withReason(carolEntries, inboxWork, inboxWork); len(got) != 1 || !strings.Contains(carolEntries[got[0]].Detail, `"state":"cancelled"`) || !strings.Contains(carolEntries[got[0]].Detail, `"role":"worker"`) {
		t.Errorf("carol hears the work she claimed cancelled: %v", got)
	}
	if got := withReason(aliceEntries, inboxWitness, inboxWitness); len(got) != 1 || aliceEntries[got[0]].Actor != keyID(dave) || aliceEntries[got[0]].Detail != `{"kind":"url","verdict":"verified"}` {
		t.Errorf("alice's witness entry: %v", got)
	}
	bobEntries := entrySet(t, s, bob)
	if bobEntries[sc.receiverItem].Kind != inboxReceived || len(withReason(bobEntries, inboxWakeup, inboxWakeup)) != 1 || len(withReason(aliceEntries, inboxWakeup, inboxWakeup)) != 1 {
		t.Errorf("received and wake-up entries: bob %+v, alice %v", bobEntries, withReason(aliceEntries, inboxWakeup, inboxWakeup))
	}
	// No entry ever holds a body: the whole table carries no word of the script.
	var leaked int
	if err := s.db.QueryRow("SELECT count(*) FROM inbox_entries WHERE reasons||subject||room||actor||detail LIKE '%alice%' OR reasons||subject||room||actor||detail LIKE '%job%'").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("entries carrying text: %d %v", leaked, err)
	}
}

// messageIDs is the ids of a result's messages.
func (r Result) messageIDs() []string {
	out := make([]string, len(r.Messages))
	for i, m := range r.Messages {
		out[i] = m.ID
	}
	return out
}

// With INBOX_ENTRIES off (the default) nothing is written, and the
// background backfill never starts.
func TestInboxEntriesOffWritesNothing(t *testing.T) {
	s := openInboxTest(t, InboxOff)
	runInboxScript(t, s, true)
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM inbox_entries").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d entries with the flag off (%v)", n, err)
	}
	if added, done, err := s.BackfillInboxEntries(testContext); added != 0 || !done || err != nil {
		t.Fatalf("backfill with the flag off: %d %v %v", added, done, err)
	}
	s.startInboxBackfill(testContext)
	if s.inboxBackfill.stop != nil {
		t.Fatal("the backfill started with the flag off")
	}
}

// The backfill derives, from what the board stores, the same entries the
// live hooks wrote (work entries aside: it does not derive them), and a
// second run adds nothing.
func TestInboxBackfillMatchesLiveAndIsIdempotent(t *testing.T) {
	s := openInboxTest(t, InboxShadow)
	runInboxScript(t, s, false)
	snapshot := func() []string {
		t.Helper()
		rows, err := s.db.Query("SELECT account,kind,reasons,subject,room,actor,detail,needs_answer,disposition FROM inbox_entries WHERE kind<>'work' ORDER BY account,kind,subject")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var account, kind, reasons, subject, room, actor, detail, disposition string
			var needs bool
			if err = rows.Scan(&account, &kind, &reasons, &subject, &room, &actor, &detail, &needs, &disposition); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(account[:8], kind, reasons, subject, room, actor, detail, needs, disposition))
		}
		return out
	}
	live := snapshot()
	if len(live) < 10 {
		t.Fatalf("the script wrote %d entries", len(live))
	}
	if _, err := s.db.Exec("DELETE FROM inbox_entries"); err != nil {
		t.Fatal(err)
	}
	backfill := func() int {
		t.Helper()
		s.restartInboxBackfill()
		total := 0
		for pass := 0; ; pass++ {
			added, done, err := s.BackfillInboxEntries(testContext)
			if err != nil {
				t.Fatal(err)
			}
			total += added
			if done {
				return total
			}
			if pass > 100 {
				t.Fatal("the backfill does not finish")
			}
		}
	}
	if n := backfill(); n != len(live) {
		t.Errorf("the backfill added %d entries, the live hooks wrote %d", n, len(live))
	}
	if got := snapshot(); !slices.Equal(got, live) {
		t.Fatalf("backfilled entries differ from the live ones:\n live %v\n back %v", live, got)
	}
	if n := backfill(); n != 0 {
		t.Fatalf("a second backfill added %d entries", n)
	}
	// Outside the window nothing is derived: 31 days on, an empty log
	// stays empty.
	if _, err := s.db.Exec("DELETE FROM inbox_entries"); err != nil {
		t.Fatal(err)
	}
	svcSetNow(s, testTime+(InboxBackfillDays+1)*86400)
	if n := backfill(); n != 0 {
		t.Fatalf("the backfill reached past %d days: %d entries", InboxBackfillDays, n)
	}
}

// Rows per post are bounded: a conversation message makes one entry per
// other active member, a lobby post none for its past posters, and every
// entry turns stale (never deleted) after InboxStaleDays.
func TestInboxEntriesBoundedAndStale(t *testing.T) {
	s := openInboxTest(t, InboxShadow)
	owner := keyFor(230)
	members := []ed25519.PrivateKey{}
	for i := byte(231); i < 237; i++ {
		members = append(members, keyFor(i))
	}
	registerAll(t, s, append([]ed25519.PrivateKey{owner}, members...)...)
	for _, m := range members {
		runBounded(t, s, signed(m, Command{Operation: "post", Room: "lobby", Text: "I was here"}))
	}
	lobby := runBounded(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "to the room"})).Receipt.ID
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM inbox_entries WHERE subject=?", lobby).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a lobby post made %d entries (%v)", n, err)
	}
	group := convRoom("inbox-group")
	runBounded(t, s, openCmd(owner, group, "group", members...))
	for _, m := range members {
		runBounded(t, s, respond(m, group, "accept"))
	}
	msg := runBounded(t, s, signed(owner, Command{Operation: "post", Room: group, Text: "to the group", Visibility: "private"})).Receipt.ID
	if err := s.db.QueryRow("SELECT count(*) FROM inbox_entries WHERE subject=? AND kind='conversation'", msg).Scan(&n); err != nil || n != len(members) {
		t.Fatalf("a group message made %d entries for %d other members (%v)", n, len(members), err)
	}
	if n > RoomMembersMax+1 {
		t.Fatal("unbounded")
	}
	list, err := s.InboxShadow(testContext, keyID(members[0]), 0)
	if err != nil || len(list) == 0 || list[len(list)-1].Stale {
		t.Fatalf("fresh entries: %+v %v", list, err)
	}
	svcSetNow(s, testTime+InboxStaleDays*86400+1)
	list, err = s.InboxShadow(testContext, keyID(members[0]), 0)
	if err != nil || len(list) == 0 || !list[len(list)-1].Stale {
		t.Fatalf("entries older than %d days are stale, and kept: %+v %v", InboxStaleDays, list, err)
	}
}

// The resolver and the writers run only in the producer's transaction: no
// function on the write path names the store's pool (one SQLite
// connection: the pool under a held tx deadlocks).
func TestInboxWritePathNeverUsesThePool(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "inbox.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The two that run outside any transaction: the operator read, and the
	// backfill pass, which opens its own.
	outside := map[string]bool{"InboxShadow": true, "BackfillInboxEntries": true}
	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || outside[fn.Name.Name] {
			continue
		}
		checked++
		ast.Inspect(fn, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "db" {
				t.Errorf("%s uses the pool (.db)", fn.Name.Name)
			}
			return true
		})
	}
	if checked < 10 {
		t.Fatalf("checked %d functions", checked)
	}
}

func TestInboxEntriesFlag(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "INBOX_ENTRIES" {
				return v
			}
			return ""
		}
	}
	for v, want := range map[string]InboxMode{"": InboxOff, "off": InboxOff, "shadow": InboxShadow} {
		f, err := ParseFeatures(env(v))
		if err != nil || f.InboxEntries != want {
			t.Fatalf("INBOX_ENTRIES=%q: %v %v", v, f.InboxEntries, err)
		}
	}
	for _, v := range []string{"on", "true", "Shadow"} {
		if _, err := ParseFeatures(env(v)); err == nil || !strings.Contains(err.Error(), "INBOX_ENTRIES must be off or shadow") {
			t.Fatalf("INBOX_ENTRIES=%q: %v", v, err)
		}
	}
}
