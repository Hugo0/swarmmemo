package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrUnscreened is ScreenInferenceFor's answer when a call without a key
// could not be screened: a classifier did not answer, so the surface's fail
// mode would let the text through flagged. The call fails closed instead.
var ErrUnscreened = errors.New("moderation: the text could not be screened")

// ScreenInference is the inference service's screening hook, in the shape of
// services.InferenceScreener (branch provider-inference) without importing
// it. Stage is "prompt" or "output"; hide true refuses the prompt (nothing is
// sent or charged) or withholds the output (kept as its hash and size); the
// reason is public. An unknown stage is an error, which fails the call closed.
// It screens a signed call's text; ScreenInferenceFor also takes unsigned ones.
//
// internal/board adapts it to services.InferenceScreener (inferenceScreener)
// and sets it as InferenceConfig.Screener when MODERATION is on.
func (e *Engine) ScreenInference(ctx context.Context, stage, model, text string) (hide bool, reason string, err error) {
	return e.ScreenInferenceFor(ctx, stage, model, text, true)
}

// ScreenInferenceFor is ScreenInference for a signed or an unsigned call.
// For an unsigned one (a call without a key) the surface never fails open:
// when a classifier could not answer (Decision.Degraded) and the verdict
// would still let the text through, it answers ErrUnscreened, so the prompt
// is not sent and the output is not returned (security review 1.21, L1; the
// published note: "refused if the screen is not running").
func (e *Engine) ScreenInferenceFor(ctx context.Context, stage, model, text string, signed bool) (hide bool, reason string, err error) {
	var s Surface
	switch stage {
	case "prompt":
		s = SurfaceInferencePrompt
	case "output":
		s = SurfaceInferenceOutput
	default:
		return true, "", fmt.Errorf("moderation: unknown inference stage %q", stage)
	}
	sum := sha256.Sum256([]byte(text))
	subject := "inference:" + bound(model, 64) + ":" + hex.EncodeToString(sum[:8])
	d := e.Screen(ctx, s, Subject{ID: subject}, Content{Text: text})
	if !signed && d.Degraded != "" && d.Action.Proceed() {
		return true, d.Reason, ErrUnscreened
	}
	return !d.Action.Proceed(), d.Reason, nil
}
