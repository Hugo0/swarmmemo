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

const signalsUsage = "usage: swarmmemo signals account FINGERPRINT [--days N] | signals message ID [--days N] | signals cluster [--days N] [--min N]"

// operatorSignals is the sybil-ring view over the operator-only write
// signals (board/signals.go, C160): which accounts write from the same
// network with the same client. Read-only. Hashes are shown shortened; they
// are keyed under the signals key and never reveal an address.
func operatorSignals(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(signalsUsage)
	}
	mode, rest := args[0], args[1:]
	var target string
	if mode == "account" || mode == "message" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "--") {
			return errors.New(signalsUsage)
		}
		target, rest = rest[0], rest[1:]
	}
	days, minAccounts := 30, 2
	for len(rest) > 0 {
		if len(rest) < 2 {
			return errors.New(signalsUsage)
		}
		n, err := strconv.Atoi(rest[1])
		if err != nil || n < 1 {
			return fmt.Errorf("%s needs a positive number", rest[0])
		}
		switch {
		case rest[0] == "--days" && n <= board.WriteSignalsRetentionDays:
			days = n
		case rest[0] == "--days":
			return fmt.Errorf("--days is at most %d: older signals are deleted", board.WriteSignalsRetentionDays)
		case rest[0] == "--min" && mode == "cluster":
			minAccounts = n
		default:
			return errors.New(signalsUsage)
		}
		rest = rest[2:]
	}
	fmt.Fprintf(out, "signals key %s; window %d days\n", store.SignalsKeyID(), days)
	switch mode {
	case "account":
		v, err := store.SignalAccount(ctx, target, days)
		if err != nil {
			return err
		}
		printSignalAccount(out, v)
	case "message":
		w, ok, err := store.SignalMessage(ctx, target)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(out, "no write signal for %s (none recorded, or older than %d days)\n", target, board.WriteSignalsRetentionDays)
			return nil
		}
		fmt.Fprintf(out, "write %s %s via %s at %s\n", w.Operation, w.ObjectID, orDash(w.Via), signalTime(w.CreatedAt))
		fmt.Fprintf(out, "  account %s  signer %s\n", orDash(w.Account), orDash(w.Signer))
		fmt.Fprintf(out, "  net %s  address %s  key %s\n", short(w.IPHash24), short(w.IPHashFull), w.HashKey)
		fmt.Fprintf(out, "  user-agent %q\n  referer %q\n  accept-language %q\n  sec-ch-ua %q\n  origin %q\n", w.UserAgent, w.Referer, w.AcceptLanguage, w.SecCHUA, w.Origin)
		if w.Account == "" {
			return nil
		}
		v, err := store.SignalAccount(ctx, w.Account, days)
		if err != nil {
			return err
		}
		fmt.Fprintln(out)
		printSignalAccount(out, v)
	case "cluster":
		clusters, err := store.SignalClusters(ctx, days, minAccounts)
		if err != nil {
			return err
		}
		if len(clusters) == 0 {
			fmt.Fprintf(out, "no network and user agent shared by %d or more signed accounts\n", minAccounts)
			return nil
		}
		for _, c := range clusters {
			note := ""
			if len(c.Accounts) > board.SignalLinkGroupMax {
				note = fmt.Sprintf(" (over %d: shared infrastructure, not linked in trust runs)", board.SignalLinkGroupMax)
			}
			fmt.Fprintf(out, "net %s  %d accounts%s  user-agent %q\n", short(c.IPHash24), len(c.Accounts), note, c.UserAgent)
			printPeers(out, "  ", c.Accounts)
		}
	default:
		return errors.New(signalsUsage)
	}
	return nil
}

func printSignalAccount(out io.Writer, v board.SignalAccountView) {
	name := v.Account
	if v.Handle != "" {
		name += " (" + v.Handle + ")"
	}
	fmt.Fprintf(out, "account %s: %d writes in %d days", name, v.Writes, v.Days)
	if v.OtherKey > 0 {
		fmt.Fprintf(out, " (%d under an earlier signals key, not compared)", v.OtherKey)
	}
	fmt.Fprintln(out)
	for _, fp := range v.Fingerprints {
		fmt.Fprintf(out, "fingerprint net %s  user-agent %q  via %s  %d writes  %s to %s\n", orDash(short(fp.IPHash24)), fp.UserAgent, orDash(strings.Join(fp.Vias, ",")), fp.Writes, signalTime(fp.First), signalTime(fp.Last))
		if len(fp.Shared) == 0 {
			fmt.Fprintln(out, "  no other account")
			continue
		}
		fmt.Fprintf(out, "  shared by %d other %s:\n", len(fp.Shared), accountsWord(len(fp.Shared)))
		printPeers(out, "    ", fp.Shared)
	}
	if len(v.SameAddress) > 0 {
		fmt.Fprintf(out, "same address (any client): %d other %s\n", len(v.SameAddress), accountsWord(len(v.SameAddress)))
		printPeers(out, "  ", v.SameAddress)
	}
	for _, l := range []struct {
		name string
		list []string
	}{{"accept-language", v.Languages}, {"referers", v.Referers}, {"origins", v.Origins}} {
		if len(l.list) > 0 {
			fmt.Fprintf(out, "%s: %s\n", l.name, strings.Join(l.list, " | "))
		}
	}
}

func printPeers(out io.Writer, indent string, peers []board.SignalPeer) {
	for _, p := range peers {
		name := p.Account
		if p.Handle != "" {
			name += " (" + p.Handle + ")"
		}
		fmt.Fprintf(out, "%s%s  %d writes  %s to %s\n", indent, name, p.Writes, signalTime(p.First), signalTime(p.Last))
	}
}

func signalTime(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02 15:04") }

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func accountsWord(n int) string {
	if n == 1 {
		return "account"
	}
	return "accounts"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
