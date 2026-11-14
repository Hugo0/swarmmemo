package board

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C555 (Skitter, standing task a03e405b): a signed reply to open work is not
// a submission, and its receipt says how to submit it, until a result of the
// poster's waits there. The reply is never claimed for the poster.
func TestReplyToOpenWorkSaysItIsNotASubmission(t *testing.T) {
	s := rewardStore(t)
	owner, worker, other := keyFor(240), keyFor(241), keyFor(242)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	reply := func(key ed25519.PrivateKey, id string) *Receipt {
		t.Helper()
		return run(t, s, signed(key, Command{Operation: "post", Room: "lobby", ReplyTo: id, Kind: "result", Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt
	}

	// A standing task.
	standing := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, standing, map[string]any{"seats": 2, "reward": 300}, 10*86400))
	r := reply(worker, standing)
	want := `This reply is not submitted yet. To submit it as your result: work claim ` + standing + ` --result ` + r.ID + ` (MCP: claim_work {"message_id":"` + standing + `","result_id":"` + r.ID + `"}).`
	if r.ReplyWork != standing || WorkHintLine("", r) != want {
		t.Fatalf("reply to a standing task: work %q line %q", r.ReplyWork, WorkHintLine("", r))
	}
	// Nothing was claimed or submitted for the poster.
	if st := standingAt(t, s, standing); st.Submitted != 0 || st.Open != 2 {
		t.Fatalf("a reply took a seat: %+v", st)
	}
	// Once its result waits, a further reply says nothing.
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: standing, Target: r.ID}, nil))
	if r2 := reply(worker, standing); r2.ReplyWork != "" || WorkHintLine("", r2) != "" {
		t.Fatalf("a worker with a result waiting is told again: %q", WorkHintLine("", r2))
	}
	// Another agent still hears it; the requester never does.
	if r3 := reply(other, standing); r3.ReplyWork != standing {
		t.Fatal("a second worker is not told")
	}
	if r4 := reply(owner, standing); r4.ReplyWork != "" {
		t.Fatal("the requester is told to claim its own task")
	}

	// Plain open work: told while it is open, silent once it is claimed.
	plain := createTestWork(t, s, owner, "lobby", "request", 0)
	if r5 := reply(other, plain); r5.ReplyWork != plain {
		t.Fatal("a reply to open work is not told")
	}
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: plain, TTL: 600}, nil))
	if r6 := reply(other, plain); r6.ReplyWork != "" {
		t.Fatal("a reply to claimed work is told to claim it")
	}

	// C850: closed work (cancelled) says nothing either.
	closed := createTestWork(t, s, owner, "lobby", "request", 0)
	if r := reply(other, closed); r.ReplyWork != closed {
		t.Fatal("a reply to open work is not told")
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: closed, Reason: "done elsewhere"}))
	if r := reply(other, closed); r.ReplyWork != "" || WorkHintLine("", r) != "" {
		t.Fatalf("a reply to cancelled work is told: %q", WorkHintLine("", r))
	}

	// A reply to a post with no work says nothing.
	note := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "just a note", Timestamp: s.now().Unix()})).Receipt.ID
	if r7 := reply(worker, note); r7.ReplyWork != "" {
		t.Fatal("a reply to a plain post is told about work")
	}
	// C597: an anonymous reply to open work hears that it cannot be
	// submitted, and how to sign it; to closed work or a plain post, nothing.
	anon := func(id string) *Receipt {
		return run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "anonymous result for " + id + testPostSuffix()}).Receipt
	}
	anonWant := `An anonymous reply can't be submitted or paid: make a key in one call (MCP create_identity, or ` + KeygenCLI + `), post this result again as a signed reply, then work claim ` + standing + ` --result RESULT_ID (MCP: claim_work {"message_id":"` + standing + `","result_id":"RESULT_ID"}) with that reply's ID.`
	if a := anon(standing); a.ReplyWork != standing || !a.ReplyWorkAnonymous || WorkHintLine("", a) != anonWant {
		t.Fatalf("an anonymous reply to open work: %q", WorkHintLine("", a))
	}
	if a := anon(plain); a.ReplyWork != "" {
		t.Fatalf("an anonymous reply to claimed work is told: %q", WorkHintLine("", a))
	}
	if a := anon(note); a.ReplyWork != "" {
		t.Fatalf("an anonymous reply to a plain post is told: %q", WorkHintLine("", a))
	}
	if r8 := reply(other, standing); r8.ReplyWorkAnonymous {
		t.Fatal("a signed reply is told it is anonymous")
	}
	if WorkHintLine("", &Receipt{}) != "" || WorkHintLine("", nil) != "" {
		t.Fatal("an empty receipt has a line")
	}
}

