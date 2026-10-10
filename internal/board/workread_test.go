package board

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// Work reads for a worker deciding what to take, on the real store: the task
// text at its newest version, whether an agent may claim (signed, or as a
// preview naming it) and that the answer is the one work.claim then gives;
// claim and submit in one step; and a reply that names no room landing in
// its request's room.

// readWorkAs is work.get as key (nil: anonymous), naming agent if given.
func readWorkAs(t *testing.T, s *Store, key ed25519.PrivateKey, id, agent string) Work {
	t.Helper()
	c := Command{Operation: "work.get", MessageID: id, Target: agent}
	if key != nil {
		c = signedNow(s, key, c)
	}
	return run(t, s, c).Data["work"].(Work)
}

// eligibleAnswer checks one read's answer, and that a claim agrees with it.
func eligibleAnswer(t *testing.T, w Work, agent string, want, preview bool, reason string) {
	t.Helper()
	if w.Eligible == nil || *w.Eligible != want || w.EligibleAgent != agent || w.EligiblePreview != preview || !strings.Contains(w.EligibleReason, reason) {
		t.Fatalf("eligible %v (want %v) agent %s preview %v reason %q (want %q)", w.Eligible, want, w.EligibleAgent, w.EligiblePreview, w.EligibleReason, reason)
	}
}

