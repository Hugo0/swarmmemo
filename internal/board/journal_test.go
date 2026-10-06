package board

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

// journalOf is a journal.get answer as a client receives it: JSON.
func journalOf(t *testing.T, r Result) (briefing map[string]any, raw json.RawMessage, seal map[string]any) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		NextCursor string `json:"next_cursor"`
		Data       struct {
			Briefing json.RawMessage `json:"briefing"`
			Seal     map[string]any  `json:"seal"`
		} `json:"data"`
	}
	if err = json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(wire.Data.Briefing, &briefing); err != nil {
		t.Fatalf("no briefing: %s", b)
	}
	if briefing["next_cursor"] != wire.NextCursor || wire.NextCursor == "" {
		t.Fatalf("next_cursor must be in the briefing and the result: %v / %q", briefing["next_cursor"], wire.NextCursor)
	}
	return briefing, wire.Data.Briefing, wire.Data.Seal
}

func journalGet(key []byte, cursor string, limit int) Command {
	return signed(key, Command{Operation: "journal.get", Cursor: cursor, Limit: limit})
}

func path(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("no %q in %v", k, v)
		}
		v = m[k]
	}
	return v
}

func messageIDs(t *testing.T, briefing map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range path(t, briefing, "since", "messages").([]any) {
		out[m.(map[string]any)["id"].(string)] = true
	}
	return out
}

func memPut(key []byte, k, v string) Command {
	return svcCall(key, "memory", "put", map[string]any{"key": k, "value": v}, 1<<20, "")
}

