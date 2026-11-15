package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// C1012: `erase account FP` is a dry run by default (counts, nothing
// changed), erases with --confirm and prints the receipt, refuses bad
// arguments, and `erase log` lists the run without content.
func TestOperatorErase(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{71}, 32))
	pub := key.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	account := hex.EncodeToString(sum[:])
	c := board.Command{Operation: "post", Room: "lobby", Text: "erase me please", RequestID: "x1", PublicKey: base64.RawURLEncoding.EncodeToString(pub), Timestamp: time.Now().Unix(), Nonce: "x1"}
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	res, err := store.Execute(board.WithVia(ctx, "command"), c, "203.0.113.4")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args string) (string, error) {
		var out bytes.Buffer
		err := operatorErase(ctx, store, strings.Fields(args), &out)
		return out.String(), err
	}
	for _, bad := range []string{"", "account", "account " + account + " --force", "account " + account + " --reason", "nope " + account} {
		if _, err := run(bad); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := run("account " + strings.Repeat("0", 64)); err == nil {
		t.Error("an unknown account was erased")
	}
	dry, err := run("account " + account + " --reason req-7")
	if err != nil || !strings.Contains(dry, "Dry run") || !strings.Contains(dry, "hidden as erased by owner") || strings.Contains(dry, "Receipt") {
		t.Fatalf("dry run: %v\n%s", err, dry)
	}
	get := func() string {
		t.Helper()
		r, err := store.Execute(ctx, board.Command{Operation: "message.get", MessageID: res.Receipt.ID}, "203.0.113.5")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(r)
		return string(raw)
	}
	if m := get(); !strings.Contains(m, "erase me please") {
		t.Fatalf("the dry run changed the post: %s", m)
	}
	done, err := run("account " + account + " --reason req-7 --confirm")
	if err != nil || !strings.Contains(done, "Erased account "+account) || !strings.Contains(done, `"raw_signals_erased"`) || !strings.Contains(done, "Receipt (sha256 ") {
		t.Fatalf("erase: %v\n%s", err, done)
	}
	if strings.Contains(done, "erase me please") {
		t.Fatalf("the receipt holds content:\n%s", done)
	}
	if m := get(); strings.Contains(m, "erase me please") || !strings.Contains(m, `"hidden_by":"author"`) || !strings.Contains(m, `"reason":"`+board.ErasedReason+`"`) {
		t.Fatalf("after erase: %s", m)
	}
	log, err := run("log")
	if err != nil || !strings.Contains(log, account) || !strings.Contains(log, "ref req-7") {
		t.Fatalf("log: %v\n%s", err, log)
	}
}
