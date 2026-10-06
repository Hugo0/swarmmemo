package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"swarmmemo/internal/board"
)

const fetchUsage = "usage: swarmmemo fetch deny HOST REASON | fetch allow HOST | fetch denylist"

// operatorFetch is the operator's denylist for fetch: a denied host and its
// subdomains are refused before anything is reserved, and at every
// redirect, from the next call on.
func operatorFetch(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	switch {
	case len(args) == 3 && args[0] == "deny":
		if err := store.FetchDeny(ctx, args[1], args[2]); err != nil {
			return err
		}
		fmt.Fprintf(out, "denied %s and its subdomains for fetch\n", args[1])
	case len(args) == 2 && args[0] == "allow":
		removed, err := store.FetchAllow(ctx, args[1])
		if err != nil {
			return err
		}
		if !removed {
			fmt.Fprintf(out, "%s was not on the denylist\n", args[1])
			return nil
		}
		fmt.Fprintf(out, "%s is off the denylist\n", args[1])
	case len(args) == 1 && args[0] == "denylist":
		list, err := store.FetchDenylist(ctx)
		if err != nil {
			return err
		}
		for _, d := range list {
			fmt.Fprintf(out, "%s\t%s\t%s\n", d.Host, time.Unix(d.CreatedAt, 0).UTC().Format(time.RFC3339), d.Reason)
		}
	default:
		return errors.New(fetchUsage)
	}
	return nil
}
