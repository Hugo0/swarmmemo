package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// C1170: windows splits the UTC day into equal refill windows. day_max 5
// over 4 windows of 6 hours is 2, 1, 1, 1; each window's share opens
// through it from its start (C1046's rule, per window), what a window
// leaves carries into the day's next ones and nothing into the next day.
func TestStandingWindowRelease(t *testing.T) {
	day := testTime - testTime%86400
	const h = int64(3600)
	for _, c := range []struct{ at, want int64 }{
		{0, 1}, {3 * h, 1}, {3*h + 1, 2}, {6*h - 1, 2}, // window 0: its 2
		{6 * h, 3}, {12*h - 1, 3}, // window 1: its 1, from its start
		{12 * h, 4}, {18 * h, 5}, {24*h - 1, 5}, // windows 2 and 3
	} {
		if got := windowReleased(5, 4, day+c.at); got != c.want {
			t.Fatalf("day_max 5 over 4 windows at +%ds: %d open, want %d", c.at, got, c.want)
		}
	}
	var shares []int64
	for k := int64(0); k < 4; k++ {
		shares = append(shares, windowShare(5, 4, k))
	}
	if got := shares; got[0] != 2 || got[1] != 1 || got[2] != 1 || got[3] != 1 || windowsShares(5, 4, 4) != 5 {
		t.Fatalf("shares %v", got)
	}
	// next_open_at: the window's next release, else the next window's start
	// exactly; all taken, the next 00:00; past the day (queued), later days.
	for _, c := range []struct{ used, now, want int64 }{
		{1, 0, 3*h + 1}, {2, 3*h + 5, 6 * h}, {3, 6 * h, 12 * h}, {4, 13 * h, 18 * h},
		{5, 19 * h, 24 * h}, {6, 23 * h, 24*h + 3*h + 1}, {10, 23 * h, 48 * h},
		{2, 7 * h, 7*h + 1}, // a window's carried seat is open already
	} {
		got := windowNextRelease(5, 4, c.used, day+c.now)
		if got != day+c.want {
			t.Fatalf("used %d at +%ds: next release +%d, want +%d", c.used, c.now, got-day, c.want)
		}
	}
	// Fewer results than windows: 3 over 4 is 1, 1, 1, 0.
	if windowReleased(3, 4, day+20*h) != 3 || windowNextRelease(3, 4, 3, day+20*h) != day+24*h {
		t.Fatal("3 over 4 windows")
	}
	// windows 1 is C1046's even release, value for value.
	for _, limit := range []int64{1, 3, 5, 6, 7, 1000} {
		for at := int64(0); at < 86400; at += 977 {
			if windowReleased(limit, 1, day+at) != dayReleased(limit, 1, day+at) {
				t.Fatalf("windows 1, day_max %d at +%d", limit, at)
			}
			for used := int64(0); used < 2*limit+2; used++ {
				if windowNextRelease(limit, 1, used, day+at) != dayNextRelease(limit, used, day+at) {
					t.Fatalf("windows 1 next release, day_max %d used %d at +%d", limit, used, at)
				}
			}
		}
	}
	// usdc_day_max 1.00 over 4 windows opens 0.25 a window, whole at each
	// start, at least the largest tier's from 00:00; windows 1 the whole cap.
	tier := func(usdc string) []WorkStandingTier { return []WorkStandingTier{{Name: "bug", Credits: 1, USDC: usdc}} }
	paced := &workStanding{USDCDayMax: 1_000_000, Windows: 4, Tiers: tier("0.10")}
	for _, c := range []struct{ at, want int64 }{{0, 250_000}, {6*h - 1, 250_000}, {6 * h, 500_000}, {12 * h, 750_000}, {18 * h, 1_000_000}} {
		if got := paced.usdcDayOpen(day + c.at); got != c.want {
			t.Fatalf("usdc over 4 windows at +%ds: %d, want %d", c.at, got, c.want)
		}
	}
	if got := paced.usdcNextRelease(250_000, day+h); got != day+6*h {
		t.Fatalf("usdc next release %d", got-day)
	}
	if got := paced.usdcNextRelease(1_000_000, day+19*h); got != day+24*h {
		t.Fatalf("usdc spent: next %d", got-day)
	}
	big := &workStanding{USDCDayMax: 1_000_000, Windows: 4, Tiers: tier("0.50")}
	if big.usdcDayOpen(day) != 500_000 || big.usdcDayOpen(day+6*h) != 500_000 || big.usdcDayOpen(day+12*h) != 750_000 {
		t.Fatal("usdc floor: one top-tier accept fits from 00:00")
	}
	whole := &workStanding{USDCDayMax: 1_000_000, Windows: 1, Tiers: tier("0.10")}
	if whole.usdcDayOpen(day) != 1_000_000 || whole.usdcNextRelease(1_000_000, day+h) != day+24*h {
		t.Fatal("windows 1: usdc_day_max whole from 00:00")
	}
}