// The wake read gathers every section in one call, resumes from the cursor
// the last session saved with journal.suspend, and advances.
func TestJournalBriefingSectionsAndCursor(t *testing.T) {
	s := openWakeTest(t, "memory", "wakeup", "notary")
	me, other := keyFor(1), keyFor(2)
	run(t, s, signed(me, Command{Operation: "agent.register", Handle: "sleeper"}))
	run(t, s, signed(other, Command{Operation: "agent.register", Handle: "neighbour"}))
	id := keyID(me)
	root := run(t, s, signed(me, Command{Operation: "post", Room: "workshop", Text: "An open question."})).Receipt.ID

	first, _, _ := journalOf(t, run(t, s, journalGet(me, "", 0)))
	if first["cursor_from"] != "none" || first["suspend"] != nil || first["agent"] != id {
		t.Fatalf("a first wake has no saved cursor or note: %v", first)
	}
	saved := first["next_cursor"].(string)

	run(t, s, memPut(me, "journal/core/identity", "I review Go patches."))
	run(t, s, memPut(me, "journal/core/goals", "Ship the export idea."))
	run(t, s, memPut(me, "notes/scratch", "not core"))
	run(t, s, svcCall(me, "wakeup", "schedule", map[string]any{"key": "morning", "at": testTime + 600}, 5, ""))
	sus := run(t, s, signed(me, Command{Operation: "journal.suspend", Text: "Was answering neighbour; next: review the patch.", Cursor: saved}))
	if svcField(t, sus.Data, "suspend", "cursor") != saved || svcField(t, sus.Data, "suspend", "note_bytes") == nil {
		t.Fatalf("suspend receipt: %+v", sus.Data)
	}
	if strings.Contains(svcFieldString(t, sus), "review the patch") {
		t.Fatal("a suspend receipt (stored for retries) must not carry the note's text")
	}

	reply := run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "An answer.", ReplyTo: root})).Receipt.ID
	mail := run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "Can you review this?", To: id})).Receipt.ID
	work := createTestWork(t, s, other, "lobby", "request", 0)
	run(t, s, workCommand(s, me, Command{Operation: "work.claim", MessageID: work, TTL: 120}))

	b, _, _ := journalOf(t, run(t, s, journalGet(me, "", 0)))
	if b["cursor_from"] != "suspend" || b["cursor"] != saved {
		t.Fatalf("without a cursor the wake read resumes from the suspend note's: %v %v", b["cursor_from"], b["cursor"])
	}
	seen := messageIDs(t, b)
	if !seen[reply] || !seen[mail] {
		t.Fatalf("since misses the reply or the mail: %v", seen)
	}
	if got := path(t, b, "since", "replies").([]any); len(got) != 1 || got[0] != reply {
		t.Fatalf("since.replies: %v", got)
	}
	items := path(t, b, "memory", "items").([]any)
	if len(items) != 2 || items[0].(map[string]any)["key"] != "journal/core/goals" || items[1].(map[string]any)["value"] != "I review Go patches." {
		t.Fatalf("memory holds the core items only, with values: %v", items)
	}
	if path(t, b, "suspend", "note") != "Was answering neighbour; next: review the patch." || path(t, b, "suspend", "cursor") != saved {
		t.Fatalf("suspend: %v", b["suspend"])
	}
	pending := path(t, b, "wakeups", "pending").([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["key"] != "morning" {
		t.Fatalf("wakeups.pending: %v", pending)
	}
	ws := path(t, b, "open_work", "work", "items").([]any)
	if len(ws) != 1 || path(t, ws[0], "role") != "worker" || path(t, ws[0], "work", "id") != work || path(t, ws[0], "work", "state") != "claimed" {
		t.Fatalf("open_work.work: %v", ws)
	}
	un := path(t, b, "open_work", "unanswered", "items").([]any)
	if len(un) != 1 || path(t, un[0], "id") != mail || path(t, un[0], "preview") != "Can you review this?" {
		t.Fatalf("open_work.unanswered: %v", un)
	}
	// The requester sees its own request as open work too.
	ob, _, _ := journalOf(t, run(t, s, journalGet(other, "", 0)))
	if ows := path(t, ob, "open_work", "work", "items").([]any); len(ows) != 1 || path(t, ows[0], "role") != "requester" {
		t.Fatalf("requester's open work: %v", ows)
	}

	// Answering takes the message off the unanswered list; the cursor moves on
	// and the agent's own reply is not news to it.
	run(t, s, signed(me, Command{Operation: "post", Room: "lobby", Text: "On it.", ReplyTo: mail}))
	next := b["next_cursor"].(string)
	if next == saved {
		t.Fatal("the cursor must advance")
	}
	again, _, _ := journalOf(t, run(t, s, journalGet(me, next, 0)))
	if again["cursor_from"] != "argument" || messageIDs(t, again)[reply] || messageIDs(t, again)[mail] || len(path(t, again, "since", "replies").([]any)) != 0 {
		t.Fatalf("a read from the new cursor does not replay what was handled: %v", again["since"])
	}
	if un := path(t, again, "open_work", "unanswered", "items").([]any); len(un) != 0 {
		t.Fatalf("an answered message is not unanswered: %v", un)
	}
}

// since is updates.get itself: the same messages and the same data for the
// same cursor, never a second implementation.
func TestJournalSinceIsUpdatesGet(t *testing.T) {
	s := openWakeTest(t, "memory", "wakeup")
	me, other := keyFor(1), keyFor(2)
	id := keyID(me)
	root := run(t, s, signed(me, Command{Operation: "post", Room: "workshop", Text: "Root."})).Receipt.ID
	cursor := run(t, s, signed(me, Command{Operation: "updates.get", Target: id})).NextCursor
	run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "Reply.", ReplyTo: root}))
	run(t, s, signed(other, Command{Operation: "post", Room: "workshop", Text: "News."}))
	run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "Mail.", To: id}))

	u := run(t, s, signed(me, Command{Operation: "updates.get", Target: id, Cursor: cursor}))
	b, _, _ := journalOf(t, run(t, s, journalGet(me, cursor, 0)))
	var want map[string]any
	raw, _ := json.Marshal(u.Data)
	_ = json.Unmarshal(raw, &want)
	raw, _ = json.Marshal(u.Messages)
	var msgs []any
	_ = json.Unmarshal(raw, &msgs)
	want["messages"] = msgs
	if !reflect.DeepEqual(b["since"], want) {
		t.Fatalf("since differs from updates.get:\n%v\n%v", b["since"], want)
	}
	got, err1 := s.parseCursor(b["next_cursor"].(string))
	wantSeq, err2 := s.parseCursor(u.NextCursor)
	if err1 != nil || err2 != nil || got != wantSeq {
		t.Fatal("next_cursor is updates.get's")
	}
}

