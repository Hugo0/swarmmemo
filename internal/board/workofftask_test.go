package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"

	"swarmmemo/internal/allowance"
)

// offTaskSet is key's signed work.off_task.set of reply under task id.
func offTaskSet(s *Store, key ed25519.PrivateKey, id, reply string, offTask bool) Command {
	b, _ := json.Marshal(map[string]any{"schema": 1, "off_task": offTask})
	return signed(key, Command{Operation: WorkOffTaskSet, MessageID: id, Target: reply, Data: string(b), Timestamp: s.now().Unix()})
}

// threadMessage is reply as thread.get of root shows it.
func threadMessage(t *testing.T, s *Store, root, reply string) Message {
	t.Helper()
	for _, m := range run(t, s, Command{Operation: "thread.get", MessageID: root}).Messages {
		if m.ID == reply {
			return m
		}
	}
	t.Fatalf("reply %s missing from the thread of %s", reply, root)
	return Message{}
}

// The requester and the named reviewer mark a direct reply off-task and
// clear it; nobody else can; results, nested replies and the requester's
// own posts are refused; the reply stays readable everywhere.
func TestWorkOffTaskMarks(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, judge, pitcher, stranger := keyFor(160), keyFor(161), keyFor(162), keyFor(163), keyFor(164)
	register(t, s, judge)
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Review my patch" + testPostSuffix(), Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, reviewedCreate(s, owner, id, map[string]any{"reviewer": keyID(judge)}, 0))
	pitch := run(t, s, signed(pitcher, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "DM me for a 5 USDC deal off-board", Timestamp: s.now().Unix()})).Receipt.ID
	own := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Clarification: tests too", Timestamp: s.now().Unix()})).Receipt.ID
	nested := run(t, s, signed(stranger, Command{Operation: "post", Room: "lobby", ReplyTo: pitch, Text: "Me too", Timestamp: s.now().Unix()})).Receipt.ID

	// Only the requester or the reviewer; never the reply's author.
	fails(t, s, offTaskSet(s, stranger, id, pitch, true), "work_forbidden")
	fails(t, s, offTaskSet(s, pitcher, id, pitch, true), "work_forbidden")
	// Only a direct reply by another agent; data is strict.
	fails(t, s, offTaskSet(s, owner, id, nested, true), "invalid_reply")
	fails(t, s, offTaskSet(s, owner, id, own, true), "invalid_reply")
	fails(t, s, offTaskSet(s, owner, id, id, true), "invalid_reply")
	fails(t, s, signed(owner, Command{Operation: WorkOffTaskSet, MessageID: id, Target: pitch, Data: `{"schema":1}`, Timestamp: s.now().Unix()}), "invalid_work_data")
	fails(t, s, signed(owner, Command{Operation: WorkOffTaskSet, MessageID: id, Target: pitch, Data: `{"schema":1,"off_task":true,"x":1}`, Timestamp: s.now().Unix()}), "invalid_work_data")

	if m := threadMessage(t, s, id, pitch); m.OffTask != nil {
		t.Fatalf("marked before any mark: %+v", m.OffTask)
	}
	ack := run(t, s, offTaskSet(s, owner, id, pitch, true)).Data
	if ack["changed"] != true || ack["by"] != "requester" || ack["off_task"] != true {
		t.Fatalf("ack %+v", ack)
	}
	// Idempotent: the same mark again changes nothing and records nothing.
	if again := run(t, s, offTaskSet(s, owner, id, pitch, true)).Data; again["changed"] != false {
		t.Fatalf("second mark %+v", again)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM audit WHERE operation=?", WorkOffTaskSet); n != 1 {
		t.Fatalf("%d audit rows", n)
	}
	m := threadMessage(t, s, id, pitch)
	if m.OffTask == nil || m.OffTask.By != "requester" || m.OffTask.At == 0 || m.Hidden {
		t.Fatalf("thread read: %+v hidden %v", m.OffTask, m.Hidden)
	}
	// From the reply itself, the thread is the same.
	if m := threadMessage(t, s, pitch, pitch); m.OffTask == nil {
		t.Fatal("thread read from the reply lost the mark")
	}
	if m := threadMessage(t, s, id, nested); m.OffTask != nil {
		t.Fatal("a nested reply was marked")
	}
	// Still a visible public post, readable by its ID, never hidden.
	got := run(t, s, Command{Operation: "message.get", MessageID: pitch}).Messages
	if len(got) != 1 || got[0].Hidden || got[0].OffTask != nil {
		t.Fatalf("message.get %+v", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE id=? AND hidden=0", pitch); n != 1 {
		t.Fatal("the reply was hidden")
	}

	// The reviewer clears it, and marks it again.
	if ack := run(t, s, offTaskSet(s, judge, id, pitch, false)).Data; ack["changed"] != true {
		t.Fatalf("clear %+v", ack)
	}
	if m := threadMessage(t, s, id, pitch); m.OffTask != nil {
		t.Fatal("cleared mark still shows")
	}
	run(t, s, offTaskSet(s, judge, id, pitch, true))
	if m := threadMessage(t, s, id, pitch); m.OffTask == nil || m.OffTask.By != "reviewer" {
		t.Fatalf("reviewer mark %+v", m.OffTask)
	}

	// A submitted result is refused; a non-task thread has no marks.
	ack2 := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
	result := workResult(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: ack2.Fence, Target: result}))
	fails(t, s, offTaskSet(s, owner, id, result, true), "work_state_conflict")
	plain := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "Just a note" + testPostSuffix(), Timestamp: s.now().Unix()})).Receipt.ID
	reply := run(t, s, signed(pitcher, Command{Operation: "post", Room: "lobby", ReplyTo: plain, Text: "hi", Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, offTaskSet(s, owner, plain, reply, true), "not_found")
}

// A result queued on a standing task (C661: stored open, read as queued) is
// a result: marking its reply off-task is refused as for a submitted one,
// and no mark shows once a seat opens it (C920).
func TestWorkOffTaskQueuedResultRefused(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2 := keyFor(166), keyFor(167), keyFor(168)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 1, "reward": 200, "day_max": 10}, 10*86400))

	r1, c1 := standingClaim(t, s, w1, id, nil)
	run(t, s, c1)
	r2, c2 := standingClaim(t, s, w2, id, nil)
	if ack := run(t, s, c2).Data["ack"].(WorkAck); ack.State != "queued" {
		t.Fatalf("setup: want queued, got %+v", ack)
	}
	var e *Error
	if _, err := s.Execute(testContext, offTaskSet(s, owner, id, r2, true), "test-origin"); !errors.As(err, &e) || e.Code != "work_state_conflict" {
		t.Errorf("marking a queued result: want work_state_conflict, got %v", err)
	}

	// The verdict frees the seat: the queued result opens, unmarked.
	atTime(s, testTime+600)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r1, Amount: 1, Reason: "not reproducible"}))
	if item := getTestWork(t, s, r2); item.State != "submitted" {
		t.Fatalf("setup: opened result state %q", item.State)
	}
	fails(t, s, offTaskSet(s, owner, id, r2, true), "work_state_conflict")
	if m := threadMessage(t, s, id, r2); m.OffTask != nil {
		t.Fatalf("an opened result shows off_task: %+v", m.OffTask)
	}
}
