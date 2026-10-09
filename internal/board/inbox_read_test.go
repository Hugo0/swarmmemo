package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/services"
)

// C61 step 2: reads over the inbox entry log (INBOX_ENTRIES=read). One store
// plays the scripted mix in shadow; the same reads then run under shadow
// and under read on the same rows, and must answer the same messages and the
// same data fields, but for data.entries (new) and the cursor (v2).

func setInboxMode(s *Store, m InboxMode) { s.config.Features.InboxEntries = m }

// readIn runs c with the store's inbox mode set to m.
func readIn(t *testing.T, s *Store, m InboxMode, c Command) Result {
	t.Helper()
	setInboxMode(s, m)
	defer setInboxMode(s, InboxShadow)
	return runBounded(t, s, c)
}

// legacyView is a result as a client of the fields that exist today sees
// it: the messages and every data field but entries, as JSON.
func legacyView(t *testing.T, r Result) string {
	t.Helper()
	data := map[string]any{}
	for k, v := range r.Data {
		if k != "entries" && k != "waiting" {
			data[k] = v
		}
	}
	raw, err := json.Marshal(map[string]any{"messages": r.Messages, "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func sameLegacyView(t *testing.T, what string, shadow, read Result) {
	t.Helper()
	if a, b := legacyView(t, shadow), legacyView(t, read); a != b {
		t.Errorf("%s: shadow and read differ\n shadow %s\n read   %s", what, a, b)
	}
}

// updateEntries is data.entries as kind|subject pairs.
func updateEntries(t *testing.T, r Result) []string {
	t.Helper()
	list, ok := r.Data["entries"].([]UpdateEntry)
	if !ok {
		t.Fatalf("no data.entries: %+v", r.Data)
	}
	out := []string{}
	for _, e := range list {
		out = append(out, e.Kind+"|"+e.Subject)
	}
	return out
}

func entryKinds(t *testing.T, r Result) []string {
	t.Helper()
	var kinds []string
	for _, e := range updateEntries(t, r) {
		kind, _, _ := strings.Cut(e, "|")
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// updatesReader is one way to read an agent's updates.
type updatesReader struct {
	name string
	cmd  func(cursor string) Command
}

func updatesReaders(sc inboxScript) []updatesReader {
	ownRead := func(key ed25519.PrivateKey) func(string) Command {
		return func(cursor string) Command {
			return signed(key, Command{Operation: "updates.get", Target: keyID(key), Cursor: cursor, Limit: PageMax})
		}
	}
	readers := []updatesReader{
		{"alice", ownRead(sc.alice)}, {"bob", ownRead(sc.bob)},
		{"carol", ownRead(sc.carol)}, {"dave", ownRead(sc.dave)},
		// carol reading alice's updates, and an anonymous reader.
		{"carol on alice", func(cursor string) Command {
			return signed(sc.carol, Command{Operation: "updates.get", Target: keyID(sc.alice), Cursor: cursor, Limit: PageMax})
		}},
		{"anonymous on alice", func(cursor string) Command {
			return Command{Operation: "updates.get", Target: keyID(sc.alice), Cursor: cursor, Limit: PageMax}
		}},
	}
	return readers
}

// receiveFor delivers one item to a new receiver of key's.
func receiveFor(t *testing.T, s *Store, key ed25519.PrivateKey, tag string) string {
	t.Helper()
	created := runBounded(t, s, svcCall(key, "receiver", "create", map[string]any{"label": tag, "screen": false}, 5, "c61r-"+tag))
	url, _ := svcField(t, created.Data, "result", "url").(string)
	parts := strings.Split(url[strings.Index(url, "/in/")+4:], "/")
	receipt, err := s.Receive(testContext, services.Delivery{ID: parts[0], Token: parts[1], ContentType: "application/json", Body: []byte(`{"n":2}`), Source: net.ParseIP("198.51.100.62")})
	if err != nil {
		t.Fatal(err)
	}
	return receipt.Item
}

// TestInboxReadParity is step 2's parity test: for seeded traffic every
// reader gets the same messages and data fields under shadow and under
// read, from no cursor, from "start", and from each mode's own cursor after
// more traffic; an old cursor keeps working under read; and read lists
// what pull clients never saw before (work updates, witnesses) in
// data.entries, once.
func TestInboxReadParity(t *testing.T) {
	s := openInboxTest(t, InboxShadow)
	sc := runInboxScript(t, s, true)
	readers := updatesReaders(sc)
	legacy, v2 := map[string]string{}, map[string]string{}
	for _, r := range readers {
		for _, cursor := range []string{"", "start"} {
			shadow := readIn(t, s, InboxShadow, r.cmd(cursor))
			read := readIn(t, s, InboxRead, r.cmd(cursor))
			sameLegacyView(t, r.name+" from "+cursor, shadow, read)
			if s.cursorEntryPart(read.NextCursor) < 0 || s.cursorEntryPart(shadow.NextCursor) >= 0 {
				t.Fatalf("%s: read answers a v2 cursor, shadow a v1 one", r.name)
			}
			if _, ok := shadow.Data["entries"]; ok {
				t.Fatalf("%s: shadow lists entries", r.name)
			}
			if cursor == "" {
				legacy[r.name], v2[r.name] = shadow.NextCursor, read.NextCursor
			}
		}
	}
	first := readIn(t, s, InboxRead, readers[0].cmd(""))
	if kinds := entryKinds(t, first); !slices.Equal(kinds, []string{"addressed", "conversation", "reply", "wakeup", "witness", "work"}) {
		t.Fatalf("alice's entries: %v", updateEntries(t, first))
	}
	third := readIn(t, s, InboxRead, readers[4].cmd(""))
	if kinds := entryKinds(t, third); !slices.Equal(kinds, []string{"addressed", "reply"}) {
		t.Fatalf("another reader sees only reply, addressed and mention entries: %v", updateEntries(t, third))
	}

	// More traffic: a reply to alice mentioning bob, a message addressed to
	// dave, a conversation message, work and a witness for alice, a
	// receiver item for bob and a wake-up for alice.
	post := func(key ed25519.PrivateKey, c Command) string { return runBounded(t, s, signed(key, c)).Receipt.ID }
	reply := post(sc.carol, Command{Operation: "post", Room: "lobby", ReplyTo: sc.root, Text: "round two for alice and @ibob"})
	post(sc.bob, Command{Operation: "post", Room: "lobby", To: keyID(sc.dave), Text: "round two for dave"})
	post(sc.bob, Command{Operation: "post", Room: sc.dm, Text: "round two in the dm", Visibility: "private"})
	work := createTestWork(t, s, sc.alice, "lobby", "request", 0)
	runBounded(t, s, workCommand(s, sc.dave, Command{Operation: "work.claim", MessageID: work, TTL: 600}))
	runBounded(t, s, witnessCommand(sc.bob, keyID(sc.alice), "url", "https://other.example.org/agents/ialice", "inbox-witness-nonce-round-two-01", "verified"))
	item := receiveFor(t, s, sc.bob, "round2")
	runBounded(t, s, svcCall(sc.alice, "wakeup", "schedule", map[string]any{"key": "later2", "at": testTime + 120}, 1, "c61-time2"))
	svcSetNow(s, testTime+120)
	if n := wakeWork(t, s); n < 1 {
		t.Fatalf("the second time wake-up fires: %d", n)
	}
	svcSetNow(s, testTime)

	for _, r := range readers {
		shadow := readIn(t, s, InboxShadow, r.cmd(legacy[r.name]))
		read := readIn(t, s, InboxRead, r.cmd(v2[r.name]))
		sameLegacyView(t, r.name+" after more traffic", shadow, read)
		// An old cursor under read: the same messages and reasons; the
		// entries written since its message sequence may repeat once, and
		// the answer carries a v2 cursor.
		old := readIn(t, s, InboxRead, r.cmd(legacy[r.name]))
		for _, k := range []string{"replies", "addressed", "mentions", "room_activity", "conversations"} {
			if !slices.Equal(ids(shadow, k), ids(old, k)) {
				t.Errorf("%s: an old cursor's %s: %v, want %v", r.name, k, ids(old, k), ids(shadow, k))
			}
		}
		if !slices.Equal(shadow.messageIDs(), old.messageIDs()) || s.cursorEntryPart(old.NextCursor) < 0 {
			t.Errorf("%s: an old cursor under read: %v, want %v", r.name, old.messageIDs(), shadow.messageIDs())
		}
		for _, e := range updateEntries(t, read) {
			if !slices.Contains(updateEntries(t, old), e) {
				t.Errorf("%s: an old cursor misses %s", r.name, e)
			}
		}
		// On a quiet board the next read repeats nothing and hands back
		// the same cursor: wake notices and receiver items included.
		quiet := readIn(t, s, InboxRead, r.cmd(read.NextCursor))
		if len(quiet.Messages) != 0 || quiet.NextCursor != read.NextCursor || len(updateEntries(t, quiet)) != 0 {
			t.Errorf("%s: a quiet read: %d messages, %v, same cursor %v", r.name, len(quiet.Messages), updateEntries(t, quiet), quiet.NextCursor == read.NextCursor)
		}
		if w, ok := quiet.Data["wakeups"]; ok && len(svcField(t, w).([]any)) != 0 {
			t.Errorf("%s: a quiet read repeats wake notices: %v", r.name, w)
		}
		// Rolled back to shadow, a v2 cursor still reads.
		if back, err := s.Execute(testContext, r.cmd(read.NextCursor), "test-origin"); err != nil || len(back.Messages) != 0 {
			t.Errorf("%s: a v2 cursor under shadow: %v %v", r.name, back.messageIDs(), err)
		}
		switch r.name {
		case "alice":
			got := updateEntries(t, read)
			for _, want := range []string{"reply|" + reply, "work|" + work + "@"} {
				if !slices.ContainsFunc(got, func(e string) bool { return strings.HasPrefix(e, want) }) {
					t.Errorf("alice's new entries %v lack %s", got, want)
				}
			}
			if kinds := entryKinds(t, read); !slices.Equal(kinds, []string{"conversation", "reply", "wakeup", "witness", "work"}) {
				t.Errorf("alice's new entry kinds: %v", got)
			}
			if len(svcField(t, read.Data["wakeups"]).([]any)) != 1 {
				t.Errorf("alice's new wake notice: %v", read.Data["wakeups"])
			}
		case "bob":
			if got := updateEntries(t, read); !slices.Contains(got, "mention|"+reply) || !slices.Contains(got, "received|"+item) || len(got) != 2 {
				t.Errorf("bob's new entries: %v", got)
			}
			if got := noticeIDs(t, read, "received", "id"); !slices.Equal(got, []string{item}) {
				t.Errorf("bob's new receiver item: %v", got)
			}
		case "carol on alice", "anonymous on alice":
			if kinds := entryKinds(t, read); !slices.Equal(kinds, []string{"reply"}) {
				t.Errorf("%s: %v", r.name, updateEntries(t, read))
			}
		}
	}

	// The operator's check (swarmmemo inbox parity) agrees.
	report, err := s.InboxReadParity(testContext, 10, 5)
	if err != nil || !report.OK || report.Checked < 3 || report.Differ != 0 {
		t.Fatalf("inbox parity: %+v %v", report, err)
	}
	for _, a := range report.Agents {
		if a.Agent == keyID(sc.alice) && (a.Messages == 0 || a.Entries == 0) {
			t.Fatalf("alice compared nothing: %+v", a)
		}
	}

	// journal.get since is updates.get: the same parity.
	setInboxMode(s, InboxShadow)
	js := runBounded(t, s, signed(sc.alice, Command{Operation: "journal.get"}))
	setInboxMode(s, InboxRead)
	jr := runBounded(t, s, signed(sc.alice, Command{Operation: "journal.get"}))
	setInboxMode(s, InboxShadow)
	since := func(r Result) Result {
		b := r.Data["briefing"].(map[string]any)["since"].(map[string]any)
		out := Result{Data: map[string]any{}}
		for k, v := range b {
			if k == "messages" {
				out.Messages = v.([]Message)
				continue
			}
			out.Data[k] = v
		}
		return out
	}
	sameLegacyView(t, "journal.get since", since(js), since(jr))
	if _, ok := since(jr).Data["entries"]; !ok {
		t.Error("journal.get since lists entries under read")
	}
}

// Cursor v2 shares the messages cursor domain, is refused when forged, and
// a v1 cursor under read is answered with a v2 one.
func TestInboxReadCursorV2(t *testing.T) {
	s := openInboxTest(t, InboxRead)
	me := keyFor(231)
	runBounded(t, s, signed(me, Command{Operation: "agent.register", Handle: "cursorv2"}))
	runBounded(t, s, Command{Operation: "post", Room: "lobby", Text: "anything"})
	r := runBounded(t, s, signed(me, Command{Operation: "updates.get", Target: keyID(me)}))
	if s.cursorEntryPart(r.NextCursor) < 0 {
		t.Fatalf("not a v2 cursor: %q", r.NextCursor)
	}
	if _, err := s.Execute(testContext, Command{Operation: "messages.list", Room: "lobby", Cursor: r.NextCursor}, "test-origin"); err != nil {
		t.Fatalf("messages.list refuses a v2 cursor: %v", err)
	}
	// A v2 layout with another tag is not a cursor.
	plain := make([]byte, updatesCursorV2Size)
	plain[9] = 7
	_, err := s.Execute(testContext, signed(me, Command{Operation: "updates.get", Target: keyID(me), Cursor: s.sealCursor(plain)}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "invalid_cursor" {
		t.Fatalf("a forged tag: %v", err)
	}
	// A v1 cursor reads, and moves to v2.
	v1 := s.cursor(0)
	if got := runBounded(t, s, signed(me, Command{Operation: "updates.get", Target: keyID(me), Cursor: v1})); s.cursorEntryPart(got.NextCursor) < 0 {
		t.Fatalf("a v1 cursor under read: %q", got.NextCursor)
	}
	// Without an agent the read is public room activity, as before.
	if got := runBounded(t, s, Command{Operation: "updates.get"}); s.cursorEntryPart(got.NextCursor) >= 0 || got.Data["entries"] != nil {
		t.Fatalf("a public read: %+v", got.Data)
	}
}

// A waiting read under read ends on an entry that comes with no message (a
// work update), which a pull client never saw before, and still times out
// caught up with the same cursor.
func TestInboxReadWaitEndsOnEntry(t *testing.T) {
	s := openInboxTest(t, InboxRead)
	owner, worker := keyFor(232), keyFor(233)
	runBounded(t, s, signed(owner, Command{Operation: "agent.register", Handle: "waitowner"}))
	runBounded(t, s, signed(worker, Command{Operation: "agent.register", Handle: "waitworker"}))
	work := createTestWork(t, s, owner, "lobby", "request", 0)
	read := func(cursor, data string) Command {
		return signed(owner, Command{Operation: "updates.get", Target: keyID(owner), Cursor: cursor, Data: data})
	}
	saved := runBounded(t, s, read("", "")).NextCursor
	if timedOut := runBounded(t, s, read(saved, waitData(1))); timedOut.NextCursor != saved || len(timedOut.Messages) != 0 {
		t.Fatalf("a timed-out wait under read: %+v", timedOut)
	}
	done := waitIn(testContext, s, read(saved, waitData(UpdatesWaitMax)), "198.51.100.63")
	untilHeld(t, s, 1)
	// A write that concerns nobody wakes the waiter without ending its wait.
	runBounded(t, s, signed(keyFor(234), Command{Operation: "agent.register", Handle: "waitbystander"}))
	runBounded(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: work, TTL: 600}))
	select {
	case got := <-done:
		if got.err != nil || got.res.NextCursor == saved || len(got.res.Messages) != 0 {
			t.Fatalf("the wait must end on the work entry: %+v %v", got.res, got.err)
		}
		if e := updateEntries(t, got.res); len(e) != 1 || !strings.HasPrefix(e[0], "work|"+work+"@") {
			t.Fatalf("the wait's entries: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting read did not end on a work entry")
	}
	untilHeld(t, s, 0)
}
