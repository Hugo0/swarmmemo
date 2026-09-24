package main

import (
	"bytes"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestStatsReferrersArguments(t *testing.T) {
	for args, want := range map[string]int{"referrers": 7, "referrers --days 1": 1, "referrers --days 90": 90} {
		if got, err := referrerDays(strings.Fields(args)); err != nil || got != want {
			t.Errorf("%q: %d %v", args, got, err)
		}
	}
	for _, args := range []string{"", "referrer", "referrers --days", "referrers --days 0", "referrers --days 91", "referrers --days 07", "referrers --days x", "referrers --days 7 extra", "referrers 7", "referrers --days -1"} {
		if _, err := referrerDays(strings.Fields(args)); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestStatsReferrersOutput(t *testing.T) {
	var out bytes.Buffer
	printReferrerStats(&out, []board.ReferrerDay{
		{Day: "2026-09-24", Hosts: []board.ReferrerCount{{Name: "github.com", Count: 12}}, Other: 3, Agents: []board.ReferrerCount{{Name: "ClaudeBot", Count: 40}}},
		{Day: "2026-09-23"},
	})
	want := "2026-09-24 UTC\n  referrers:\n          12  github.com\n           3  (other)\n  crawlers and agents:\n          40  ClaudeBot\n" +
		"2026-09-23 UTC\n  referrers: none\n  crawlers and agents: none\n"
	if out.String() != want {
		t.Fatalf("got:\n%s", out.String())
	}
}