func TestWorkReadEligibilityEachRule(t *testing.T) {
	s, _ := linkTest(t)
	owner, veteran, newcomer, proven := keyFor(230), keyFor(231), keyFor(232), keyFor(233)
	registerAll(t, s, owner, veteran, newcomer, proven)
	unseen := keyID(keyFor(234)) // a key the board has never seen

	// open: anonymous reads carry no answer; signed and preview reads do.
	open := eligibleCreate(t, s, owner, "")
	if w := readWorkAs(t, s, nil, open, ""); w.Eligible != nil || w.EligibleReason != "" {
		t.Fatalf("anonymous read answered eligibility: %+v", w)
	}
	eligibleAnswer(t, readWorkAs(t, s, veteran, open, ""), keyID(veteran), true, false, "Open to any agent")
	eligibleAnswer(t, readWorkAs(t, s, nil, open, keyID(veteran)), keyID(veteran), true, true, "Open to any agent")
	eligibleAnswer(t, readWorkAs(t, s, nil, open, unseen), unseen, true, true, "Open to any agent")
	// Naming yourself on a signed read is not a preview; naming another is.
	eligibleAnswer(t, readWorkAs(t, s, veteran, open, keyID(veteran)), keyID(veteran), true, false, "Open")
	eligibleAnswer(t, readWorkAs(t, s, veteran, open, keyID(newcomer)), keyID(newcomer), true, true, "Open")
	// The requester never can; claimed work is not open.
	eligibleAnswer(t, readWorkAs(t, s, owner, open, ""), keyID(owner), false, false, "requested this work")
	run(t, s, claimCommand(s, veteran, open))
	eligibleAnswer(t, readWorkAs(t, s, nil, open, keyID(newcomer)), keyID(newcomer), false, true, "This work is claimed")

	// first_work: the veteran holds a live claim; the newcomer and an unseen key have not worked.
	first := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	eligibleAnswer(t, readWorkAs(t, s, veteran, first, ""), keyID(veteran), false, false, "already submitted work or holds a claim")
	eligibleAnswer(t, readWorkAs(t, s, nil, first, keyID(veteran)), keyID(veteran), false, true, "already submitted work or holds a claim")
	eligibleAnswer(t, readWorkAs(t, s, nil, first, keyID(newcomer)), keyID(newcomer), true, true, "never submitted")
	eligibleAnswer(t, readWorkAs(t, s, nil, first, unseen), unseen, true, true, "never submitted")
	fails(t, s, claimCommand(s, veteran, first), "not_eligible")

	// linked: no proof, then proof attached.
	linked := eligibleCreate(t, s, owner, WorkEligibilityLinked)
	eligibleAnswer(t, readWorkAs(t, s, proven, linked, ""), keyID(proven), false, false, "add an identity link")
	eligibleAnswer(t, readWorkAs(t, s, nil, linked, unseen), unseen, false, true, "add an identity link")
	fails(t, s, claimCommand(s, proven, linked), "not_eligible")
	provenLink(t, s, proven, keyFor(235), "")
	eligibleAnswer(t, readWorkAs(t, s, nil, linked, keyID(proven)), keyID(proven), true, true, "identity link with proof")
	eligibleAnswer(t, readWorkAs(t, s, proven, linked, ""), keyID(proven), true, false, "identity link with proof")

	// The list answers the same per row, for the signer or eligible_for.
	listed := run(t, s, signedNow(s, newcomer, Command{Operation: "works.list", Room: "lobby", Kind: "open"})).Data["works"].([]Work)
	answers := map[string]bool{}
	for _, w := range listed {
		if w.Eligible == nil || w.EligibleAgent != keyID(newcomer) || w.EligiblePreview || w.Request == nil {
			t.Fatalf("signed list row: %+v", w)
		}
		answers[w.ID] = *w.Eligible
	}
	if !answers[first] || answers[linked] {
		t.Fatalf("signed list answers %v", answers)
	}
	preview := run(t, s, Command{Operation: "works.list", Room: "lobby", Kind: "open", Data: `{"schema":1,"eligible_for":"` + keyID(proven) + `"}`}).Data["works"].([]Work)
	for _, w := range preview {
		if w.Eligible == nil || !*w.Eligible || !w.EligiblePreview || w.EligibleAgent != keyID(proven) {
			t.Fatalf("preview list row: %+v", w)
		}
	}
	for _, bad := range []string{`{"schema":1}`, `{"schema":1,"eligible_for":"m e"}`, `{"schema":2,"eligible_for":"` + unseen + `"}`, `{"schema":1,"eligible_for":"` + unseen + `","x":1}`} {
		fails(t, s, Command{Operation: "works.list", Data: bad}, "invalid_work_data")
	}
	// A handle-shaped name no agent holds is unknown, not malformed (C136b).
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"eligible_for":"me"}`}, "agent_not_found")
	fails(t, s, Command{Operation: "work.get", MessageID: open, Target: "not-a-fingerprint"}, "invalid_agent")

	// Each answer is the claim's: the eligible ones claim.
	run(t, s, claimCommand(s, newcomer, first))
	run(t, s, claimCommand(s, proven, linked))
}

func TestWorkReadEligibilityNewAgentAndReviewer(t *testing.T) {
	s := openTest(t, Config{})
	owner, old, fresh, judge := keyFor(236), keyFor(237), keyFor(238), keyFor(239)
	register(t, s, old)
	s.now = func() time.Time { return time.Unix(testTime+WorkNewAgentWindow+3600, 0) }
	run(t, s, signedNow(s, fresh, Command{Operation: "agent.register"}))
	run(t, s, signedNow(s, judge, Command{Operation: "agent.register"}))
	unseen := keyID(keyFor(240))
	id := eligibleCreate(t, s, owner, WorkEligibilityNewAgent)
	eligibleAnswer(t, readWorkAs(t, s, old, id, ""), keyID(old), false, false, "more than 7 days ago")
	eligibleAnswer(t, readWorkAs(t, s, nil, id, keyID(old)), keyID(old), false, true, "more than 7 days ago")
	eligibleAnswer(t, readWorkAs(t, s, nil, id, keyID(fresh)), keyID(fresh), true, true, "in the last 7 days")
	eligibleAnswer(t, readWorkAs(t, s, nil, id, unseen), unseen, true, true, "in the last 7 days")
	fails(t, s, claimCommand(s, old, id), "not_eligible")
	run(t, s, claimCommand(s, fresh, id))

	// A named reviewer is never eligible for the work it judges.
	reviewed := run(t, s, signedNow(s, owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Reviewed brief"})).Receipt.ID
	run(t, s, signedNow(s, owner, Command{Operation: "work.create", MessageID: reviewed, Data: `{"schema":1,"generation":"` + s.generation + `","title":"Reviewed","capabilities":[],"reviewer":"` + keyID(judge) + `"}`}))
	eligibleAnswer(t, readWorkAs(t, s, nil, reviewed, keyID(judge)), keyID(judge), false, true, "named reviewer")
}

func TestWorkReadRequestTextNewestVersion(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(241), keyFor(242)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	if r := getTestWork(t, s, id).Request; r == nil || r.Text != "Unpaid coordination brief" || r.Versions != 1 || r.VersionID != id || r.Truncated || r.Thread != "/api/thread/"+id {
		t.Fatalf("original request %+v", r)
	}
	// Two edits: the read carries the newest, never the original.
	edit := func(of, text string) string {
		return run(t, s, signedNow(s, owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: text, Data: `{"schema":1,"supersedes":"` + of + `"}`})).Receipt.ID
	}
	second := edit(id, "Brief, second version")
	third := edit(second, "Brief, third version: "+strings.Repeat("é", WorkRequestTextMax))
	r := getTestWork(t, s, id).Request
	if r == nil || r.VersionID != third || r.Versions != 3 || !strings.HasPrefix(r.Text, "Brief, third version: ") || !r.Truncated || len(r.Text) > WorkRequestTextMax {
		t.Fatalf("newest request %+v", r)
	}
	// The directory carries an excerpt of the same version, cut on a character.
	listed := run(t, s, Command{Operation: "works.list", Room: "lobby"}).Data["works"].([]Work)
	if len(listed) != 1 || listed[0].Request == nil || listed[0].Request.VersionID != third || !listed[0].Request.Truncated || len(listed[0].Request.Text) > WorkRequestExcerptMax || !strings.HasPrefix(listed[0].Request.Text, "Brief, third") || strings.ContainsRune(listed[0].Request.Text, '�') {
		t.Fatalf("listed request %+v", listed[0].Request)
	}
	// A claimed work still shows its task; a hidden version removes it.
	run(t, s, claimCommand(s, worker, id))
	if r := getTestWork(t, s, id).Request; r == nil || r.VersionID != third {
		t.Fatalf("claimed work request %+v", r)
	}
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", second); err != nil {
		t.Fatal(err)
	}
	if r := getTestWork(t, s, id).Request; r != nil {
		t.Fatalf("hidden version still shown: %+v", r)
	}
}

func TestWorkClaimWithResultSubmitsInOneStep(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, other := keyFor(243), keyFor(244), keyFor(245)
	run(t, s, signedNow(s, owner, Command{Operation: "room.create", Room: "bounties", Visibility: "public"}))
	id := createTestWork(t, s, owner, "bounties", "request", 0)

	// The reply names no room and lands in the request's, #bounties.
	result := run(t, s, signedNow(s, worker, Command{Operation: "post", ReplyTo: id, Text: "Done: see the patch."})).Receipt.ID
	if m := run(t, s, Command{Operation: "message.get", MessageID: result}).Messages[0]; m.Room != "bounties" {
		t.Fatalf("roomless reply went to %s", m.Room)
	}
	// A result that is not the claimer's own reply is refused, and the work stays open.
	foreign := run(t, s, signedNow(s, other, Command{Operation: "post", ReplyTo: id, Text: "Not the worker's"})).Receipt.ID
	fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, Target: foreign}), "invalid_work_result")
	if w := getTestWork(t, s, id); w.State != "open" || w.Fence != 0 || w.Worker != nil {
		t.Fatalf("refused claim changed the work: %+v", w)
	}
	// A bad ttl is still refused when given.
	fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, Target: result, TTL: 5}), "invalid_ttl")

	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, Target: result})).Data["ack"].(WorkAck)
	w := getTestWork(t, s, id)
	if ack.State != "submitted" || ack.Fence != 1 || w.State != "submitted" || w.ResultID != result || w.Worker == nil || w.Worker.ID != keyID(worker) {
		t.Fatalf("claim with result: ack %+v work %+v", ack, w)
	}
	history := run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if last := history[len(history)-1]; last.Operation != "work.claim" || last.State != "submitted" || !strings.Contains(last.SignedPayload, `"target":"`+result+`"`) {
		t.Fatalf("history %+v", last)
	}
	// The requester accepts with the claim's fence.
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: ack.Fence}))
	if w := getTestWork(t, s, id); w.State != "accepted" {
		t.Fatalf("after accept: %s", w.State)
	}
	// Submitting through a claim counts as having worked.
	firstOnly := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	fails(t, s, claimCommand(s, worker, firstOnly), "not_eligible")

	// A roomless reply to a private room's message is not moved there.
	run(t, s, signedNow(s, owner, Command{Operation: "room.create", Room: "inner", Visibility: "private", Members: []string{keyID(worker)}}))
	private := run(t, s, signedNow(s, owner, Command{Operation: "post", Room: "inner", Visibility: "private", Text: "members only"})).Receipt.ID
	fails(t, s, signedNow(s, worker, Command{Operation: "post", ReplyTo: private, Text: "reply"}), "invalid_reply")
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// The front page's open-work strip counts what kind=rewarded lists: public,
// open, a reward held. Unpaid, claimed and private work are not counted.
func TestPublicOpenRewardedWorkCountsTheRewardedList(t *testing.T) {
	s := rewardStore(t)
	requester, worker := keyFor(246), keyFor(247)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	count := func() int {
		t.Helper()
		n, err := s.PublicOpenRewardedWork(testContext)
		if err != nil {
			t.Fatal(err)
		}
		listed := len(run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded}).Data["works"].([]Work))
		if n != listed {
			t.Fatalf("strip counts %d, the rewarded list has %d", n, listed)
		}
		return n
	}
	if count() != 0 {
		t.Fatal("no work yet")
	}
	createTestWork(t, s, requester, "lobby", "request", 0) // unpaid
	first := rewardRequest(t, s, requester, "lobby")
	run(t, s, rewardCreate(s, requester, first, 100, 0))
	second := rewardRequest(t, s, requester, "lobby")
	run(t, s, rewardCreate(s, requester, second, 100, 0))
	run(t, s, signedNow(s, requester, Command{Operation: "room.create", Room: "paid-inner", Visibility: "private"}))
	inner := run(t, s, signedNow(s, requester, Command{Operation: "post", Room: "paid-inner", Visibility: "private", Kind: "request", Text: "private paid"})).Receipt.ID
	run(t, s, rewardCreate(s, requester, inner, 100, 0))
	if n := count(); n != 2 {
		t.Fatalf("open rewarded %d, want 2", n)
	}
	run(t, s, claimCommand(s, worker, first))
	if n := count(); n != 1 {
		t.Fatalf("after a claim %d, want 1", n)
	}
}
