package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mentionReasons is each webhook delivery's message ID and reason for one
// subscription, in queue order.
func mentionReasons(t *testing.T, s *Store, subscription string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, row := range webhookRows(t, s) {
		if row["subscription"] != subscription {
			continue
		}
		var body struct {
			Reason string `json:"reason"`
			Event  struct {
				ID string `json:"id"`
			} `json:"event"`
		}
		if err := json.Unmarshal([]byte(row["body"]), &body); err != nil {
			t.Fatal(err)
		}
		// The message it is about: the queue's key is the message id, or
		// under INBOX_ENTRIES=read the entry id.
		out[body.Event.ID] = body.Reason
	}
	return out
}

func handleKey(t *testing.T, s *Store, n byte, handle string) ed25519.PrivateKey {
	t.Helper()
	key := keyFor(n)
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: handle}))
	return key
}

// An @handle naming a registered agent reaches its inbox as an addressed
// message does: updates.get lists it under mentions and a webhook carries
// reason mention. Unknown handles, the author's own, code spans, a private
// room's non-members and hidden messages notify no one; at most
// MentionsMax agents per message; an edit notifies only the mentions it adds.
func TestMentionsReachUpdatesAndWebhooks(t *testing.T) {
	forInboxModes(t, testMentionsReachUpdatesAndWebhooks)
}

func testMentionsReachUpdatesAndWebhooks(t *testing.T, mode InboxMode) {
	s := openTest(t, withInbox(updatesConfig(), mode))
	alice := handleKey(t, s, 1, "alice")
	bob := handleKey(t, s, 2, "bob")
	carol := handleKey(t, s, 3, "carol")
	me := keyID(alice)
	hook, _ := activeWebhook(t, s, me, "https://hooks.example.org/alice")
	saved := run(t, s, Command{Operation: "updates.get", Target: me})
	post := func(key ed25519.PrivateKey, room, text, data string) string {
		t.Helper()
		return run(t, s, signed(key, Command{Operation: "post", Room: room, Text: text, Data: data})).Receipt.ID
	}
	mentioned := func(id string) int64 {
		return sqlCount(t, s, "SELECT count(*) FROM post_mentions WHERE root=?", id)
	}

	// Case-insensitive; the unknown handle is ignored.
	hello := post(bob, "lobby", "hey @Alice, and @nobody-here", "")
	if mentioned(hello) != 1 {
		t.Fatalf("hello mentions %d", mentioned(hello))
	}
	// Underscore emphasis notifies like *@handle* (NewBotLabor 70bc7b8b).
	handleKey(t, s, 4, "dave")
	if em := post(bob, "lobby", "thanks _@dave_ for it", ""); mentioned(em) != 1 {
		t.Fatalf("_@dave_ mentions %d", mentioned(em))
	}
	// The author's own handle, a code span and a fenced block mention no one.
	self := post(alice, "scratch", "note to @alice", "")
	code := post(bob, "lobby", "the syntax is `@alice`\n```\n@alice\n```", "")
	if mentioned(self) != 0 || mentioned(code) != 0 {
		t.Fatalf("self %d, code %d", mentioned(self), mentioned(code))
	}
	// A private room's message mentions only its members.
	run(t, s, signed(bob, Command{Operation: "room.create", Room: "den", Visibility: "private", Members: []string{keyID(carol)}}))
	private := post(bob, "den", "@alice @carol see this", "")
	if mentioned(private) != 1 || sqlCount(t, s, "SELECT count(*) FROM post_mentions WHERE root=? AND account=?", private, keyID(carol)) != 1 {
		t.Fatalf("private room mentions: %d", mentioned(private))
	}
	// At most MentionsMax agents, the first ones written.
	var many []string
	for i := 0; i < MentionsMax+2; i++ {
		handleKey(t, s, byte(40+i), fmt.Sprintf("m%d", i))
		many = append(many, fmt.Sprintf("@m%d", i))
	}
	crowd := post(bob, "crowd", strings.Join(many, " "), "")
	if n := mentioned(crowd); n != MentionsMax || sqlCount(t, s, "SELECT count(*) FROM post_mentions WHERE root=? AND account=?", crowd, keyID(keyFor(byte(40+MentionsMax)))) != 0 {
		t.Fatalf("cap: %d mentions", n)
	}
	// Hidden: it notifies no one, and its queued delivery is dropped.
	hidden := post(bob, "lobby", "@alice click here", "")
	if mentionReasons(t, s, hook)[hidden] != "mention" {
		t.Fatal("the hidden message was queued before its hide")
	}
	if err := s.Moderate(testContext, hidden, "test hide", true); err != nil {
		t.Fatal(err)
	}
	// Edits: a mention added by an edit notifies once, on that version; the
	// next version keeps it and notifies nothing new.
	draft := post(bob, "lobby", "draft", "")
	edit1 := post(bob, "lobby", "draft for @alice", dataJSON(`"supersedes":"`+draft+`"`))
	edit2 := post(bob, "lobby", "draft for @alice and @carol", dataJSON(`"supersedes":"`+edit1+`"`))
	if mentioned(draft) != 2 || sqlCount(t, s, "SELECT count(*) FROM post_mentions WHERE root=? AND account=? AND event_id=?", draft, me, edit1) != 1 {
		t.Fatalf("edit mentions: %d", mentioned(draft))
	}

	back := run(t, s, Command{Operation: "updates.get", Target: me, Cursor: saved.NextCursor})
	if got := ids(back, "mentions"); strings.Join(got, ",") != hello+","+edit1 {
		t.Fatalf("updates mentions %v, want [%s %s]", got, hello, edit1)
	}
	for _, e := range back.Messages {
		if e.ID == hidden || e.ID == private || e.ID == code || e.ID == edit2 || e.ID == draft {
			t.Fatalf("updates returned %s, which does not concern alice", e.ID)
		}
	}
	if slicesContain(ids(back, "room_activity"), hello) {
		t.Fatal("a mention is not also room activity")
	}
	reasons := mentionReasons(t, s, hook)
	if len(reasons) != 2 || reasons[hello] != "mention" || reasons[edit1] != "mention" {
		t.Fatalf("webhook reasons %v", reasons)
	}
	// The message page links the mentions of registered agents only.
	if got := run(t, s, Command{Operation: "message.get", MessageID: hello}).Messages[0].MentionAgents; len(got) != 1 || got["alice"] != me {
		t.Fatalf("mention links %v", got)
	}
}

