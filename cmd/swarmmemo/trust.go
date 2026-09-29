package main

// Owned by builder D (RFC0012 §12.1): the trust module's operator commands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"swarmmemo/internal/board"
)

const trustUsage = "usage: swarmmemo trust run | trust show AGENT | trust inputs | trust lift EVIDENCE_ID --reason TEXT"

// operatorTrust is "swarmmemo trust run", "trust show AGENT", "trust inputs"
// (the JSONL snapshot a run as of today would read, for verifiers) and
// "trust lift EVIDENCE_ID --reason TEXT" (the only human action on
// evidence: lifting it, with a public reason). Run and show need TRUST set.
func operatorTrust(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(trustUsage)
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			return errors.New(trustUsage)
		}
		sum, err := store.RunTrust(ctx)
		if err != nil {
			return trustError(err)
		}
		fmt.Fprintf(out, "trust run %d as_of=%d params_version=%d state=%s nodes=%d edges=%d work=%d output_sha256=%s\n",
			sum.ID, sum.AsOf, sum.ParamsVersion, sum.State, sum.Nodes, sum.Edges, sum.Work, sum.OutputSHA256)
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New(trustUsage)
		}
		res, err := store.Execute(ctx, board.Command{Operation: "trust.get", Target: args[1]}, "operator")
		if err != nil {
			return trustError(err)
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(res.Data)
	case "inputs":
		if len(args) != 1 {
			return errors.New(trustUsage)
		}
		return store.WriteTrustInputs(ctx, out)
	case "lift":
		if len(args) != 4 || args[2] != "--reason" {
			return errors.New(trustUsage)
		}
		if err := store.LiftTrustEvidence(ctx, args[1], strings.TrimSpace(args[3])); err != nil {
			return err
		}
		fmt.Fprintf(out, "lifted %s\n", args[1])
		return nil
	}
	return errors.New(trustUsage)
}

func trustError(err error) error {
	var be *board.Error
	if errors.As(err, &be) && be.Code == "service_unavailable" {
		return errors.New("the trust module is off: set TRUST=shadow (or allocation) for this command")
	}
	return err
}
