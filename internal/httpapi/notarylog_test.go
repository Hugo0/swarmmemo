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

// A notary stamp is a leaf of the transparency log: /api/log/proof?notary=HASH
// proves it, with the notary key's leaf as related, on every surface.
func TestNotaryProofRoute(t *testing.T) {
	store, s := anonCallServer(t, 2000)
	get := func(path string) (int, string) {
		w := makeRequest(s, "GET", "https://swarmmemo.com"+path, "", "")
		return w.Code, w.Body.String()
	}
	code, raw := get("/call/notary/stamp?text=" + url.QueryEscape("logged stamp") + "&max_cost=1&request_id=request-id-notary-log-1")
	var stamped map[string]any
	_ = json.Unmarshal([]byte(raw), &stamped)
	hash, _ := dig(stamped, "data", "result", "receipt", "hash").(string)
	if code != 200 || hash == "" || dig(stamped, "data", "result", "log", "proof") != "/api/log/proof?notary="+hash {
		t.Fatalf("stamp: %d %s", code, raw)
	}
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, raw = get("/api/notary/" + hash); code != 200 || !strings.Contains(raw, `"proof":"/api/log/proof?notary=`+hash+`"`) {
		t.Fatalf("receipt read: %d %s", code, raw)
	}
	code, raw = get("/api/log/proof?notary=" + hash)
	var proof board.LogInclusion
	if err := json.Unmarshal([]byte(raw), &proof); err != nil || code != 200 || proof.Leaf.Kind != "notary" || len(proof.Related) != 1 {
		t.Fatalf("proof: %d %s", code, raw)
	}
	text, err := tlog.OpenNote([]byte(proof.Checkpoint.Note), proof.Checkpoint.VerifierKey)
	if err != nil {
		t.Fatal(err)
	}
	body, err := tlog.ParseCheckpoint(text)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []board.LogInclusion{proof, proof.Related[0]} {
		hashes := make([]tlog.Hash, len(p.Proof))
		for i, h := range p.Proof {
			if hashes[i], err = tlog.ParseHash(h); err != nil {
				t.Fatal(err)
			}
		}
		if err = tlog.VerifyInclusion(p.Leaf.Index, body.Size, tlog.LeafHash([]byte(p.Leaf.Data)), hashes, body.Root); err != nil {
			t.Fatalf("leaf %d: %v", p.Leaf.Index, err)
		}
	}
	if !strings.Contains(proof.Leaf.Data, `"hash":"`+hash+`"`) || !strings.Contains(proof.Related[0].Leaf.Data, `"op":"notary.key"`) {
		t.Fatalf("leaves: %s / %s", proof.Leaf.Data, proof.Related[0].Leaf.Data)
	}
	if code, raw = get("/api/log/proof?notary=key"); code != 200 || !strings.Contains(raw, `notary.key`) {
		t.Fatalf("key proof: %d %s", code, raw)
	}
	if code, raw = get("/api/notary/key"); code != 200 || !strings.Contains(raw, `"proof":"/api/log/proof?notary=key"`) {
		t.Fatalf("key read: %d %s", code, raw)
	}
	for path, want := range map[string]int{
		"/api/log/proof?notary=nothex":                        400,
		"/api/log/proof?notary=" + strings.ToUpper(hash):      400,
		"/api/log/proof?notary=" + hash + "&leaf=0":           400,
		"/api/log/proof?notary=" + hash + "&message=abc":      400,
		"/api/log/proof?notary=" + strings.Repeat("0", 64):    404,
		"/api/log/proof?notary=" + hash + "&size=99999":       404,
		"/api/log/proof?notary=" + hash + "&size=not-a-count": 400,
	} {
		if code, raw = get(path); code != want {
			t.Errorf("%s: %d, want %d: %s", path, code, want, raw)
		}
	}
	// The published verifier checks all of it end to end.
	if python, err := exec.LookPath("python3"); err == nil && exec.Command(python, "-c", "import cryptography").Run() == nil {
		srv := httptest.NewServer(s)
		defer srv.Close()
		out, err := exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"), "--base", srv.URL, "notary", hash).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "OK receipt signature verifies under logged key") {
			t.Fatalf("verify_log.py notary: %v\n%s", err, out)
		}
	}
	// MCP log_proof takes notary too.
	_, res, err := s.mcpLogProof(context.Background(), logProofInput{Notary: hash})
	if err != nil || !res.OK {
		t.Fatalf("log_proof notary: %v %+v", err, res)
	}
	if _, _, err = s.mcpLogProof(context.Background(), logProofInput{Notary: hash, MessageID: "x"}); err == nil {
		t.Fatal("log_proof took notary and message_id together")
	}
}
