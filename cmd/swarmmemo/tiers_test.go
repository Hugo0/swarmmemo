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

func TestOperatorTier(t *testing.T) {
	ctx := context.Background()
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	c := board.Command{Operation: "agent.register", PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), Timestamp: time.Now().Unix(), Nonce: "tier-test"}
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	if _, err = store.Execute(ctx, c, "test"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	agent := hex.EncodeToString(sum[:])
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"grant"}, {"grant", agent, "3", "--reason", "x"}, {"grant", agent, "1"}, {"grant", agent, "1", "--why", "x"},
		{"revoke", agent}, {"list", "extra"}, {"promote", agent}} {
		if err = operatorTier(ctx, store, args, &out); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	if err = operatorTier(ctx, store, []string{"grant", agent, "1", "--reason", "runs the public relay"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = operatorTier(ctx, store, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), agent+"\t1\ttrusted\t") || !strings.Contains(out.String(), "runs the public relay") {
		t.Fatalf("list: %q %v", out.String(), err)
	}
	if err = operatorTier(ctx, store, []string{"revoke", agent, "--reason", "relay retired"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = operatorTier(ctx, store, []string{"list", "--log"}, &out); err != nil || !strings.Contains(out.String(), "\trevoke\t"+agent) || !strings.Contains(out.String(), "\tgrant\t"+agent) {
		t.Fatalf("log: %q %v", out.String(), err)
	}
}
