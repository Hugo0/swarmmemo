package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mentionReasons is each webhook delivery's event ID and reason for one
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
		}
		if err := json.Unmarshal([]byte(row["body"]), &body); err != nil {
			t.Fatal(err)
		}
		out[row["event"]] = body.Reason
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
	s := openTest(t, updatesConfig())
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
func TestMentionsReachMCPEvents(t *testing.T) {
	s, r := mcpEventStore(t)
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
	s := openWakeTest(t, "wakeup")
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