// The same mention reaches an MCP Events mention subscription, once, and an
// edit that adds it notifies once.
func TestMentionsReachMCPEvents(t *testing.T) { forInboxModes(t, testMentionsReachMCPEvents) }

func testMentionsReachMCPEvents(t *testing.T, mode InboxMode) {
	s, r := mcpEventStore(t)
	setInboxMode(s, mode)
	alice, _, _ := hostedPrincipal(t, s, "alice-events")
	if _, err := s.SubscribeMCPEvent(testContext, alice, subscribeRequest(t, "mention", nil, "https://example.com/hook", r.secret)); err != nil {
		t.Fatal(err)
	}
	bob := keyFor(71)
	ping := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "ping @Alice-Events"})).Receipt.ID
	draft := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "draft"})).Receipt.ID
	edit1 := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "draft @alice-events", Data: dataJSON(`"supersedes":"` + draft + `"`)})).Receipt.ID
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "draft @alice-events again", Data: dataJSON(`"supersedes":"` + edit1 + `"`)}))
	drain(t, s)
	var got []string
	for _, e := range r.events() {
		data, _ := e.body["data"].(map[string]any)
		if e.body["name"] != "mention" {
			t.Fatalf("event %s", e.raw)
		}
		got = append(got, data["message_id"].(string))
	}
	if strings.Join(got, ",") != ping+","+edit1 {
		t.Fatalf("mention events %v, want [%s %s]", got, ping, edit1)
	}
}

// Wake-ups on mention read the same parse: a code span does not wake.
func TestMentionWakeupUsesTheSameParse(t *testing.T) {
	forInboxModes(t, testMentionWakeupUsesTheSameParse)
}

func testMentionWakeupUsesTheSameParse(t *testing.T, mode InboxMode) {
	s := openWakeTest(t, "wakeup")
	setInboxMode(s, mode)
	wakeWork := wakeFired
	alice := handleKey(t, s, 1, "alice")
	bob := handleKey(t, s, 2, "bob")
	wakeWork(t, s)
	run(t, s, svcCall(alice, "wakeup", "schedule", map[string]any{"key": "m", "on": "mention"}, 1, "w-m"))
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "type `@alice` to mention"}))
	if n := wakeWork(t, s); n != 0 {
		t.Fatalf("a code span woke: %d", n)
	}
	mention := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "@ALICE look"})).Receipt.ID
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("the mention fired %d", n)
	}
	back := run(t, s, Command{Operation: "updates.get", Target: keyID(alice)})
	if !slicesContain(ids(back, "mentions"), mention) {
		t.Fatalf("the wake-up's read lists the mention: %v", back.Data["mentions"])
	}
}

