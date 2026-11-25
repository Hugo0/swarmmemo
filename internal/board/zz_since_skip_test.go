package board

import (
	"testing"

	"swarmmemo/internal/allowance"
)

// works.list --since hashes only the FIRST page. Rows are ordered by work id
// and a work id is random, so work created after the poll sorts after page one
// whenever its id is larger than the last id on it. When page one already has
// has_more true, its rows, has_more and its requester records are all
// unchanged after that work appears, so the poll answered unchanged with no
// rows and the note "Nothing new since this cursor" -- forever, while the work
// existed and a paged walk reached it. An agent looping --since never learned
// of that work.
//
// The poll may say nothing is new only when the page holds the whole list. A
// page with has_more leaves rows unread, so it answers the full first page; its
// has_more already tells the reader to page on.
func TestSinceCursorReportsWorkPastPageOne(t *testing.T) {
	list := Command{Operation: "works.list", Kind: WorkKindRewarded, Limit: 3}
	for attempt := 0; attempt < 25; attempt++ {
		s := rewardStore(t)
		poster := keyFor(byte(200 + attempt))
		mintCredit(t, s, keyID(poster), allowance.Paid, 500000)
		post := func(title string) string {
			id := rewardRequest(t, s, poster, "lobby")
			run(t, s, earnCreate(s, poster, id, title, []string{"review"}, 20))
			return id
		}
		for _, title := range []string{"a", "b", "c", "d"} {
			post(title)
		}
		first := run(t, s, list)
		since, _ := first.Data["since"].(string)
		page := first.Data["works"].([]Work)
		if since == "" || len(page) != 3 || first.Data["has_more"] != true {
			t.Fatalf("need a full first page with has_more: since=%q rows=%d has_more=%v", since, len(page), first.Data["has_more"])
		}
		maxID := page[len(page)-1].ID

		late := post("late arrival")
		if late <= maxID {
			continue // this id landed inside page one; try a fresh store
		}

		poll := list
		poll.Cursor = since
		got := run(t, s, poll)
		// The row set of page one is identical, so a token-only hash cannot
		// tell. The answer must still not claim there is nothing new.
		if got.Data["unchanged"] == true {
			t.Fatalf("the poll answered unchanged with 0 rows after work %s was created past page one: --since never announces it", late[:8])
		}
		if rowCount(got.Data) == 0 {
			t.Fatalf("the poll answered no rows and no unchanged flag")
		}
		// And the work is really there, on a later page.
		seen, cursor := false, ""
		for i := 0; i < 40; i++ {
			c := list
			c.Cursor = cursor
			r := run(t, s, c)
			for _, w := range r.Data["works"].([]Work) {
				if w.ID == late {
					seen = true
				}
			}
			if cursor = r.NextCursor; cursor == "" {
				break
			}
		}
		if !seen {
			t.Fatalf("work %s is on no page; the premise is wrong", late[:8])
		}
		return
	}
	t.Fatal("no new work id landed past page one in 25 attempts")
}

// A page that holds the whole list still answers unchanged: the fix must not
// cost the poll its point.
func TestSinceCursorStillAnswersUnchangedOnAWholePage(t *testing.T) {
	s := rewardStore(t)
	poster := keyFor(231)
	mintCredit(t, s, keyID(poster), allowance.Paid, 50000)
	for _, title := range []string{"a", "b"} {
		id := rewardRequest(t, s, poster, "lobby")
		run(t, s, earnCreate(s, poster, id, title, []string{"review"}, 20))
	}
	list := Command{Operation: "works.list", Kind: WorkKindRewarded, Limit: 10}
	first := run(t, s, list)
	if first.Data["has_more"] == true {
		t.Fatal("two works in a page of ten must not have has_more")
	}
	since, _ := first.Data["since"].(string)
	poll := list
	poll.Cursor = since
	again := run(t, s, poll)
	if again.Data["unchanged"] != true {
		t.Fatalf("a page holding the whole list must still answer unchanged, got %v", again.Data["unchanged"])
	}
}

// rowCount reads a list answer's rows whatever slice type the branch built.
func rowCount(data map[string]any) int {
	switch v := data["works"].(type) {
	case []Work:
		return len(v)
	case []any:
		return len(v)
	}
	return 0
}
