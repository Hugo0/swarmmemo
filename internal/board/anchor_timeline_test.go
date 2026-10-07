package board

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/ots"
)

func TestAnchorNextCheck(t *testing.T) {
	const sub = int64(1_000_000)
	for _, c := range []struct{ checked, want int64 }{
		{0, sub + anchorFirstCheck},       // never checked
		{sub - 5, sub + anchorFirstCheck}, // a clock step back counts as never
		{sub + 60, sub + anchorFirstCheck},
		{sub + anchorFirstCheck, sub + anchorFirstCheck + anchorFastEvery},
		{sub + anchorFastUntil - 1, sub + anchorFastUntil - 1 + anchorFastEvery},
		{sub + anchorFastUntil, sub + anchorFastUntil + anchorSlowEvery},
		{sub + anchorSlowUntil, sub + anchorSlowUntil + anchorIdleEvery},
	} {
		if got := anchorNextCheck(sub, c.checked); got != c.want {
			t.Errorf("checked %d: next %d, want %d", c.checked-sub, got-sub, c.want-sub)
		}
	}
}

// TestTransparencyAnchorPollBudget runs the background schedule against a
// calendar that never finishes, on a real database: polls come every few
// minutes with jitter, yet each pending anchor is asked about once per
// Bitcoin block interval for the first three hours and less often after,
// every poll within its cap, and nothing in the first half hour.
func TestTransparencyAnchorPollBudget(t *testing.T) {
	s := openTest(t, Config{})
	var ready atomic.Bool
	var gets atomic.Int64
	cal := fakeCalendar(t, &ready, &gets)
	cfg := TransparencyConfig{OTS: &ots.Client{Calendars: []string{cal.URL}}}
	clock := int64(testTime)
	s.now = func() time.Time { return time.Unix(clock, 0) }
	// Three checkpoints, five minutes apart, each submitted when signed.
	for i := range 3 {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("bracket %d", i)})
		s.transparencyTick(testContext, cfg)
		clock += 5 * 60
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_anchors WHERE state='pending'"); n != 3 {
		t.Fatalf("pending anchors: %d", n)
	}
	// Poll every five minutes plus up to 30 s of jitter for four hours.
	end := int64(testTime) + 4*3600
	for clock < end {
		if clock < testTime+anchorFirstCheck-anchorCheckEarly && gets.Load() != 0 {
			t.Fatalf("%d calendar reads in the first half hour", gets.Load())
		}
		clock += int64(anchorPollEvery/time.Second) + clock%31
		before := gets.Load()
		s.anchorTick(testContext, cfg, 1)
		if d := gets.Load() - before; d > 12 {
			t.Fatalf("one poll made %d calendar reads; the cap is 12", d)
		}
	}
	// Per anchor: one read per ~10 minutes from 30 minutes to 3 hours (about
	// 15), then one per half hour.
	per := float64(gets.Load()) / 3
	t.Logf("%.1f calendar reads per pending anchor over four hours", per)
	if per < 14 || per > 22 {
		t.Fatalf("%.1f reads per pending anchor over four hours", per)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_anchors WHERE state='pending'"); n != 3 {
		t.Fatalf("pending anchors: %d", n)
	}
	// The calendar commits: the next due check of each anchor confirms it.
	ready.Store(true)
	clock += anchorSlowEvery
	s.anchorTick(testContext, cfg, 1)
	if n := sqlCount(t, s, fmt.Sprintf("SELECT count(*) FROM tlog_anchors WHERE state='confirmed' AND bitcoin_height=100 AND checked_at=%d", clock)); n != 3 {
		t.Fatalf("confirmed anchors: %d", n)
	}
	a, err := s.ReadLogAnchors(testContext, 0, 10)
	if err != nil || len(a) != 3 || a[0].ConfirmedAt != clock || a[0].NextCheckAt != 0 || a[0].CheckpointAt == 0 {
		t.Fatalf("anchors: %+v %v", a, err)
	}
}

// TestTransparencyAnchorRetry: a checkpoint no calendar accepted is
// submitted again on the next poll, not a quarter-hour later.
func TestTransparencyAnchorRetry(t *testing.T) {
	s := openTest(t, Config{})
	var ready atomic.Bool
	cal := fakeCalendar(t, &ready, nil)
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "retry me"})
	down := TransparencyConfig{OTS: &ots.Client{Calendars: []string{cal.URL + "/down"}}}
	s.transparencyTick(testContext, down)
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_anchors"); n != 0 {
		t.Fatalf("anchors from a calendar that is down: %d", n)
	}
	s.anchorTick(testContext, TransparencyConfig{OTS: &ots.Client{Calendars: []string{cal.URL}}}, 1)
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_anchors WHERE state='pending'"); n != 1 {
		t.Fatalf("anchors after the retry: %d", n)
	}
}

// TestTransparencyAnchoringOff: with OTS_CALENDARS=off the job signs
// checkpoints, anchors nothing and proofs carry no anchor.
func TestTransparencyAnchoringOff(t *testing.T) {
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "unanchored"})
	s.transparencyTick(testContext, TransparencyConfig{})
	s.anchorTick(testContext, TransparencyConfig{}, 1)
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_checkpoints"); n != 1 {
		t.Fatalf("checkpoints: %d", n)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_anchors"); n != 0 {
		t.Fatalf("anchors with anchoring off: %d", n)
	}
	proof, err := s.ReadLogProof(testContext, 0, "", -1)
	if err != nil || proof.Anchor != nil {
		t.Fatalf("proof: %+v %v", proof.Anchor, err)
	}
	if a, err := s.ReadLogAnchors(testContext, 0, 10); err != nil || len(a) != 0 {
		t.Fatalf("anchors: %v %v", a, err)
	}
}

// A stale anchor is no bracket: the proof names the next live one.
func TestTransparencyProofSkipsStaleAnchor(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	var sizes []int64
	for i := range 2 {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("stale %d", i)})
		if _, err := s.SignCheckpoint(testContext); err != nil {
			t.Fatal(err)
		}
		cp, err := s.ReadLogCheckpoint(testContext, -1)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, cp.Size)
	}
	for i, state := range []string{"stale", "confirmed"} {
		if _, err := s.db.Exec("INSERT INTO tlog_anchors(size,digest,ots,state,bitcoin_height,calendars,submitted_at,checked_at) VALUES(?,'d',x'00',?,?,'',?,?)", sizes[i], state, 900000+i, testTime+int64(i), testTime+3600+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	proof, err := s.ReadLogProof(testContext, 0, "", -1)
	if err != nil || proof.Anchor == nil || proof.Anchor.Size != sizes[1] || proof.Anchor.BitcoinHeight != 900001 || proof.Anchor.ConfirmedAt != testTime+3601 {
		t.Fatalf("proof anchor: %+v %v", proof.Anchor, err)
	}
}