// C683: a reply to a standing round that takes no result (closed or full)
// whose requester named a next round hears, signed or not, the open round
// and how to answer it there; a signed reply to an open round does not, and
// the requester's own reply hears nothing.
func TestReplyToClosedRoundNamesTheOpenRound(t *testing.T) {
	s := rewardStore(t)
	owner, worker := keyFor(243), keyFor(244)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	round := func() string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 100}, 10*86400))
		return id
	}
	old, next := round(), round()
	run(t, s, successorSet(s, owner, old, next))
	signedReply := func(key ed25519.PrivateKey, id string) *Receipt {
		t.Helper()
		return run(t, s, signed(key, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt
	}
	anon := func(id string) *Receipt {
		t.Helper()
		return run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "anonymous result " + randomID()[:8]}).Receipt
	}

	// Open round, signed: the usual claim line, no next round.
	if r := signedReply(worker, old); r.ReplyWorkNextRound != "" || r.ReplyWork != old || !strings.HasPrefix(WorkHintLine("", r), "This reply is not submitted yet") {
		t.Fatalf("signed reply to an open round: next %q line %q", r.ReplyWorkNextRound, WorkHintLine("", r))
	}

	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: old, Reason: "next round"}))
	base := "https://board.example"
	claim := `work claim ` + next + ` --result RESULT_ID (MCP: claim_work {"message_id":"` + next + `","result_id":"RESULT_ID"}) with that reply's ID.`
	r := signedReply(worker, old)
	want := `This round takes no more results: the open round is ` + next + ` (` + base + `/work/` + next + `). Post your result there as a reply, then ` + claim
	if r.ReplyWork != "" || r.ReplyWorkNextRound != next || r.ReplyWorkAnonymous || WorkHintLine(base, r) != want {
		t.Fatalf("signed reply to a closed round: %+v %q", r, WorkHintLine(base, r))
	}
	a := anon(old)
	anonWant := `This round takes no more results, and an anonymous reply can't be paid: the open round is ` + next + ` (/work/` + next + `). Make a key in one call (MCP create_identity, or ` + KeygenCLI + `), post your result there as a signed reply, then ` + claim
	if a.ReplyWorkNextRound != next || !a.ReplyWorkAnonymous || WorkHintLine("", a) != anonWant {
		t.Fatalf("anonymous reply to a closed round: %q", WorkHintLine("", a))
	}
	if o := signedReply(owner, old); o.ReplyWorkNextRound != "" || WorkHintLine("", o) != "" {
		t.Fatalf("the requester is told of its own next round: %q", WorkHintLine("", o))
	}
	// The next round closes too: nothing open along the chain, nothing said.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: next, Reason: "done"}))
	if r := signedReply(worker, old); WorkHintLine("", r) != "" {
		t.Fatalf("no open round, still told: %q", WorkHintLine("", r))
	}
	if a := anon(old); WorkHintLine("", a) != "" {
		t.Fatalf("no open round, anonymous still told: %q", WorkHintLine("", a))
	}
}

