package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C830: a standing task's optional per_agent_cooldown_days is one result per
// agent per N days across rounds. A requester running weekly rounds (each
// next one named with work.successor.set) wrote "one accepted result per
// account per 7 days" in the task text, and nothing enforced it. Accepted,
// waiting and queued results count, on the task and on the rounds before it,
// across linked keys; rejected and lapsed ones do not.
func TestWorkStandingPerAgentCooldown(t *testing.T) {
	s := rewardStore(t)
	owner, w1, sibling, w2, w3, w4 := keyFor(250), keyFor(251), keyFor(252), keyFor(253), keyFor(254), keyFor(255)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	storeBurst(t, s, "", w1, sibling)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }
	ttl := int64(20 * 86400)
	round := func(extra map[string]any) string {
		t.Helper()
		id := rewardRequest(t, s, owner, "lobby")
		data := map[string]any{"seats": 3, "reward": 100, "day_max": 20}
		for k, v := range extra {
			data[k] = v
		}
		run(t, s, standingCreate(s, owner, id, data, ttl))
		return id
	}

	bad := rewardRequest(t, s, owner, "lobby")
	for _, d := range []map[string]any{
		{"seats": 2, "reward": 100, "per_agent_cooldown_days": 0},
		{"seats": 2, "reward": 100, "per_agent_cooldown_days": WorkStandingCooldownDaysMax + 1},
		{"seats": 2, "reward": 100, "per_agent_cooldown_days": -1},
		{"seats": 2, "reward": 100, "per_agent_cooldown_days": "7"},
		{"reward": 100, "per_agent_cooldown_days": 7},
	} {
		fails(t, s, standingCreate(s, owner, bad, d, ttl), "invalid_work_data")
	}

	// Round 1 sets no cool-down: its results still count for later rounds.
	round1 := round(nil)
	r1, c := standingClaim(t, s, w1, round1, nil)
	run(t, s, c)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	r2, c := standingClaim(t, s, w2, round1, nil)
	run(t, s, c)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r2, Amount: 1, Reason: "not reproducible"}))
	_, c = standingClaim(t, s, w3, round1, nil)
	run(t, s, c) // left waiting for a verdict

	at(60)
	round2 := round(map[string]any{"per_agent_cooldown_days": 7})
	run(t, s, successorSet(s, owner, round1, round2))
	round3 := round(map[string]any{"per_agent_cooldown_days": 7})
	run(t, s, successorSet(s, owner, round2, round3))
	st := standingAt(t, s, round2)
	raw, _ := json.Marshal(st)
	if st.PerAgentCooldownDays != 7 || !strings.Contains(string(raw), `"per_agent_cooldown_days":7`) {
		t.Fatalf("round 2 standing: %s", raw)
	}

	at(120)
	refused := func(key ed25519.PrivateKey, id string) *Error {
		t.Helper()
		_, c := standingClaim(t, s, key, id, nil)
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Status != 409 || e.Code != "per_agent_cooldown" {
			t.Fatalf("want 409 per_agent_cooldown, got %v", err)
		}
		d, _ := e.Details.(map[string]any)
		next, _ := d["next_open_at"].(int64)
		if d["blocked_by"] != StandingBlockedCooldown || d["per_agent_cooldown_days"] != int64(7) || next != e.NextOpenAt ||
			e.RetryAfter <= 0 || int64(e.RetryAfter) != next-s.now().Unix() || next < testTime+7*86400 || next > testTime+7*86400+60 {
			t.Fatalf("refusal: retry_after %d next_open_at %d details %#v", e.RetryAfter, e.NextOpenAt, e.Details)
		}
		if !strings.Contains(e.Message, "per 7 days (per_agent_cooldown_days)") || !strings.Contains(e.Message, time.Unix(next, 0).UTC().Format(time.RFC3339)) {
			t.Fatalf("message: %q", e.Message)
		}
		return e
	}
	// w1's accepted result on round 1 holds it off round 2, and off round 3
	// two links on; its linked sibling shares the limit.
	refused(w1, round2)
	refused(w1, round3)
	refused(sibling, round2)
	// w1's own read says so, with when it lifts; another agent may claim.
	w := run(t, s, signed(w1, Command{Operation: "work.get", MessageID: round2, Timestamp: s.now().Unix()})).Data["work"].(Work)
	if w.Eligible == nil || *w.Eligible || w.Standing.BlockedBy != StandingBlockedCooldown || w.Standing.NextOpenAt < testTime+7*86400 ||
		w.Standing.BlockedNote != "your last result here or in an earlier round is under 7 days old (per_agent_cooldown_days, shared with keys linked to yours)" {
		t.Fatalf("w1's read: eligible %v %+v", w.Eligible, w.Standing.WorkStandingSeats)
	}
	// C1065: and you names its own limit and next claim, the same time.
	if w.You == nil || w.You.BlockedBy != StandingBlockedCooldown || w.You.NextClaimAt != w.Standing.NextOpenAt {
		t.Fatalf("w1's you: %+v standing %+v", w.You, w.Standing.WorkStandingSeats)
	}
	if w = run(t, s, signed(w4, Command{Operation: "work.get", MessageID: round2, Timestamp: s.now().Unix()})).Data["work"].(Work); !*w.Eligible || w.Standing.BlockedBy != "" || w.You != nil {
		t.Fatalf("another worker: eligible %v %+v you %+v", *w.Eligible, w.Standing.WorkStandingSeats, w.You)
	}
	if w = run(t, s, Command{Operation: "work.get", MessageID: round2}).Data["work"].(Work); w.You != nil {
		t.Fatalf("anonymous read has you: %+v", w.You)
	}
	// C1065: a reply by w1 cannot be accepted while its cool-down holds; the
	// refusal is worded for the requester and names w1's next claim.
	reply := replyTo(t, s, w1, round2, "A full result inside the cool-down")
	_, err := s.Execute(testContext, acceptReplyCmd(s, owner, round2, reply, nil), "test-origin")
	var acc *Error
	if !errors.As(err, &acc) || acc.Code != "per_agent_cooldown" {
		t.Fatalf("accept of a cooled-down reply: %v", err)
	}
	if d, _ := acc.Details.(map[string]any); d["next_open_at"] == nil || !strings.Contains(acc.Message, "The reply's author is in this standing task's cool-down") ||
		!strings.Contains(acc.Message, time.Unix(d["next_open_at"].(int64), 0).UTC().Format(time.RFC3339)) || strings.Contains(acc.Message, "Your next claim") {
		t.Fatalf("requester refusal: %q %#v", acc.Message, acc.Details)
	}
	// A rejected result does not count; one waiting for a verdict does.
	_, c = standingClaim(t, s, w2, round2, nil)
	run(t, s, c)
	refused(w3, round2)

	// Once w3's round-1 result lapses undecided it no longer counts.
	at(WorkStandingVerdictTTL + 600)
	_, c = standingClaim(t, s, w3, round2, nil)
	run(t, s, c)

	// Seven days after w1's result, w1 may claim again.
	at(7*86400 + 600)
	_, c = standingClaim(t, s, w1, round2, nil)
	run(t, s, c)
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}
