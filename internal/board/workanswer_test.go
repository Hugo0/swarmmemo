package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A sealed known answer (C587) on the real store: committed at create and
// public while sealed, revealed by the requester only after close, verified
// against the commit, once, and idempotent for the same values.

const (
	testAnswer = "42"
	testSalt   = "3f9c2a7d1e6b4c58a0d2e4f6a8b0c1d2"
)

func revealCommand(s *Store, key ed25519.PrivateKey, id, answer, salt string) Command {
	b, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "answer": answer, "salt": salt})
	return signed(key, Command{Operation: WorkReveal, MessageID: id, Data: string(b), Timestamp: s.now().Unix()})
}

func TestWorkAnswerCommitFormat(t *testing.T) {
	// sha256("3f9c2a7d1e6b4c58a0d2e4f6a8b0c1d2\n42"), as `printf '%s\n%s' SALT 42 | sha256sum` prints it.
	if got := WorkAnswerCommit(testSalt, testAnswer); got != "c01544b3ecadb14ba1b4c8a99b3f94694509716d89e9b2c2914214e584b735b6" {
		t.Fatalf("commit %q", got)
	}
}

func TestWorkKnownAnswerSealRevealVerify(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, other := keyFor(250), keyFor(251), keyFor(252)
	commit := WorkAnswerCommit(testSalt, testAnswer)
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Compute N and report it", Timestamp: s.now().Unix()})).Receipt.ID

	// Bad commits are refused at create.
	for _, bad := range []map[string]any{
		{"answer_commit": strings.ToUpper(commit)},
		{"answer_commit": commit[:63]},
		{"answer_commit_note": "the integer N"},
		{"answer_commit": commit, "answer_commit_note": "two\nlines"},
	} {
		fails(t, s, noteCreate(s, owner, id, bad), "invalid_work_data")
	}
	run(t, s, noteCreate(s, owner, id, map[string]any{"answer_commit": commit, "answer_commit_note": "the integer N"}))

	// Shown publicly while sealed: work.get, works.list and the history.
	w := getTestWork(t, s, id)
	if w.KnownAnswer == nil || w.KnownAnswer.Commit != commit || w.KnownAnswer.Note != "the integer N" || w.KnownAnswer.State != WorkAnswerSealed || w.KnownAnswer.Answer != "" {
		t.Fatalf("sealed %+v", w.KnownAnswer)
	}
	listed := false
	for _, item := range run(t, s, Command{Operation: "works.list"}).Data["works"].([]Work) {
		listed = listed || item.ID == id && item.KnownAnswer != nil && item.KnownAnswer.Commit == commit
	}
	if !listed {
		t.Fatal("works.list leaves out the commit")
	}
	if plain := getTestWork(t, s, createTestWork(t, s, owner, "lobby", "request", 0)); plain.KnownAnswer != nil {
		t.Fatalf("work without a commit reads %+v", plain.KnownAnswer)
	}

	// Before close: wrong values refused, the right ones held, not
	// published (C734, workheldanswer_test.go has the readers).
	fails(t, s, revealCommand(s, owner, id, "43", testSalt), "reveal_mismatch")
	if ack := run(t, s, revealCommand(s, owner, id, testAnswer, testSalt)).Data["ack"].(WorkAck); !strings.HasPrefix(ack.Note, "Held, not published") {
		t.Fatalf("hold ack %+v", ack)
	}
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600}))
	result := workResult(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}))
	if ack := run(t, s, revealCommand(s, owner, id, testAnswer, testSalt)).Data["ack"].(WorkAck); !strings.HasPrefix(ack.Note, "Already held") {
		t.Fatalf("second hold ack %+v", ack)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}))

	// After close: not the requester's, wrong values, malformed data.
	fails(t, s, revealCommand(s, other, id, testAnswer, testSalt), "work_forbidden")
	fails(t, s, revealCommand(s, worker, id, testAnswer, testSalt), "work_forbidden")
	fails(t, s, revealCommand(s, owner, id, "43", testSalt), "reveal_mismatch")
	fails(t, s, revealCommand(s, owner, id, testAnswer, "wrong"), "reveal_mismatch")
	fails(t, s, revealCommand(s, owner, id, testAnswer+"\n", testSalt), "reveal_mismatch")
	fails(t, s, revealCommand(s, owner, id, testAnswer, "a\nb"), "invalid_work_data")
	fails(t, s, revealCommand(s, owner, id, "", testSalt), "invalid_work_data")
	fails(t, s, signed(owner, Command{Operation: WorkReveal, MessageID: id, Data: `{"schema":1,"generation":"` + s.generation + `","answer":"42"}`, Timestamp: s.now().Unix()}), "invalid_work_data")
	if got := getTestWork(t, s, id).KnownAnswer; got.State != WorkAnswerSealed {
		t.Fatalf("a refused reveal changed %+v", got)
	}

	before := getTestWork(t, s, id)
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	ack := run(t, s, revealCommand(s, owner, id, testAnswer, testSalt)).Data["ack"].(WorkAck)
	if ack.State != "accepted" || !strings.Contains(ack.Note, "verified") {
		t.Fatalf("reveal ack %+v", ack)
	}
	w = getTestWork(t, s, id)
	k := w.KnownAnswer
	if k == nil || k.State != WorkAnswerRevealed || k.Answer != testAnswer || k.Salt != testSalt || !k.Verified || k.RevealedAt != testTime+60 {
		t.Fatalf("revealed %+v", k)
	}
	if w.State != "accepted" || w.UpdatedAt != before.UpdatedAt || w.Fence != before.Fence {
		t.Fatalf("the reveal changed the work: %+v", w)
	}
	b, _ := json.Marshal(w)
	if !strings.Contains(string(b), `"known_answer":{"commit":"`+commit+`","note":"the integer N","state":"revealed","held":true,"answer":"42"`) {
		t.Fatalf("work JSON %s", b)
	}

	// Once: the same values again are idempotent, other values refused.
	history := len(run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition))
	again := run(t, s, revealCommand(s, owner, id, testAnswer, testSalt)).Data["ack"].(WorkAck)
	if !strings.Contains(again.Note, "Already revealed") {
		t.Fatalf("second reveal %+v", again)
	}
	if n := len(run(t, s, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)); n != history {
		t.Fatalf("an idempotent reveal added a transition: %d to %d", history, n)
	}
	fails(t, s, revealCommand(s, owner, id, "43", testSalt), "work_state_conflict")
}

