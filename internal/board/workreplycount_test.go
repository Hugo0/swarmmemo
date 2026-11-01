package board

import (
	"encoding/json"
	"slices"
	"testing"

	"swarmmemo/internal/allowance"
)

// C692: a result accepted as a direct reply (C691) counts for its author
// exactly like a claimed and accepted one: the record's counts.work and its
// accepted proofs, works.list worker, the work reputation, the funnel's
// work acts and repeat-paid, and the account export. The open case (the
// board claims the reply), the claimed case (the author held a signed
// claim) and the already-submitted case each count once.
func TestWorkAcceptReplyCountsForAuthor(t *testing.T) {
	s := rewardStore(t)
	owner, author, other := keyFor(90), keyFor(91), keyFor(92)
	register(t, s, owner)
	register(t, s, author)
	register(t, s, other)
	mintCredit(t, s, keyID(owner), allowance.Paid, 50_000)

	// Open: the author never claimed; the board claims and submits.
	open := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, open, 12_000, 0))
	run(t, s, acceptReplyCmd(s, owner, open, replyTo(t, s, author, open, "Result one"), nil))
	// Claimed: the author claimed, replied, never submitted.
	held := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, held, 12_000, 0))
	run(t, s, workCommand(s, author, Command{Operation: "work.claim", MessageID: held, TTL: 600}))
	run(t, s, acceptReplyCmd(s, owner, held, replyTo(t, s, author, held, "Result two"), nil))
	// Already submitted by the author itself: the ordinary accept.
	done := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, done, 12_000, 0))
	claimAndSubmit(t, s, author, done, "lobby")
	run(t, s, acceptReplyCmd(s, owner, done, getTestWork(t, s, done).ResultID, nil))
	for _, id := range []string{open, held, done} {
		if w := getTestWork(t, s, id); w.State != "accepted" {
			t.Fatalf("%s: %+v", id, w)
		}
	}

	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	rec, err := s.ReadLogRecord(testContext, keyID(author))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Record.Counts.Work, (RecordWork{Claimed: 3, Submitted: 3, Accepted: 3, Paid: 3}); got != want {
		t.Fatalf("author's counts.work %+v, want %+v", got, want)
	}
	if rec, err = s.ReadLogRecord(testContext, keyID(owner)); err != nil {
		t.Fatal(err)
	} else if got := rec.Record.Counts.Work; got.Claimed != 0 || got.Submitted != 0 || got.AcceptedAsRequester != 3 {
		t.Fatalf("requester's counts.work %+v", got)
	}

	var listed []string
	for _, w := range run(t, s, Command{Operation: "works.list", Data: `{"schema":1,"worker":"` + keyID(author) + `"}`}).Data["works"].([]Work) {
		listed = append(listed, w.ID)
	}
	want := []string{open, held, done}
	slices.Sort(listed)
	slices.Sort(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("works.list worker: %v, want %v", listed, want)
	}

	proofs, err := s.ReadRecordProofs(testContext, keyID(author), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var proven []string
	for _, p := range proofs.Proofs {
		proven = append(proven, p.WorkID)
	}
	slices.Sort(proven)
	if !slices.Equal(proven, want) {
		t.Fatalf("accepted proofs: %+v", proofs.Proofs)
	}

	records, err := workRecords(testContext, s.db, []string{keyID(author)}, s.now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if r := records[keyID(author)]; r == nil || r.AsWorker.Accepted != 3 || r.AsWorker.Paid != 3 || r.AsWorker.PaidCredits != 36_000 {
		t.Fatalf("author's work reputation %+v", r)
	}

	now := s.now().Unix()
	days, err := s.deriveAccount(testContext, keyID(author), now-86400, now+86400, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := days[now/86400]; d == nil || d.acts&ActBit("work") == 0 {
		t.Fatalf("author's funnel acts: %+v", d)
	}
	rows, err := s.db.Query(repeatPaidWorkSQL, now-86400, now+86400)
	if err != nil {
		t.Fatal(err)
	}
	worked := 0
	for rows.Next() {
		var account string
		var at int64
		if err = rows.Scan(&account, &at); err != nil {
			t.Fatal(err)
		}
		if account == keyID(author) {
			worked++
		}
	}
	rows.Close()
	// Each accept of a work the author holds, plus its own signed claims
	// and submit (held's claim; done's claim and submit).
	if worked < 3+3 {
		t.Fatalf("repeat-paid work acts for the author: %d", worked)
	}

	refs := run(t, s, signed(author, Command{Operation: "account.export"})).Data["account_refs"].(map[string]any)
	var claimed []struct {
		ID string `json:"id"`
	}
	raw, _ := json.Marshal(refs["work_claimed"])
	if err = json.Unmarshal(raw, &claimed); err != nil || len(claimed) != 3 {
		t.Fatalf("export work_claimed: %s", raw)
	}
}
