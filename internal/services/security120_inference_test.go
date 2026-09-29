package services_test

// Security review 1.20 regression tests for inference. Each one inverts a
// proof of concept (TestSecPoC_*, branch security-review-1.20): it fails
// while the weakness is present.

import "testing"

// M3: calls whose every step times out after reaching the upstream still
// burn the upstream's daily cap (it may have billed us), but no longer for
// free: the caller is charged for it, up to the call's quote.
func TestSec120_InferenceTimeoutsAreChargedToTheCaller(t *testing.T) {
	v := newInfEnv(t, [3]int64{2 * primaryStep, 2 * 137, 10000})
	v.primary.setMode("timeout")
	v.backup.setMode("timeout")
	var charged int64
	for i := 0; i < 2; i++ {
		res, key, err := v.call(t, smallArgs, 1000)
		if err != nil {
			t.Fatalf("attack call %d: %v", i, err)
		}
		if result, _ := res["result"].(map[string]any); result["error"] != "upstream_busy" {
			t.Fatalf("attack call %d: %v", i, res)
		}
		h := v.hold(t, key)
		if h.State != "committed" || h.Units != smallQuote {
			t.Fatalf("the attacker is charged the quote: %+v", h)
		}
		charged += h.Units
	}
	if charged < 2*primaryStep {
		t.Fatalf("burning %d units of cap cost the caller only %d", 2*primaryStep+2*137, charged)
	}
}

// M3: an oversized reply (billed by the upstream) is charged to the caller.
func TestSec120_InferenceOversizedReplyIsCharged(t *testing.T) {
	v := newInfEnv(t, [3]int64{10000, 10000, 10000})
	v.primary.setMode("oversized")
	v.backup.setMode("oversized")
	res, key, err := v.call(t, smallArgs, 1000)
	if err != nil {
		t.Fatalf("oversized: %v", err)
	}
	if result, _ := res["result"].(map[string]any); result["error"] != "upstream_failed" || result["output"] != nil {
		t.Fatalf("oversized: %v", res)
	}
	if h := v.hold(t, key); h.State != "committed" || h.Units != smallQuote {
		t.Fatalf("caller charged: %+v", h)
	}
}
