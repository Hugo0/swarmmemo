package board

import (
	"strings"
	"testing"
	"time"
)

// A verdict on an agent's result reaches it: its home lists a work_verdict
// item and its own updates.get carries data.verdicts, with the reward paid
// and the rating, for WorkVerdictDays (cold-start evaluation 2026-10-21:
// the worker saw neither). A reject says so too; the requester's own home
// and anyone else's read list nothing.
func TestWorkVerdictReachesTheWorker(t *testing.T) {
	s := reputationStore(t)
	owner, worker := keyFor(150), keyFor(151)
	for i, k := range [][]byte{owner, worker} {
		run(t, s, signed(k, Command{Operation: "agent.register", Handle: "verdictagent" + string(rune('a'+i))}))
	}
	requester, payee := keyID(owner), keyID(worker)
	mintCredit(t, s, requester, "paid", 1000)

	paid := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, paid, 300, 0))
	fence := claimAndSubmit(t, s, worker, paid, "lobby")
	run(t, s, ratedAccept(t, s, owner, paid, fence, payee, 80, ""))

	home := run(t, s, signed(worker, Command{Operation: "home"})).Data
	var item *HomeItem
	for _, it := range home["items"].([]HomeItem) {
		if it.Kind == "work_verdict" {
			item = &it
		}
	}
	want := "Your result on “Rewarded review” was accepted: +300 credits paid, rated 80/100."
	if item == nil || item.Text != want || item.Call["operation"] != "work.get" || item.Call["message_id"] != paid || item.Tool.Name != "read_work" {
		t.Fatalf("worker home: %+v", home)
	}
	updates := run(t, s, signed(worker, Command{Operation: "updates.get", Target: payee})).Data
	verdicts, _ := updates["verdicts"].([]WorkVerdict)
	if len(verdicts) != 1 || verdicts[0].WorkID != paid || verdicts[0].Verdict != "accepted" || verdicts[0].Reward != 300 || verdicts[0].RewardState != "paid" ||
		verdicts[0].Rating == nil || *verdicts[0].Rating != 80 || verdicts[0].ResultID == "" || verdicts[0].Text != want {
		t.Fatalf("worker updates verdicts: %+v", updates["verdicts"])
	}
	// The requester's home and updates carry no verdict of its own.
	for _, it := range run(t, s, signed(owner, Command{Operation: "home"})).Data["items"].([]HomeItem) {
		if it.Kind == "work_verdict" {
			t.Fatalf("requester home: %+v", it)
		}
	}
	if v := run(t, s, signed(owner, Command{Operation: "updates.get", Target: requester})).Data["verdicts"]; v != nil {
		t.Fatalf("requester verdicts: %+v", v)
	}
	// A public read of the worker's home lists no verdict.
	for _, it := range run(t, s, Command{Operation: "home", Target: payee}).Data["items"].([]HomeItem) {
		if it.Kind == "work_verdict" {
			t.Fatalf("public home: %+v", it)
		}
	}

	// A reject, newer, comes first.
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	rejected := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, rejected, 100, 0))
	fence = claimAndSubmit(t, s, worker, rejected, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: rejected, Amount: fence, Reason: "not what was asked"}))
	home = run(t, s, signed(worker, Command{Operation: "home"})).Data
	item = nil
	for _, it := range home["items"].([]HomeItem) {
		if it.Kind == "work_verdict" {
			item = &it
		}
	}
	if item == nil || item.Count != 2 || !strings.HasPrefix(item.Text, "Your result on “Rewarded review” was rejected.") || item.Call["message_id"] != rejected {
		t.Fatalf("worker home after a reject: %+v", home)
	}
	verdicts, _ = run(t, s, signed(worker, Command{Operation: "updates.get", Target: payee})).Data["verdicts"].([]WorkVerdict)
	if len(verdicts) != 2 || verdicts[0].Verdict != "rejected" || verdicts[0].Reward != 0 || verdicts[1].WorkID != paid ||
		verdicts[0].Reason != "not what was asked" || verdicts[1].Reason != "" {
		t.Fatalf("verdicts after a reject: %+v", verdicts)
	}
	// C1029: the reject's signed reason reaches the worker, on home and on
	// work.get last_verdict, which any reader sees (it is public in history).
	if !strings.Contains(item.Text, "was rejected. Reason: “not what was asked”.") {
		t.Fatalf("home reject text: %q", item.Text)
	}
	got := run(t, s, Command{Operation: "work.get", MessageID: rejected}).Data["work"].(Work).LastVerdict
	if got == nil || got.Operation != "work.reject" || got.Reason != "not what was asked" || got.By.ID != requester || got.By.Handle != "verdictagenta" || got.At != testTime+60 || got.Sequence == 0 {
		t.Fatalf("work.get last_verdict after a reject: %+v", got)
	}
	got = run(t, s, Command{Operation: "work.get", MessageID: paid}).Data["work"].(Work).LastVerdict
	if got == nil || got.Operation != "work.accept" || got.Reason != "" || got.By.ID != requester {
		t.Fatalf("work.get last_verdict after an accept: %+v", got)
	}
	if long := verdictReasonLine(strings.Repeat("why\n", 100)); len(long) > VerdictReasonLineMax+len("…") || strings.Contains(long, "\n") {
		t.Fatalf("reason line: %q", long)
	}

	// After WorkVerdictDays they are no longer listed.
	s.now = func() time.Time { return time.Unix(testTime+WorkVerdictDays*86400+61, 0) }
	for _, it := range run(t, s, signed(worker, Command{Operation: "home", Timestamp: s.now().Unix()})).Data["items"].([]HomeItem) {
		if it.Kind == "work_verdict" {
			t.Fatalf("an old verdict: %+v", it)
		}
	}
}
