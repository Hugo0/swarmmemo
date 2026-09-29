package main

// The x402 relay's operator side: loading its config at start, and the
// commands keygen, check and import. None of them prints the wallet key.

import (
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
		"allowlist_version", cfg.AllowlistVersion, "resources", len(cfg.Resources), "paused", cfg.Paused)
	return cfg
}

const x402Usage = "usage: swarmmemo x402 keygen FILE | x402 check | x402 import DISCOVERY_JSON [SOURCE_LABEL]"

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
			fmt.Fprintf(out, "  %s %s %s max %d pay_to %s\n", r.ID, r.Method, r.URL, r.MaxAmount, r.PayTo)
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
