package services

import (
	"strings"
	"testing"
	"time"
)

// A working morning before the ECB publishes is not a holiday (45028cae, 11-18).
func TestFXDateNoteWorkingMorning(t *testing.T) {
	days := []fxDay{{Date: "2026-11-17"}, {Date: "2026-11-16"}, {Date: "2026-11-12"}}
	morning := time.Date(2026, 11, 18, 6, 41, 59, 0, time.UTC)
	out := fxAnswer(days, "EUR", []string{"USD"}, "2026-11-18", morning)
	if *out.RateDate != "2026-11-17" || !strings.Contains(out.DateNote, "yet") || strings.Contains(out.DateNote, "holiday") {
		t.Errorf("working morning: rate_date %v note %q", *out.RateDate, out.DateNote)
	}
	if out = fxAnswer(days, "EUR", []string{"USD"}, "2026-11-13", morning); !strings.Contains(out.DateNote, "TARGET holiday") {
		t.Errorf("a skipped working day: %q", out.DateNote)
	}
	if out = fxAnswer(days, "EUR", []string{"USD"}, "2026-11-15", morning); !strings.Contains(out.DateNote, "weekend") {
		t.Errorf("a Sunday: %q", out.DateNote)
	}
}
