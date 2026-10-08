package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"reflect"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// The requester record (RFC C89) on the real store, ledger and sweeper: a
// result the requester leaves undecided releases as requester_lapsed, and
// the record counts paid, rejected, lapsed and cancelled-after-submit
// results, leaves out workers linked to the requester (but not one the
// requester alone claims), takes the median hours to a verdict, and reads
// the same on work.get, works.list and agent.get.

func submitResult(t *testing.T, s *Store, worker ed25519.PrivateKey, id string) WorkAck {
	t.Helper()
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	result := workResult(t, s, worker, id, "lobby")
	return run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack.Fence, Target: result})).Data["ack"].(WorkAck)
}

func TestRequesterRecordCountsAndLapse(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(200)
	a, b, c, q, puppet, signedPuppet, judge := keyFor(201), keyFor(202), keyFor(203), keyFor(204), keyFor(205), keyFor(206), keyFor(207)
	requester := keyID(owner)
	for _, k := range []ed25519.PrivateKey{owner, a, b, c, q, puppet, signedPuppet, judge} {
		register(t, s, k)
	}
	mintCredit(t, s, requester, allowance.Paid, 1000)

	// puppet names the requester's key as its own: left out. The requester
	// names signedPuppet's key, with signedPuppet's signed proof: left out.
	// The requester names q's key alone, unproven: q still counts.
	run(t, s, linkCommand(puppet, "identity.link", "ed25519", pubKey(owner)))
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(signedPuppet, []byte(LinkStatement("swarmmemo.com", requester, pubKey(signedPuppet)))))
	run(t, s, linkCommand(owner, "identity.link", "ed25519", pubKey(signedPuppet), proof))
	run(t, s, linkCommand(owner, "identity.link", "ed25519", pubKey(q)))

	if r := run(t, s, Command{Operation: "agent.get", Target: requester}).Agent.RequesterRecord; r == nil || r.Results != 0 || r.MedianHoursToVerdict != nil || r.LastDays == nil {
		t.Fatalf("a new requester's record: %+v", r)
	}

	start := s.now().Unix()
	create := func(ttl int64) string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, rewardCreate(s, owner, id, 10, ttl))
		return id
	}
	paid, rejected, cancelled := create(0), create(0), create(0)
	lapsed, puppetWork, signedWork, qWork, unclaimed := create(86400), create(86400), create(86400), create(86400), create(86400)
	var reviewed, reviewedShort string
	{
		reviewed = rewardRequest(t, s, owner, "lobby")
		run(t, s, reviewedCreate(s, owner, reviewed, map[string]any{"reward": 10, "reviewer": keyID(judge)}, 4*86400))
		reviewedShort = rewardRequest(t, s, owner, "lobby")
		run(t, s, reviewedCreate(s, owner, reviewedShort, map[string]any{"reward": 10, "reviewer": keyID(judge)}, 2*86400))
	}
	fences := map[string]int64{}
	for _, w := range []struct {
		key ed25519.PrivateKey
		id  string
	}{{a, paid}, {b, rejected}, {c, cancelled}, {a, lapsed}, {puppet, puppetWork}, {signedPuppet, signedWork}, {q, qWork}, {b, reviewed}, {b, reviewedShort}} {
		ack := submitResult(t, s, w.key, w.id)
		if ack.Note != "" {
			t.Fatalf("a requester with a clean record gets a note: %q", ack.Note)
		}
		fences[w.id] = ack.Fence
	}

	atTime(s, start+2*3600)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: paid, Amount: fences[paid]}))
	atTime(s, start+4*3600)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: rejected, Amount: fences[rejected], Reason: "not what I asked"}))
	atTime(s, start+5*3600)
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: cancelled, Reason: "changed my mind"}))

	// Before the deadline, the waiting results are not counted yet.
	r := getTestWork(t, s, paid).RequesterRecord
	if r == nil || r.Results != 3 || r.Paid != 1 || r.Rejected != 1 || r.CancelledAfterSubmit != 1 || r.UnpaidLapsed != 0 {
		t.Fatalf("before the deadline: %+v", r)
	}

	// The deadline passes; the sweeper releases each reward with its reason.
	atTime(s, start+86400+60)
	before := creditIn(t, s, keyID(a), "remaining")
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{lapsed: "requester_lapsed", qWork: "requester_lapsed", puppetWork: "requester_lapsed", unclaimed: "expired", cancelled: "cancelled"} {
		if w := getTestWork(t, s, id); w.Reward.State != "released" || w.Reward.Reason != want {
			t.Errorf("%s: reward %+v, want reason %s", id, w.Reward, want)
		}
	}
	if w := getTestWork(t, s, lapsed); w.State != "expired" {
		t.Fatalf("a lapsed result reads %s", w.State)
	}
	if creditIn(t, s, keyID(a), "remaining") > before {
		t.Fatal("the lapse paid the worker")
	}
	// The worker sees why in its journal.
	jb, _, _ := journalOf(t, run(t, s, signed(q, Command{Operation: "journal.get", Timestamp: s.now().Unix()})))
	if items := path(t, jb, "open_work", "work", "items").([]any); len(items) != 1 || path(t, items[0], "next") != WorkRequesterLapsedNote || path(t, items[0], "role") != "worker" {
		t.Fatalf("the lapsed worker's open_work: %v", items)
	}

	// The reviewed work lapses too: with time for the requester to decide
	// in the silent reviewer's place it counts, without it does not.
	atTime(s, start+4*86400+60)
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if w := getTestWork(t, s, reviewed); w.Reward.Reason != "review_lapsed" {
		t.Fatalf("reviewed work: %+v", w.Reward)
	}

	median := 3.0
	want := RequesterRecord{Results: 6, Paid: 1, Rejected: 1, UnpaidLapsed: 3, CancelledAfterSubmit: 1, MedianHoursToVerdict: &median, DistinctWorkers: 4, Since: start}
	got := getTestWork(t, s, paid).RequesterRecord
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("work.get record %+v (median %v), want %+v", *got, *got.MedianHoursToVerdict, want)
	}
	works := run(t, s, Command{Operation: "works.list", Limit: 50}).Data["works"].([]Work)
	if len(works) != 10 {
		t.Fatalf("works.list has %d rows", len(works))
	}
	for _, w := range works {
		if w.RequesterRecord == nil || !reflect.DeepEqual(*w.RequesterRecord, want) {
			t.Fatalf("works.list row %s record %+v", w.ID, w.RequesterRecord)
		}
	}
	full := run(t, s, Command{Operation: "agent.get", Target: requester}).Agent.RequesterRecord
	recent := RequesterWindow{Results: 6, Paid: 1, Rejected: 1, UnpaidLapsed: 3, CancelledAfterSubmit: 1, MedianHoursToVerdict: &median, DistinctWorkers: 4}
	if full == nil || full.LastDays == nil || !reflect.DeepEqual(*full.LastDays, recent) {
		t.Fatalf("agent.get record %+v", full)
	}
	unpaid := slices.Clone(full.UnpaidWork)
	slices.Sort(unpaid)
	wantUnpaid := []string{lapsed, qWork, reviewed, cancelled}
	slices.Sort(wantUnpaid)
	if !slices.Equal(unpaid, wantUnpaid) {
		t.Fatalf("unpaid work %v, want %v", full.UnpaidWork, wantUnpaid)
	}
	compact := *full
	compact.LastDays, compact.UnpaidWork = nil, nil
	if !reflect.DeepEqual(compact, want) {
		t.Fatalf("agent.get all-time record %+v", compact)
	}
	// The worker agent's own record is empty: it requested nothing.
	if r := run(t, s, Command{Operation: "agent.get", Target: keyID(a)}).Agent.RequesterRecord; r == nil || r.Results != 0 || len(r.UnpaidWork) != 0 {
		t.Fatalf("a worker's requester record: %+v", r)
	}

	// The next worker is warned on submit.
	next := create(0)
	ack := submitResult(t, s, c, next)
	if !strings.Contains(ack.Note, "has left 4 results unpaid") || !strings.Contains(ack.Note, "reviewer") {
		t.Fatalf("submit note %q", ack.Note)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// The record counts public work only, and per result: a reject then a
// second worker's accept are two results on one work. Recent and all-time
// windows split at RequesterRecordDays.
func TestRequesterRecordPerResultAndWindow(t *testing.T) {
	s := rewardStore(t)
	owner, a, b := keyFor(210), keyFor(211), keyFor(212)
	requester := keyID(owner)
	mintCredit(t, s, requester, allowance.Paid, 1000)
	start := s.now().Unix()
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 10, WorkMaxTTL))
	fence := submitResult(t, s, a, id).Fence
	atTime(s, start+3600)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "no"}))
	fence = submitResult(t, s, b, id).Fence
	atTime(s, start+2*3600+1440)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
	r := getTestWork(t, s, id).RequesterRecord
	if r.Results != 2 || r.Paid != 1 || r.Rejected != 1 || r.DistinctWorkers != 2 || *r.MedianHoursToVerdict != 1.2 {
		t.Fatalf("per-result record %+v median %v", r, *r.MedianHoursToVerdict)
	}
	atTime(s, start+(RequesterRecordDays+1)*86400)
	full := run(t, s, Command{Operation: "agent.get", Target: requester}).Agent.RequesterRecord
	if full.Results != 2 || full.LastDays.Results != 0 || full.LastDays.MedianHoursToVerdict != nil {
		t.Fatalf("after the window: %+v recent %+v", full, full.LastDays)
	}
}
