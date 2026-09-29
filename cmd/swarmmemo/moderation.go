package main

// The moderation operator commands: the steward's review queue, the decision
// log, alerts, spend and the policy in force. See deploy/RUNBOOK.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/moderation"
)

const moderationUsage = `usage: swarmmemo moderation queue [--surface S] [--state pending|approved|rejected|all] [--limit N]
       swarmmemo moderation show ITEM_ID [--unsafe]
       swarmmemo moderation approve|reject ITEM_ID [--note TEXT]
       swarmmemo moderation log [--surface S] [--subject ID] [--action A] [--limit N]
       swarmmemo moderation stats [--days N] | alerts [--limit N] | spend
       swarmmemo moderation policy | policy check FILE`

// moderationConfig reads the engine's file paths from the environment.
func moderationConfig() board.ModerationConfig {
	return board.ModerationConfig{PolicyFile: os.Getenv("MODERATION_POLICY_FILE"), JevKeyFile: os.Getenv("JEV_KEY_FILE")}
}

// moderationFlags parses "--name value" pairs after the positional arguments.
func moderationFlags(args []string, allowed ...string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 2 && a[:2] == "--" {
			name := a[2:]
			ok := false
			for _, n := range allowed {
				if n == name {
					ok = true
				}
			}
			if !ok {
				return nil, nil, errors.New(moderationUsage)
			}
			if name == "unsafe" {
				flags[name] = "true"
				continue
			}
			if i+1 >= len(args) {
				return nil, nil, errors.New(moderationUsage)
			}
			flags[name] = args[i+1]
			i++
			continue
		}
		rest = append(rest, a)
	}
	return flags, rest, nil
}

func flagInt(flags map[string]string, name string, def int) (int, error) {
	v, ok := flags[name]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("--%s must be a positive whole number", name)
	}
	return n, nil
}

func operatorModeration(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(moderationUsage)
	}
	if args[0] == "policy" && len(args) == 3 && args[1] == "check" {
		body, err := os.ReadFile(args[2])
		if err != nil {
			return err
		}
		p, err := moderation.ParsePolicy(body)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Policy version %d is valid (sha256 %s).\n", p.Version, p.SHA256)
		return nil
	}
	e := store.Moderation()
	if e == nil {
		return errors.New("moderation is off; set MODERATION=true (and MODERATION_POLICY_FILE, JEV_KEY_FILE) as the service does")
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	switch args[0] {
	case "queue":
		flags, rest, err := moderationFlags(args[1:], "surface", "state", "limit")
		if err != nil || len(rest) != 0 {
			return errors.New(moderationUsage)
		}
		limit, err := flagInt(flags, "limit", 50)
		if err != nil {
			return err
		}
		items, err := e.Queue(ctx, moderation.QueueQuery{Surface: moderation.Surface(flags["surface"]), State: flags["state"], Limit: limit})
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		fmt.Fprintf(out, "%-32s  %6s  %-16s  %-6s  %-6s  %-20s  %5s  %s\n", "item", "age", "surface", "cause", "action", "category", "p", "subject")
		for _, it := range items {
			d := it.Decision
			fmt.Fprintf(out, "%-32s  %6s  %-16s  %-6s  %-6s  %-20s  %5.2f  %s\n", it.ID, age(now-it.CreatedAt), d.Surface, it.Cause, d.Action, d.Category, d.P, d.Subject)
		}
		fmt.Fprintf(out, "%d item(s). Content is not shown here; `swarmmemo moderation show ITEM_ID` prints one item.\n", len(items))
	case "show":
		flags, rest, err := moderationFlags(args[1:], "unsafe")
		if err != nil || len(rest) != 1 {
			return errors.New(moderationUsage)
		}
		it, content, err := e.Item(ctx, rest[0])
		if err != nil {
			return err
		}
		// Content is untrusted: it is printed JSON-escaped, and a suspected
		// prompt injection is withheld unless asked for explicitly.
		if p := it.Decision.Scores["injection"]; p >= 0.6 && flags["unsafe"] != "true" {
			content = fmt.Sprintf("[withheld: suspected prompt injection, p=%.2f; --unsafe prints it]", p)
		}
		return enc.Encode(map[string]any{"item": it, "content_untrusted": content})
	case "approve", "reject":
		flags, rest, err := moderationFlags(args[1:], "note")
		if err != nil || len(rest) != 1 {
			return errors.New(moderationUsage)
		}
		resolve := e.Approve
		if args[0] == "reject" {
			resolve = e.Reject
		}
		it, err := resolve(ctx, rest[0], "operator", flags["note"])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Queue item %s %s (%s %s on %s).\n", it.ID, it.State, it.Decision.Action, it.Decision.Subject, it.Decision.Surface)
	case "log":
		flags, rest, err := moderationFlags(args[1:], "surface", "subject", "action", "limit")
		if err != nil || len(rest) != 0 {
			return errors.New(moderationUsage)
		}
		limit, err := flagInt(flags, "limit", 50)
		if err != nil {
			return err
		}
		ds, err := e.Log(ctx, moderation.LogQuery{Surface: moderation.Surface(flags["surface"]), Subject: flags["subject"], Action: moderation.Action(flags["action"]), Limit: limit})
		if err != nil {
			return err
		}
		for _, d := range ds {
			fmt.Fprintf(out, "%s  %-16s  %-6s  %-20s  %5.2f  v%-4d  %-24s  %s%s\n", time.Unix(d.CreatedAt, 0).UTC().Format("2006-01-02T15:04:05Z"), d.Surface, d.Action, d.Category, d.P, d.PolicyVersion, d.Model, d.Subject, suffix(d))
		}
	case "stats":
		flags, rest, err := moderationFlags(args[1:], "days")
		if err != nil || len(rest) != 0 {
			return errors.New(moderationUsage)
		}
		days, err := flagInt(flags, "days", 7)
		if err != nil {
			return err
		}
		st, err := e.Stats(ctx, days)
		if err != nil {
			return err
		}
		return enc.Encode(st)
	case "alerts":
		flags, rest, err := moderationFlags(args[1:], "limit")
		if err != nil || len(rest) != 0 {
			return errors.New(moderationUsage)
		}
		limit, err := flagInt(flags, "limit", 50)
		if err != nil {
			return err
		}
		alerts, err := e.Alerts(ctx, limit)
		if err != nil {
			return err
		}
		return enc.Encode(alerts)
	case "spend":
		s, err := e.SpendToday(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(s)
	case "policy":
		if len(args) != 1 {
			return errors.New(moderationUsage)
		}
		p := e.Policy(ctx)
		fmt.Fprintf(out, "Policy version %d from %s (sha256 %s).\n", p.Version, p.Source, p.SHA256)
		_, err := out.Write(p.Canonical())
		return err
	default:
		return errors.New(moderationUsage)
	}
	return nil
}

func suffix(d moderation.Decision) string {
	s := ""
	if d.Burst {
		s += " burst"
	}
	if d.Degraded != "" {
		s += " degraded=" + d.Degraded
	}
	return s
}

func age(seconds int64) string {
	switch {
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	}
	return fmt.Sprintf("%dd", seconds/86400)
}
