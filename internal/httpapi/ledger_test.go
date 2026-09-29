package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func ledgerServer(t *testing.T, mode board.LedgerMode) *Server {
	t.Helper()
	if os.Getenv("SWARMMEMO_TEST_ALLOWANCE_LEDGER") != "" {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	features := board.Features{Ledger: mode}
	store, err := board.Open(filepath.Join(t.TempDir(), "ledger.db"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store, nil, Config{ServiceID: "swarmmemo.com", Features: features})
}

func TestLedgerRoutesFallThroughWhileOff(t *testing.T) {
	s := ledgerServer(t, board.LedgerOff)
	for _, path := range []string{"/api/allowance", "/api/ledger", "/api/params/allowance", "/api/params/allowance/0"} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code == 200 {
			t.Fatalf("%s answered with the ledger off: %s", path, w.Body.String())
		}
	}
	if _, ok := s.capabilities()["allowance"]; ok {
		t.Fatal("an allowance capability with the ledger off")
	}
}

func TestLedgerRoutes(t *testing.T) {
	s := ledgerServer(t, board.LedgerOn)
	get := func(path string, status int) map[string]any {
		t.Helper()
		w := makeRequest(s, "GET", path, "", "")
		var body map[string]any
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return body
	}
	// An anonymous caller reads its own prospective share; a read claims nothing.
	a := get("/api/allowance", 200)["data"].(map[string]any)
	if a["tier_name"] != "anonymous" || a["resources"].([]any)[0].(map[string]any)["prospective"] != true {
		t.Fatalf("allowance %v", a)
	}
	makeRequest(s, "GET", "/w/lobby/main?text=hello&request_id=r1&format=json", "", "")
	entries := get("/api/ledger?limit=5", 200)["data"].(map[string]any)["entries"].([]any)
	for _, e := range entries {
		// Day-level lines (spill, lever) have no account; anonymous subjects
		// never appear.
		if strings.HasPrefix(e.(map[string]any)["account"].(string), "anon:") {
			t.Fatal("an anonymous journal line is public")
		}
	}
	get("/api/ledger?limit=1000", 400)
	params := get("/api/params/allowance", 200)["data"].(map[string]any)
	if params["current"].(map[string]any)["version"] != float64(0) || len(params["versions"].([]any)) != 1 {
		t.Fatalf("params %v", params)
	}
	if body := get("/api/params/allowance/0", 200)["data"].(map[string]any); body["body"].(map[string]any)["schema"] != float64(1) {
		t.Fatalf("version 0 %v", body)
	}
	get("/api/params/allowance/9", 404)
	get("/api/params/nothing", 404)
	get("/api/params/allowance/x", 400)
	if w := makeRequest(s, "POST", "/api/allowance", "", ""); w.Code != 405 {
		t.Fatalf("POST /api/allowance: %d", w.Code)
	}
	caps, ok := s.capabilities()["allowance"].(map[string]any)
	if !ok || caps["model"] != "waterfall" || caps["ledger"] != "on" || caps["explanation"] != board.WaterfallSentence {
		t.Fatalf("capabilities %v", caps)
	}
}

// /metrics (loopback only) reports the shadow comparisons while the ledger
// is shadow or on, and nothing new while it is off.
func TestLedgerShadowCountsInMetrics(t *testing.T) {
	for _, mode := range []board.LedgerMode{board.LedgerOff, board.LedgerShadow} {
		s := ledgerServer(t, mode)
		makeRequest(s, "GET", "/w/lobby/main?text=hello&request_id=m1&format=json", "", "")
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		body := w.Body.String()
		has := strings.Contains(body, "swarmmemo_allowance_shadow_unexplained_total 0\n")
		if w.Code != 200 || has != (mode == board.LedgerShadow) || mode == board.LedgerShadow && !strings.Contains(body, "swarmmemo_allowance_shadow_agree_total 1\n") {
			t.Fatalf("%v: %d %s", mode, w.Code, body)
		}
	}
}
