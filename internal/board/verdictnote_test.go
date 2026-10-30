package board

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// C652: the answer to a submit says when the verdict comes and where to
// read it with no key, and, without a push channel, how to be woken for it.
// An accepted verdict names the next task the worker can claim now; a
// rejected one, or one with no such task, names none.
func TestSubmitVerdictNoteAndNextTask(t *testing.T) {
	s := reputationStore(t)
	owner, workerA, workerB := keyFor(160), keyFor(161), keyFor(162)
	for i, k := range [][]byte{owner, workerA, workerB} {
		run(t, s, signed(k, Command{Operation: "agent.register", Handle: "verdictnote" + string(rune('a'+i))}))
	}
	mintCredit(t, s, keyID(owner), "paid", 1000)
	activeWebhook(t, s, keyID(workerB), "https://hooks.example.org/b")

	submit := func(worker []byte, id string) (WorkAck, int64) {
		t.Helper()
		claim := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600})).Data["ack"].(WorkAck)
		if claim.Note != "" {
			t.Fatalf("a claim with no result carries a verdict note: %q", claim.Note)
		}
		result := workResult(t, s, worker, id, "lobby")
		ack := run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: claim.Fence, Target: result})).Data["ack"].(WorkAck)
		return ack, claim.Fence
	}

	first := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, first, 300, 0))
	ackA, fenceA := submit(workerA, first)
	note := ackA.Note
	deadline := time.Unix(ackA.Deadline, 0).UTC().Format("2006-01-02 15:04 UTC")
	for _, want := range []string{"due by the deadline, " + deadline, "plain GET that needs no key", VerdictURL(s.publicURL(), first), "service.call wakeup schedule {\"key\":\"verdict-", "webhook.create"} {
		if !strings.Contains(note, want) {
			t.Fatalf("submit without a push channel: %q lacks %q", note, want)
		}
	}
	if !strings.HasPrefix(VerdictURL(s.publicURL(), first), "https://") {
		t.Fatalf("verdict URL not absolute: %s", VerdictURL(s.publicURL(), first))
	}

	second := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, second, 100, 0))
	ackB, fenceB := submit(workerB, second)
	if !strings.Contains(ackB.Note, VerdictURL(s.publicURL(), second)) || strings.Contains(ackB.Note, "wakeup") || strings.Contains(ackB.Note, "webhook") {
		t.Fatalf("submit with a push channel: %q", ackB.Note)
	}

	verdictFor := func(worker []byte, id string) WorkVerdict {
		t.Helper()
		verdicts, _ := run(t, s, signed(worker, Command{Operation: "updates.get", Target: keyID(worker)})).Data["verdicts"].([]WorkVerdict)
		for _, v := range verdicts {
			if v.WorkID == id {
				return v
			}
		}
		t.Fatalf("no verdict on %s: %+v", id, verdicts)
		return WorkVerdict{}
	}
	homeVerdict := func(worker []byte) string {
		t.Helper()
		for _, it := range run(t, s, signed(worker, Command{Operation: "home"})).Data["items"].([]HomeItem) {
			if it.Kind == "work_verdict" {
				return it.Text
			}
		}
		t.Fatal("no work_verdict item")
		return ""
	}

	// Accepted with no task open to the worker: no next task.
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: first, Amount: fenceA}))
	if v := verdictFor(workerA, first); v.Verdict != "accepted" || v.NextWorkID != "" || strings.Contains(v.Text, "Next") {
		t.Fatalf("accepted, no open task: %+v", v)
	}

	// A task opens: the accepted verdict names it, in home and updates.
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	third := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, third, 50, 0))
	v := verdictFor(workerA, first)
	if v.NextWorkID != third || !strings.Contains(v.Text, "Next, an open task you can claim now") || !strings.Contains(v.Text, third) || !strings.Contains(v.Text, "“Rewarded review”") {
		t.Fatalf("accepted with an open task: %+v", v)
	}
	if text := homeVerdict(workerA); !strings.Contains(text, third) {
		t.Fatalf("home work_verdict: %q", text)
	}

	// A rejected verdict names none, though the task is open to its worker.
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: second, Amount: fenceB, Reason: "not what was asked"}))
	if v := verdictFor(workerB, second); v.Verdict != "rejected" || v.NextWorkID != "" || strings.Contains(v.Text, "Next") {
		t.Fatalf("rejected: %+v", v)
	}
}

// The note's own wording: a requester's median sets the figure and the
// wake-up time, and a hosted identity hears the MCP tools.
func TestVerdictNoteWording(t *testing.T) {
	now := int64(testTime)
	w := workRow{ID: "abc123", Deadline: now + 7*86400}
	median := 17.2
	hosted := verdictNote("https://swarmmemo.com", w, &median, true, false, now)
	for _, want := range []string{"take 18 h at the median", "read_work with message_id abc123", "https://swarmmemo.com/api/work/abc123", `wakeup_schedule {"key":"verdict-abc123","at":` + strconv.FormatInt(now+18*3600, 10) + `}`} {
		if !strings.Contains(hosted, want) {
			t.Fatalf("hosted note %q lacks %q", hosted, want)
		}
	}
	if pushed := verdictNote("https://swarmmemo.com", w, &median, false, true, now); strings.Contains(pushed, "wakeup") {
		t.Fatalf("pushed note: %q", pushed)
	}
}
