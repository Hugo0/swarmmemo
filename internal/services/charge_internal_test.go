package services

import (
	"encoding/json"
	"testing"
)

// C815: call.charge is cost, paid_from and allowance_left as one object,
// its parts the result's cost_split, summing to the total.
func TestChargeOf(t *testing.T) {
	left := int64(0)
	rec := CallRecord{State: "done", Resource: "credit", Cost: 47, PaidFrom: PaidFromCredits, AllowanceLeft: &left}
	split := func(forced bool) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"cost_split": map[string]int64{"fetch": 12, "screening": 35}, "screen_forced": forced})
		return b
	}
	c := chargeOf(rec, split(false))
	if c == nil || c.Total != 47 || c.From != PaidFromCredits || c.Resource != "credit" || c.AllowanceLeft == nil || *c.AllowanceLeft != 0 || len(c.Parts) != 2 {
		t.Fatalf("credits call with screening: %+v", c)
	}
	var sum int64
	for _, p := range c.Parts {
		sum += p.Amount
	}
	if sum != c.Total || c.Parts[0] != (ChargePart{"fetch", 12}) || c.Parts[1] != (ChargePart{"screening", 35}) || c.WithoutScreening == nil || *c.WithoutScreening != 12 {
		t.Fatalf("parts %+v without_screening %v", c.Parts, c.WithoutScreening)
	}
	raw, _ := json.Marshal(c)
	if want := `{"total":47,"resource":"credit","from":"credits","parts":[{"name":"fetch","amount":12},{"name":"screening","amount":35}],"without_screening":12,"allowance_left":0}`; string(raw) != want {
		t.Fatalf("charge JSON %s, want %s", raw, want)
	}
	if c := chargeOf(rec, split(true)); c.WithoutScreening != nil || len(c.Parts) != 2 {
		t.Fatalf("forced screening is not optional: %+v", c)
	}
	// A free-allowance call without a split: no parts.
	free := CallRecord{State: "done", Resource: "credit", Cost: 8, PaidFrom: PaidFromFreeAllowance}
	if c := chargeOf(free, json.RawMessage(`{"echo":"hi"}`)); c == nil || c.Total != 8 || c.From != PaidFromFreeAllowance || c.Parts != nil || c.AllowanceLeft != nil {
		t.Fatalf("free call: %+v", c)
	}
	// A split that does not sum to the cost is left out, not reported wrong.
	rec.Cost = 50
	if c := chargeOf(rec, split(false)); c.Parts != nil || c.WithoutScreening != nil {
		t.Fatalf("mismatched split: %+v", c)
	}
	if chargeOf(CallRecord{State: "running"}, nil) != nil {
		t.Fatal("a running call has no charge")
	}
}
