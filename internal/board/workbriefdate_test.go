package board

import (
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C1097: a standing task's brief that names another results date than
// standing.results_until.

func TestBriefDateMismatch(t *testing.T) {
	at := func(s string) int64 {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v.Unix()
	}
	// Skitter's "Round 11: route QA" (dcfabedc), 11-18.
	until := at("2026-11-19 11:00")
	for _, c := range []struct {
		name, text string
		until      int64
		want       string
	}{
		{"real round 11", "Round 11: route QA. Check each route. Results are accepted until 2026-11-22 00:45Z.", until, "2026-11-22 00:45 UTC"},
		{"quotes results_until exactly", "Round 12. Results are accepted until 2026-11-19 11:00 UTC.", until, ""},
		{"quotes it as RFC 3339", "Results until 2026-11-19T11:00:00Z.", until, ""},
		{"within the hour", "Results are taken until 2026-11-19 11:45Z.", until, ""},
		{"just past the hour", "Results are taken until 2026-11-19 12:01Z.", until, "2026-11-19 12:01 UTC"},
		{"day alone, same day", "Accepted until 2026-11-19.", until, ""},
		{"day alone, other day", "Accepted until 2026-11-21.", until, "2026-11-21"},
		{"day alone, the day before", "Submissions close 2026-11-18.", until, "2026-11-18"},
		{"closes", "This round closes on 2026-11-25 18:00 UTC.", until, "2026-11-25 18:00 UTC"},
		{"claims", "Claims close 2026-11-30.", until, "2026-11-30"},
		{"latest of two", "Results until 2026-11-19 11:00Z (claims close 2026-11-23 09:00Z).", until, "2026-11-23 09:00 UTC"},
		{"a date with other words", "Released 2026-11-22. Bug found on 2026-11-25 in the build of 2026-11-01.", until, ""},
		{"a deadline word alone", "Deadline 2026-11-22.", until, ""},
		{"no date", "Results are accepted until the round fills.", until, ""},
		{"invalid date", "Results are accepted until 2026-13-45.", until, ""},
		{"invalid time", "Results are accepted until 2026-11-22 25:61Z.", until, ""},
		{"an offset at the same UTC time", "Results are accepted until 2026-11-19 13:00+02:00.", until, ""},
		{"an offset at another time", "Results are accepted until 2026-11-22 02:45 +0200.", until, "2026-11-22 00:45 UTC"},
		{"another zone's name", "Results are accepted until 2026-11-22 08:00 PST.", until, ""},
		{"a lowercase word after the time", "Results until 2026-11-22 00:45 and no later.", until, "2026-11-22 00:45 UTC"},
		{"no results_until", "Results are accepted until 2026-11-22.", 0, ""},
	} {
		if got := BriefDateMismatch(c.text, c.until); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	line := BriefDateLine("2026-11-22 00:45 UTC", until)
	if line != "The brief says 2026-11-22 00:45 UTC; results are taken until 2026-11-19 11:00 UTC (the board enforces this date)." {
		t.Fatalf("line: %q", line)
	}
	if BriefDateLine("", until) != "" || BriefDateAdvice("", until) != "" {
		t.Fatal("no mismatch, no line")
	}
	if advice := BriefDateAdvice("2026-11-22 00:45 UTC", until); !strings.Contains(advice, "2026-11-22 00:45 UTC") || !strings.Contains(advice, "2026-11-19 11:00 UTC") || !strings.Contains(advice, "Edit the brief") {
		t.Fatalf("advice: %q", advice)
	}
}

// work.get carries brief_date_mismatch; the create's note and an edit's
// work_hint tell the requester; an edit that quotes results_until clears it.
func TestStandingBriefDateMismatch(t *testing.T) {
	OpenStandingDayForTest(t)
	s := rewardStore(t)
	owner := keyFor(124)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	ttl := int64(10 * 86400)
	until := testTime + ttl - WorkStandingVerdictTTL
	untilText := time.Unix(until, 0).UTC().Format("2006-01-02 15:04") + " UTC"
	later := time.Unix(until+3*86400, 0).UTC().Format("2006-01-02 15:04")
	brief := "Round 11: route QA. Results are accepted until " + later + "Z." + testPostSuffix()
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: brief, Timestamp: s.now().Unix()})).Receipt.ID
	ack := run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 2, "reward": 200}, ttl)).Data["ack"].(WorkAck)
	if !strings.Contains(ack.Note, "The brief says "+later+" UTC, but results are taken until "+untilText) {
		t.Fatalf("create note: %q", ack.Note)
	}
	if st := standingAt(t, s, id); st.ResultsUntil != until || st.BriefDateMismatch != later+" UTC" {
		t.Fatalf("work.get standing: until %d mismatch %q", st.ResultsUntil, st.BriefDateMismatch)
	}

	// An edit that quotes results_until: no hint, no field.
	edit := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Round 11: route QA. Results are accepted until " + untilText + "." + testPostSuffix(), Data: dataJSON(`"supersedes":"` + id + `"`), Timestamp: s.now().Unix()}))
	if line := WorkHintLine("", edit.Receipt); line != "" {
		t.Fatalf("matching edit's hint: %q", line)
	}
	if st := standingAt(t, s, id); st.BriefDateMismatch != "" {
		t.Fatalf("after a matching edit: %q", st.BriefDateMismatch)
	}

	// An edit that names another date again hears it.
	edit = run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: brief + " v3", Data: dataJSON(`"supersedes":"` + edit.Receipt.ID + `"`), Timestamp: s.now().Unix()}))
	if line := WorkHintLine("", edit.Receipt); !strings.Contains(line, "The brief says "+later+" UTC") || !strings.Contains(line, "Edit the brief") {
		t.Fatalf("edit's hint: %q", line)
	}

	// A standing task whose brief names no date: nothing.
	plain := rewardRequest(t, s, owner, "lobby")
	ack = run(t, s, standingCreate(s, owner, plain, map[string]any{"seats": 1, "reward": 100}, ttl)).Data["ack"].(WorkAck)
	if strings.Contains(ack.Note, "The brief says") || standingAt(t, s, plain).BriefDateMismatch != "" {
		t.Fatalf("plain brief: %q", ack.Note)
	}
}