// Every section is capped and says has_more past its cap.
func TestJournalCapsAndHasMore(t *testing.T) {
	s := openWakeTest(t, "memory")
	me, other := keyFor(1), keyFor(2)
	id := keyID(me)
	run(t, s, signed(me, Command{Operation: "post", Room: "lobby", Text: "here"}))
	cursor := run(t, s, journalGet(me, "", 0)).NextCursor
	for i := 0; i < JournalSinceMax+5; i++ {
		run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("Question %d for you?", i), To: id}))
	}
	for i := 0; i <= JournalCoreItems; i++ {
		run(t, s, memPut(me, fmt.Sprintf("journal/core/k%02d", i), "v"))
	}
	run(t, s, memPut(me, "journal/core/k00", strings.Repeat("é", JournalCoreValueBytes)))

	b, _, _ := journalOf(t, run(t, s, journalGet(me, cursor, 0)))
	if n := len(messageIDs(t, b)); n != JournalSinceMax || path(t, b, "since", "has_more") != true {
		t.Fatalf("since: %d messages, has_more %v", n, path(t, b, "since", "has_more"))
	}
	if b2, _, _ := journalOf(t, run(t, s, journalGet(me, cursor, 500))); len(messageIDs(t, b2)) != JournalSinceMax {
		t.Fatal("a larger limit is held to the cap")
	}
	if b3, _, _ := journalOf(t, run(t, s, journalGet(me, cursor, 3))); len(messageIDs(t, b3)) != 3 {
		t.Fatal("a smaller limit is honoured")
	}
	items := path(t, b, "memory", "items").([]any)
	if len(items) != JournalCoreItems || path(t, b, "memory", "has_more") != true {
		t.Fatalf("memory: %d items, has_more %v", len(items), path(t, b, "memory", "has_more"))
	}
	first := items[0].(map[string]any)
	if v := first["value"].(string); first["truncated"] != true || len(v) > JournalCoreValueBytes || len(v) < JournalCoreValueBytes-1 || !strings.HasPrefix(v, "é") {
		t.Fatalf("a long value is cut on a character boundary: %d bytes, %v", len(first["value"].(string)), first["truncated"])
	}
	if un := path(t, b, "open_work", "unanswered", "items").([]any); len(un) != JournalUnansweredMax || path(t, b, "open_work", "unanswered", "has_more") != true {
		t.Fatalf("unanswered: %d, has_more %v", len(un), path(t, b, "open_work", "unanswered", "has_more"))
	}
	if path(t, b, "open_work", "work", "has_more") != false {
		t.Fatal("no work, no more")
	}
}

func TestJournalSuspendRoundTrip(t *testing.T) {
	s := openWakeTest(t, "memory")
	me := keyFor(1)
	fails(t, s, signed(me, Command{Operation: "journal.suspend", Text: strings.Repeat("x", JournalSuspendBytes+1)}), "field_limit")
	fails(t, s, signed(me, Command{Operation: "journal.suspend", Text: "  "}), "invalid_request")
	fails(t, s, signed(me, Command{Operation: "journal.suspend", Text: "ok", Cursor: "not-a-cursor"}), "invalid_cursor")
	note := strings.Repeat("n", JournalSuspendBytes)
	c := signed(me, Command{Operation: "journal.suspend", Text: note, RequestID: "suspend-1"})
	run(t, s, c)
	if again := run(t, s, c); again.Receipt != nil && !again.Receipt.Duplicate {
		t.Fatal("an exact retry is a duplicate")
	}
	b, _, _ := journalOf(t, run(t, s, journalGet(me, "", 0)))
	if path(t, b, "suspend", "note") != note || b["cursor_from"] != "none" {
		t.Fatalf("suspend round trip: %v", b["suspend"])
	}
	// It is an ordinary memory item: readable and replaceable there too.
	got := run(t, s, svcRead(me, "memory", "get", map[string]any{"key": JournalSuspendKey}))
	if !strings.Contains(svcFieldString(t, got.Data), `\"note\"`) {
		t.Fatalf("the note is the memory item %s: %v", JournalSuspendKey, got.Data)
	}
	run(t, s, memPut(me, JournalSuspendKey, "plain words"))
	if b, _, _ = journalOf(t, run(t, s, journalGet(me, "", 0))); path(t, b, "suspend", "note") != "plain words" {
		t.Fatalf("a plain memory value is the note: %v", b["suspend"])
	}
	run(t, s, signed(me, Command{Operation: "journal.suspend", Text: "newer"}))
	if b, _, _ = journalOf(t, run(t, s, journalGet(me, "", 0))); path(t, b, "suspend", "note") != "newer" {
		t.Fatal("a later suspend replaces the note")
	}
}

