package services_test

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"swarmmemo/internal/services"
)

// testNotaryKey signs notary and run receipts in the provider tests.
var testNotaryKey = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))

// The notary key lives in its own 0600 file: made once when absent (every
// concurrent first start agrees on it), read back unchanged, and refused when
// others can read it or it does not hold a matching key.
func TestNotaryKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), services.NotaryKeyFileName)
	keys := make([]ed25519.PrivateKey, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := services.LoadOrCreateNotaryKey(path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}()
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !keys[0].Equal(k) || len(k) != ed25519.PrivateKeySize {
			t.Fatal("concurrent first starts made different keys")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", info, err)
	}
	again, err := services.LoadOrCreateNotaryKey(path)
	if err != nil || !again.Equal(keys[0]) {
		t.Fatalf("reload: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = services.LoadOrCreateNotaryKey(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("a readable key file must be refused: %v", err)
	}
	for name, body := range map[string]string{
		"garbage":  "not json",
		"unknown":  strings.Replace(string(raw), `"version"`, `"extra":1,"version"`, 1),
		"mismatch": strings.Replace(string(raw), `"key_id":"`, `"key_id":"0`, 1),
	} {
		bad := filepath.Join(t.TempDir(), name)
		if err = os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err = services.LoadOrCreateNotaryKey(bad); err == nil {
			t.Fatalf("%s key file accepted", name)
		}
		if got, _ := os.ReadFile(bad); string(got) != body {
			t.Fatalf("%s key file was overwritten", name)
		}
	}
	if _, err = services.LoadOrCreateNotaryKey(filepath.Join(t.TempDir(), "missing-dir", "notary.key")); err == nil {
		t.Fatal("a key file in a missing directory must fail, not be skipped")
	}
	if _, err = services.LoadOrCreateNotaryKey(""); err == nil {
		t.Fatal("an empty path must fail")
	}
}
