package board

import (
	"errors"
	"strings"
	"testing"
)

// C968: fetch_redirect_refused names the rule that stopped the redirect and,
// when recorded, the target to fetch instead; without details it keeps the
// general sentence.
func TestFetchRedirectRefusedNamesTheTarget(t *testing.T) {
	var e *Error
	err := fetchRedirectRefused(map[string]any{"reason": "another_site", "redirect_to": "https://other.test/landing"})
	if !errors.As(err, &e) || e.Status != 502 || e.Code != "fetch_redirect_refused" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(e.Message, "another site") || !strings.Contains(e.Message, "https://other.test/landing (details.redirect_to)") || e.Details == nil {
		t.Fatalf("message %q details %v", e.Message, e.Details)
	}
	err = fetchRedirectRefused(map[string]any{"reason": "downgrade"})
	if !errors.As(err, &e) || !strings.Contains(e.Message, "https down to http") || strings.Contains(e.Message, "redirect_to") {
		t.Fatalf("downgrade: %q", e.Message)
	}
	err = fetchError("fetch_redirect_refused")
	if !errors.As(err, &e) || e.Details != nil || !strings.Contains(e.Message, "Fetch the target itself") {
		t.Fatalf("no details: %v", err)
	}
}
