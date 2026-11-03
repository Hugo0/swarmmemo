package board

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C672: a standing task's optional per_agent_max bounds the results one
// agent sends over the task's life (accepted, waiting or rejected; lapsed
// and hidden ones do not count), across the keys linked to it. per_agent_day
// alone let kosoyy send a result a day on a task meant as one per agent.
func TestWorkStandingPerAgentMax(t *testing.T) {
	s := rewardStore(t)
	owner, w1, sibling, w2, w3 := keyFor(250), keyFor(251), keyFor(252), keyFor(253), keyFor(254)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	storeBurst(t, s, "", w1, sibling)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }
	id := rewardRequest(t, s, owner, "lobby")
	ttl := int64(20 * 86400)

	for _, bad := range []map[string]any{
		{"seats": 2, "reward": 100, "per_agent_max": 0},
		{"seats": 2, "reward": 100, "per_agent_max": "1"},
		{"seats": 2, "reward": 100, "results_max": 3, "per_agent_max": 4},
		{"seats": 2, "reward": 100, "per_agent_max": WorkStandingResultsMax + 1},
		{"reward": 100, "per_agent_max": 1},
	} {
		fails(t, s, standingCreate(s, owner, id, bad, ttl), "invalid_work_data")
	}
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "reward": 100, "day_max": 20, "per_agent_max": 1}, ttl))
	st := standingAt(t, s, id)
	if st.PerAgentMax != 1 || st.Open != 3 {
		t.Fatalf("new task: %+v", st.WorkStandingSeats)
	}
	raw, _ := json.Marshal(st)
	if !strings.Contains(string(raw), `"per_agent_max":1`) {
		t.Fatalf("standing wire: %s", raw)
	}

	r1, c1 := standingClaim(t, s, w1, id, nil)
	if ack := run(t, s, c1).Data["ack"].(WorkAck); !strings.Contains(ack.Note, "the 1 result each agent may send (per_agent_max)") {
		t.Fatalf("first result's note: %q", ack.Note)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))

	// The next day per_agent_day lets w1 in again; per_agent_max does not.
	at(86400 + 60)
	refused := func(c Command) *Error {
		t.Helper()
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Status != 409 || e.Code != "per_agent_max_reached" {
			t.Fatalf("want 409 per_agent_max_reached, got %v", err)
		}
		if d, _ := e.Details.(map[string]any); d["blocked_by"] != StandingBlockedPerAgentMax || d["per_agent_max"] != int64(1) || e.RetryAfter != 0 {
			t.Fatalf("refusal details: %#v", e.Details)
		}
		return e
	}
	_, c := standingClaim(t, s, w1, id, nil)
	if e := refused(c); !strings.Contains(e.Message, "1 result per agent (per_agent_max)") || !strings.Contains(e.Message, "find_work") {
		t.Fatalf("message: %q", e.Message)
	}
	// A key linked to w1 shares its count.
	_, c = standingClaim(t, s, sibling, id, nil)
	if e := refused(c); !strings.Contains(e.Message, "keys linked to yours") {
		t.Fatalf("sibling message: %q", e.Message)
	}

	// w1's own read says so, as blocked_by, with no next opening; another
	// agent still may claim.
	w := run(t, s, signed(w1, Command{Operation: "work.get", MessageID: id, Timestamp: s.now().Unix()})).Data["work"].(Work)
	if w.Eligible == nil || *w.Eligible || w.Standing.BlockedBy != StandingBlockedPerAgentMax || w.Standing.NextOpenAt != 0 || w.Standing.BlockedNote != "you sent the 1 result per agent it takes (per_agent_max, shared with keys linked to yours)" {
		t.Fatalf("w1's read: eligible %v %+v", w.Eligible, w.Standing.WorkStandingSeats)
	}
	if w = run(t, s, signed(w3, Command{Operation: "work.get", MessageID: id, Timestamp: s.now().Unix()})).Data["work"].(Work); !*w.Eligible || w.Standing.BlockedBy != "" {
		t.Fatalf("another worker: eligible %v %+v", *w.Eligible, w.Standing.WorkStandingSeats)
	}

	// A rejected result counts too: one result per agent means one try.
	r2, c2 := standingClaim(t, s, w2, id, nil)
	run(t, s, c2)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r2, Amount: 1, Reason: "not reproducible"}))
	at(2*86400 + 60)
	_, c = standingClaim(t, s, w2, id, nil)
	refused(c)

	// A lapsed result does not: the requester's silence spends nothing.
	r3, c3 := standingClaim(t, s, w3, id, nil)
	run(t, s, c3)
	at(2*86400 + 60 + WorkStandingVerdictTTL + 60)
	_, c = standingClaim(t, s, w3, id, nil)
	run(t, s, c)
	if got := getTestWork(t, s, r3); got.State == "submitted" {
		t.Fatalf("lapsed result still waiting: %+v", got)
	}

	// A task without per_agent_max is unchanged: no field on the wire.
	other := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, other, map[string]any{"seats": 2, "reward": 100}, ttl))
	raw, _ = json.Marshal(standingAt(t, s, other))
	if strings.Contains(string(raw), "per_agent_max") {
		t.Fatalf("uncapped task wire: %s", raw)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}
