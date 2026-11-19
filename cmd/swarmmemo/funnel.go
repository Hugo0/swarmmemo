package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

const funnelUsage = "usage: swarmmemo funnel [--days N] [--split X] [--organic] [--json|--html FILE] | funnel kpi [--days N] [--json] | funnel mcp-tools [--json] | funnel backfill [--since YYYY-MM-DD] | funnel recompute | funnel exclude add|rm FINGERPRINT | funnel exclude list | feedback list [--limit N]"

// operatorFunnel is the operator's funnel (board/funnel.go, C293a): the
// same board functions GET /admin/funnel serves. Every view leaves the
// exclude list's accounts out and refuses to print while it finds none.
func operatorFunnel(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "backfill":
			since := time.Now().Unix() - board.FunnelBackfillDays*86400
			if len(args) == 3 && args[1] == "--since" {
				t, err := time.Parse("2006-01-02", args[2])
				if err != nil {
					return errors.New(funnelUsage)
				}
				since = t.Unix()
			} else if len(args) != 1 {
				return errors.New(funnelUsage)
			}
			n, err := store.FunnelBackfill(ctx, since)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "funnel backfill from %s: %d accounts derived; agent_funnel recomputed\n", time.Unix(since, 0).UTC().Format("2006-01-02"), n)
			return nil
		case "recompute":
			n, err := store.FunnelRecompute(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "agent_funnel recomputed: %d accounts\n", n)
			return nil
		case "exclude":
			return funnelExclude(ctx, store, args[1:], out)
		case "kpi":
			return funnelKPI(ctx, store, args[1:], out)
		case "mcp-tools":
			return funnelMCPTools(ctx, store, args[1:], out)
		}
	}
	days, split, organic, asJSON, htmlFile := board.FunnelBackfillDays, "", false, false, ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--days", "--split", "--html":
			if i+1 >= len(args) {
				return errors.New(funnelUsage)
			}
			v := args[i+1]
			i++
			switch args[i-1] {
			case "--days":
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 366 {
					return errors.New("--days is 1 to 366")
				}
				days = n
			case "--split":
				split = v
			default:
				htmlFile = v
			}
		case "--organic":
			organic = true
		case "--json":
			asJSON = true
		default:
			return errors.New(funnelUsage)
		}
	}
	now := time.Now().Unix()
	rep, err := store.FunnelReport(ctx, board.FunnelQuery{From: now - int64(days)*86400, To: now, Split: split, Organic: organic})
	if err != nil {
		return err
	}
	if rep.ExcludedFound == 0 {
		return errors.New("funnel: the exclude list finds none of the operator's accounts; add them first (swarmmemo funnel exclude add FINGERPRINT), or these numbers count our own agents")
	}
	switch {
	case asJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", " ")
		return enc.Encode(rep)
	case htmlFile != "":
		f, err := os.Create(htmlFile)
		if err != nil {
			return err
		}
		if err = web.WriteFunnelDashboard(f, rep); err != nil {
			f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		fmt.Fprintln(out, "funnel page written to", htmlFile)
		return nil
	}
	fmt.Fprintf(out, "funnel %s to %s (excluded accounts found: %d)\n", day(rep.From), day(rep.To), rep.ExcludedFound)
	for _, s := range rep.Steps {
		line := fmt.Sprintf("  %-12s %6d", s.Stage, s.Count)
		if s.FromPrevious != nil {
			line += fmt.Sprintf("  %5.1f%% of previous (n=%d)", s.FromPrevious.Pct, s.FromPrevious.N)
		}
		fmt.Fprintln(out, line)
	}
	for _, c := range rep.Cohorts {
		fmt.Fprintf(out, "  week %s %s n=%d activated %.0f%% retained %.0f%% d7 %.0f%% (n=%d)\n", c.Week, c.Value, c.N, c.Activated.Pct, c.Retained.Pct, c.Retention["d7"].Pct, c.Retention["d7"].N)
	}
	b := rep.Burst
	fmt.Fprintf(out, "burst clusters: %d (%d keys; %d of %d new agents in range)\n", b.Clusters, b.Members, b.InRange, b.Overall.N)
	fmt.Fprintf(out, "  activated: all %s, excluding bursts %s\n", rateLine(b.Overall.Activated), rateLine(b.ExcludingBurst.Activated))
	fmt.Fprintf(out, "  retained:  all %s, excluding bursts %s\n", rateLine(b.Overall.Retained), rateLine(b.ExcludingBurst.Retained))
	fmt.Fprintf(out, "  d7:        all %s, excluding bursts %s\n", rateLine(b.Overall.Retention["d7"]), rateLine(b.ExcludingBurst.Retention["d7"]))
	l := rep.Loop
	fmt.Fprintln(out, "judgement loop (organic: no importer, lab or excluded account)")
	for _, w := range l.Weeks {
		fmt.Fprintf(out, "  week %s judgements %d: reviews %d, rated accepts %d, vouches %d\n", w.Week, w.Total, w.Reviews, w.RatedAccepts, w.Vouches)
	}
	fmt.Fprintf(out, "  rated within %dd: paid calls %s, accepted works %s\n", l.RateDays, rateLine(l.PaidCalls), rateLine(l.Works))
	for _, n := range l.Nudges {
		fmt.Fprintf(out, "  %s: shown %d, reviewed %s; holdout %d, reviewed %s\n", n.Code, n.Shown, rateLine(n.Followed), n.HoldoutRows, rateLine(n.HoldoutFol))
	}
	printNudgeLift(out, rep.Nudges)
	printRepeatPaid(out, rep.RepeatPaid)
	printMCPTools(out, rep.MCPTools)
	return nil
}

