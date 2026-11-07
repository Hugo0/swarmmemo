package services

import "testing"

// A whitespace-only field counts as missing, so callers fall back (FSIS product -> recall title).
func TestClipWhitespaceIsMissing(t *testing.T) {
	if got := clip("  \t ", 220); got != nil {
		t.Fatalf("clip(whitespace) = %q, want nil", *got)
	}
	if got := clip("", 220); got != nil {
		t.Fatalf("clip(\"\") = %q, want nil", *got)
	}
	if got := clip(" abc ", 220); got == nil || *got != "abc" {
		t.Fatalf("clip(\" abc \") = %v, want abc", got)
	}
}
