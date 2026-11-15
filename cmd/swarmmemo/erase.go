package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"swarmmemo/internal/board"
)

const eraseUsage = "usage: swarmmemo erase account FINGERPRINT [--reason REQUEST_REF] [--confirm] | erase log"

// operatorErase is owner erasure on a mailed request (board/erase.go,
// C1012). Without --confirm it is a dry run: the same erasure in a
// transaction that is rolled back, so the counts are exact and nothing
// changes. With --confirm it erases and prints the receipt.
func operatorErase(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "log" {
		entries, err := store.ErasureLog(ctx)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Fprintln(out, "no erasures recorded")
		}
		for _, e := range entries {
			fmt.Fprintf(out, "%s  %s  ref %s  raw held %d  receipt %s\n", board.ErasureTime(e.ErasedAt), e.Account, orDash(e.Reference), e.RawHeld, e.ReceiptSHA256)
		}
		return nil
	}
	if len(args) < 2 || args[0] != "account" {
		return errors.New(eraseUsage)
	}
	account, rest := args[1], args[2:]
	reference, confirm := "", false
	for len(rest) > 0 {
		switch {
		case rest[0] == "--confirm":
			confirm, rest = true, rest[1:]
		case rest[0] == "--reason" && len(rest) > 1:
			reference, rest = rest[1], rest[2:]
		default:
			return errors.New(eraseUsage)
		}
	}
	r, err := store.OperatorEraseAccount(ctx, account, reference, confirm)
	if err != nil {
		return err
	}
	if r.DryRun {
		fmt.Fprintf(out, "Dry run for account %s: nothing changed. Add --confirm to erase.\n", account)
	} else {
		fmt.Fprintf(out, "Erased account %s at %s.\n", account, board.ErasureTime(r.ErasedAt))
	}
	fmt.Fprintln(out, "Per store:")
	for _, c := range r.Counts {
		fmt.Fprintf(out, "  %-22s %6d  %s\n", c.Store, c.Rows, c.Action)
	}
	fmt.Fprintf(out, "  %-22s %6d  removed\n", "raw_signals", r.RawErased)
	if r.RawHeld > 0 {
		fmt.Fprintf(out, "  %-22s %6d  held as evidence for an open report or sanction\n", "raw_signals", r.RawHeld)
	}
	fmt.Fprintln(out, "Kept:")
	for _, k := range r.Kept {
		fmt.Fprintln(out, "  - "+k)
	}
	fmt.Fprintln(out, r.Backups)
	if r.DryRun {
		return nil
	}
	receipt, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintf(out, "Receipt (sha256 %s, recorded in erasure_log):\n%s\n", r.ReceiptHash, receipt)
	fmt.Fprintf(out, "Signed by the log key (an account.erase leaf in the transparency log once serve catches up):\n%s", r.Signed)
	return nil
}
