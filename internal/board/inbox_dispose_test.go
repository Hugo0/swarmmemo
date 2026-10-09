package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// C61 step 3 (C71): dispositions. Under INBOX_ENTRIES=read an agent marks
// entries of its own inbox; replies, accepts and verdicts mark them by
// themselves; data.waiting and journal.get's unanswered list what is left.
// Under off and shadow nothing an agent reads changes.

func disposeCmd(key ed25519.PrivateKey, state string, ids ...string) Command {
	raw, _ := json.Marshal(map[string]any{"schema": 1, "ids": ids, "state": state})
	return signed(key, Command{Operation: "updates.dispose", Data: string(raw)})
}

func waitingOf(t *testing.T, s *Store, key ed25519.PrivateKey) int64 {
	t.Helper()
	r := runBounded(t, s, signed(key, Command{Operation: "updates.get", Target: keyID(key), Data: `{"schema":1,"counts":true}`}))
	n, ok := r.Data["waiting"].(int64)
	if !ok {
		t.Fatalf("no data.waiting: %+v", r.Data)
	}
	return n
}

// unansweredOf is journal.get's open_work.unanswered as subject ids.
func unansweredOf(t *testing.T, s *Store, key ed25519.PrivateKey) []string {
	t.Helper()
	r := runBounded(t, s, signed(key, Command{Operation: "journal.get", Limit: 1}))
	items := r.Data["briefing"].(map[string]any)["open_work"].(map[string]any)["unanswered"].(map[string]any)["items"].([]map[string]any)
	out := []string{}
	for _, it := range items {
		out = append(out, it["id"].(string))
	}
	slices.Sort(out)
	return out
}

func disposeCode(t *testing.T, s *Store, c Command) string {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("%s: want an error, got %v", c.Operation, err)
	}
	return e.Code
}

