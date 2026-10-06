package main

// Credit top-ups' operator side (board/topup.go): loading TOPUP_CONFIG at
// start, checking it, and resolving a top-up whose settlement is unknown.
// None of it holds or needs a key: SwarmMemo only receives.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

const topupUsage = "usage: swarmmemo topup check | topup unknown | topup resolve ID credit TXHASH | topup resolve ID fail"

// topupFromEnvironment loads TOPUP_CONFIG when it is set. Top-ups also need
// ALLOWANCE_LEDGER=on; a config that is missing, invalid or disabled leaves
// them off and says why in the log, and the board still starts.
func topupFromEnvironment(features board.Features) *services.TopupConfig {
	path := os.Getenv("TOPUP_CONFIG")
	if path == "" {
		return nil
	}
	cfg, err := services.LoadTopupConfig(path)
	if err != nil {
		slog.Error("Credit top-ups stay off", "reason", err.Error())
		return nil
	}
	if features.Ledger != board.LedgerOn {
		slog.Error("Credit top-ups stay off", "reason", "they need ALLOWANCE_LEDGER=on")
		return nil
	}
	slog.Info("Credit top-ups configured", "network", cfg.Network, "pay_to", cfg.PayTo.String(), "facilitator", cfg.FacilitatorURL,
		"min", cfg.Min, "max", cfg.Max, "account_daily", cfg.AccountDaily)
	return cfg
}

// topupCheck is swarmmemo topup check: the config as loaded.
func topupCheck(out io.Writer) error {
	cfg, err := services.LoadTopupConfig(os.Getenv("TOPUP_CONFIG"))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "network %s, asset %s %q v%s\n", cfg.Network, cfg.Asset, cfg.AssetName, cfg.AssetVersion)
	fmt.Fprintf(out, "pay_to %s (receiving only; no key is loaded)\n", cfg.PayTo)
	fmt.Fprintf(out, "facilitator %s\n", cfg.FacilitatorURL)
	fmt.Fprintf(out, "limits (credits = micro-USDC): min %d (%s USDC), max %d (%s USDC), per account per day %d (%s USDC)\n",
		cfg.Min, services.FormatUSDC(cfg.Min), cfg.Max, services.FormatUSDC(cfg.Max), cfg.AccountDaily, services.FormatUSDC(cfg.AccountDaily))
	return nil
}

// operatorTopup is swarmmemo topup unknown and topup resolve: the operator
// checks an unknown top-up's payer and nonce on chain, then credits it with
// the transaction found, or fails it.
func operatorTopup(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "unknown":
		list, err := store.UnknownTopups(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(list)
	case len(args) == 4 && args[0] == "resolve" && args[2] == "credit", len(args) == 3 && args[0] == "resolve" && args[2] == "fail":
		tx := ""
		if len(args) == 4 {
			tx = args[3]
		}
		v, err := store.ResolveTopup(ctx, args[1], args[2], tx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(v)
	}
	return errors.New(topupUsage)
}
