package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/tlog"
)

// TestLogKeysRoute (C983): /api/log/keys serves the genesis key with its
// key_id, the checkpoint, its note (X-Log-Key-ID, body unchanged) and the
// anchors name the same key, and capabilities route to it.
func TestLogKeysRoute(t *testing.T) {
	s := realServer(t)
	store := s.service.(*board.Store)
	if w := get(s, "/api/log/keys", ""); w.Code != 503 || !strings.Contains(w.Body.String(), "no_checkpoint") {
		t.Fatalf("before any checkpoint: %d %s", w.Code, w.Body)
	}
	get(s, "/w/lobby/main?format=json&text="+url.QueryEscape("hello keys"), "application/json")
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cp struct {
		Checkpoint board.LogCheckpoint
		KeysURL    string `json:"keys_url"`
	}
	if w := get(s, "/api/log/checkpoint", ""); json.Unmarshal(w.Body.Bytes(), &cp) != nil || cp.KeysURL != "/api/log/keys" {
		t.Fatalf("checkpoint: %s", w.Body)
	}
	id, err := tlog.VerifierKeyID(cp.Checkpoint.VerifierKey)
	if err != nil || cp.Checkpoint.KeyID != id || len(id) != 64 {
		t.Fatalf("checkpoint key_id %q, want %q (%v)", cp.Checkpoint.KeyID, id, err)
	}
	note := get(s, "/api/log/checkpoint/note", "")
	if note.Header().Get("X-Log-Key-ID") != id || note.Body.String() != cp.Checkpoint.Note {
		t.Fatalf("note: %v %q", note.Header(), note.Body)
	}
	w := get(s, "/api/log/keys", "")
	var keys struct {
		Keys    []board.LogKey      `json:"keys"`
		Current string              `json:"current_key_id"`
		Pins    []map[string]string `json:"pins"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &keys); err != nil || w.Code != 200 {
		t.Fatalf("keys: %d %s", w.Code, w.Body)
	}
	if len(keys.Keys) != 1 || keys.Keys[0].KeyID != id || keys.Keys[0].VerifierKey != cp.Checkpoint.VerifierKey || keys.Current != id ||
		keys.Keys[0].FirstSize != cp.Checkpoint.Size || keys.Keys[0].FirstAnchor != nil || len(keys.Pins) != 2 || !strings.HasSuffix(keys.Pins[0]["url"], "/clients/python/log_keys.json") {
		t.Fatalf("keys: %s", w.Body)
	}
	for _, field := range []string{`"retired_size":null`, `"rotation_note":null`, `"first_anchor":null`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Errorf("keys lack %s: %s", field, w.Body)
		}
	}
	if w = get(s, "/api/log/keys?size=1", ""); w.Code != 400 {
		t.Fatalf("a query on /api/log/keys: %d", w.Code)
	}
	if w = get(s, "/api/log/anchors", ""); !strings.Contains(w.Body.String(), `"keys_url":"/api/log/keys"`) {
		t.Fatalf("anchors: %s", w.Body)
	}
	caps := get(s, capabilitiesFullPath, "").Body.String()
	if !strings.Contains(caps, `"key_id":"`+id+`"`) || !strings.Contains(caps, `"keys":"/api/log/keys"`) {
		t.Fatal("capabilities lack key_id or the keys route")
	}
	if w = get(s, "/api/log/nothing", ""); !strings.Contains(w.Body.String(), "/api/log/keys") {
		t.Fatalf("the 404 route list lacks /api/log/keys: %s", w.Body)
	}
	// verify_log.py keys walks the served history, pinned by key_id.
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import cryptography").Run() != nil {
		t.Skip("python3 with cryptography is not available")
	}
	ts := httptest.NewServer(s)
	defer ts.Close()
	out, err := exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"), "--base", ts.URL, "--key", id, "keys").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK current key "+id) {
		t.Fatalf("verify_log.py keys: %v\n%s", err, out)
	}
	if out, err = exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"), "--base", ts.URL, "--key", strings.Repeat("0", 64), "keys").CombinedOutput(); err == nil {
		t.Fatalf("verify_log.py keys accepted another key_id:\n%s", out)
	}
}