func sortedIDs(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func TestInboxDispositions(t *testing.T) {
	s := openInboxTest(t, InboxRead)
	alice, bob, carol, dave := keyFor(241), keyFor(242), keyFor(243), keyFor(244)
	for i, k := range []ed25519.PrivateKey{alice, bob, carol, dave} {
		runBounded(t, s, signed(k, Command{Operation: "agent.register", Handle: []string{"dalice", "dbob", "dcarol", "ddave"}[i]}))
	}
	post := func(key ed25519.PrivateKey, c Command) string { return runBounded(t, s, signed(key, c)).Receipt.ID }

	// Three things wait for alice: a message addressed to her (then edited),
	// a mention, and a public reply that needs nothing.
	root := post(alice, Command{Operation: "post", Room: "lobby", Text: "root by alice"})
	post(bob, Command{Operation: "post", Room: "lobby", ReplyTo: root, Text: "a plain reply"})
	asked := post(carol, Command{Operation: "post", Room: "lobby", To: keyID(alice), Text: "alice, a question"})
	askedAgain := post(carol, Command{Operation: "post", Room: "lobby", To: keyID(alice), Text: "alice, a better question", Data: dataJSON(`"supersedes":"` + asked + `"`)})
	mention := post(dave, Command{Operation: "post", Room: "lobby", Text: "what does @dalice think"})
	if n := waitingOf(t, s, alice); n != 2 {
		t.Fatalf("alice waits on %d, want 2 (the newest version of carol's question, dave's mention)", n)
	}
	if got := unansweredOf(t, s, alice); !slices.Equal(got, sortedIDs(askedAgain, mention)) {
		t.Fatalf("alice's unanswered: %v", got)
	}

	// Replying to the first version answers every version.
	post(alice, Command{Operation: "post", Room: "lobby", ReplyTo: asked, Text: "an answer"})
	if n := waitingOf(t, s, alice); n != 1 {
		t.Fatalf("after a reply alice waits on %d", n)
	}
	for _, e := range entrySet(t, s, alice) {
		if (e.Subject == asked || e.Subject == askedAgain) && e.Disposition != DispositionReplied {
			t.Fatalf("a replied version: %+v", e)
		}
	}
	// Own read: data.entries carries the disposition; another reader's never.
	own := runBounded(t, s, signed(alice, Command{Operation: "updates.get", Target: keyID(alice), Cursor: "start"}))
	if !slices.ContainsFunc(own.Data["entries"].([]UpdateEntry), func(e UpdateEntry) bool { return e.Subject == askedAgain && e.Disposition == DispositionReplied }) {
		t.Fatalf("own entries lack the disposition: %+v", own.Data["entries"])
	}
	other := runBounded(t, s, signed(carol, Command{Operation: "updates.get", Target: keyID(alice), Cursor: "start"}))
	if _, ok := other.Data["waiting"]; ok {
		t.Fatal("another reader sees data.waiting")
	}
	for _, e := range other.Data["entries"].([]UpdateEntry) {
		if e.Disposition != "" {
			t.Fatalf("another reader sees a disposition: %+v", e)
		}
	}

	// By message id: answered elsewhere. A reply after it changes nothing.
	r := runBounded(t, s, disposeCmd(alice, DispositionAnsweredElsewhere, mention))
	if r.Data["waiting"].(int64) != 0 || r.Data["changed"].(int64) != 1 {
		t.Fatalf("dispose: %+v", r.Data)
	}
	if again := runBounded(t, s, disposeCmd(alice, DispositionAnsweredElsewhere, mention)); again.Data["changed"].(int64) != 0 {
		t.Fatalf("a repeat changes nothing: %+v", again.Data)
	}
	post(alice, Command{Operation: "post", Room: "lobby", ReplyTo: mention, Text: "late reply"})
	var mentionEntry InboxEntry
	for _, e := range entrySet(t, s, alice) {
		if e.Subject == mention {
			mentionEntry = e
		}
	}
	if mentionEntry.Disposition != DispositionAnsweredElsewhere {
		t.Fatalf("a manual state was overwritten: %+v", mentionEntry)
	}
	if got := unansweredOf(t, s, alice); len(got) != 0 {
		t.Fatalf("nothing should wait: %v", got)
	}
	// By entry id: open undoes, and stays open through a later reply.
	runBounded(t, s, disposeCmd(alice, DispositionOpen, mentionEntry.ID))
	post(alice, Command{Operation: "post", Room: "lobby", ReplyTo: mention, Text: "another reply"})
	if n := waitingOf(t, s, alice); n != 1 {
		t.Fatalf("a reopened entry waits: %d", n)
	}
	runBounded(t, s, disposeCmd(alice, DispositionClosure, mentionEntry.ID))

	// Refusals.
	if code := disposeCode(t, s, disposeCmd(alice, "done", mention)); code != "invalid_disposition" {
		t.Fatalf("a bad state: %s", code)
	}
	if code := disposeCode(t, s, disposeCmd(alice, DispositionClosure, "nothing-here")); code != "entry_not_found" {
		t.Fatalf("an unknown id: %s", code)
	}
	if code := disposeCode(t, s, disposeCmd(carol, DispositionClosure, mentionEntry.ID)); code != "entry_not_found" {
		t.Fatalf("someone else's entry: %s", code)
	}
	target := disposeCmd(alice, DispositionClosure, mention)
	target.Target, target.Signature = keyID(carol), ""
	if code := disposeCode(t, s, signed(alice, target)); code != "own_inbox_only" {
		t.Fatalf("another agent's inbox: %s", code)
	}
	unsigned := Command{Operation: "updates.dispose", Data: `{"schema":1,"ids":["x"],"state":"closure"}`, RequestID: "unsigned-dispose"}
	if code := disposeCode(t, s, unsigned); code != "signature_required" {
		t.Fatalf("unsigned: %s", code)
	}
	if code := disposeCode(t, s, signed(alice, Command{Operation: "updates.dispose", Data: `{"schema":1,"ids":[],"state":"closure"}`})); code != "invalid_request" {
		t.Fatalf("no ids: %s", code)
	}

	// A request waits until it is accepted (replied) or declined.
	room := convRoom("dispose-req")
	runBounded(t, s, openCmd(alice, room, "dm", bob))
	if n := waitingOf(t, s, bob); n != 1 {
		t.Fatalf("bob's request waits: %d", n)
	}
	if got := unansweredOf(t, s, bob); !slices.Equal(got, []string{room}) {
		t.Fatalf("bob's unanswered: %v", got)
	}
	runBounded(t, s, respond(bob, room, "decline"))
	if n := waitingOf(t, s, bob); n != 0 {
		t.Fatalf("a declined request waits: %d", n)
	}
	if e := entrySet(t, s, bob)[room]; e.Disposition != DispositionDeclined {
		t.Fatalf("the declined request: %+v", e)
	}

	// Work submitted for review waits for the requester until its verdict.
	work := createTestWork(t, s, alice, "lobby", "request", 0)
	runBounded(t, s, workCommand(s, dave, Command{Operation: "work.claim", MessageID: work, TTL: 600}))
	result := post(dave, Command{Operation: "post", Room: "lobby", ReplyTo: work, Text: "the result"})
	runBounded(t, s, workCommand(s, dave, Command{Operation: "work.submit", MessageID: work, Amount: 1, Target: result}))
	if n := waitingOf(t, s, alice); n != 1 {
		t.Fatalf("a submitted result waits for review: %d", n)
	}
	runBounded(t, s, workCommand(s, alice, Command{Operation: "work.accept", MessageID: work, Amount: 1}))
	if n := waitingOf(t, s, alice); n != 0 {
		t.Fatalf("a verdict closes the review: %d", n)
	}

	// /stats counts.
	st, err := s.InboxStats(testContext)
	if err != nil || st == nil || st.Dispositions[DispositionReplied] == 0 || st.Dispositions[DispositionDeclined] != 1 || st.Dispositions[DispositionClosure] < 2 || st.Entries[inboxRequest] != 1 {
		t.Fatalf("inbox stats: %+v %v", st, err)
	}
}

// Under off and shadow, updates.get and journal.get answer exactly as
// before: no data.waiting, the unanswered list by the old rule, and
// updates.dispose is unavailable. On the same rows (written in shadow, with
// the automatic dispositions), off and shadow answer the same.
func TestInboxDispositionsOffAndShadowUnchanged(t *testing.T) {
	s := openInboxTest(t, InboxShadow)
	sc := runInboxScript(t, s, true)
	// A reply that disposes an entry in the log, and a mention left open.
	runBounded(t, s, signed(sc.alice, Command{Operation: "post", Room: "lobby", ReplyTo: sc.addressed, Text: "answering carol"}))
	if e := entrySet(t, s, sc.alice)[sc.addressed]; e.Disposition != DispositionReplied {
		t.Fatalf("shadow writes the automatic disposition: %+v", e)
	}
	views := map[InboxMode]string{}
	for _, mode := range []InboxMode{InboxOff, InboxShadow} {
		setInboxMode(s, mode)
		if code := disposeCode(t, s, disposeCmd(sc.alice, DispositionClosure, sc.triple)); code != "service_unavailable" {
			t.Fatalf("%v: updates.dispose: %s", mode, code)
		}
		view := map[string]any{}
		for _, k := range []ed25519.PrivateKey{sc.alice, sc.bob, sc.carol} {
			u := runBounded(t, s, signed(k, Command{Operation: "updates.get", Target: keyID(k), Cursor: "start"}))
			if _, ok := u.Data["waiting"]; ok {
				t.Fatalf("%v: data.waiting", mode)
			}
			j := runBounded(t, s, signed(k, Command{Operation: "journal.get"}))
			un := j.Data["briefing"].(map[string]any)["open_work"].(map[string]any)["unanswered"]
			// The old rule, run directly, answers the same.
			tx, err := s.db.BeginTx(testContext, nil)
			if err != nil {
				t.Fatal(err)
			}
			legacy, more, err := s.journalUnanswered(testContext, tx, actor{id: keyID(k), account: keyID(k), signed: true}, s.now().Unix())
			tx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"items": legacy, "has_more": more}
			a, _ := json.Marshal(un)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("%v: journal unanswered\n got  %s\n want %s", mode, a, b)
			}
			view[keyID(k)] = []any{legacyView(t, u), string(a)}
		}
		raw, _ := json.Marshal(view)
		views[mode] = string(raw)
	}
	setInboxMode(s, InboxShadow)
	if views[InboxOff] != views[InboxShadow] {
		t.Fatalf("off and shadow answer differently:\n off    %s\n shadow %s", views[InboxOff], views[InboxShadow])
	}
}