// Unknown @words before a registered handle never use up the mention, however
// many there are (C101): the registered handle is mentioned and linked, the
// author's own still skipped, and the first MentionsMax registered ones win.
func TestMentionAfterManyUnknownHandles(t *testing.T) {
	s := openTest(t, updatesConfig())
	alice := handleKey(t, s, 1, "alice")
	bob := handleKey(t, s, 2, "bob")
	carol := handleKey(t, s, 3, "carol")
	var unknown []string
	for i := 0; i < 450; i++ {
		unknown = append(unknown, fmt.Sprintf("@ghost%d", i))
	}
	text := strings.Join(unknown, " ") + " @bob @alice_ and @carol"
	id := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: text})).Receipt.ID
	for _, k := range []ed25519.PrivateKey{alice, carol} {
		if sqlCount(t, s, "SELECT count(*) FROM post_mentions WHERE root=? AND account=?", id, keyID(k)) != 1 {
			t.Fatalf("%s is not mentioned after %d unknown handles", keyID(k), len(unknown))
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM post_mentions WHERE root=?", id); n != 2 {
		t.Fatalf("mentions %d, want alice and carol (the author's own skipped)", n)
	}
	got := run(t, s, Command{Operation: "message.get", MessageID: id}).Messages[0].MentionAgents
	if len(got) != 3 || got["alice"] != keyID(alice) || got["carol"] != keyID(carol) || got["bob"] != keyID(bob) {
		t.Fatalf("mention links %v", got)
	}
	// The resolution itself: the first MentionsMax registered handles in order.
	var named []string
	for i := 0; i < MentionsMax+1; i++ {
		handleKey(t, s, byte(60+i), fmt.Sprintf("r%d", i))
		named = append(named, fmt.Sprintf("@r%d", i))
	}
	accounts, err := resolveMentions(testContext, s.db, strings.Join(unknown, " ")+" "+strings.Join(named, " "), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != MentionsMax || accounts[0] != keyID(keyFor(60)) || accounts[MentionsMax-1] != keyID(keyFor(byte(60+MentionsMax-1))) {
		t.Fatalf("resolved %v", accounts)
	}
}

// An edit that adds a mention wakes the newly mentioned agent once, on that
// version (C100); a later version that keeps the mention, or an edit of a
// message that already made it, wakes no one again, and an edit wakes no
// room wake-up.
func TestMentionWakeupFiresOnTheEditThatAddsIt(t *testing.T) {
	forInboxModes(t, testMentionWakeupFiresOnTheEditThatAddsIt)
}

func testMentionWakeupFiresOnTheEditThatAddsIt(t *testing.T, mode InboxMode) {
	s := openWakeTest(t, "wakeup")
	setInboxMode(s, mode)
	wakeWork := wakeFired
	alice := handleKey(t, s, 1, "alice")
	bob := handleKey(t, s, 2, "bob")
	carol := handleKey(t, s, 3, "carol")
	wakeWork(t, s)
	schedule := func(key ed25519.PrivateKey, name string, args map[string]any) {
		t.Helper()
		args["key"] = name
		run(t, s, svcCall(key, "wakeup", "schedule", args, 1, "w-"+name))
	}
	edit := func(prev, text string) string {
		t.Helper()
		return run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: text, Data: dataJSON(`"supersedes":"` + prev + `"`)})).Receipt.ID
	}
	schedule(alice, "m1", map[string]any{"on": "mention"})
	draft := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "draft"})).Receipt.ID
	if n := wakeWork(t, s); n != 0 {
		t.Fatalf("no mention yet: %d", n)
	}
	schedule(carol, "r", map[string]any{"on": "room", "room": "lobby"})
	edit1 := edit(draft, "draft for @alice")
	if n := wakeWork(t, s); n != 1 {
		t.Fatalf("the edit adding @alice (and no room wake-up) fired %d", n)
	}
	page := run(t, s, svcRead(alice, "wakeup", "notices", map[string]any{}))
	notices := svcField(t, page.Data, "result", "notices").([]any)
	if len(notices) != 1 || svcField(t, notices[0], "on") != "mention" || svcField(t, notices[0], "event") != edit1 {
		t.Fatalf("notices %+v, want the mention on %s", notices, edit1)
	}
	schedule(alice, "m2", map[string]any{"on": "mention"})
	edit(edit1, "draft for @alice, revised")
	if n := wakeWork(t, s); n != 0 {
		t.Fatalf("a version keeping the mention fired %d", n)
	}
	original := run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "hi @alice"})).Receipt.ID
	if n := wakeWork(t, s); n != 2 {
		t.Fatalf("the original mention wakes m2 and the room wake-up: %d", n)
	}
	schedule(alice, "m3", map[string]any{"on": "mention"})
	edit(original, "hi again @alice")
	if n := wakeWork(t, s); n != 0 {
		t.Fatalf("an edit of a message that already mentioned alice fired %d", n)
	}
}
