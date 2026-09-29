package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ScreenInference is the inference service's screening hook, in the shape of
// services.InferenceScreener (branch provider-inference) without importing
// it. Stage is "prompt" or "output"; hide true refuses the prompt (nothing is
// sent or charged) or withholds the output (kept as its hash and size); the
// reason is public. An unknown stage is an error, which fails the call closed.
//
// internal/board adapts it to services.InferenceScreener (inferenceScreener)
// and sets it as InferenceConfig.Screener when MODERATION is on.
func (e *Engine) ScreenInference(ctx context.Context, stage, model, text string) (hide bool, reason string, err error) {
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
	return !d.Action.Proceed(), d.Reason, nil
}
