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

const statsUsage = "usage: swarmmemo stats referrers|assistants [--days N] (N from 1 to 90, default 7)"

// statsDays parses "referrers|assistants [--days N]".
func statsDays(args []string) (int, error) {
	if len(args) == 0 || args[0] != "referrers" && args[0] != "assistants" {
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

// operatorStats prints the private daily referrer and agent counts, or with
// assistants the landing pages each assistant fetch family read, newest day
// first. Counts written in the last half minute may not be stored yet.
func operatorStats(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	days, err := statsDays(args)
	if err != nil {
		return err
	}
	stats, err := store.ReadReferrerStats(ctx, time.Now(), days)
	if err != nil {
		return err
	}
	if args[0] == "assistants" {
		printAssistantStats(out, stats)
		return nil
	}
	printReferrerStats(out, stats)
	return nil
}

// printAssistantStats prints each day's assistant fetch families: landing
// fetches out of all their requests, then the folded paths, largest first.
func printAssistantStats(out io.Writer, stats []board.ReferrerDay) {
	for _, day := range stats {
		fmt.Fprintf(out, "%s UTC\n", day.Day)
		if len(day.Assistants) == 0 {
			fmt.Fprintln(out, "  assistant fetches: none")
			continue
		}
		for _, a := range day.Assistants {
			var requests int64
			for _, agent := range day.Agents {
				if agent.Name == a.Family {
					requests = agent.Count
				}
			}
			fmt.Fprintf(out, "  %s: %d landing-page fetches of %d requests\n", a.Family, a.Total(), requests)
			for _, path := range a.Paths {
				fmt.Fprintf(out, "    %8d  %s\n", path.Count, path.Name)
			}
			if a.Other > 0 {
				fmt.Fprintf(out, "    %8d  (other)\n", a.Other)
			}
		}
	}
}

func printReferrerStats(out io.Writer, stats []board.ReferrerDay) {
	for _, day := range stats {
		fmt.Fprintf(out, "%s UTC\n  referrers (visiting networks; days before 2026-10-24 count requests):", day.Day)
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
		if len(day.Assets) > 0 || day.AssetOther > 0 {
			fmt.Fprintln(out, "  embeds and hotlinks (networks, not visits):")
			for _, host := range day.Assets {
				fmt.Fprintf(out, "    %8d  %s\n", host.Count, host.Name)
			}
			if day.AssetOther > 0 {
				fmt.Fprintf(out, "    %8d  (other)\n", day.AssetOther)
			}
		}
		fmt.Fprint(out, "  crawlers and agents:")
		if len(day.Agents) == 0 {
			fmt.Fprint(out, " none")
		}
		fmt.Fprintln(out)
		for _, agent := range day.Agents {
			fmt.Fprintf(out, "    %8d  %s\n", agent.Count, agent.Name)
		}
		if len(day.Refs) == 0 && day.RefOther == 0 {
			continue
		}
		fmt.Fprintln(out, "  ?ref= venues (views, distinct visiting networks):")
		for _, ref := range day.Refs {
			fmt.Fprintf(out, "    %8d %6d  %s\n", ref.Count, day.RefVisitors[ref.Name], ref.Name)
		}
		if day.RefOther > 0 {
			fmt.Fprintf(out, "    %8d %6d  (other)\n", day.RefOther, day.RefVisitorOther)
		}
	}
}
