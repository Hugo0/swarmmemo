package main

// Owned by builder A (RFC0012 §6.1, §12.1): the operator tier list.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"swarmmemo/internal/board"
)

const tierUsage = "usage: swarmmemo tier grant AGENT 1|2 --reason TEXT | tier revoke AGENT --reason TEXT | tier list [--log]"

// operatorTier is "swarmmemo tier grant AGENT 1|2 --reason TEXT, tier revoke AGENT --reason TEXT, tier list".
// Grants are public allocation decisions (tier 1 trusted, tier 2 proven), at
// most board.TierGrantsMax at once, each with a reason in the public log.
func operatorTier(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	reasonOf := func(rest []string) (string, error) {
		if len(rest) != 2 || rest[0] != "--reason" {
			return "", errors.New(tierUsage)
		}
		return rest[1], nil
	}
	switch {
	case len(args) >= 1 && args[0] == "grant":
		if len(args) != 5 {
			return errors.New(tierUsage)
		}
		tier, err := strconv.Atoi(args[2])
		if err != nil || (tier != 1 && tier != 2) {
			return errors.New("tier must be 1 (trusted) or 2 (proven); " + tierUsage)
		}
		reason, err := reasonOf(args[3:])
		if err != nil {
			return err
		}
		if err = store.GrantTier(ctx, args[1], tier, reason); err != nil {
			return err
		}
		fmt.Fprintf(out, "Tier %d (%s) granted to %s\n", tier, board.TierNames[tier], args[1])
		return nil
	case len(args) >= 1 && args[0] == "revoke":
		if len(args) != 4 {
			return errors.New(tierUsage)
		}
		reason, err := reasonOf(args[2:])
		if err != nil {
			return err
		}
		if err = store.RevokeTier(ctx, args[1], reason); err != nil {
			return err
		}
		fmt.Fprintf(out, "Tier grant revoked for %s\n", args[1])
		return nil
	case len(args) == 1 && args[0] == "list":
		grants, err := store.TierGrants(ctx)
		if err != nil {
			return err
		}
		for _, g := range grants {
			fmt.Fprintf(out, "%s\t%d\t%s\t%d\t%s\n", g.Account, g.Tier, board.TierNames[g.Tier], g.GrantedAt, g.Reason)
		}
		fmt.Fprintf(out, "%d of at most %d accounts hold a tier grant\n", len(grants), board.TierGrantsMax)
		return nil
	case len(args) == 2 && args[0] == "list" && args[1] == "--log":
		entries, err := store.TierGrantLog(ctx, 100)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fmt.Fprintf(out, "%d\t%d\t%s\t%s\t%d\t%s\n", e.Seq, e.CreatedAt, e.Action, e.Account, e.Tier, e.Reason)
		}
		return nil
	}
	return errors.New(tierUsage)
}
