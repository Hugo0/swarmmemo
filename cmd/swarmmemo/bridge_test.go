package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bridge secret is read only from a private regular file of sane size; a
// group- or world-readable one stops startup rather than being trusted.
func TestBridgeTokenFileMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	token := strings.Repeat("s", 40)
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good", token+"\n", 0o400)
	t.Setenv("BRIDGE_TOKEN_EMAIL_FILE", good)
	if tokens, err := bridgeTokens(); err != nil || tokens["email"] != token {
		t.Fatalf("private token: %v %v", tokens, err)
	}
	for name, path := range map[string]string{
		"group readable": write("group", token, 0o640),
		"world readable": write("world", token, 0o644),
		"too short":      write("short", "short", 0o600),
		"too large":      write("large", strings.Repeat("s", 5000), 0o600),
		"directory":      dir,
		"missing":        filepath.Join(dir, "absent"),
	} {
		t.Setenv("BRIDGE_TOKEN_EMAIL_FILE", path)
		if _, err := bridgeTokens(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Setenv("BRIDGE_TOKEN_EMAIL_FILE", "")
	if tokens, err := bridgeTokens(); err != nil || len(tokens) != 0 {
		t.Fatalf("unset: %v %v", tokens, err)
	}
}
