package board

import (
	"crypto/ed25519"
	"maps"
	"slices"
	"testing"

	"swarmmemo/internal/allowance"
)

// C606: a discovery list without kind shows the work a worker could act on:
// closed work and a standing task's results stay out (each result is its own
// work item, and finished results crowded out the tasks); kind=closed and
// kind=all list the history, and a list by standing task, worker or target
// still lists the results. Cursors page every filter exactly once.
func TestWorksListDefaultListsWorkNotClosed(t *testing.T) {
	s := rewardStore(t)
	owner, w1, w2 := keyFor(230), keyFor(231), keyFor(232)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)

	// A standing task with two seats, both taken by results waiting for a
	// verdict: it takes no claim now, but it is real work and stays listed.
	standing := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, standing, map[string]any{"seats": 2, "results_max": WorkStandingResultsMax, "reward": 100}, 10*86400))
	for _, worker := range []ed25519.PrivateKey{w1, w2} {
		_, c := standingClaim(t, s, worker, standing, nil)
		run(t, s, c)
	}
	open := map[string]bool{standing: true}
	for i := range 4 {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, earnCreate(s, owner, id, "open task", []string{"review"}, int64(100+i)))
		open[id] = true
	}
	closed := map[string]bool{}
	for i := range 2 {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, earnCreate(s, owner, id, "cancelled task", []string{"review"}, int64(200+i)))
		run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "no longer needed"}))
		closed[id] = true
	}

	// walk pages c by limit, failing on a repeated row, and returns every ID.
	walk := func(c Command, limit int) []string {
		t.Helper()
		c.Limit = limit
		ids := []string{}
		for range 50 {
			r := run(t, s, c)
			for _, w := range r.Data["works"].([]Work) {
				if slices.Contains(ids, w.ID) {
					t.Fatalf("%+v listed %s twice", c, w.ID)
				}
				ids = append(ids, w.ID)
			}
			if more, _ := r.Data["has_more"].(bool); more != (r.NextCursor != "") {
				t.Fatalf("has_more %v with cursor %q", more, r.NextCursor)
			}
			if r.NextCursor == "" {
				slices.Sort(ids)
				return ids
			}
			c.Cursor = r.NextCursor
		}
		t.Fatalf("%+v never ran out of pages", c)
		return nil
	}
	sorted := func(sets ...map[string]bool) []string {
		all := map[string]bool{}
		for _, set := range sets {
			maps.Copy(all, set)
		}
		return slices.Sorted(maps.Keys(all))
	}

	list := Command{Operation: "works.list"}
	for _, limit := range []int{50, 2, 1} {
		if got, want := walk(list, limit), sorted(open); !slices.Equal(got, want) {
			t.Fatalf("default list, limit %d: %v, want %v", limit, got, want)
		}
	}
	room := Command{Operation: "works.list", Room: "lobby"}
	if got, want := walk(room, 3), sorted(open); !slices.Equal(got, want) {
		t.Fatalf("lobby list: %v, want %v", got, want)
	}
	closedList := Command{Operation: "works.list", Kind: WorkKindClosed}
	for _, limit := range []int{50, 1} {
		if got, want := walk(closedList, limit), sorted(closed); !slices.Equal(got, want) {
			t.Fatalf("kind=closed, limit %d: %v, want %v", limit, got, want)
		}
	}
	all := Command{Operation: "works.list", Kind: WorkKindAll}
	if got, want := walk(all, 3), sorted(open, closed); !slices.Equal(got, want) {
		t.Fatalf("kind=all: %v, want %v", got, want)
	}
	if got := walk(Command{Operation: "works.list", Kind: "cancelled"}, 50); !slices.Equal(got, sorted(closed)) {
		t.Fatalf("kind=cancelled: %v", got)
	}

	// The full standing task keeps its seats and why it takes no claim.
	for _, w := range run(t, s, list).Data["works"].([]Work) {
		if w.ID != standing {
			continue
		}
		if w.Standing == nil || w.Standing.Open != 0 || w.Standing.BlockedBy != StandingBlockedSeats || w.Claimable {
			t.Fatalf("full standing task row: %+v standing %+v", w, w.Standing)
		}
	}

	// The standing task's history lists its results, paged like any other.
	byStanding := Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + standing + `"}`}
	results := walk(byStanding, 1)
	if len(results) != 2 {
		t.Fatalf("standing=ID results: %v", results)
	}
	for _, id := range results {
		if open[id] || closed[id] {
			t.Fatalf("standing=ID listed %s, not a result", id)
		}
		if w := getTestWork(t, s, id); w.StandingID != standing {
			t.Fatalf("result %s: standing_id %q", id, w.StandingID)
		}
	}
	if r := run(t, s, byStanding); r.Data["standing"] == nil {
		t.Fatalf("standing=ID carries no data.standing: %v", r.Data)
	}
	// A worker's history and an agent's page list its result too.
	byWorker := walk(Command{Operation: "works.list", Data: `{"schema":1,"worker":"` + keyID(w1) + `"}`}, 50)
	if len(byWorker) != 1 || !slices.Contains(results, byWorker[0]) {
		t.Fatalf("worker=w1: %v, results %v", byWorker, results)
	}
	byTarget := walk(Command{Operation: "works.list", Target: keyID(owner)}, 4)
	if want := sorted(open, closed, map[string]bool{results[0]: true, results[1]: true}); !slices.Equal(byTarget, want) {
		t.Fatalf("target=owner: %v, want %v", byTarget, want)
	}
	if got := walk(Command{Operation: "works.list", Target: keyID(owner), Kind: WorkKindClosed}, 50); !slices.Equal(got, sorted(closed)) {
		t.Fatalf("target=owner kind=closed: %v", got)
	}

	// A refusal names the kinds, closed and all among them.
	fails(t, s, Command{Operation: "works.list", Kind: "finished"}, "invalid_work_state")
}
