package board

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// A refusal that carries a seller's pricing_url answers with it as
// details.pricing_url, beside the details it had; one without it answers
// exactly as before (C793).
func TestServiceRefusalCarriesThePricingURL(t *testing.T) {
	const u = "https://seller.example/x402/prices"
	wire := func(err error) (Error, string) {
		t.Helper()
		var be *Error
		if !errors.As(err, &be) {
			t.Fatalf("not a board error: %v", err)
		}
		raw, _ := json.Marshal(be)
		return *be, string(raw)
	}
	for _, code := range []string{"fetch_upstream_error", "x402_payment_rejected", "x402_cap_reached", "quota_exhausted"} {
		be, raw := wire(serviceError(&allowance.Err{Code: code, PricingURL: u}))
		if be.Code == "" || !strings.Contains(raw, `"details":{"pricing_url":"`+u+`"}`) {
			t.Errorf("%s: %s", code, raw)
		}
		if _, raw = wire(serviceError(&allowance.Err{Code: code})); strings.Contains(raw, "pricing_url") || strings.Contains(raw, `"details"`) {
			t.Errorf("%s without one: %s", code, raw)
		}
	}
	// Existing details are kept beside it.
	_, raw := wire(serviceError(&allowance.Err{Code: "price_exceeds_max", Details: map[string]int64{"quote": 7}, PricingURL: u}))
	if !strings.Contains(raw, `"quote":7`) || !strings.Contains(raw, `"pricing_url":"`+u+`"`) {
		t.Fatalf("with details: %s", raw)
	}
}