func TestWorkKnownAnswerAfterCancelAndExpiry(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(253)
	commit := WorkAnswerCommit(testSalt, testAnswer)
	post := func() string {
		return run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Check" + testPostSuffix(), Timestamp: s.now().Unix()})).Receipt.ID
	}
	cancelled := post()
	run(t, s, noteCreate(s, owner, cancelled, map[string]any{"answer_commit": commit}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: cancelled, Reason: "done"}))
	run(t, s, revealCommand(s, owner, cancelled, testAnswer, testSalt))
	if k := getTestWork(t, s, cancelled).KnownAnswer; k.State != WorkAnswerRevealed {
		t.Fatalf("after cancel %+v", k)
	}

	expiring := post()
	c := noteCreate(s, owner, expiring, map[string]any{"answer_commit": commit})
	c.TTL = 120
	run(t, s, signed(owner, c))
	if ack := run(t, s, revealCommand(s, owner, expiring, testAnswer, testSalt)).Data["ack"].(WorkAck); !strings.HasPrefix(ack.Note, "Held") {
		t.Fatalf("open work's reveal is held: %+v", ack)
	}
	s.now = func() time.Time { return time.Unix(testTime+121, 0) }
	run(t, s, revealCommand(s, owner, expiring, testAnswer, testSalt))

	// No commit, nothing to reveal.
	plain := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: plain, Reason: "done"}))
	fails(t, s, revealCommand(s, owner, plain, testAnswer, testSalt), "work_state_conflict")
}