// printNudgeLift is each code's weekly follow lift against the holdout:
// accounts, not rows (board.FunnelNudgeLift).
func printNudgeLift(out io.Writer, nudges []board.FunnelNudge) {
	fmt.Fprintln(out, "nudge follow lift vs holdout, by week (accounts, follow window closed)")
	for _, n := range nudges {
		code := n.Code
		if n.Retired {
			code += " (retired)"
		}
		for _, l := range n.Lift {
			lift := "n/a"
			if l.LiftPP != nil {
				lift = fmt.Sprintf("%+.1f pp", *l.LiftPP)
			}
			fmt.Fprintf(out, "  %s week %s: nudged %s, holdout %s, lift %s\n", code, l.Week, rateLine(l.Nudged), rateLine(l.Holdout), lift)
		}
	}
}

// funnelMCPTools prints the hosted MCP tool calls of the last 7 and 30
// days (board/mcptoolstats.go): operator-only, never on /stats.
func funnelMCPTools(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 1 || len(args) == 1 && !asJSON {
		return errors.New(funnelUsage)
	}
	windows, err := store.MCPToolReport(ctx, time.Now())
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", " ")
		return enc.Encode(windows)
	}
	printMCPTools(out, windows)
	return nil
}

// printMCPTools is the "MCP tools" tables: each window's profiles, then
// its tools by calls. Callers are distinct per day (salted daily, never
// linked across days), summed over the window: caller-days.
func printMCPTools(out io.Writer, windows []board.MCPToolWindow) {
	for _, w := range windows {
		fmt.Fprintf(out, "MCP tools, last %d days (%s to %s; callers = distinct per day, summed)\n", w.Days, w.From, w.To)
		if w.Skipped > 0 {
			fmt.Fprintf(out, "  %d entries skipped for an invalid key (a counting bug: see board/mcptoolstats.go)\n", w.Skipped)
		}
		if len(w.Tools) == 0 {
			fmt.Fprintln(out, "  no tool calls recorded")
			continue
		}
		fmt.Fprintf(out, "  %-10s %-28s %8s %8s %7s  %s\n", "profile", "tool", "calls", "callers", "errors", "top families")
		for _, r := range append(append([]board.MCPToolRow(nil), w.Profiles...), w.Tools...) {
			tool := r.Tool
			if tool == board.MCPToolAny {
				tool = "(any tool)"
			}
			fmt.Fprintf(out, "  %-10s %-28s %8d %8d %7d  %s%s\n", r.Profile, tool, r.Calls, r.CallerDays, r.Errors, topFamilies(r.Families, 3), topErrorCodes(r, 3))
		}
	}
}

