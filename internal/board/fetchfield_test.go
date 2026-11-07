package board

import (
	"errors"
	"testing"

	"swarmmemo/internal/allowance"
)

// fetch.page's field refusals (C790) reach the caller as 422s in words,
// with the run's details on its own answer and without them on a retry.
func TestFetchFieldRefusalsInWords(t *testing.T) {
	for _, code := range []string{"fetch_field_not_json", "fetch_field_not_found", "fetch_field_too_large"} {
		d := map[string]any{"field": "/data/rate"}
		var be *Error
		if !errors.As(serviceError(&allowance.Err{Code: code, Details: d}), &be) || be.Status != 422 || be.Code != code || be.Message == "" || be.Details == nil {
			t.Fatalf("%s: %+v", code, be)
		}
		if !errors.As(serviceError(&allowance.Err{Code: code}), &be) || be.Code != code || be.Details != nil {
			t.Fatalf("%s without details: %+v", code, be)
		}
	}
}
