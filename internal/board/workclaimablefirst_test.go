package board

import (
	"slices"
	"testing"

	"swarmmemo/internal/allowance"
)

// C625: the lists of open work (no kind, kind open) put claimable work
// first, then the rest, each group in ID order; a full round with an open
// next round is listed with the rest, full and pointing at it. Pages walk
// the two groups once each at any limit, also past the scan cap, and data
// claimable lists only the claimable group.
func TestWorksListClaimableFirst(t *testing.T) {
	s := rewardStore(t)
	owner := keyFor(200)
	mintCredit(t, s, keyID(owner), allowance.Paid, 30000)
	ttl := int64(10 * 86400)
	standing := func(extra map[string]any) string {
		id := rewardRequest(t, s, owner, "lobby")
		data := map[string]any{"seats": 1, "reward": 100}
		for k, v := range extra {
			data[k] = v
		}
		run(t, s, standingCreate(s, owner, id, data, ttl))
		return id
	}
	plain := func() string {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, rewardCreate(s, owner, id, 50, 86400))
		return id
	}
	claimable, rest := []string{}, []string{}
	// Three full rounds, the first with an open next round.
	full := []string{}
	for i := range 3 {
		id := standing(map[string]any{"results_max": 1})
		_, c := standingClaim(t, s, keyFor(byte(210+i)), id, nil)
		run(t, s, c)
		full = append(full, id)
	}
	next := standing(nil)
	run(t, s, successorSet(s, owner, full[0], next))
	claimable = append(claimable, next)
	rest = append(rest, full...)
	for range 3 {
		claimable = append(claimable, plain())
	}
	// Claimed plain work is listed without kind, not with kind open.
	taken := plain()
	run(t, s, claimCommand(s, keyFor(220), taken))
	slices.Sort(claimable)
	slices.Sort(rest)

	walk := func(kind, data string, limit int) []string {
		t.Helper()
		ids, cursor := []string{}, ""
		for range 50 {
			r := run(t, s, Command{Operation: "works.list", Kind: kind, Data: data, Limit: limit, Cursor: cursor})
			for _, w := range r.Data["works"].([]Work) {
				ids = append(ids, w.ID)
				if w.ID == full[0] && (w.Claimable || w.Standing == nil || !w.Standing.Full || w.Standing.NextRound != next) {
					t.Fatalf("full round row: claimable %v standing %+v", w.Claimable, w.Standing)
				}
			}
			if more, _ := r.Data["has_more"].(bool); more != (r.NextCursor != "") {
				t.Fatalf("has_more %v with cursor %q", more, r.NextCursor)
			}
			if r.NextCursor == "" {
				return ids
			}
			cursor = r.NextCursor
		}
		t.Fatalf("%s %s never ran out of pages", kind, data)
		return nil
	}
	restAll := append(slices.Clone(rest), taken)
	slices.Sort(restAll)
	wantOpen := append(slices.Clone(claimable), rest...)
	wantDefault := append(slices.Clone(claimable), restAll...)
	check := func(label string) {
		t.Helper()
		for _, limit := range []int{1, 2, 3, 100} {
			if got := walk("open", "", limit); !slices.Equal(got, wantOpen) {
				t.Fatalf("%s kind open limit %d:\n got %v\nwant %v", label, limit, got, wantOpen)
			}
			if got := walk("", "", limit); !slices.Equal(got, wantDefault) {
				t.Fatalf("%s default limit %d:\n got %v\nwant %v", label, limit, got, wantDefault)
			}
			for _, kind := range []string{"", "open", WorkKindRewarded} {
				if got := walk(kind, `{"schema":1,"claimable":true}`, limit); !slices.Equal(got, claimable) {
					t.Fatalf("%s kind %q claimable limit %d: %v", label, kind, limit, got)
				}
			}
		}
	}
	check("uncapped")
	// has_more says whether a row the list keeps follows a full page.
	n := len(claimable)
	if r := run(t, s, Command{Operation: "works.list", Kind: "open", Data: `{"schema":1,"claimable":true}`, Limit: n}); r.Data["has_more"] != false || r.NextCursor != "" {
		t.Fatalf("claimable page of %d: has_more %v", n, r.Data["has_more"])
	}
	if r := run(t, s, Command{Operation: "works.list", Kind: "open", Limit: n}); r.Data["has_more"] != true {
		t.Fatalf("open page of %d: has_more %v", n, r.Data["has_more"])
	}
	// Past the scan cap a page may come back short, even empty, with
	// has_more; the walk still lists every row once, in order.
	defer func(n int) { workClaimableScanMax = n }(workClaimableScanMax)
	workClaimableScanMax = 2
	check("capped")
	workClaimableScanMax = 500

	// A cursor from before C625 (a bare ID) resumes the claimable group.
	r := run(t, s, Command{Operation: "works.list", Kind: "open", Cursor: s.encodeConversationCursor(conversationCursor{Domain: "works.list", Scope: `["","open","",""]`, Page: claimable[0]})})
	got := []string{}
	for _, w := range r.Data["works"].([]Work) {
		got = append(got, w.ID)
	}
	if !slices.Equal(got, wantOpen[1:]) {
		t.Fatalf("bare-ID cursor: %v", got)
	}
	// claimable false is no filter; a cursor of another filter is refused.
	if got := walk("open", `{"schema":1,"claimable":false}`, 100); !slices.Equal(got, wantOpen) {
		t.Fatalf("claimable false: %v", got)
	}
	r = run(t, s, Command{Operation: "works.list", Kind: "open", Limit: 1})
	fails(t, s, Command{Operation: "works.list", Kind: "open", Limit: 1, Data: `{"schema":1,"claimable":true}`, Cursor: r.NextCursor}, "invalid_cursor")
	fails(t, s, Command{Operation: "works.list", Data: `{"schema":1,"claimable":"yes"}`}, "invalid_work_data")
}
