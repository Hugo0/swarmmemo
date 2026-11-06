package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
)

const statsUsage = "usage: swarmmemo stats referrers|assistants|identity [--days N] (N from 1 to 90, default 7)"

// statsDays parses "referrers|assistants|identity [--days N]".
func statsDays(args []string) (int, error) {
	if len(args) == 0 || args[0] != "referrers" && args[0] != "assistants" && args[0] != "identity" {
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
// first, or with identity the keyless MCP callers' identity creation over
// the window (C778). Counts written in the last half minute may not be
// stored yet.
func operatorStats(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	days, err := statsDays(args)
	if err != nil {
		return err
	}
	if args[0] == "identity" {
		w, err := store.MCPIdentityStats(ctx, time.Now(), days)
		if err != nil {
			return err
		}
		printIdentityStats(out, w)
		return nil
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
		fmt.Fprintln(out, "  ?ref= venues (views, distinct visiting networks, views by family):")
		for _, ref := range day.Refs {
			fmt.Fprintf(out, "    %8d %6d  %s%s\n", ref.Count, day.RefVisitors[ref.Name], ref.Name, refAgentList(day.RefAgents[ref.Name]))
		}
		if day.RefOther > 0 {
			fmt.Fprintf(out, "    %8d %6d  (other)\n", day.RefOther, day.RefVisitorOther)
		}
	}
}

// refAgentList is "  [Discordbot 40, none 19]" for a venue's views by
// family (C777), or "" for days counted before them.
func refAgentList(families []board.ReferrerCount) string {
	if len(families) == 0 {
		return ""
	}
	parts := make([]string, len(families))
	for i, f := range families {
		parts[i] = fmt.Sprintf("%s %d", f.Name, f.Count)
	}
	return "  [" + strings.Join(parts, ", ") + "]"
}

// printIdentityStats prints keyless MCP identity creation by client family
// (C778): create_identity calls and identities created, then the tool called
// just before an identity was created, and the last tool of keyless callers
// that went idle without one.
func printIdentityStats(out io.Writer, w board.MCPIdentityWindow) {
	fmt.Fprintf(out, "keyless MCP identity creation %s to %s UTC (%d days)\n", w.From, w.To, w.Days)
	if len(w.Families) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, f := range w.Families {
		fmt.Fprintf(out, "  %s: %d create_identity calls, %d created%s\n", f.Family, f.Calls, f.Created, profileList(f.Profiles))
		fmt.Fprintf(out, "    last tool before an identity: %s\n", toolCountList(f.LastBefore))
		fmt.Fprintf(out, "    last tool, idle without one:  %s\n", toolCountList(f.LastNone))
	}
	if w.Overflow > 0 {
		fmt.Fprintf(out, "  %d keyless callers not followed (table full)\n", w.Overflow)
	}
	if w.Skipped > 0 {
		fmt.Fprintf(out, "  %d entries skipped for an invalid key\n", w.Skipped)
	}
}

// profileList is "  [assistant 2, mcp 1]", profiles sorted, or "".
func profileList(profiles map[string]int64) string {
	var parts []string
	for _, p := range board.MCPToolProfiles {
		if n := profiles[p]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", p, n))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  [" + strings.Join(parts, ", ") + "]"
}

// toolCountList is "read_feed 3, none 1", or "none counted".
func toolCountList(tools []board.MCPToolCount) string {
	if len(tools) == 0 {
		return "none counted"
	}
	parts := make([]string, len(tools))
	for i, t := range tools {
		parts[i] = fmt.Sprintf("%s %d", t.Tool, t.Count)
	}
	return strings.Join(parts, ", ")
}
