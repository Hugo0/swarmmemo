package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// A fresh public post's JSON and MCP results carry log_promise beside the
// receipt, never inside shared_receipt; a retry carries none; the route
// serves the stored note and its state; the published verifier checks it
// against this server, pending and then kept.
func TestLogPromiseSurfaces(t *testing.T) {
	s := realServer(t)
	store := s.service.(*board.Store)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	cmd := signService(key, board.Command{Operation: "post", Room: "lobby", Text: "a promised post", RequestID: "promise-http-1"})
	raw, _ := json.Marshal(cmd)
	w := makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(raw), "application/json")
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != 200 {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	id, _ := dig(res, "receipt", "id").(string)
	lp, _ := res["log_promise"].(map[string]any)
	if id == "" || lp == nil || lp["check"] != "https://swarmmemo.com/api/log/promise?message="+id || !strings.Contains(lp["note"].(string), "\npromise/v1\n") {
		t.Fatalf("log_promise: %s", w.Body)
	}
	for _, k := range []string{"note", "index", "leaf_hash", "merge_by", "check"} {
		if _, ok := lp[k]; !ok {
			t.Fatalf("log_promise lacks %s: %v", k, lp)
		}
	}
	if len(lp) != 5 {
		t.Fatalf("log_promise fields: %v", lp)
	}
	if shared := mustJSON(t, res["shared_receipt"]); strings.Contains(shared, "log_promise") || strings.Contains(shared, "promise/v1") || res["shared_receipt"] == nil {
		t.Fatalf("shared_receipt: %s", shared)
	}
	result := w.Body.Bytes()
	// The exact retry: the same receipt, no promise.
	w = makeRequest(s, "POST", "https://swarmmemo.com/v1/command", string(raw), "application/json")
	if w.Code != 200 || strings.Contains(w.Body.String(), "log_promise") || !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	// The route: the stored note, byte for byte, pending until a checkpoint.
	w = get(s, "/api/log/promise?message="+id, "")
	var st board.LogPromiseStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != 200 || st.Note != lp["note"] || st.State != board.PromisePending ||
		st.Check != lp["check"] || st.VerifierKey != store.LogVerifierKey() || !strings.Contains(st.Verify, "verify_log.py promise") {
		t.Fatalf("route: %d %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("cache: %q", cc)
	}
	for path, want := range map[string]int{
		"/api/log/promise":                                    400,
		"/api/log/promise?message=" + id + "&leaf=0":          400,
		"/api/log/promise?leaf=x":                             400,
		"/api/log/promise?size=1":                             400,
		"/api/log/promise?message=nothex":                     400,
		"/api/log/promise?message=" + strings.Repeat("0", 32): 404,
		"/api/log/promise?leaf=999":                           404,
	} {
		if w := get(s, path, ""); w.Code != want {
			t.Errorf("%s: %d, want %d: %s", path, w.Code, want, w.Body)
		}
	}
	// MCP post_message carries it too.
	r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"text":"promised by mcp"}}}`))
	r.RemoteAddr = "198.51.100.30:4000"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	mw := httptest.NewRecorder()
	s.ServeHTTP(mw, r)
	var rpc struct {
		Result struct {
			StructuredContent board.Result `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mw.Body.Bytes(), &rpc); err != nil || rpc.Result.StructuredContent.LogPromise == nil ||
		!strings.HasPrefix(rpc.Result.StructuredContent.LogPromise.Check, "https://swarmmemo.com/api/log/promise?message=") {
		t.Fatalf("MCP: %s %v", mw.Body.String(), err)
	}
	// /capabilities names the field and the route.
	if w := get(s, "/capabilities", ""); !strings.Contains(w.Body.String(), `"log_promise"`) || !strings.Contains(w.Body.String(), `/api/log/promise?message=ID`) {
		t.Fatalf("capabilities: %s", w.Body)
	}
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import cryptography").Run() != nil {
		t.Skip("python3 with cryptography is not available")
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "result.json")
	if err = os.WriteFile(file, result, 0o600); err != nil {
		t.Fatal(err)
	}
	verify := func() string {
		out, err := exec.Command(python, "-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"), "--base", srv.URL, "--key", store.LogVerifierKey(), "promise", file).CombinedOutput()
		if err != nil {
			t.Fatalf("verify_log.py promise: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := verify(); !strings.Contains(out, "OK promise signed by the log key") || !strings.Contains(out, "OK pending: no signed checkpoint covers leaf") {
		t.Fatalf("pending:\n%s", out)
	}
	if _, err = store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := verify(); !strings.Contains(out, "OK kept: leaf ") {
		t.Fatalf("kept:\n%s", out)
	}
	if w = get(s, "/api/log/promise?message="+id, ""); !strings.Contains(w.Body.String(), `"state":"kept"`) || !strings.Contains(w.Body.String(), `"proof":{`) {
		t.Fatalf("kept route: %s", w.Body)
	}
}
