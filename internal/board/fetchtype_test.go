package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// TestFetchUnsupportedTypeNamesTheType: the 415 says which media type the
// page was and carries it as details.received_type; with none recorded it
// stays the static sentence with no details (C650).
func TestFetchUnsupportedTypeNamesTheType(t *testing.T) {
	err := ServiceRefusal(&allowance.Err{Code: "fetch_unsupported_type", Details: map[string]any{"received_type": "application/pdf"}})
	var be *Error
	if !errors.As(err, &be) || be.Status != 415 || be.Code != "fetch_unsupported_type" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(be.Message, "this page is application/pdf.") || !strings.Contains(be.Message, "CSV") {
		t.Fatalf("message: %q", be.Message)
	}
	if d, _ := be.Details.(map[string]any); d["received_type"] != "application/pdf" || len(d) != 1 {
		t.Fatalf("details: %#v", be.Details)
	}
	err = ServiceRefusal(&allowance.Err{Code: "fetch_unsupported_type"})
	if !errors.As(err, &be) || be.Details != nil || !strings.Contains(be.Message, "this page is something else.") {
		t.Fatalf("no type: %#v", err)
	}
}
