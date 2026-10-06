package main

// RFC0012 seam: flag parsing (§9) and the operator commands tier, params,
// allowance, lever and trust. Each command's stub lives in the file its
// builder owns: tiers.go (A), allowance.go (B: params, allowance), levers.go
// (E), trust.go (D).

import (
	"context"
	"fmt"
	"io"
	"os"

	"swarmmemo/internal/board"
)

// featuresFromEnvironment reads the RFC0012 flags; all default off.
func featuresFromEnvironment() (board.Features, error) { return board.ParseFeatures(os.Getenv) }

func rfc0012Operator(ctx context.Context, store *board.Store, command string, args []string, out io.Writer) error {
	switch command {
	case "tier":
		return operatorTier(ctx, store, args, out)
	case "params":
		return operatorParams(ctx, store, args, out)
	case "allowance":
		return operatorAllowance(ctx, store, args, out)
	case "lever":
		return operatorLever(ctx, store, args, out)
	case "trust":
		return operatorTrust(ctx, store, args, out)
	}
	return fmt.Errorf("unknown command %q", command)
}