// The seal is sha256 over the canonical briefing, recomputable from the JSON
// a client received, and signed with the notary key when there is one.
func TestJournalSealVerifies(t *testing.T) {
	s := openWakeTest(t, "memory", "notary")
	me, other := keyFor(1), keyFor(2)
	id := keyID(me)
	run(t, s, memPut(me, "journal/core/who", "Tags <b> & \"quotes\" stay as they are."))
	run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "x < y && café", To: id}))
	_, raw, seal := journalOf(t, run(t, s, journalGet(me, "", 0)))

	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	canonical, err := JournalCanonicalJSON(tree)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	if seal["hash"] != hash || seal["algorithm"] != "sha256" {
		t.Fatalf("the seal must be recomputable from the received briefing: %v != %s", seal["hash"], hash)
	}
	public := svcField(t, run(t, s, svcRead(nil, "notary", "key", map[string]any{})).Data, "result", "public_key").(string)
	var sig services.JournalSealSignature
	raw2, _ := json.Marshal(seal["signature"])
	if err = json.Unmarshal(raw2, &sig); err != nil || sig.Signature == "" {
		t.Fatalf("a board with a notary key signs the seal: %v", seal)
	}
	if !services.VerifyJournalSeal(public, sig, id, hash) {
		t.Fatal("the seal signature must verify against the published notary key")
	}
	if services.VerifyJournalSeal(public, sig, keyID(other), hash) || services.VerifyJournalSeal(public, sig, id, strings.Repeat("0", 64)) {
		t.Fatal("a seal is for one agent and one hash")
	}
	// A briefing altered after it was handed over no longer matches.
	tampered := strings.Replace(string(raw), "café", "cafe", 1)
	_ = json.Unmarshal([]byte(tampered), &tree)
	canonical, _ = JournalCanonicalJSON(tree)
	if sum2 := sha256.Sum256(canonical); hex.EncodeToString(sum2[:]) == hash {
		t.Fatal("tampering must change the hash")
	}

	// Without a notary key the seal is the hash alone.
	plain := openWakeTest(t, "memory")
	if _, _, seal := journalOf(t, run(t, plain, journalGet(me, "", 0))); seal["signature"] != nil || seal["hash"] == "" {
		t.Fatalf("no notary, no signature: %v", seal)
	}
}

func TestJournalRefusesUnsignedAndWorksWithoutServices(t *testing.T) {
	s := openTest(t, updatesConfig())
	me := keyFor(1)
	fails(t, s, Command{Operation: "journal.get"}, "signature_required")
	fails(t, s, Command{Operation: "journal.suspend", Text: "hi", RequestID: "anon-suspend-1"}, "signature_required")
	fails(t, s, signed(me, Command{Operation: "journal.get", Target: keyID(keyFor(2))}), "unexpected_field")
	// With no services the briefing still answers, saying what is off.
	b, _, seal := journalOf(t, run(t, s, journalGet(me, "", 0)))
	if path(t, b, "memory", "available") != false || path(t, b, "wakeups", "available") != false || seal["hash"] == "" {
		t.Fatalf("services off: %v", b)
	}
	fails(t, s, signed(me, Command{Operation: "journal.suspend", Text: "hi"}), "service_unavailable")
}
