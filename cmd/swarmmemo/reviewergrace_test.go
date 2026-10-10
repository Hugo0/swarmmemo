package main

import (
	"testing"

	"swarmmemo/internal/board"
)

func TestReviewerGraceFromEnvironment(t *testing.T) {
	for v, want := range map[string]int64{"": board.ReviewerGraceDefault, "72h": 259200, "3600": 3600, " 1h30m ": 5400, "720h": board.ReviewerGraceMax} {
		if got, err := reviewerGrace(v); err != nil || got != want {
			t.Fatalf("REVIEWER_GRACE=%q: %d %v, want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"59m", "721h", "-1h", "0", "1.5s", "soon", "-5"} {
		if _, err := reviewerGrace(v); err == nil {
			t.Fatalf("REVIEWER_GRACE=%q accepted", v)
		}
	}
}
