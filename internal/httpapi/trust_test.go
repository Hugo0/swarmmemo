package httpapi

import (
	"bytes"
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
	"swarmmemo/internal/trust"
)

// With TRUST off the trust routes fall through to today's answers and
// /capabilities has no trust key.
func TestTrustRoutesOffAreUnchanged(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	for path, code := range map[string]int{"/api/trust/runs": 404, "/api/trust/runs/1": 404, "/api/trust/evidence": 404, "/api/agent/abc/trust": 400} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	if w := makeRequest(s, "GET", "/capabilities", "", ""); strings.Contains(w.Body.String(), `"trust":{`) {
		t.Fatal("trust capability with TRUST off")
	}
}

func TestTrustRoutesShadow(t *testing.T) {
	features := board.Features{Trust: board.TrustShadow}
	store, err := board.Open(filepath.Join(t.TempDir(), "trust.db"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com", Features: features})
	seed := make([]byte, 32)
	seed[0] = 42
	key := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	fingerprint := hex.EncodeToString(sum[:])
	if w := tlsCommand(s, signedLinkCommand(key, "agent.register", "", 0)); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	if _, err = store.RunTrust(t.Context()); err != nil {
		t.Fatal(err)
	}

	w := makeRequest(s, "GET", "/api/agent/"+fingerprint+"/trust", "", "")
	var answer struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != 200 || !answer.OK {
		t.Fatalf("trust answer: %d %s", w.Code, w.Body.String())
	}
	for _, key := range []string{"collateral", "proofs", "endorsements", "liability", "sponsor", "breaker", "tier", "caveats", "params_version", "as_of", "run", "mode"} {
		if _, ok := answer.Data[key]; !ok {
			t.Errorf("answer lacks %s", key)
		}
	}
	if answer.Data["boolean"] != false || answer.Data["mode"] != "shadow" || answer.Data["agent"] != fingerprint {
		t.Fatalf("answer: %+v", answer.Data)
	}
	// The same answer through the command endpoint (parity).
	cmd := makeRequest(s, "POST", "/v1/command", `{"operation":"trust.get","target":"`+fingerprint+`"}`, "application/json")
	var viaCommand struct {
		Data map[string]any `json:"data"`
	}
	if err = json.Unmarshal(cmd.Body.Bytes(), &viaCommand); err != nil || viaCommand.Data["run"] != answer.Data["run"] {
		t.Fatalf("trust.get via /v1/command: %d %s", cmd.Code, cmd.Body.String())
	}

	w = makeRequest(s, "GET", "/api/trust/runs", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"output_sha256":"`) || !strings.Contains(w.Body.String(), `"state":"done"`) {
		t.Fatalf("runs: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/api/trust/runs/1", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"capture_bound":{`) || !strings.Contains(w.Body.String(), `"seeds_a":[`) {
		t.Fatalf("run 1: %d %s", w.Code, w.Body.String())
	}
	if w = makeRequest(s, "GET", "/api/trust/runs/2", "", ""); w.Code != 404 {
		t.Fatalf("missing run: %d %s", w.Code, w.Body.String())
	}
	// The run's input snapshot is published, and recomputing it gives the
	// run's output_sha256: in Go, and with the independent recompute.py.
	var detail struct {
		Data struct {
			OutputSHA256 string `json:"output_sha256"`
			Snapshot     struct {
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
				Bytes  int64  `json:"bytes"`
			} `json:"snapshot"`
		} `json:"data"`
	}
	if err = json.Unmarshal(makeRequest(s, "GET", "/api/trust/runs/1", "", "").Body.Bytes(), &detail); err != nil || detail.Data.Snapshot.URL != "/api/trust/runs/1/snapshot" {
		t.Fatalf("run 1 snapshot field: %+v %v", detail, err)
	}
	w = makeRequest(s, "GET", detail.Data.Snapshot.URL, "", "")
	body := w.Body.Bytes()
	bodySum := sha256.Sum256(body)
	if w.Code != 200 || w.Header().Get("X-Snapshot-SHA256") != detail.Data.Snapshot.SHA256 || hex.EncodeToString(bodySum[:]) != detail.Data.Snapshot.SHA256 ||
		int64(len(body)) != detail.Data.Snapshot.Bytes || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("snapshot: %d %v %d bytes", w.Code, w.Header(), len(body))
	}
	snap, err := trust.ReadJSONL(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := trust.Compute(t.Context(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if outSum := sha256.Sum256(out.Canonical()); hex.EncodeToString(outSum[:]) != detail.Data.OutputSHA256 {
		t.Fatal("recomputing the published snapshot does not give output_sha256")
	}
	if python, err := exec.LookPath("python3"); err == nil {
		file := filepath.Join(t.TempDir(), "run.jsonl")
		if err = os.WriteFile(file, body, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := exec.Command(python, "-B", filepath.Join("..", "..", "scripts", "trust", "recompute.py"), "run", file).Output()
		if pySum := sha256.Sum256(got); err != nil || hex.EncodeToString(pySum[:]) != detail.Data.OutputSHA256 {
			t.Fatalf("recompute.py run on the published snapshot: %v", err)
		}
	}
	for path, code := range map[string]int{"/api/trust/runs/2/snapshot": 404, "/api/trust/runs/abc/snapshot": 400, "/api/trust/runs/1/snapshot?x=1": 400} {
		if w = makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	if w = makeRequest(s, "GET", "/api/trust/evidence", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"evidence":[]`) {
		t.Fatalf("evidence: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/trust/runs?limit=0", "/api/trust/runs?limit=51", "/api/trust/runs?before=x", "/api/trust/runs?cursor=1", "/api/trust/runs/abc", "/api/agent/a/b/trust"} {
		if w = makeRequest(s, "GET", path, "", ""); w.Code != 400 {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w = makeRequest(s, "POST", "/api/trust/runs", "", ""); w.Code != 405 {
		t.Errorf("POST runs: %d", w.Code)
	}
	w = makeRequest(s, "GET", "/capabilities", "", "")
	var caps struct {
		Trust map[string]any `json:"trust"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &caps); err != nil || caps.Trust["mode"] != "shadow" || caps.Trust["boolean"] != false || caps.Trust["ledger_effects"] != false {
		t.Fatalf("capabilities trust: %+v", caps.Trust)
	}
}

func TestTrustOpenAPIDiscovery(t *testing.T) {
	for _, mode := range []board.TrustMode{board.TrustOff, board.TrustShadow} {
		s := New(&fakeService{}, nil, Config{Features: board.Features{Trust: mode}})
		var spec map[string]any
		if err := json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &spec); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/trust/runs", "/api/trust/runs/{id}", "/api/trust/runs/{id}/snapshot", "/api/trust/evidence", "/api/agent/{agent}/trust"} {
			if got := dig(spec, "paths", path, "get"); (got != nil) != (mode != board.TrustOff) {
				t.Errorf("mode %v path %s: %v", mode, path, got)
			}
		}
		if mode != board.TrustOff {
			summary := dig(spec, "paths", "/api/trust/runs/{id}", "get", "summary").(string)
			for _, field := range []string{"inputs", "capture_bound", "snapshot"} {
				if !strings.Contains(summary, field) {
					t.Errorf("run detail omits %s", field)
				}
			}
		}
	}
}
