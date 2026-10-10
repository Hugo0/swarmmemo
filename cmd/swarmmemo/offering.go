package main

// Agent offerings' operator side (board/offerings.go, RFC 0017): loading the
// facilitator at start, and resolving a call whose settlement is unknown.
// None of it holds or needs a key that moves money: the facilitator settles
// each call to its provider.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

const offeringUsage = "usage: swarmmemo offering unknown | offering resolve ID paid TXHASH | offering resolve ID fail"

// offeringsFromEnvironment is agent offerings' facilitator: OFFERINGS=on
// with the TOPUP_CONFIG file's network, asset and facilitator (its pay_to
// and top-up limits are not used: a call pays the provider). The ledger is
// not needed. Off, or a config that does not load, leaves offerings off and
// the log says why.
func offeringsFromEnvironment() *services.TopupConfig {
	if os.Getenv("OFFERINGS") != "on" {
		return nil
	}
	cfg, err := services.LoadTopupConfig(os.Getenv("TOPUP_CONFIG"))
	if err != nil {
		slog.Error("Agent offerings stay off", "reason", err.Error())
		return nil
	}
	slog.Info("Agent offerings configured", "network", cfg.Network, "facilitator", cfg.FacilitatorURL,
		"max_price", services.FormatUSDC(cfg.OfferingMaxPrice), "max_timeout_seconds", cfg.OfferingMaxTimeout)
	return cfg
}

// operatorOffering is swarmmemo offering unknown and offering resolve: the
// operator checks an unknown call's payer and authorization nonce on chain,
// then marks it paid with the transaction found, or failed.
func operatorOffering(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "unknown":
		list, err := store.UnknownOfferingCalls(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(list)
	case len(args) == 4 && args[0] == "resolve" && args[2] == "paid", len(args) == 3 && args[0] == "resolve" && args[2] == "fail":
		tx := ""
		if len(args) == 4 {
			tx = args[3]
		}
		v, err := store.ResolveOffering(ctx, args[1], args[2], tx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(v)
	}
	return errors.New(offeringUsage)
}
