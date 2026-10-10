package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
	// Submitted but never judged: the requester cancels one with the result
	// waiting, and lets another reach its deadline (below, once the clock
	// moves past it).
	cancelled := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: cancelled, Target: workResult(t, s, worker, cancelled, "lobby")}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: cancelled, Reason: "no longer needed"}))
	lapsed := createTestWork(t, s, owner, "lobby", "request", 120)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: lapsed, Target: workResult(t, s, worker, lapsed, "lobby")}))
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
	// Before its deadline the lapsing result is pending: submitted, not
	// judged, not expired.
	if got, want := record(worker), (RecordWork{Claimed: 7, Submitted: 6, Accepted: 2, Rejected: 1, ExpiredUnjudged: 1, Paid: 1}); got != want {
		t.Fatalf("worker's counts.work before the deadline %+v, want %+v", got, want)
	}
	// The requester's side mirrors it (C138): of the results submitted to its
	// work, one rejected (retried's first), one cancelled while it waited;
	// the lapsing one is not unjudged yet.
	if got, want := record(owner), (RecordWork{Posted: 6, AcceptedAsRequester: 2, RejectedAsRequester: 1, UnjudgedAsRequester: 1}); got != want {
		t.Fatalf("requester's counts.work before the deadline %+v, want %+v", got, want)
	}
	now := s.now().Unix() + 121
	s.now = func() time.Time { return time.Unix(now, 0) }
	if state := getTestWork(t, s, lapsed).State; state != "expired" {
		t.Fatalf("lapsed work is %q", state)
	}
	// submitted counts every result; the two never judged are
	// expired_unjudged, the claim-and-submit still waiting (6-2-1-2) pending.
	if got, want := record(worker), (RecordWork{Claimed: 7, Submitted: 6, Accepted: 2, Rejected: 1, ExpiredUnjudged: 2, Paid: 1}); got != want {
		t.Fatalf("worker's counts.work %+v, want %+v", got, want)
	}
	if got, want := record(owner), (RecordWork{Posted: 6, AcceptedAsRequester: 2, RejectedAsRequester: 1, UnjudgedAsRequester: 2}); got != want {
		t.Fatalf("requester's counts.work %+v, want %+v", got, want)
	}
	if got := record(idle); got != (RecordWork{}) {
		t.Fatalf("an agent with no work: %+v", got)
	}

	// One statement, by the schema-23 indexes, never a scan of every transition.
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+recordWorkSQL, recordWorkArgs(0, "a")...)
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
	want := []string{paid, retried, claimed, waiting, cancelled, lapsed}
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
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"worker":"nope"}`}, "agent_not_found")
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"eligible_for":"nope"}`}, "agent_not_found")
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"worker":"no pe"}`}, "invalid_work_data")

	// A registered handle names the agent as its fingerprint does (C136b), in
	// any case: the same pages, page by page, and the same cursors.
	run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", Text: "taking work", Handle: "record-worker", Timestamp: s.now().Unix()}))
	if _, err := s.ReadLogRecord(testContext, "Record-Worker"); err != nil {
		t.Fatal(err)
	}
	page := func(data, cursor string) (string, string) {
		t.Helper()
		res := run(t, s, Command{Operation: "works.list", Data: data, Limit: 2, Cursor: cursor})
		raw, err := json.Marshal(res.Data)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw), res.NextCursor
	}
	for _, field := range []string{"worker", "eligible_for"} {
		byKey, byHandle := `{"schema":1,"`+field+`":"`+keyID(worker)+`"}`, `{"schema":1,"`+field+`":"Record-Worker"}`
		keyCursor, handleCursor, pages := "", "", 0
		for {
			keyPage, nextKey := page(byKey, keyCursor)
			handlePage, nextHandle := page(byHandle, handleCursor)
			if keyPage != handlePage {
				t.Fatalf("%s page %d by handle differs:\n%s\n%s", field, pages, handlePage, keyPage)
			}
			// Each cursor pages the other form's listing the same way.
			if nextKey != "" {
				crossed, _ := page(byHandle, nextKey)
				same, _ := page(byKey, nextKey)
				if crossed != same {
					t.Fatalf("%s: a fingerprint's cursor pages the handle's listing differently", field)
				}
			}
			if (nextKey == "") != (nextHandle == "") {
				t.Fatalf("%s: cursors %q and %q", field, nextKey, nextHandle)
			}
			pages++
			if nextKey == "" {
				break
			}
			keyCursor, handleCursor = nextKey, nextHandle
		}
		if pages < 2 {
			t.Fatalf("%s listed %d pages", field, pages)
		}
	}
	if got := list(Command{Operation: "works.list", Data: `{"schema":1,"worker":"RECORD-WORKER"}`, Limit: 1}); !slices.Equal(got, want) {
		t.Fatalf("works.list worker by handle: %v, want %v", got, want)
	}
}

// The proofs bundle (C139): the inclusion proofs of an agent's accepted
// results on public work, newest first, paged, each the very proof
// ReadLogProof gives for the result message against the bundle's
// checkpoint, verifying with the log key. Rejected, private and hidden
// work stay out; a result newer than the checkpoint waits, without a proof.
func TestRecordProofsBundle(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, idle := keyFor(230), keyFor(231), keyFor(232)
	register(t, s, owner)
	register(t, s, worker)
	register(t, s, idle)
	clock := s.now().Unix()
	s.now = func() time.Time { return time.Unix(clock, 0) }
	accept := func(room string) (work, result string) {
		t.Helper()
		work = createTestWork(t, s, owner, room, "request", 0)
		result = workResult(t, s, worker, work, room)
		ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: work, Target: result})).Data["ack"].(WorkAck)
		clock++ // each accept a second after the last
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: work, Amount: ack.Fence}))
		return work, result
	}
	type pair struct{ work, result string }
	var want []pair // newest first
	for range 3 {
		w, r := accept("lobby")
		want = append([]pair{{w, r}}, want...)
	}
	// Rejected only: never in the bundle.
	rejected := createTestWork(t, s, owner, "lobby", "request", 0)
	fence := claimAndSubmit(t, s, worker, rejected, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: rejected, Amount: fence, Reason: "no"}))
	// Private and hidden work, accepted: never in the bundle.
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "proofs-private", Visibility: "private", Members: []string{keyID(worker)}, Timestamp: s.now().Unix()}))
	accept("proofs-private")
	hidden, _ := accept("lobby")
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", hidden); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	// Accepted after the checkpoint: listed first, its proof pending.
	late, lateResult := accept("lobby")

	var got []RecordResultProof
	var cp LogCheckpoint
	cursor, pages := "", 0
	for {
		page, err := s.ReadRecordProofs(testContext, keyID(worker), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if page.Agent != keyID(worker) || len(page.Proofs) > 2 || page.HasMore != (page.NextCursor != "") {
			t.Fatalf("page %d: %+v", pages, page)
		}
		if pages > 0 && page.Checkpoint.Size != cp.Size {
			t.Fatalf("checkpoint moved between pages: %d, %d", cp.Size, page.Checkpoint.Size)
		}
		cp = page.Checkpoint
		got, pages = append(got, page.Proofs...), pages+1
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 2 || len(got) != 4 {
		t.Fatalf("%d pages, %d proofs: %+v", pages, len(got), got)
	}
	if got[0].WorkID != late || got[0].ResultID != lateResult || got[0].Proof != nil || got[0].LeafIndex != nil || got[0].Pending == "" {
		t.Fatalf("the result newer than the checkpoint: %+v", got[0])
	}
	body := verifyCheckpoint(t, s, cp)
	for i, p := range got[1:] {
		if p.WorkID != want[i].work || p.ResultID != want[i].result || p.Proof == nil || p.LeafIndex == nil || *p.LeafIndex != p.Proof.Leaf.Index || p.Pending != "" {
			t.Fatalf("proof %d: %+v, want %+v", i, p, want[i])
		}
		if p.AcceptedAt > got[i].AcceptedAt {
			t.Fatalf("not newest first: %d after %d", p.AcceptedAt, got[i].AcceptedAt)
		}
		// The leaf is the result message's, and the proof verifies.
		var leaf logLeaf
		if json.Unmarshal([]byte(p.Proof.Leaf.Data), &leaf) != nil || leaf.ID != p.ResultID || p.Proof.Leaf.Kind != "message" {
			t.Fatalf("proof %d proves leaf %s", i, p.Proof.Leaf.Data)
		}
		verifyInclusion(t, s, *p.Proof, body)
		if p.Proof.Checkpoint.Note != cp.Note || p.Proof.Text == nil {
			t.Fatalf("proof %d: checkpoint %d, text %v", i, p.Proof.Checkpoint.Size, p.Proof.Text)
		}
		// The same object /api/log/proof?message=RESULT_ID&size=SIZE gives.
		alone, err := s.ReadLogProof(testContext, -1, p.ResultID, cp.Size)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(alone)
		b, _ := json.Marshal(*p.Proof)
		if string(a) != string(b) {
			t.Fatalf("proof %d differs from /api/log/proof:\n%s\n%s", i, b, a)
		}
	}

	// An agent with no accepted work has an empty bundle; the requester too.
	for _, k := range []ed25519.PrivateKey{idle, owner} {
		if page, err := s.ReadRecordProofs(testContext, keyID(k), "", 0); err != nil || len(page.Proofs) != 0 || page.HasMore {
			t.Fatalf("empty bundle: %+v %v", page, err)
		}
	}
	// A cursor pages only the agent it was given for.
	first, err := s.ReadRecordProofs(testContext, keyID(worker), "", 1)
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	for _, c := range []struct{ who, cursor, code string }{
		{keyID(idle), first.NextCursor, "invalid_cursor"},
		{keyID(worker), "nope", "invalid_cursor"},
		{keyID(worker), s.generation + ":AAAA", "invalid_cursor"},
		{"nobody-here", "", "agent_not_found"},
	} {
		_, err := s.ReadRecordProofs(testContext, c.who, c.cursor, 1)
		if e := (*Error)(nil); !errors.As(err, &e) || e.Code != c.code {
			t.Fatalf("%s %q: %v, want %s", c.who, c.cursor, err, c.code)
		}
	}

	// One indexed statement: the account's transitions by
	// work_transitions_author, never a scan.
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+recordAcceptedSQL, 1, 1, "", "a", "a", 1)
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
	if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "work_transitions_author") || strings.Contains(joined, "SCAN ") {
		t.Fatalf("accepted results plan:\n%s", joined)
	}
}
