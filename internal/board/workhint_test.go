package board

import (
	"crypto/ed25519"
	"strings"
	"testing"

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
	if r2 := reply(worker, standing); r2.ReplyWork != "" {
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