// topErrorCodes is a row's n most frequent error codes, the rest added to
// other: "; errors 949: offering_not_found 900, other 49", or "" with no
// errors.
func topErrorCodes(r board.MCPToolRow, n int) string {
	if r.Errors == 0 && len(r.ErrorCodes) == 0 {
		return ""
	}
	var parts []string
	var other int64
	for _, c := range r.ErrorCodes {
		if c.Code == board.MCPToolOther || len(parts) >= n {
			other += c.Errors
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d", c.Code, c.Errors))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("%s %d", board.MCPToolOther, other))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("; errors %d", r.Errors)
	}
	return fmt.Sprintf("; errors %d: %s", r.Errors, strings.Join(parts, ", "))
}

// topFamilies is the n families with the most calls, "claude 12, openai 3".
func topFamilies(m map[string]int64, n int) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		return m[names[i]] > m[names[j]] || m[names[i]] == m[names[j]] && names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// rateLine is a rate as k/n and its percent, "(low n)" below the minimum.
func rateLine(r board.Rate) string {
	s := fmt.Sprintf("%d/%d (%.1f%%)", r.K, r.N, r.Pct)
	if r.Low {
		s += " (low n)"
	}
	return s
}

func day(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02") }

func funnelExclude(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "list":
		list, err := store.FunnelExcludeList(ctx)
		if err != nil {
			return err
		}
		for _, a := range list {
			fmt.Fprintln(out, a)
		}
		fmt.Fprintf(out, "%d excluded\n", len(list))
		return nil
	case len(args) == 2 && (args[0] == "add" || args[0] == "rm"):
		account, err := store.FunnelExclude(ctx, args[1], args[0] == "add")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s\n", map[bool]string{true: "excluded", false: "included"}[args[0] == "add"], account)
		return nil
	}
	return errors.New(funnelUsage)
}

func funnelKPI(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	days, asJSON := 7, false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			asJSON = true
		case args[i] == "--days" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return errors.New("--days needs a number")
			}
			days = n
			i++
		default:
			return errors.New(funnelUsage)
		}
	}
	k, err := store.FunnelKPIFor(ctx, days)
	if err != nil {
		return err
	}
	if k.ExcludedFound == 0 {
		return errors.New("funnel kpi: the exclude list finds none of the operator's accounts; add them first")
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", " ")
		return enc.Encode(k)
	}
	fmt.Fprintf(out, "WAU (verifiable, outside agents, %dd): %d\nrepeat (active 3+ distinct days): %d; organic: %d\nby action: %v\n", days, k.Active, k.Repeat, k.RepeatOrganic, k.ByAction)
	printRepeatPaid(out, k.RepeatPaid)
	return nil
}

// printRepeatPaid is the unprompted repeat paid use lines (C541): totals,
// then per kind, unprompted first and all paid calls for comparison.
func printRepeatPaid(out io.Writer, r board.FunnelRepeatPaid) {
	fmt.Fprintf(out, "paid use %s to %s (unprompted: no work claim, renew, submit or accept as worker in the %dd before the call; repeat: paid on %d+ distinct UTC days; %d burst payers left out)\n",
		day(r.From), day(r.To), r.LookbackDays, r.MinDays, r.BurstPayersLeftOut)
	line := func(label string, p board.RepeatPaidPair) {
		u, a := p.Unprompted, p.All
		fmt.Fprintf(out, "  %-9s unprompted payers %d, repeat %d, paid calls %d; all payers %d, repeat %d, paid calls %d\n", label, u.Payers, u.RepeatPayers, u.Calls, a.Payers, a.RepeatPayers, a.Calls)
	}
	line("total", r.Total)
	for _, k := range board.RepeatPaidKinds {
		line(k, r.ByKind[k])
	}
}

func operatorFeedback(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	limit := 50
	if len(args) == 0 || args[0] != "list" {
		return errors.New(funnelUsage)
	}
	if len(args) == 3 && args[1] == "--limit" {
		n, err := strconv.Atoi(args[2])
		if err != nil {
			return errors.New(funnelUsage)
		}
		limit = n
	} else if len(args) != 1 {
		return errors.New(funnelUsage)
	}
	items, err := store.FeedbackList(ctx, limit)
	if err != nil {
		return err
	}
	for _, f := range items {
		fmt.Fprintf(out, "%s %s %s %s screen=%s", day(f.CreatedAt), f.ID[:8], f.About, f.Rating, f.Screen)
		switch {
		case f.Withheld:
			fmt.Fprintf(out, " (text withheld until screened pass; scores %s)\n", f.Scores)
		case f.Text != "":
			fmt.Fprintf(out, "\n  %q\n", f.Text)
		default:
			fmt.Fprintln(out)
		}
	}
	return nil
}