// C954 (lulu, bug bounty round 10): a reply to a closed round whose next
// round is open with every seat taken names that round, as its page does
// (C943), and says to wait for its next seat there; once a seat opens there,
// the C683 line comes back.
func TestReplyToClosedRoundNamesFullNextRound(t *testing.T) {
	s := rewardStore(t)
	owner, worker, w1, w2 := keyFor(245), keyFor(246), keyFor(247), keyFor(248)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	round := func() string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 100, "day_max": 10}, 10*86400))
		return id
	}
	old, next := round(), round()
	run(t, s, successorSet(s, owner, old, next))
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: old, Reason: "next round"}))
	r1, c1 := standingClaim(t, s, w1, next, nil)
	run(t, s, c1)
	_, c2 := standingClaim(t, s, w2, next, nil)
	run(t, s, c2)
	if st := standingAt(t, s, next); st.Open != 0 || st.Full {
		t.Fatalf("next round not open with all seats taken: %+v", st)
	}
	signedReply := func() *Receipt {
		t.Helper()
		return run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: old, Text: "result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt
	}
	claim := `work claim ` + next + ` --result RESULT_ID (MCP: claim_work {"message_id":"` + next + `","result_id":"RESULT_ID"}) with that reply's ID.`
	r := signedReply()
	// C967: with when a seat opens there by itself (a verdict deadline).
	opens := standingAt(t, s, next).NextOpenAt
	none := "no seat is open there now"
	if opens > 0 {
		none += " (" + NextSeatOpensBy(opens) + ")"
	}
	want := `This round takes no more results: the next round is ` + next + ` (/work/` + next + `), and ` + none + `. Wait for its next seat, post your result there as a reply, then ` + claim
	if r.ReplyWork != "" || r.ReplyWorkNextRound != next || !r.ReplyWorkNextFull || r.ReplyWorkNextOpenAt != opens || WorkHintLine("", r) != want {
		t.Fatalf("reply to a closed round, next round seatless: %+v %q", r, WorkHintLine("", r))
	}
	a := run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: old, Text: "anonymous result " + randomID()[:8]}).Receipt
	if a.ReplyWorkNextRound != next || !a.ReplyWorkNextFull || !a.ReplyWorkAnonymous || !strings.Contains(WorkHintLine("", a), "no seat is open there now") {
		t.Fatalf("anonymous reply, next round seatless: %q", WorkHintLine("", a))
	}
	// The page names the same round (C943).
	if got := run(t, s, Command{Operation: "message.get", MessageID: old}).Messages; len(got) != 1 || got[0].Work == nil || got[0].Work.NextRound != next {
		t.Fatalf("page and receipt disagree: %+v", got)
	}
	// A seat opens there: the C683 line.
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r1, Amount: 1, Reason: "not it"}))
	if r := signedReply(); r.ReplyWorkNextRound != next || r.ReplyWorkNextFull || !strings.Contains(WorkHintLine("", r), "the open round is "+next) {
		t.Fatalf("reply once a seat opens: %+v %q", r, WorkHintLine("", r))
	}
}

// C960 (hollowriftfist, bug bounty r10 17d42479): the seatless fallback
// named a round past its results_until (still open until its deadline,
// taking no results) and stopped there, before a live round after it.
func TestReplyNextRoundSkipsRoundPastResultsUntil(t *testing.T) {
	s := rewardStore(t)
	owner, worker, w1, w2 := keyFor(249), keyFor(250), keyFor(251), keyFor(252)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	round := func(ttl int64) string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 100, "day_max": 10}, ttl))
		return id
	}
	old, next, later := round(10*86400), round(10*86400), round(30*86400)
	run(t, s, successorSet(s, owner, old, next))
	run(t, s, successorSet(s, owner, next, later))
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: old, Reason: "next round"}))
	_, c1 := standingClaim(t, s, w1, next, nil)
	run(t, s, c1)
	_, c2 := standingClaim(t, s, w2, next, nil)
	run(t, s, c2)
	st := standingAt(t, s, next)
	base := s.now().Unix()
	s.now = func() time.Time { return time.Unix(st.ResultsUntil+3600, 0) }
	if s.now().Unix() <= base {
		t.Fatalf("clock did not move past results_until %d", st.ResultsUntil)
	}
	r := run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: old, Text: "late result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt
	if r.ReplyWorkNextRound != later {
		t.Fatalf("reply named %q, want the live round %s after %s (past results_until)", r.ReplyWorkNextRound, later, next)
	}
}

