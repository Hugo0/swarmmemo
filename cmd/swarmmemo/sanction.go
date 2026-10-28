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

const sanctionUsage = `usage: swarmmemo sanction apply --message ID --category CATEGORY [--reason TEXT]
 | sanction manual (--account FINGERPRINT | --message ID) --scope exact|net24|account --days N --reason TEXT
 | sanction lift ID --reason TEXT
 | sanction list [--active] [--limit N]
 | sanction ladder`

// operatorSanction applies, lists and lifts abuse sanctions
// (board/sanctions.go, C449). The operator names a message; the board
// resolves its write signal, so no hash or address is ever printed.
func operatorSanction(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(sanctionUsage)
	}
	mode, rest := args[0], args[1:]
	var id string
	if mode == "lift" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "--") {
			return errors.New(sanctionUsage)
		}
		id, rest = rest[0], rest[1:]
	}
	flags := map[string]string{}
	allowed := map[string][]string{
		"apply":  {"--message", "--category", "--reason"},
		"manual": {"--account", "--message", "--scope", "--days", "--reason"},
		"lift":   {"--reason"},
		"list":   {"--active", "--limit"},
		"ladder": {},
	}[mode]
	if allowed == nil && mode != "ladder" {
		return errors.New(sanctionUsage)
	}
	for len(rest) > 0 {
		name := rest[0]
		ok := false
		for _, a := range allowed {
			ok = ok || a == name
		}
		if !ok {
			return fmt.Errorf("%s does not take %s\n%s", mode, name, sanctionUsage)
		}
		if name == "--active" {
			flags[name], rest = "1", rest[1:]
			continue
		}
		if len(rest) < 2 {
			return fmt.Errorf("%s needs a value", name)
		}
		flags[name], rest = rest[1], rest[2:]
	}
	switch mode {
	case "apply":
		if flags["--message"] == "" || flags["--category"] == "" {
			return errors.New(sanctionUsage)
		}
		o, err := store.ApplySanction(ctx, flags["--message"], flags["--category"], flags["--reason"], board.SanctionOperator)
		if err != nil {
			return err
		}
		printSanctionOutcome(out, o)
	case "manual":
		days, err := strconv.Atoi(flags["--days"])
		if err != nil {
			return errors.New("--days needs a whole number of days")
		}
		o, err := store.ApplyManualSanction(ctx, board.ManualSanction{Account: flags["--account"], MessageID: flags["--message"], Scope: flags["--scope"], Days: days, Reason: flags["--reason"], Author: board.SanctionOperator})
		if err != nil {
			return err
		}
		printSanctionOutcome(out, o)
	case "lift":
		lift, err := store.LiftSanction(ctx, id, flags["--reason"], "operator")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "sanction %s lifted\n", id)
		if lift != "" {
			fmt.Fprintf(out, "  its trust penalty lifted too (%s, public penalty log)\n", lift)
		}
	case "list":
		limit := 100
		if v, ok := flags["--limit"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				return errors.New("--limit is 1-1000")
			}
			limit = n
		}
		list, err := store.Sanctions(ctx, flags["--active"] != "", limit)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(out, "no sanctions")
		}
		for _, r := range list {
			who := "anonymous"
			if r.Account != "" {
				who = "account " + short(r.Account)
			}
			fmt.Fprintf(out, "%s  %-7s %-12s rung %d  %s  [%s]  %s, from %s to %s\n", r.ID, r.State, r.Category, r.Rung, who, board.FormatSanctionSubjects(r.Subjects),
				orDash(r.MessageID), sanctionTime(r.CreatedAt), sanctionTime(r.EndsAt))
			fmt.Fprintf(out, "    reason: %s\n", r.Reason)
			if r.Penalty != "" {
				fmt.Fprintf(out, "    trust penalty: %s\n", r.Penalty)
			}
			if r.State == "lifted" {
				fmt.Fprintf(out, "    lifted %s: %s\n", sanctionTime(r.LiftedAt), r.LiftReason)
			}
		}
	case "ladder":
		fmt.Fprintf(out, "repeat window %d days (same account, exact address, or network and client)\n", board.SanctionRepeatDays)
		for _, r := range board.SanctionLadder {
			if r.NoSanction != "" {
				fmt.Fprintf(out, "  %-13s no sanction: %s\n", r.Category, r.NoSanction)
				continue
			}
			widen := "repeat: network with the client"
			if r.WidenNet {
				widen = "repeat: the /24 when 2+ of its addresses offended, else network with the client"
			}
			fmt.Fprintf(out, "  %-13s %d / %d / %d days; first: account and exact address; %s\n", r.Category, r.Days[0], r.Days[1], r.Days[2], widen)
		}
	}
	return nil
}

// operatorModerate is "moderate EVENT_ID hide|restore REASON [--sanction
// CATEGORY]": the operator's hide or restore and, with --sanction on a hide,
// the ladder's sanction for its author in the same call (the operator
// screener's one step).
func operatorModerate(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	usage := errors.New("usage: swarmmemo moderate EVENT_ID hide/restore REASON [--sanction CATEGORY]")
	category := ""
	if len(args) == 5 && args[3] == "--sanction" {
		category, args = args[4], args[:3]
	}
	if len(args) != 3 || (args[1] != "hide" && args[1] != "restore") || (category != "" && args[1] != "hide") {
		return usage
	}
	if err := store.Moderate(ctx, args[0], args[2], args[1] == "hide"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Moderation recorded for", args[0])
	if category == "" {
		return nil
	}
	o, err := store.ApplySanction(ctx, args[0], category, args[2], board.SanctionOperator)
	if err != nil {
		return fmt.Errorf("the post is hidden, but no sanction was applied: %w", err)
	}
	printSanctionOutcome(out, o)
	return nil
}

func printSanctionOutcome(out io.Writer, o board.SanctionOutcome) {
	rung := "manual"
	if o.Rung > 0 {
		rung = "rung " + strconv.Itoa(o.Rung)
	}
	fmt.Fprintf(out, "sanction %s: %s, %s, %d days, until %s\n  covers %s\n", o.ID, o.Category, rung, o.Days, sanctionTime(o.EndsAt), board.FormatSanctionSubjects(o.Subjects))
	switch {
	case o.PenaltyExisting:
		fmt.Fprintf(out, "  trust penalty already filed for this message: %s\n", o.Penalty)
	case o.Penalty != "":
		fmt.Fprintf(out, "  trust penalty filed: %s (pending until the next trust run)\n", o.Penalty)
	}
	for _, n := range o.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
}

func sanctionTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04Z")
}
