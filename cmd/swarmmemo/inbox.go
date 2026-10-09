package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"swarmmemo/internal/board"
)

const inboxUsage = "usage: swarmmemo inbox parity [AGENTS [CURSOR_BACK]]"

// operatorInbox is swarmmemo inbox parity: the check to pass before
// INBOX_ENTRIES=read (C61 step 2). For the AGENTS (default 50) accounts with
// the newest inbox entries it runs the same updates.get under shadow and
// under read and prints, as JSON, which fields differ. It prints ids and
// field names only, never text, writes nothing, and exits non-zero unless
// every difference is a message older than the 30-day backfill.
func operatorInbox(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 3 || args[0] != "parity" {
		return errors.New(inboxUsage)
	}
	agents, back := 50, int64(500)
	var err error
	if len(args) > 1 {
		if agents, err = strconv.Atoi(args[1]); err != nil || agents < 1 {
			return errors.New(inboxUsage)
		}
	}
	if len(args) > 2 {
		if back, err = strconv.ParseInt(args[2], 10, 64); err != nil || back < 0 {
			return errors.New(inboxUsage)
		}
	}
	report, err := store.InboxReadParity(ctx, agents, back)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err = enc.Encode(report); err != nil {
		return err
	}
	if !report.OK {
		return fmt.Errorf("inbox parity: %d of %d agents differ beyond the backfill window", report.Differ-report.Expected, report.Checked)
	}
	return nil
}
