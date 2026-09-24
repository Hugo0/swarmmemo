package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"swarmmemo/internal/board"
)

const statsUsage = "usage: swarmmemo stats referrers [--days N] (N from 1 to 90, default 7)"

// referrerDays parses "referrers [--days N]".
func referrerDays(args []string) (int, error) {
	if len(args) == 0 || args[0] != "referrers" {
		return 0, errors.New(statsUsage)
	}
	switch {
	case len(args) == 1:
		return 7, nil
	case len(args) == 3 && args[1] == "--days":
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 1 || n > 90 || strconv.Itoa(n) != args[2] {
			return 0, errors.New(statsUsage)
		}
		return n, nil
	}
	return 0, errors.New(statsUsage)
}

// operatorStats prints the private daily referrer and agent counts, newest day
// first. Counts written in the last half minute may not be stored yet.
func operatorStats(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	days, err := referrerDays(args)
	if err != nil {
		return err
	}
	stats, err := store.ReadReferrerStats(ctx, time.Now(), days)
	if err != nil {
		return err
	}
	printReferrerStats(out, stats)
	return nil
}

func printReferrerStats(out io.Writer, stats []board.ReferrerDay) {
	for _, day := range stats {
		fmt.Fprintf(out, "%s UTC\n  referrers:", day.Day)
		if len(day.Hosts) == 0 && day.Other == 0 {
			fmt.Fprint(out, " none")
		}
		fmt.Fprintln(out)
		for _, host := range day.Hosts {
			fmt.Fprintf(out, "    %8d  %s\n", host.Count, host.Name)
		}
		if day.Other > 0 {
			fmt.Fprintf(out, "    %8d  (other)\n", day.Other)
		}
		fmt.Fprint(out, "  crawlers and agents:")
		if len(day.Agents) == 0 {
			fmt.Fprint(out, " none")
		}
		fmt.Fprintln(out)
		for _, agent := range day.Agents {
			fmt.Fprintf(out, "    %8d  %s\n", agent.Count, agent.Name)
		}
	}
}
