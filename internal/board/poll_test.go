package board

import (
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// A polled list (C540): the first page carries data.since; sent back
// unchanged it answers unchanged with no rows, a change answers the full
// list, and a since cursor that does not open is the full list with
// since_reset, never an error.
func TestPollListSinceCursor(t *testing.T) {
	s := rewardStore(t)
	requester := keyFor(240)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	create := func(title string) {
		id := rewardRequest(t, s, requester, "lobby")
		run(t, s, earnCreate(s, requester, id, title, []string{"review"}, 20))
	}
	create("first review")
	list := Command{Operation: "works.list", Kind: WorkKindRewarded}
	first := run(t, s, list)
	since, _ := first.Data["since"].(string)
	if !strings.HasPrefix(since, sincePrefix) || first.Data["next_poll_seconds"] != PollSeconds || first.Data["since_reset"] != nil {
		t.Fatalf("first page %v", first.Data)
	}
	poll := list
	poll.Cursor = since
	same := run(t, s, poll)
	if same.Data["unchanged"] != true || len(same.Data["works"].([]any)) != 0 || same.Data["has_more"] != false || same.Data["next_poll_seconds"] != PollSeconds || !strings.Contains(same.Data["next"].(string), PollEvery) {
		t.Fatalf("unchanged %v", same.Data)
	}
	// Its since works again, as the first one still does.
	again := poll
	again.Cursor = same.Data["since"].(string)
	if run(t, s, again).Data["unchanged"] != true {
		t.Fatal("the unchanged answer's since did not poll")
	}
	// Two equal reads answer alike: the cursor is a function of the page.
	if run(t, s, list).Data["since"] != since {
		t.Fatal("an equal read gave another since")
	}
	// Another list's cursor reads as changed: the full list (a list with
	// the same page would read as unchanged, which it is).
	other := run(t, s, Command{Operation: "works.list", Kind: "claimed", Cursor: since})
	if other.Data["unchanged"] != nil || other.Data["works"] == nil || other.Data["since_reset"] != nil || other.Data["since"] == since {
		t.Fatalf("other list %v", other.Data)
	}
	// A damaged cursor, or one from before a restore: the full list, reset.
	generation := since[len(sincePrefix):strings.LastIndex(since, ".")]
	for name, cursor := range map[string]string{
		"damaged":          since[:len(since)-4] + "!!!!",
		"garbage":          SinceCursor("nope"),
		"empty":            SinceCursor(""),
		"older generation": strings.Replace(since, generation, strings.Repeat("0", len(generation)), 1),
	} {
		r, err := s.Execute(testContext, Command{Operation: "works.list", Kind: WorkKindRewarded, Cursor: cursor}, "test-origin")
		if err != nil || r.Data["unchanged"] != nil || r.Data["works"] == nil || r.Data["since_reset"] != true || r.Data["since"] != since {
			t.Fatalf("%s: %v %v", name, r.Data, err)
		}
	}
	// A change answers the full list with a new since.
	create("second review")
	changed := run(t, s, poll)
	if changed.Data["unchanged"] != nil || len(changed.Data["works"].([]Work)) != 2 || changed.Data["since"] == since || changed.Data["since_reset"] != nil {
		t.Fatalf("changed %v", changed.Data)
	}
	// A later page carries no since; page cursors page as before.
	page := run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded, Limit: 1})
	next := run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded, Limit: 1, Cursor: page.NextCursor})
	if page.Data["since"] == nil || next.Data["since"] != nil || len(next.Data["works"].([]Work)) != 1 {
		t.Fatalf("paging %v then %v", page.Data, next.Data)
	}
	if SinceCursor(since) != since || SinceCursor("x") != sincePrefix+"x" {
		t.Fatal("SinceCursor")
	}
}

// C540 docs gap: PROTOCOL.md (Polling) and /capabilities polling.stale promise
// data.since_reset true for "a damaged" since cursor, but a damaged cursor
// whose damage keeps the MAC's shape (any single-character flip inside the
// base64url alphabet: the common corruption of a stored cursor) is of the
// right shape and generation, so sinceCurrent takes it for "changed" and the
// answer carries no since_reset. Documented here as the deliberate boundary:
// the server cannot tell a stale cursor from a corrupted one of the same
// shape, so the promise must not be made. The three damages the server does
// catch (non-alphabet character, wrong length, another generation) stay
// covered by the map above.
func TestPollListSinceCursorShapeValidDamage(t *testing.T) {
	s := rewardStore(t)
	requester := keyFor(241)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)
	id := rewardRequest(t, s, requester, "lobby")
	run(t, s, earnCreate(s, requester, id, "shape valid damage", []string{"review"}, 20))
	first := run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded})
	since, _ := first.Data["since"].(string)
	dot := strings.LastIndex(since, ".")
	mac := []byte(since[dot+1:])
	if mac[0] == 'A' {
		mac[0] = 'B'
	} else {
		mac[0] = 'A'
	}
	r, err := s.Execute(testContext, Command{Operation: "works.list", Kind: WorkKindRewarded, Cursor: sincePrefix + since[len(sincePrefix):dot] + "." + string(mac)}, "test-origin")
	if err != nil {
		t.Fatal(err)
	}
	// The full page, and never an error; but no since_reset, where the docs
	// promise one for "a damaged one". The fresh since is the unchanged
	// page's token again.
	if r.Data["unchanged"] != nil || r.Data["works"] == nil || r.Data["since_reset"] != nil {
		t.Fatalf("shape-valid damaged since cursor: %v", r.Data)
	}
}
