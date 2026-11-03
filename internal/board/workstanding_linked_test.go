package board

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C707 (bounty 759b36fc): a standing task's per-agent limits count the
// linked-key group in the preview and in the claim alike, from one count
// (standingAgentUsed). The preview counted per_agent_max for the key alone
// and said claimable where the claim refused; per_agent_day let each
// linked key take a seat of its own.
func TestWorkStandingLinkedKeysShareLimits(t *testing.T) {
	s := rewardStore(t)
	owner, w1, sibling, other := keyFor(160), keyFor(161), keyFor(162), keyFor(163)
	mintCredit(t, s, keyID(owner), allowance.Paid, 10000)
	storeBurst(t, s, "", w1, sibling)
	ttl := int64(20 * 86400)

	// The preview, three ways: work.get's eligible, the signed claimable
	// list (works.list data claimable) and home's open task; and the claim.
	type view struct {
		eligible, listed bool
		blockedBy        string
	}
	preview := func(k ed25519.PrivateKey, id string) view {
		t.Helper()
		w := run(t, s, signed(k, Command{Operation: "work.get", MessageID: id, Timestamp: s.now().Unix()})).Data["work"].(Work)
		v := view{eligible: w.Eligible != nil && *w.Eligible, blockedBy: w.Standing.BlockedBy}
		r := run(t, s, signed(k, Command{Operation: "works.list", Limit: DirectoryPageMax, Data: `{"schema":1,"claimable":true}`, Timestamp: s.now().Unix()}))
		for _, row := range r.Data["works"].([]Work) {
			if row.ID == id && (row.Eligible == nil || *row.Eligible) {
				v.listed = true
			}
		}
		return v
	}
	claim := func(k ed25519.PrivateKey, id string) error {
		t.Helper()
		_, c := standingClaim(t, s, k, id, nil)
		_, err := s.Execute(testContext, c, "test-origin")
		return err
	}
	refusal := func(err error, code string) *Error {
		t.Helper()
		var e *Error
		if !errors.As(err, &e) || e.Status != 409 || e.Code != code {
			t.Fatalf("want 409 %s, got %v", code, err)
		}
		return e
	}

	// per_agent_max: w1's result spends the group's one result; the
	// sibling's preview says so, as its claim does.
	capped := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, capped, map[string]any{"seats": 3, "reward": 100, "per_agent_day": 3, "day_max": 20, "per_agent_max": 1}, ttl))
	if v := preview(sibling, capped); !v.eligible || !v.listed {
		t.Fatalf("before any result: sibling preview %+v", v)
	}
	r1, c1 := standingClaim(t, s, w1, capped, nil)
	run(t, s, c1)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r1, Amount: 1}))
	if v := preview(sibling, capped); v.eligible || v.listed || v.blockedBy != StandingBlockedPerAgentMax {
		t.Fatalf("sibling preview after w1's result: %+v", v)
	}
	if e := refusal(claim(sibling, capped), "per_agent_max_reached"); !strings.Contains(e.Message, "(per_agent_max), and keys linked to yours share that limit: together you have sent 1") {
		t.Fatalf("sibling per_agent_max message: %q", e.Message)
	}
	if v := preview(other, capped); !v.eligible || !v.listed {
		t.Fatalf("unlinked preview: %+v", v)
	}

	// per_agent_day: w1's result today spends the group's day; a second
	// linked key is refused, its preview agreeing; an unlinked key is not.
	daily := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, daily, map[string]any{"seats": 3, "reward": 100, "day_max": 20}, ttl))
	r2, c2 := standingClaim(t, s, w1, daily, nil)
	run(t, s, c2)
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: r2, Amount: 1, Reason: "not reproducible"}))
	if v := preview(sibling, daily); v.eligible || v.listed || v.blockedBy != StandingBlockedPerAgentDay {
		t.Fatalf("sibling preview after w1's result today: %+v", v)
	}
	e := refusal(claim(sibling, daily), "seat_limit")
	if d, _ := e.Details.(map[string]any); d["blocked_by"] != StandingBlockedPerAgentDay {
		t.Fatalf("sibling per_agent_day details: %#v", e.Details)
	}
	if !strings.Contains(e.Message, "it takes 1 result per agent a day (per_agent_day), and keys linked to yours share that limit: together you have sent 1 today") {
		t.Fatalf("sibling per_agent_day message: %q", e.Message)
	}
	if v := preview(other, daily); !v.eligible || !v.listed {
		t.Fatalf("unlinked preview on the daily task: %+v", v)
	}
	if err := claim(other, daily); err != nil {
		t.Fatalf("unlinked claim: %v", err)
	}
	// home's open task for the sibling is neither of them.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var generation string
	if err = tx.QueryRowContext(testContext, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	task, err := s.claimableTaskTx(testContext, tx, keyID(sibling), s.now().Unix(), generation)
	if err != nil {
		t.Fatal(err)
	}
	if task.id == capped || task.id == daily {
		t.Fatalf("home open task for the sibling: %s", task.id)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}
