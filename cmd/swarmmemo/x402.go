package main

// The x402 relay's operator side: loading its config at start, and the
// commands keygen, check and import. None of them prints the wallet key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// x402FromEnvironment loads X402_CONFIG when SERVICES names x402. A config
// that is missing or invalid leaves the relay unconfigured (every call is
// service_unavailable) and says why in the log; the board still starts.
func x402FromEnvironment(features board.Features) *services.X402Config {
	if !features.ServiceEnabled("x402") {
		return nil
	}
	cfg, err := services.LoadX402Config(os.Getenv("X402_CONFIG"))
	if err != nil {
		slog.Error("x402 relay stays off", "reason", err.Error())
		return nil
	}
	slog.Info("x402 relay configured", "network", cfg.Network, "wallet", cfg.Signer.Address().String(),
		"allowlist_version", cfg.AllowlistVersion, "resources", len(cfg.Resources), "paused", cfg.Paused, "open_catalogue", cfg.Catalogue != nil)
	for _, b := range cfg.Bundlers {
		slog.Info("x402 bundler", "name", b.Name(), "ready", b.Ready())
	}
	return cfg
}

const x402Usage = "usage: swarmmemo x402 keygen FILE | x402 check | x402 import DISCOVERY_JSON [SOURCE_LABEL] | x402 vet ID | x402 unvet ID"

// x402VetCommand reports whether args are x402 vet or unvet, which need the
// database (operator in main.go).
func x402VetCommand(args []string) bool {
	return len(args) > 0 && (args[0] == "vet" || args[0] == "unvet")
}

// operatorX402 is swarmmemo x402 vet ID and x402 unvet ID: an open catalogue
// resource becomes callable once vetted (bound to its URL, method and
// recipient; vetting also clears its automatic deny), and stops being
// callable when unvetted. The relay applies it at its next catalogue load,
// within five minutes; a call already quoted is checked again before it pays.
func operatorX402(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) != 2 || !x402VetCommand(args) {
		return errors.New(x402Usage)
	}
	v, err := store.X402Vet(ctx, args[1], args[0] == "vet")
	if err != nil {
		return err
	}
	if !v.Vetted {
		fmt.Fprintf(out, "unvetted %s: no longer callable (within five minutes; calls already quoted are refused before they pay)\n", v.ID)
		return nil
	}
	fmt.Fprintf(out, "vetted %s: %s %s, pay_to %s, at most %d atomic units a call\n", v.ID, v.Method, v.URL, v.PayTo, v.MaxAmount)
	fmt.Fprintf(out, "upstream summary (unreviewed text): %q\n", v.Summary)
	if v.Cleared > 0 {
		fmt.Fprintf(out, "cleared %d automatic deny entries for its URL and recipient\n", v.Cleared)
	}
	fmt.Fprintln(out, "callable within five minutes (the next catalogue load); a changed URL, method or recipient needs vetting again")
	return nil
}

func x402Command(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(x402Usage)
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return errors.New(x402Usage)
		}
		addr, err := services.WriteX402Key(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "x402 wallet key written to", args[1], "(mode 0600); address", addr.String())
		fmt.Fprintln(out, "Fund this address with USDC on the configured network; keep only a few days' float in it.")
		return nil
	case "check":
		if len(args) != 1 {
			return errors.New(x402Usage)
		}
		cfg, err := services.LoadX402Config(os.Getenv("X402_CONFIG"))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "network %s (chain %d), asset %s %q v%s, %d decimals\n", cfg.Network, cfg.ChainID, cfg.Asset, cfg.AssetName, cfg.AssetVersion, cfg.Decimals)
		fmt.Fprintf(out, "wallet %s\n", cfg.Signer.Address())
		fmt.Fprintf(out, "caps (atomic units): per call %d, per agent per day %d, global per day %d\n", cfg.PerCall, cfg.AgentDaily, cfg.GlobalDaily)
		fmt.Fprintf(out, "paused %v, kill file %q\n", cfg.Paused, cfg.KillFile)
		fmt.Fprintf(out, "allowlist version %d, %d resources\n", cfg.AllowlistVersion, len(cfg.Resources))
		for _, r := range cfg.Resources {
			if r.Bundler != services.X402Bundler {
				fmt.Fprintf(out, "  %s via %s tool %s max %d\n", r.ID, r.Bundler, r.Tool, r.MaxAmount)
				continue
			}
			fmt.Fprintf(out, "  %s %s %s max %d pay_to %s\n", r.ID, r.Method, r.URL, r.MaxAmount, r.PayTo)
		}
		fmt.Fprintf(out, "denylist: %d domains, %d URL prefixes, %d recipients, categories %v\n", len(cfg.Deny.Domains), len(cfg.Deny.URLs), len(cfg.Deny.PayTo), cfg.Deny.Categories)
		if cc := cfg.Catalogue; cc != nil {
			fmt.Fprintf(out, "open catalogue: every %s from %v; at most %d resources at %d each, open %d a day, %d per recipient a day, min %d payers in 30 days\n",
				cc.Refresh, cc.DiscoveryURLs, cc.MaxResources, cc.MaxPrice, cc.OpenDaily, cc.RecipientDaily, cc.MinPayers)
		} else {
			fmt.Fprintln(out, "open catalogue: off (allowlist only)")
		}
		for _, b := range cfg.Bundlers {
			fmt.Fprintf(out, "bundler %s: ready %v (false: its key_file is missing or not one token)\n", b.Name(), b.Ready())
		}
		return nil
	case "import":
		if len(args) != 2 && len(args) != 3 {
			return errors.New(x402Usage)
		}
		cfg, err := services.LoadX402Config(os.Getenv("X402_CONFIG"))
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		label := "bazaar " + time.Now().UTC().Format("2006-01-02")
		if len(args) == 3 {
			label = args[2]
		}
		entries, skipped, err := services.ImportX402Bazaar(raw, cfg, label)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		fmt.Fprintf(os.Stderr, "%d candidates, %d skipped (other network or asset, not https, over the per-call cap). Review every entry before adding it to the allowlist.\n", len(entries), skipped)
		return enc.Encode(map[string]any{"candidates": entries})
	}
	return errors.New(x402Usage)
}
