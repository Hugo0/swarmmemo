package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// /api/log/proof?message=ID is self-sufficient: it carries the text and the
// signed bytes, so verify_log.py checks the text against the leaf's
// text_sha256 and the leaf's signature from the saved answer alone.
func TestLogProofCarriesSignedPayload(t *testing.T) {
	s := realServer(t)
	store := s.service.(*board.Store)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	text := "Signed <b>&</b> \"log\" line\nwith / and 🌍"
	cmd := signService(key, board.Command{Operation: "post", Room: "lobby", Text: text, RequestID: "log-payload-1", Data: `{"schema":1,"format":"markdown"}`})
	raw, _ := json.Marshal(cmd)
	w := makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(raw), "application/json")
	var res struct {
		Receipt struct{ ID string } `json:"receipt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 200 || res.Receipt.ID == "" {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	cp, err := store.ReadLogCheckpoint(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	w = get(s, "/api/log/proof?message="+res.Receipt.ID+"&size="+itoa(cp.Size), "")
	var proof board.LogInclusion
	if err = json.Unmarshal(w.Body.Bytes(), &proof); err != nil || w.Code != 200 || proof.Text == nil || *proof.Text != text {
		t.Fatalf("proof: %d %s", w.Code, w.Body)
	}
	if string(board.Canonical("swarmmemo.com", cmd)) != proof.SignedPayload {
		t.Fatalf("signed_payload is not the signed bytes: %s", proof.SignedPayload)
	}
	sum := sha256.Sum256([]byte(text))
	if !strings.Contains(proof.Leaf.Data, `"text_sha256":"`+hex.EncodeToString(sum[:])+`"`) || strings.Contains(proof.Leaf.Data, "Signed") {
		t.Fatalf("leaf: %s", proof.Leaf.Data)
	}
	// It carries text, so a hide must reach it soon: never cached for a day.
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("proof with text cached as %q", cc)
	}
	// MCP log_proof answers the same.
	if _, out, err := s.mcpLogProof(context.Background(), logProofInput{MessageID: res.Receipt.ID}); err != nil || !strings.Contains(mustJSON(t, out), `"signed_payload"`) {
		t.Fatalf("log_proof: %v %s", err, mustJSON(t, out))
	}
	// The published verifier checks all of it from the saved answer, with
	// no server at all.
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import cryptography").Run() != nil {
		t.Skip("python3 with cryptography is not available")
	}
	file := filepath.Join(t.TempDir(), "proof.json")
	if err = os.WriteFile(file, w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"),
		"--base", "http://127.0.0.1:9", "--key", cp.VerifierKey, "message", res.Receipt.ID, "--proof", file).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK text matches the logged SHA-256") || !strings.Contains(string(out), "verifies over signed_payload") {
		t.Fatalf("verify_log.py message --proof: %v\n%s", err, out)
	}
	// A tampered text fails.
	tampered := strings.Replace(w.Body.String(), `"text":"Signed`, `"text":"signed`, 1)
	if err = os.WriteFile(file, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err = exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"),
		"--base", "http://127.0.0.1:9", "--key", cp.VerifierKey, "message", res.Receipt.ID, "--proof", file).CombinedOutput(); err == nil {
		t.Fatalf("a tampered text verified:\n%s", out)
	}
}

// A signed top-level post that names no room is placed in lobby (C140): its
// saved proof verifies with verify_log.py, whose payload check mirrors that
// default and nothing looser.
func TestLogProofRoomlessTopLevel(t *testing.T) {
	s := realServer(t)
	store := s.service.(*board.Store)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	text := "A top-level post with no room"
	cmd := signService(key, board.Command{Operation: "post", Text: text, RequestID: "log-roomless-1"})
	if signed := string(board.Canonical("swarmmemo.com", cmd)); strings.Contains(signed, `"room"`) || strings.Contains(signed, `"page"`) {
		t.Fatalf("the signed payload names a room or page: %s", signed)
	}
	raw, _ := json.Marshal(cmd)
	w := makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(raw), "application/json")
	var res struct {
		Receipt struct{ ID string } `json:"receipt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 200 || res.Receipt.ID == "" {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	cp, err := store.ReadLogCheckpoint(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	w = get(s, "/api/log/proof?message="+res.Receipt.ID+"&size="+itoa(cp.Size), "")
	var proof board.LogInclusion
	if err = json.Unmarshal(w.Body.Bytes(), &proof); err != nil || w.Code != 200 || !strings.Contains(proof.Leaf.Data, `"room":"lobby"`) {
		t.Fatalf("proof: %d %s", w.Code, w.Body)
	}
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import cryptography").Run() != nil {
		t.Skip("python3 with cryptography is not available")
	}
	file := filepath.Join(t.TempDir(), "proof.json")
	if err = os.WriteFile(file, w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"),
		"--base", "http://127.0.0.1:9", "--key", cp.VerifierKey, "message", res.Receipt.ID, "--proof", file).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "verifies over signed_payload") {
		t.Fatalf("verify_log.py message --proof: %v\n%s", err, out)
	}
}
