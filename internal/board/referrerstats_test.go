package board

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Storage refuses anything but a domain, a known family or other: a caller
// cannot smuggle a path, a full URL or a raw User-Agent into a scope string.
func TestReferrerCountsAcceptOnlyAggregates(t *testing.T) {
	s := openTest(t, Config{})
	day := time.Unix(testTime, 0).UTC().Format("2006-01-02")
	for _, key := range []string{
		"host:example.com/path", "host:https://example.com", "host:Example.com", "host:example", "host:127.0.0.1",
		"host:user@example.com", "host:example.com:443", "host:" + strings.Repeat("a", 250) + ".com", "host:",
		"agent:Mozilla/5.0 (compatible; ClaudeBot/1.0)", "agent:SomeoneElse", "agent:", "reader:llms_txt", "other:x",
		"host:exa mple.com", "host:example.com\nreferrer:x", "", "host:bücher.de",
	} {
		if err := s.AddReferrerCounts(testContext, day, map[string]int64{key: 1}); err == nil {
			t.Errorf("accepted key %q", key)
		}
	}
	for _, bad := range []string{"2026-9-1", "2026-02-30", "x", "2026-09-24:host:a.com", ""} {
		if err := s.AddReferrerCounts(testContext, bad, map[string]int64{"other": 1}); err == nil {
			t.Errorf("accepted day %q", bad)
		}
	}
	if err := s.AddReferrerCounts(testContext, day, map[string]int64{"other": -1}); err == nil {
		t.Error("accepted a negative count")
	}
	days, err := s.ReadReferrerStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil || len(days[0].Hosts) != 0 || days[0].Other != 0 || len(days[0].Agents) != 0 {
		t.Fatalf("a refused write stored something: %+v %v", days, err)
	}
}

// Only the largest ReferrerHostsPerDay domains keep their names; the rest are
// folded into other, and nothing is lost from the day's total.
func TestReferrerDomainsAreBoundedPerDay(t *testing.T) {
	s := openTest(t, Config{})
	day := time.Unix(testTime, 0).UTC().Format("2006-01-02")
	counts := map[string]int64{"other": 5, "agent:ClaudeBot": 7, "agent:Googlebot": 9}
	var total int64 = 5
	for i := 1; i <= ReferrerHostsPerDay+50; i++ {
		counts["host:site"+strconv.Itoa(i)+".example"] = int64(i)
		total += int64(i)
	}
	if err := s.AddReferrerCounts(testContext, day, counts); err != nil {
		t.Fatal(err)
	}
	// A second write adds to existing rows and folds again.
	if err := s.AddReferrerCounts(testContext, day, map[string]int64{"host:site250.example": 1, "host:newcomer.example": 1}); err != nil {
		t.Fatal(err)
	}
	total += 2
	days, err := s.ReadReferrerStats(testContext, time.Unix(testTime, 0), 2)
	if err != nil || len(days) != 2 || days[0].Day != day {
		t.Fatalf("%+v %v", days, err)
	}
	today := days[0]
	if len(today.Hosts) != ReferrerHostsPerDay {
		t.Fatalf("%d named domains kept", len(today.Hosts))
	}
	if today.Hosts[0] != (ReferrerCount{"site250.example", 251}) || today.Hosts[ReferrerHostsPerDay-1].Name != "site51.example" {
		t.Fatalf("largest first, smallest folded: %v ... %v", today.Hosts[0], today.Hosts[ReferrerHostsPerDay-1])
	}
	sum := today.Other
	for _, h := range today.Hosts {
		sum += h.Count
	}
	if sum != total {
		t.Fatalf("folding lost counts: %d != %d", sum, total)
	}
	if len(today.Agents) != 2 || today.Agents[0] != (ReferrerCount{"Googlebot", 9}) {
		t.Fatalf("agents: %+v", today.Agents)
	}
	if yesterday := days[1]; len(yesterday.Hosts) != 0 || yesterday.Other != 0 {
		t.Fatalf("yesterday: %+v", yesterday)
	}
	// Reader statistics do not see referrer rows.
	stats, err := s.ReadDailyStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil || len(stats[0].Reads) != 0 {
		t.Fatalf("reader stats: %+v %v", stats, err)
	}
	if _, err := s.ReadReferrerStats(testContext, time.Unix(testTime, 0), 0); err == nil {
		t.Fatal("zero days accepted")
	}
}
