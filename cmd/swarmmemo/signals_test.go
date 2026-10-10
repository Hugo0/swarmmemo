package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

func TestOperatorSignals(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	post := func(seed byte, source, ua, text string) (string, string) {
		t.Helper()
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
		pub := key.Public().(ed25519.PublicKey)
		c := board.Command{Operation: "post", Room: "lobby", Text: text, RequestID: text, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Timestamp: time.Now().Unix(), Nonce: text}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		wctx := board.WithRequestSignals(board.WithVia(ctx, "command"), board.RequestSignals{UserAgent: ua, AcceptLanguage: "de"})
		res, err := store.Execute(wctx, c, source)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(pub)
		return hex.EncodeToString(sum[:]), res.Receipt.ID
	}
	ring1, msg := post(41, "203.0.113.4", "ring/1", "one")
	ring2, _ := post(42, "203.0.113.77", "ring/1", "two")
	other, _ := post(43, "198.51.100.1", "ring/1", "three")
	run := func(args string) string {
		t.Helper()
		var out bytes.Buffer
		if err := operatorSignals(ctx, store, strings.Fields(args), &out); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		return out.String()
	}
	cluster := run("cluster --days 7")
	if !strings.Contains(cluster, "2 accounts") || !strings.Contains(cluster, ring1) || !strings.Contains(cluster, ring2) || strings.Contains(cluster, other) {
		t.Fatalf("cluster:\n%s", cluster)
	}
	account := run("account " + ring1)
	if !strings.Contains(account, "shared by 1 other account:") || !strings.Contains(account, ring2) || !strings.Contains(account, "accept-language: de") {
		t.Fatalf("account:\n%s", account)
	}
	t.Logf("signals cluster:\n%s\nsignals account:\n%s", cluster, account)
	message := run("message " + msg + " --days 30")
	if !strings.Contains(message, "write post "+msg+" via command") || !strings.Contains(message, `user-agent "ring/1"`) || !strings.Contains(message, ring2) {
		t.Fatalf("message:\n%s", message)
	}
	if out := run("message nope"); !strings.Contains(out, "no write signal for nope") {
		t.Fatalf("missing: %s", out)
	}
	for _, bad := range []string{"", "account", "account --days 3", "cluster --days 91", "cluster --days 0", "cluster --min", "account x --min 3", "nope"} {
		if err := operatorSignals(ctx, store, strings.Fields(bad), &bytes.Buffer{}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
