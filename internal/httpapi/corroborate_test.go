package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"swarmmemo/internal/board"
)

const corroborateTestAnswer = `{"addresses":["0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"],"score":0.1489,"total_cents":0.41,"independent_roots":0,
"roots":[{"root":"social-account:lens","contribution_cents":0.41,"saturated":false,"adapters":["lens-account"],
"strongest":{"adapter":"lens-account","name":"Lens account (Lens Chain)","evidence_class":"Behavioral","observed_on":"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
"forge_cents":1,"rent_cents":1,"live":true,"age_curve":"Ramp","half_life_days":730,"issued_at":1743762999,"age_days":554,"age_weight":0.4091,"source":"research/protocols/lens-onchain-read.md"}}],
"checks":{"total":18,"held":1,"unavailable":0},"unavailable":[],"caveats":[{"code":"independent-control-not-attested","message":"x"}],
"registry":{"address":"0x977b028b900cce8ee89c46877e814eff3060aa07","chain":"sepolia","chain_id":11155111,"revision":44,"block":11883996,"block_time":1791628836,"sha256":"147b29c2d3787e8b2c1a11accedb0ba2a5ff982942c2d554cca1bc9d52fa6e53"},
"as_of":null,"computed_at":1791628932}`

// corroborateServer is a board with corroborate on, its sidecar a fake that
// answers status and body, counting what it served.
func corroborateServer(t *testing.T, status *atomic.Int64, body *atomic.Value) (*Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if code := int(status.Load()); code != 200 {
			w.Header().Set("Retry-After", "15")
			w.WriteHeader(code)
		}
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	t.Cleanup(sidecar.Close)
	f := board.Features{Services: []string{"corroborate"}, CorroborateURL: sidecar.URL, AnonPrefix: true}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store, nil, Config{Features: f, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", AllowInsecureLocal: true}), &hits
}

func TestCorroborateWire(t *testing.T) {
	var status atomic.Int64
	var body atomic.Value
	status.Store(200)
	body.Store(corroborateTestAnswer)
	s, hits := corroborateServer(t, &status, &body)
	get := func(path string) (int, map[string]any, string) {
		w := makeRequest(s, "GET", "https://swarmmemo.com"+path, "", "")
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out, w.Body.String()
	}

	// Happy path over /call/, without a key: a score, never a verdict.
	code, out, raw := get("/call/corroborate/resolve?addresses=0xd8da6bf26964af9d7eed9e03e53415d37aa96045")
	if code != 200 || dig(out, "data", "result", "total_cents") != 0.41 || dig(out, "data", "result", "registry", "revision") != float64(44) ||
		dig(out, "data", "result", "cached") != false || strings.Contains(raw, `"human"`) {
		t.Fatalf("resolve: %d %s", code, raw)
	}
	// The same set, checksummed: from the cache.
	if code, out, raw = get("/call/corroborate/resolve?addresses=0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"); code != 200 || dig(out, "data", "result", "cached") != true || hits.Load() != 1 {
		t.Fatalf("cached: %d %s hits=%d", code, raw, hits.Load())
	}
	// The MCP tool is the same read.
	result := mustTool(t, s, "/mcp", "", "corroborate_resolve", map[string]any{"addresses": "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"})
	if dig(result, "data", "result", "score") != 0.1489 || dig(result, "data", "result", "registry", "sha256") == nil {
		t.Fatalf("mcp: %v", result)
	}

	// Input validation: a bad checksum, a name, more than 10.
	many := strings.TrimSuffix(strings.Repeat("0x1111111111111111111111111111111111111111,", 1), ",")
	for i := 0; i < 10; i++ {
		many += ",0x00000000000000000000000000000000000000" + string("0123456789"[i]) + "f"
	}
	for _, q := range []string{"addresses=0xd8DA6BF26964aF9D7eEd9e03E53415D37aA96045", "addresses=vitalik.eth", "addresses=" + many, "addresses=0x123", "", "addresses=0xd8da6bf26964af9d7eed9e03e53415d37aa96045&as_of=12"} {
		if code, out, raw = get("/call/corroborate/resolve?" + q); code != 400 || dig(out, "error", "code") != "invalid_service_data" {
			t.Errorf("%s: %d %s", q, code, raw)
		}
	}

	// The sidecar timing out or failing is 503 service_unavailable with
	// retry_after, never a zero score; nothing is cached.
	status.Store(503)
	body.Store(`{"error":"timeout","retry_after":15}`)
	for i := 0; i < 2; i++ {
		w := makeRequest(s, "GET", "https://swarmmemo.com/call/corroborate/resolve?addresses=0x2222222222222222222222222222222222222222", "", "")
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 503 || dig(out, "error", "code") != "service_unavailable" || dig(out, "error", "retry_after") != float64(15) || w.Header().Get("Retry-After") != "15" ||
			strings.Contains(w.Body.String(), "total_cents") {
			t.Fatalf("unavailable: %d %s %v", w.Code, w.Body.String(), w.Header())
		}
	}
	// An answer with nothing read is not a zero either.
	status.Store(200)
	body.Store(strings.Replace(corroborateTestAnswer, `"checks":{"total":18,"held":1,"unavailable":0}`, `"checks":{"total":18,"held":0,"unavailable":18}`, 1))
	if code, out, raw = get("/call/corroborate/resolve?addresses=0x3333333333333333333333333333333333333333"); code != 503 || dig(out, "error", "code") != "service_unavailable" {
		t.Fatalf("nothing read: %d %s", code, raw)
	}

	// Discovery: the catalogue, /capabilities and the tool page say what it is.
	if code, _, raw = get("/api/services"); code != 200 || !strings.Contains(raw, `"id":"corroborate"`) || !strings.Contains(raw, `"free":true`) {
		t.Fatalf("catalogue: %d", code)
	}
	if _, out, _ = get("/capabilities"); dig(out, "services", "corroborate", "free") != true || dig(out, "services", "network") != true {
		t.Fatalf("capabilities: %v", dig(out, "services", "corroborate"))
	}
}

// Unreachable sidecar: 503, with a retry_after.
func TestCorroborateWireSidecarDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	f := board.Features{Services: []string{"corroborate"}, CorroborateURL: url, AnonPrefix: true}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{Features: f, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", AllowInsecureLocal: true})
	w := makeRequest(s, "GET", "https://swarmmemo.com/call/corroborate/resolve?addresses=0xd8da6bf26964af9d7eed9e03e53415d37aa96045", "", "")
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 503 || dig(out, "error", "code") != "service_unavailable" || dig(out, "error", "retry_after") != float64(30) {
		t.Fatalf("down: %d %s", w.Code, w.Body.String())
	}
}
