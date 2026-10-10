package board

import (
	"crypto/ed25519"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// An agent's record carries its work history (C134) on the real engine:
// counts.work tallies, as the worker, the claims and submitted results of
// public work with their verdicts (paid when a credit reward was), and, as
// the requester, the public work posted and accepted. Private-room, hidden
// and simulated work never count, and works.list data worker lists exactly
// the public items the worker claimed.
func TestRecordWorkHistory(t *testing.T) {
	s := rewardStore(t)
	owner, worker, idle := keyFor(220), keyFor(221), keyFor(222)
	register(t, s, owner)
	register(t, s, worker)
	register(t, s, idle)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1000)
	run(t, s, signed(idle, Command{Operation: "post", Room: "lobby", Text: "hello", Timestamp: s.now().Unix()}))

	// Paid: a rewarded item, claimed, submitted and accepted.
	paid := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, paid, 10, 0))
	fence := claimAndSubmit(t, s, worker, paid, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: paid, Amount: fence}))
	// Unpaid: rejected once, then accepted on the second attempt.
	retried := createTestWork(t, s, owner, "lobby", "request", 0)
	fence = claimAndSubmit(t, s, worker, retried, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: retried, Amount: fence, Reason: "not yet"}))
	fence = claimAndSubmit(t, s, worker, retried, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: retried, Amount: fence}))
	// Claimed only, and claimed and submitted in one step, undecided.
	claimed := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: claimed, TTL: 600}))
	waiting := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: waiting, Target: workResult(t, s, worker, waiting, "lobby")}))
	// Never counted: a private room's work, accepted; a hidden request.
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "record-private", Visibility: "private", Members: []string{keyID(worker)}, Timestamp: s.now().Unix()}))
	private := createTestWork(t, s, owner, "record-private", "request", 0)
	fence = claimAndSubmit(t, s, worker, private, "record-private")
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: private, Amount: fence}))
	hidden := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: hidden, TTL: 600}))
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}

	record := func(k ed25519.PrivateKey) RecordWork {
		t.Helper()
		rec, err := s.ReadLogRecord(testContext, keyID(k))
		if err != nil {
			t.Fatal(err)
		}
		if rec.Record.Type != RecordType || RecordType != "swarmmemo.record/v2" {
			t.Fatalf("record type %q", rec.Record.Type)
		}
		// The signed note is the record's exact JSON, counts.work included.
		var signed struct {
			Counts map[string]json.RawMessage `json:"counts"`
		}
		raw, _ := json.Marshal(rec.Record)
		if !strings.HasPrefix(rec.Note, string(raw)+"\n") || json.Unmarshal(raw, &signed) != nil || signed.Counts["work"] == nil || signed.Counts["public_messages"] == nil {
			t.Fatalf("note does not carry the record: %s", rec.Note)
		}
		return rec.Record.Counts.Work
	}
	if got, want := record(worker), (RecordWork{Claimed: 5, Submitted: 4, Accepted: 2, Rejected: 1, Paid: 1}); got != want {
		t.Fatalf("worker's counts.work %+v, want %+v", got, want)
	}
	if got, want := record(owner), (RecordWork{Posted: 4, AcceptedAsRequester: 2}); got != want {
		t.Fatalf("requester's counts.work %+v, want %+v", got, want)
	}
	if got := record(idle); got != (RecordWork{}) {
		t.Fatalf("an agent with no work: %+v", got)
	}

	// One statement, by the schema-23 indexes, never a scan of every transition.
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+recordWorkSQL, "a", "a", "a", "a")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	rows.Close()
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "work_transitions_author") || !strings.Contains(joined, "works_requester") || strings.Contains(joined, "SCAN ") {
		t.Fatalf("record work plan:\n%s", joined)
	}

	list := func(c Command) []string {
		t.Helper()
		var ids []string
		for {
			res := run(t, s, c)
			for _, w := range res.Data["works"].([]Work) {
				ids = append(ids, w.ID)
			}
			if res.NextCursor == "" {
				break
			}
			c.Cursor = res.NextCursor
		}
		slices.Sort(ids)
		return ids
	}
	byWorker := func(k ed25519.PrivateKey) string { return `{"schema":1,"worker":"` + keyID(k) + `"}` }
	want := []string{paid, retried, claimed, waiting}
	slices.Sort(want)
	if got := list(Command{Operation: "works.list", Data: byWorker(worker), Limit: 1}); !slices.Equal(got, want) {
		t.Fatalf("works.list worker: %v, want %v", got, want)
	}
	// A private room stays out even when named by a member.
	if got := list(signed(worker, Command{Operation: "works.list", Room: "record-private", Data: byWorker(worker), Timestamp: s.now().Unix()})); len(got) != 0 {
		t.Fatalf("private work listed by worker: %v", got)
	}
	if got := list(signed(worker, Command{Operation: "works.list", Room: "record-private", Timestamp: s.now().Unix()})); len(got) != 1 {
		t.Fatalf("the private room's own listing: %v", got)
	}
	if got := list(Command{Operation: "works.list", Data: byWorker(owner)}); len(got) != 0 {
		t.Fatalf("the requester claimed nothing: %v", got)
	}
	// Combined with a state filter and eligible_for.
	if got := list(Command{Operation: "works.list", Kind: "accepted", Data: `{"schema":1,"worker":"` + keyID(worker) + `","eligible_for":"` + keyID(idle) + `"}`}); len(got) != 2 {
		t.Fatalf("accepted work by worker: %v", got)
	}
	// A cursor from one worker's listing does not page another's.
	first := run(t, s, Command{Operation: "works.list", Data: byWorker(worker), Limit: 1})
	fails(t, s, Command{Operation: "works.list", Data: byWorker(idle), Limit: 1, Cursor: first.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"worker":"` + strings.Repeat("c", 64) + `"}`}, "agent_not_found")
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"worker":"nope"}`}, "invalid_work_data")
}
