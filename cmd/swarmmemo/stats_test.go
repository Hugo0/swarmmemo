package main

import (
	"bytes"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestStatsReferrersArguments(t *testing.T) {
	for args, want := range map[string]int{"referrers": 7, "referrers --days 1": 1, "referrers --days 90": 90} {
		if got, err := statsDays(strings.Fields(args)); err != nil || got != want {
			t.Errorf("%q: %d %v", args, got, err)
		}
	}
	for _, args := range []string{"", "referrer", "referrers --days", "referrers --days 0", "referrers --days 91", "referrers --days 07", "referrers --days x", "referrers --days 7 extra", "referrers 7", "referrers --days -1"} {
		if _, err := statsDays(strings.Fields(args)); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestStatsReferrersOutput(t *testing.T) {
	var out bytes.Buffer
	printReferrerStats(&out, []board.ReferrerDay{
		{Day: "2026-09-24", Hosts: []board.ReferrerCount{{Name: "github.com", Count: 12}}, Other: 3, Agents: []board.ReferrerCount{{Name: "ClaudeBot", Count: 40}},
			Refs: []board.ReferrerCount{{Name: "reddit", Count: 5}}, RefOther: 2, RefVisitors: map[string]int64{"reddit": 4}, RefVisitorOther: 1},
		{Day: "2026-09-23"},
	})
	want := "2026-09-24 UTC\n  referrers (visiting networks; days before 2026-10-24 count requests):\n          12  github.com\n           3  (other)\n  crawlers and agents:\n          40  ClaudeBot\n" +
		"  ?ref= venues (views, distinct visiting networks):\n           5      4  reddit\n           2      1  (other)\n" +
		"2026-09-23 UTC\n  referrers (visiting networks; days before 2026-10-24 count requests): none\n  crawlers and agents: none\n"
	if out.String() != want {
		t.Fatalf("got:\n%s", out.String())
	}
}

func TestStatsAssistantsArguments(t *testing.T) {
	for args, want := range map[string]int{"assistants": 7, "assistants --days 3": 3} {
		if got, err := statsDays(strings.Fields(args)); err != nil || got != want {
			t.Errorf("%q: %d %v", args, got, err)
		}
	}
	for _, args := range []string{"assistant", "assistants --days 0", "assistants 7", "assistants --days 91"} {
		if _, err := statsDays(strings.Fields(args)); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
}

func TestStatsAssistantsOutput(t *testing.T) {
	var out bytes.Buffer
	printAssistantStats(&out, []board.ReferrerDay{
		{Day: "2026-10-18", Agents: []board.ReferrerCount{{Name: "Claude-User", Count: 9}, {Name: "ClaudeBot", Count: 40}},
			Assistants: []board.AssistantFetches{
				{Family: "ChatGPT-User", Paths: []board.ReferrerCount{{Name: "/for-agents", Count: 1}}},
				{Family: "Claude-User", Paths: []board.ReferrerCount{{Name: "/llms.txt", Count: 4}, {Name: "/e/{id}", Count: 3}}, Other: 1},
			}},
		{Day: "2026-10-17"},
	})
	want := "2026-10-18 UTC\n  ChatGPT-User: 1 landing-page fetches of 0 requests\n           1  /for-agents\n" +
		"  Claude-User: 8 landing-page fetches of 9 requests\n           4  /llms.txt\n           3  /e/{id}\n           1  (other)\n" +
		"2026-10-17 UTC\n  assistant fetches: none\n"
	if out.String() != want {
		t.Fatalf("got:\n%s", out.String())
	}
}