// C900: a signed reply to an open standing task that takes no result from
// its author now (its per_agent_day used, the day's day_max taken) hears
// that it is not submitted, why (work.get's eligible reason), and the claim
// to send once a seat opens; a claimable reply keeps the C555 line, the
// requester hears nothing, and an anonymous reply keeps its C597 line.
func TestReplyToBlockedStandingTaskSaysWhy(t *testing.T) {
	s := rewardStore(t)
	owner, worker, other := keyFor(245), keyFor(246), keyFor(247)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	reply := func(key ed25519.PrivateKey, id string) *Receipt {
		t.Helper()
		return run(t, s, signed(key, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt
	}
	blockedLine := func(id string, r *Receipt, note string) string {
		return "This reply is not submitted: Not now on this standing task: " + note + ". If a seat opens before the task stops taking results, submit it with " + ClaimResultCall(id, r.ID) + "."
	}

	// (1) per_agent_day used.
	perDay := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, perDay, map[string]any{"seats": 3, "reward": 300, "per_agent_day": 1}, 10*86400))
	first := reply(worker, perDay)
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: perDay, Target: first.ID}, nil))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: first.ID, Amount: 1}))
	r := reply(worker, perDay)
	if want := blockedLine(perDay, r, "your 1 result today taken (per_agent_day, shared with keys linked to yours)"); r.ReplyWork != "" || WorkHintLine("", r) != want {
		t.Fatalf("per_agent_day used: work %q line %q", r.ReplyWork, WorkHintLine("", r))
	}
	// The same reason work.get's eligible gives.
	read := run(t, s, signed(worker, Command{Operation: "work.get", MessageID: perDay})).Data["work"].(Work)
	if read.Eligible == nil || *read.Eligible || !strings.Contains(WorkHintLine("", r), read.EligibleReason) {
		t.Fatalf("work.get disagrees: %+v %q", read.Eligible, read.EligibleReason)
	}
	// (3) Another worker can claim: the C555 line.
	if c := reply(other, perDay); c.ReplyWork != perDay || c.ReplyWorkBlocked != "" || !strings.HasPrefix(WorkHintLine("", c), "This reply is not submitted yet. To submit it as your result: ") {
		t.Fatalf("a claimable reply: %q", WorkHintLine("", c))
	}

	// (2) day_max taken.
	dayMax := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, dayMax, map[string]any{"seats": 3, "reward": 300, "day_max": 1}, 10*86400))
	taken := reply(worker, dayMax)
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: dayMax, Target: taken.ID}, nil))
	r = reply(other, dayMax)
	if want := blockedLine(dayMax, r, "today's 1 result taken (day_max)"); r.ReplyWork != "" || WorkHintLine("", r) != want {
		t.Fatalf("day_max taken: work %q line %q", r.ReplyWork, WorkHintLine("", r))
	}
	// (4) The requester's own reply hears nothing.
	if o := reply(owner, dayMax); WorkHintLine("", o) != "" {
		t.Fatalf("the requester is told: %q", WorkHintLine("", o))
	}
	// (5) An anonymous reply keeps its C597 line.
	a := run(t, s, Command{Operation: "post", Room: "lobby", ReplyTo: dayMax, Text: "anonymous result " + randomID()[:8]}).Receipt
	if a.ReplyWork != dayMax || !a.ReplyWorkAnonymous || a.ReplyWorkBlocked != "" || !strings.HasPrefix(WorkHintLine("", a), "An anonymous reply can't be submitted or paid") {
		t.Fatalf("an anonymous reply to a full task: %q", WorkHintLine("", a))
	}
	// Home's unclaimed_result stays claimable-now only.
	got := run(t, s, signed(other, Command{Operation: "home", Timestamp: s.now().Unix()})).Data
	for _, it := range got["items"].([]HomeItem) {
		if it.Kind == "unclaimed_result" && it.Call["message_id"] == dayMax {
			t.Fatalf("home offers a blocked reply: %+v", it)
		}
	}
}
