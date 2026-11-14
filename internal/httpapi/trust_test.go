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
	"reflect"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/trust"
)

// With TRUST off the trust routes fall through to today's answers and
// /capabilities has no trust key.
func TestTrustRoutesOffAreUnchanged(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	for path, code := range map[string]int{"/api/trust/runs": 404, "/api/trust/runs/1": 404, "/api/trust/evidence": 404, "/api/trust/penalties": 404, "/api/trust/standing?agent=abc": 404, "/api/agent/abc/trust": 400} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	if w := makeRequest(s, "GET", "/capabilities", "", ""); strings.Contains(w.Body.String(), `"trust":{`) {
		t.Fatal("trust capability with TRUST off")
	}
}

func TestTrustRoutesOn(t *testing.T) {
	features := board.Features{Trust: board.TrustOn}
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
	// Before any run finishes, latest is a 404 and the list names no
	// latest_run (C962).
	if w := makeRequest(s, "GET", "/api/trust/runs/latest", "", ""); w.Code != 404 || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Fatalf("latest before any run: %d %s", w.Code, w.Body.String())
	}
	if w := makeRequest(s, "GET", "/api/trust/runs", "", ""); w.Code != 200 || strings.Contains(w.Body.String(), "latest_run") {
		t.Fatalf("runs before any run: %d %s", w.Code, w.Body.String())
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
	// The standing breakdown (RFC 0021 §13), schema 2.
	for _, key := range []string{"standing", "world", "world_cents", "own_cents", "received_cents", "passed_cents", "node_cap_cents", "backers", "backers_total",
		"backs", "accuracy", "inactivity", "penalty", "penalties", "burn", "next_steps", "bands", "viewer", "recompute", "caveats"} {
		if _, ok := answer.Data[key]; !ok {
			t.Errorf("answer lacks %s", key)
		}
	}
	for _, gone := range []string{"collateral", "proofs", "endorsements", "liability", "sponsor", "breaker", "tier", "mode"} {
		if _, ok := answer.Data[gone]; ok {
			t.Errorf("answer still has %s", gone)
		}
	}
	if answer.Data["boolean"] != false || answer.Data["schema"] != 2.0 || answer.Data["agent"] != fingerprint || answer.Data["viewer"] != nil {
		t.Fatalf("answer: %+v", answer.Data)
	}
	// The same answer through the command endpoint and /api/trust/standing
	// (parity: one breakdown).
	cmd := makeRequest(s, "POST", "/v1/command", `{"operation":"trust.get","target":"`+fingerprint+`"}`, "application/json")
	var viaCommand, viaStanding struct {
		Data map[string]any `json:"data"`
	}
	if err = json.Unmarshal(cmd.Body.Bytes(), &viaCommand); err != nil || !reflect.DeepEqual(viaCommand.Data, answer.Data) {
		t.Fatalf("trust.get via /v1/command: %d %s", cmd.Code, cmd.Body.String())
	}
	w = makeRequest(s, "GET", "/api/trust/standing?agent="+fingerprint, "", "")
	if err = json.Unmarshal(w.Body.Bytes(), &viaStanding); err != nil || w.Code != 200 || !reflect.DeepEqual(viaStanding.Data, answer.Data) {
		t.Fatalf("/api/trust/standing: %d %s", w.Code, w.Body.String())
	}
	// A viewer adds its own view; without a graph or outside it, it says so.
	w = makeRequest(s, "GET", "/api/trust/standing?agent="+fingerprint+"&viewer="+fingerprint, "", "")
	if err = json.Unmarshal(w.Body.Bytes(), &viaStanding); err != nil || w.Code != 200 {
		t.Fatalf("/api/trust/standing with a viewer: %d %s", w.Code, w.Body.String())
	}
	if viewer, _ := viaStanding.Data["viewer"].(map[string]any); viewer == nil || viewer["account"] == nil || viewer["computed"] == nil {
		t.Fatalf("viewer: %+v", viaStanding.Data["viewer"])
	}
	for path, code := range map[string]int{
		"/api/trust/standing": 400, "/api/trust/standing?agent=": 400, "/api/trust/standing?agent=" + fingerprint + "&agent=" + fingerprint: 400,
		"/api/trust/standing?agent=" + fingerprint + "&x=1": 400, "/api/trust/standing?agent=" + strings.Repeat("0", 64): 404,
		"/api/trust/standing?agent=" + fingerprint + "&viewer=" + strings.Repeat("0", 64): 404,
	} {
		if w = makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d: %s", path, w.Code, code, w.Body.String())
		}
	}

	w = makeRequest(s, "GET", "/api/trust/runs", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"output_sha256":"`) || !strings.Contains(w.Body.String(), `"state":"done"`) {
		t.Fatalf("runs: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/api/trust/runs/1", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"inputs":{`) || !strings.Contains(w.Body.String(), `"output_sha256":"`) {
		t.Fatalf("run 1: %d %s", w.Code, w.Body.String())
	}
	if w = makeRequest(s, "GET", "/api/trust/runs/2", "", ""); w.Code != 404 {
		t.Fatalf("missing run: %d %s", w.Code, w.Body.String())
	}
	// C962: the list names the newest finished run, and latest reads it
	// exactly as its number does (the run and its snapshot).
	if w = makeRequest(s, "GET", "/api/trust/runs", "", ""); !strings.Contains(w.Body.String(), `"latest_run":1`) {
		t.Fatalf("runs lacks latest_run: %s", w.Body.String())
	}
	for _, sub := range []string{"", "/snapshot"} {
		byNumber, latest := makeRequest(s, "GET", "/api/trust/runs/1"+sub, "", ""), makeRequest(s, "GET", "/api/trust/runs/latest"+sub, "", "")
		if latest.Code != 200 || latest.Body.String() != byNumber.Body.String() {
			t.Fatalf("latest%s: %d %s, run 1 is %d", sub, latest.Code, latest.Body.String(), byNumber.Code)
		}
	}
	for _, path := range []string{"/api/trust/runs/abc", "/api/trust/runs/latest?x=1", "/api/trust/runs/0/snapshot", "/api/trust/runs/newest"} {
		if w = makeRequest(s, "GET", path, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "a run number from /api/trust/runs") || !strings.Contains(w.Body.String(), "or latest") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
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
	// The public penalty log: /api/trust/penalties and /api/trust/evidence
	// serve one log (trust.penalties), newest first, paged, by agent.
	if w = makeRequest(s, "GET", "/api/trust/penalties", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Fatalf("penalties: %d %s", w.Code, w.Body.String())
	}
	var ids []string
	for i := range 3 {
		id, err := store.PenalizeTrust(t.Context(), "override", []string{fingerprint}, "test evidence "+strconv.Itoa(i), "a test penalty", "operator")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var page struct {
		Data struct {
			Entries []struct {
				Seq     int64    `json:"seq"`
				ID      string   `json:"id"`
				Trigger string   `json:"trigger"`
				Members []string `json:"members"`
				Reason  string   `json:"reason"`
			} `json:"entries"`
			NextBefore int64 `json:"next_before"`
		} `json:"data"`
	}
	w = makeRequest(s, "GET", "/api/trust/penalties?limit=2", "", "")
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Data.Entries) != 2 || page.Data.Entries[0].ID != ids[2] ||
		page.Data.Entries[0].Trigger != "override" || page.Data.Entries[0].Reason != "a test penalty" || page.Data.NextBefore == 0 {
		t.Fatalf("penalties page 1: %d %s", w.Code, w.Body.String())
	}
	if evidence := makeRequest(s, "GET", "/api/trust/evidence?limit=2", "", ""); evidence.Code != 200 || evidence.Body.String() != w.Body.String() {
		t.Fatalf("/api/trust/evidence is not the penalty log: %d %s", evidence.Code, evidence.Body.String())
	}
	w = makeRequest(s, "GET", "/api/trust/penalties?limit=2&before="+strconv.FormatInt(page.Data.NextBefore, 10)+"&agent="+fingerprint, "", "")
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Data.Entries) != 1 || page.Data.Entries[0].ID != ids[0] || page.Data.NextBefore != 0 {
		t.Fatalf("penalties page 2: %d %s", w.Code, w.Body.String())
	}
	for path, code := range map[string]int{
		"/api/trust/penalties?limit=0": 400, "/api/trust/penalties?limit=101": 400, "/api/trust/penalties?before=x": 400, "/api/trust/penalties?x=1": 400,
		"/api/trust/penalties?agent=": 400, "/api/trust/penalties?agent=" + strings.Repeat("0", 64): 404, "/api/trust/penalties?limit=100": 200,
	} {
		if w = makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d: %s", path, w.Code, code, w.Body.String())
		}
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
	if err = json.Unmarshal(w.Body.Bytes(), &caps); err != nil || caps.Trust["mode"] != "on" || caps.Trust["boolean"] != false || caps.Trust["penalties"] != "/api/trust/penalties" ||
		caps.Trust["standing_url"] != "/api/trust/standing?agent=AGENT&viewer=AGENT" || caps.Trust["run_latest"] != "/api/trust/runs/latest" {
		t.Fatalf("capabilities trust: %+v", caps.Trust)
	}
}

func TestTrustOpenAPIDiscovery(t *testing.T) {
	for _, mode := range []board.TrustMode{board.TrustOff, board.TrustOn} {
		s := New(&fakeService{}, nil, Config{Features: board.Features{Trust: mode}})
		var spec map[string]any
		if err := json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &spec); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/trust/runs", "/api/trust/runs/{id}", "/api/trust/runs/{id}/snapshot", "/api/trust/evidence", "/api/trust/penalties", "/api/trust/standing", "/api/agent/{agent}/trust"} {
			if got := dig(spec, "paths", path, "get"); (got != nil) != (mode != board.TrustOff) {
				t.Errorf("mode %v path %s: %v", mode, path, got)
			}
		}
		if mode != board.TrustOff {
			summary := dig(spec, "paths", "/api/trust/runs/{id}", "get", "summary").(string)
			for _, field := range []string{"inputs", "snapshot", "output_sha256"} {
				if !strings.Contains(summary, field) {
					t.Errorf("run detail omits %s", field)
				}
			}
		}
	}
}
