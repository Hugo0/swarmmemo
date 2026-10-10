package trust

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// C160: standing.signal_link_days makes accounts that wrote from one network
// with one client not independent of each other. Off (0) the links are read
// and ignored; on, an early voter whose only confirmers share its network
// earns nothing from them. The links never reach the published snapshot.
func TestSignalLinksAreNotIndependent(t *testing.T) {
	build := func(days int64, links bool) Output {
		b := stakesWorld("judge", "late", "dud")
		b.snap.Params.Standing.ArbiterSeedCents = 100000
		b.snap.Params.Standing.SignalLinkDays = days
		if links {
			for i := 1; i <= 5; i++ {
				b.snap.SignalLinks = append(b.snap.SignalLinks, Record{Type: "signal_link", Account: acct("judge"), LinkAccount: acct("s" + strconv.Itoa(i))})
			}
		}
		for i := 0; i < 4; i++ {
			b.voteOn("s"+strconv.Itoa(i+1), "author", "usual-"+strconv.Itoa(i), int64(5000+i))
		}
		b.voteOn("judge", "author", "hit", 3000)
		for i := 0; i < 5; i++ {
			b.voteOn("s"+strconv.Itoa(i+1), "author", "hit", int64(2000-10*i))
		}
		b.voteOn("late", "author", "hit", 100)
		b.voteOn("dud", "author", "dud", 3000)
		return compute(t, b)
	}
	base, off, on := build(0, false), build(0, true), build(30, true)
	j, jOff, jOn := standingOf(base, "judge"), standingOf(off, "judge"), standingOf(on, "judge")
	t.Logf("judge: no links %d, links off %d, links on %d (seed %d)", j.Cents, jOff.Cents, jOn.Cents, j.SeedCents)
	if j.Cents <= j.SeedCents || jOff.Cents != j.Cents {
		t.Fatalf("off, the links must change nothing: %d vs %d", jOff.Cents, j.Cents)
	}
	if (jOn.Cents-jOn.SeedCents)*10 > j.Cents-j.SeedCents {
		t.Fatalf("confirmation from signal-linked accounts earned: %+v", jOn)
	}
	if _, ok := off.Standing.Inputs["signal_links"]; ok || on.Standing.Inputs["signal_links"] != 5 {
		t.Fatalf("summary off %v, on %v", off.Standing.Inputs, on.Standing.Inputs)
	}
}

func TestSignalLinksNeverInTheSnapshot(t *testing.T) {
	var s Snapshot
	if err := s.Add(Record{Type: "signal_link", Account: acct("a"), LinkAccount: acct("b")}); err != nil || len(s.SignalLinks) != 1 {
		t.Fatalf("add: %v", err)
	}
	for _, r := range s.Records() {
		if r.Type == "signal_link" {
			t.Fatal("Records lists a signal link")
		}
	}
	var buf bytes.Buffer
	if err := s.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "signal_link") {
		t.Fatalf("the published snapshot carries signal links:\n%s", buf.String())
	}
}

func TestSignalLinkDaysBounds(t *testing.T) {
	for _, tc := range []struct {
		rule, days int64
		ok         bool
	}{{3, 0, true}, {3, 30, true}, {3, 90, true}, {3, 91, false}, {3, -1, false}, {2, 7, false}} {
		p := DefaultParams()
		p.Standing.Rule, p.Standing.SignalLinkDays = tc.rule, tc.days
		if tc.rule < 3 {
			p.Standing.StakePPM, p.Standing.StakeBudgetPPM, p.Standing.PriorMinPosts = 0, 0, 0
		}
		if err := p.Validate(); (err == nil) != tc.ok {
			t.Errorf("rule %d, days %d: %v", tc.rule, tc.days, err)
		}
	}
	// Off, version 6's body is unchanged: the parameter is omitted.
	if strings.Contains(string(DefaultParams().Body()), "signal_link_days") {
		t.Fatal("version 6 names signal_link_days")
	}
}
