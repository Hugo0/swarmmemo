package board

import (
	"crypto/ed25519"
	"testing"

	"swarmmemo/internal/allowance"
)

// listedWorkIDs pages works.list kind through with limit per page and
// returns every id listed, in order.
func listedWorkIDs(t *testing.T, s *Store, kind string, limit int) []string {
	t.Helper()
	ids, cursor := []string{}, ""
	for range 100 {
		res := run(t, s, Command{Operation: "works.list", Kind: kind, Limit: limit, Cursor: cursor})
		for _, w := range res.Data["works"].([]Work) {
			ids = append(ids, w.ID)
		}
		if res.NextCursor == "" {
			return ids
		}
		cursor = res.NextCursor
	}
	t.Fatal("works.list never ran out of pages")
	return nil
}

// C840: discovery (works.list without kind or kind=open, which MCP
// find_work and the web's /work read) leaves out a standing round that
// seats no one now nor by itself (a final short pool, or results_max
// reached) while the successor its requester named is open; work.get
// still shows it and names the successor. A spent round without a
// successor, a round with seats left, and one whose successor is closed
// stay listed; pages stay whole.
func TestStandingSupersededRoundLeftOutOfDiscovery(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(250)
	mintCredit(t, s, keyID(owner), allowance.Paid, 50000)
	standing := func(key ed25519.PrivateKey, extra map[string]any) string {
		id := rewardRequest(t, s, key, "lobby")
		data := map[string]any{"seats": 2, "reward": 100, "day_max": 10}
		for k, v := range extra {
			data[k] = v
		}
		run(t, s, standingCreate(s, key, id, data, 10*86400))
		return id
	}
	drain := func(id string) {
		t.Helper()
		if _, err := s.db.Exec("UPDATE work_rewards SET amount=0 WHERE work_id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	drained, drainedNext := standing(owner, nil), standing(owner, nil)
	run(t, s, successorSet(s, owner, drained, drainedNext))
	drain(drained)
	full, fullNext := standing(owner, map[string]any{"seats": 1, "results_max": 1}), standing(owner, nil)
	run(t, s, successorSet(s, owner, full, fullNext))
	r, c := standingClaim(t, s, keyFor(251), full, nil)
	run(t, s, c)
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))
	alone := standing(owner, nil)
	drain(alone)
	seated, seatedNext := standing(owner, nil), standing(owner, nil)
	run(t, s, successorSet(s, owner, seated, seatedNext))
	orphan, orphanNext := standing(owner, nil), standing(owner, nil)
	run(t, s, successorSet(s, owner, orphan, orphanNext))
	drain(orphan)
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: orphanNext, Reason: "done"}))

	// The claim path agrees: the drained round's refusal is final.
	_, c = standingClaim(t, s, keyFor(252), drained, nil)
	fails(t, s, c, "seats_full")

	want := []string{drainedNext, fullNext, alone, seated, seatedNext, orphan}
	for _, kind := range []string{"open", ""} {
		for _, limit := range []int{1, 2, 25} {
			ids := listedWorkIDs(t, s, kind, limit)
			got := map[string]int{}
			for _, id := range ids {
				got[id]++
			}
			for _, id := range []string{drained, full, orphanNext} {
				if got[id] != 0 {
					t.Fatalf("kind %q limit %d lists %s: %v", kind, limit, id, ids)
				}
			}
			for _, id := range want {
				if got[id] != 1 {
					t.Fatalf("kind %q limit %d lists %s %d times: %v", kind, limit, id, got[id], ids)
				}
			}
		}
	}
	// work.get is unchanged: the round shows, names its successor and,
	// not claimable, the next round.
	for id, next := range map[string]string{drained: drainedNext, full: fullNext} {
		w := getTestWork(t, s, id)
		if w.State != "open" || w.Claimable || w.Standing.Successor != next || w.Standing.NextRound != next {
			t.Fatalf("work.get %s: state %s claimable %v successor %q next %q", id, w.State, w.Claimable, w.Standing.Successor, w.Standing.NextRound)
		}
	}
	// The successor closes: the old round is listed again.
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: drainedNext, Reason: "done"}))
	found := false
	for _, id := range listedWorkIDs(t, s, "open", 25) {
		found = found || id == drained
	}
	if !found {
		t.Fatal("a spent round whose successor closed is not listed")
	}
}