// C1170 end to end: a round with day_max 5 and windows 4 opens 2, 1, 1, 1
// across the day with carry-over, says when the next window opens, queues
// a result for it, and keeps per_agent_day per UTC day.
func TestWorkStandingWindowsPaceDayMax(t *testing.T) {
	s := rewardStore(t)
	standingDayWhole = false
	const h = int64(3600)
	day := testTime - testTime%86400
	owner := keyFor(60)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	id := rewardRequest(t, s, owner, "lobby")
	for _, bad := range []map[string]any{{"seats": 5, "reward": 100, "windows": 5}, {"seats": 5, "reward": 100, "windows": 0}, {"seats": 5, "reward": 100, "windows": 8}, {"reward": 100, "windows": 4}} {
		fails(t, s, standingCreate(s, owner, id, bad, 10*86400), "invalid_work_data")
	}
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 5, "day_max": 5, "results_max": 20, "windows": 4, "reward": 100}, 10*86400))
	st := standingAt(t, s, id)
	if st.Windows != 4 || st.Open != 1 || st.WindowLeft == nil || *st.WindowLeft != 2 || st.WindowEndsAt != day+6*h {
		t.Fatalf("00:00: %+v", st)
	}
	claim := func(key byte, at int64) (string, WorkAck) {
		t.Helper()
		atTime(s, day+at)
		result, c := standingClaim(t, s, keyFor(key), id, nil)
		return result, run(t, s, c).Data["ack"].(WorkAck)
	}
	ra, ack := claim(61, 0)
	if ack.State != "submitted" {
		t.Fatalf("first: %+v", ack)
	}
	if st = standingAt(t, s, id); st.Open != 0 || st.NextOpenAt != day+3*h+1 || st.BlockedBy != StandingBlockedDayMax || !strings.Contains(st.BlockedNote, "4 windows of the UTC day") || *st.WindowLeft != 1 {
		t.Fatalf("after the first: %+v", st)
	}
	if _, ack = claim(62, 3*h+1); ack.State != "submitted" {
		t.Fatalf("window 0's second: %+v", ack)
	}
	// Window 0 is spent: a claim queues for window 1, which opens at 06:00.
	rc, ack := claim(63, 3*h+2)
	if ack.State != "queued" || ack.NextOpenAt != day+6*h {
		t.Fatalf("queued for window 1: %+v", ack)
	}
	if st = standingAt(t, s, id); *st.WindowLeft != 0 || st.NextOpenAt != day+12*h {
		t.Fatalf("window 0 spent, one queued: %+v", st)
	}
	atTime(s, day+6*h)
	if _, err := s.SweepAllowance(testContext); err != nil {
		t.Fatal(err)
	}
	if got := getTestWork(t, s, rc); got.State != "submitted" {
		t.Fatalf("queued result at window 1: %q", got.State)
	}
	// per_agent_day stays per UTC day: an accepted worker waits for 00:00,
	// not for the next window.
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: ra, Amount: 1}))
	atTime(s, day+12*h)
	_, again := standingClaim(t, s, keyFor(61), id, nil)
	var e *Error
	if _, err := s.Execute(testContext, again, "test-origin"); !errors.As(err, &e) || e.Details.(map[string]any)["blocked_by"] != StandingBlockedPerAgentDay || e.Details.(map[string]any)["next_open_at"] != day+86400 {
		t.Fatalf("per_agent_day: %v", err)
	}
	// Window 2 goes untaken; at 18:00 its seat carries: 2 open.
	atTime(s, day+18*h)
	if st = standingAt(t, s, id); st.OpenedToday != 3 || st.Open != 2 || *st.WindowLeft != 2 || st.WindowEndsAt != day+24*h {
		t.Fatalf("18:00 carry-over: %+v", st)
	}
	// Nothing carries into the next day: 00:00 opens window 0's first only.
	atTime(s, day+24*h)
	if st = standingAt(t, s, id); st.OpenedToday != 0 || st.Open != 1 || *st.WindowLeft != 2 {
		t.Fatalf("next day: %+v", st)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// C1170: usdc_day_max paces the same way. 0.20 a day over 2 windows is 0.10
// each: a second 0.10 accept at 01:00 is paid in credits only and told when
// the next window's USDC opens; at 12:00 an accept owes USDC again. On
// windows 1 the same second accept owes USDC (usdc_day_max whole from 00:00).
func TestWorkStandingWindowsPaceUSDC(t *testing.T) {
	s, _ := usdcStore(t)
	const h = int64(3600)
	day := testTime - testTime%86400
	owner := keyFor(70)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	tiers := []any{map[string]any{"name": "bug", "credits": 100, "usdc": "0.10"}}
	paced := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, paced, map[string]any{"seats": 3, "results_max": 20, "tiers": tiers, "windows": 2, "usdc_day_max": "0.20"}, 10*86400))
	whole := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, whole, map[string]any{"seats": 3, "results_max": 20, "tiers": tiers, "usdc_day_max": "0.20"}, 10*86400))
	results := map[string][]string{}
	for i, id := range []string{paced, whole} {
		for j := 0; j < 3; j++ {
			r, c := standingClaim(t, s, keyFor(byte(71+3*i+j)), id, map[string]any{"payout_address": usdcPayee})
			if ack := run(t, s, c).Data["ack"].(WorkAck); ack.State != "submitted" {
				t.Fatalf("claim: %+v", ack)
			}
			results[id] = append(results[id], r)
		}
	}
	if st := standingAt(t, s, paced); st.USDCRoom != "0.1" || st.USDCDayMax != "0.2" {
		t.Fatalf("paced room at 00:00: %+v", st)
	}
	atTime(s, day+h)
	for _, id := range []string{paced, whole} {
		run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: results[id][0], Amount: 1}))
	}
	res := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: results[paced][1], Amount: 1}))
	if note := res.Data["ack"].(WorkAck).Note; !strings.Contains(note, "paid in credits only") || !strings.Contains(note, "released in 2 windows of the UTC day") || !strings.Contains(note, "released at 2026") {
		t.Fatalf("paced capped note: %q", note)
	}
	if w := getTestWork(t, s, results[paced][1]); w.RewardUSDC.State != "void" || w.RewardUSDC.Reason != WorkUSDCCapReason {
		t.Fatalf("paced second accept: %+v", w.RewardUSDC)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: results[whole][1], Amount: 1}))
	if w := getTestWork(t, s, results[whole][1]); w.RewardUSDC.State != "payable" {
		t.Fatalf("windows 1 second accept: %+v", w.RewardUSDC)
	}
	atTime(s, day+12*h)
	if st := standingAt(t, s, paced); st.USDCRoom != "0.1" {
		t.Fatalf("paced room at 12:00: %q", st.USDCRoom)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: results[paced][2], Amount: 1}))
	if w := getTestWork(t, s, results[paced][2]); w.RewardUSDC.State != "payable" {
		t.Fatalf("paced accept at 12:00: %+v", w.RewardUSDC)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}
