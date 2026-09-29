package moderation

import (
	"context"
	"errors"
	"testing"
)

// Security review 1.21, L1: the inference output surface fails open (flag)
// when Jev cannot answer, which is right for a signed caller; a call without
// a key is published as "refused if the screen is not running", so its
// output fails closed.
func TestSecReview121UnsignedInferenceOutputFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"down", "over its spend cap"} {
		policy := ""
		if how == "over its spend cap" {
			policy = `{"schema":1,"version":1,"jev":{"model":"jev-1.13.0","daily_spend_cap_microusd":1,"price_per_mtok_microusd":1,"max_text_bytes":12000,"timeout_ms":5000}}`
		}
		v := newEnv(t, policy)
		if how == "down" {
			v.jev.fail(503)
		} else {
			v.screen(t, SurfacePost, "first", "x") // spends the cap
		}
		if hide, _, err := v.e.ScreenInferenceFor(ctx, "output", "small", "an answer", false); !hide || !errors.Is(err, ErrUnscreened) {
			t.Fatalf("Jev %s: an unsigned call's output: hide=%v err=%v", how, hide, err)
		}
		if hide, _, err := v.e.ScreenInferenceFor(ctx, "prompt", "small", "a question", false); !hide {
			t.Fatalf("Jev %s: an unsigned call's prompt: hide=%v err=%v", how, hide, err)
		}
		// Signed calls keep the surface's policy: the output is flagged, not withheld.
		if hide, _, err := v.e.ScreenInferenceFor(ctx, "output", "small", "an answer", true); hide || err != nil {
			t.Fatalf("Jev %s: a signed call's output: hide=%v err=%v", how, hide, err)
		}
	}
	// With Jev answering, an unsigned call's clean output passes.
	v := newEnv(t, "")
	if hide, _, err := v.e.ScreenInferenceFor(ctx, "output", "small", "an answer", false); hide || err != nil {
		t.Fatalf("a screened clean output: hide=%v err=%v", hide, err)
	}
}
