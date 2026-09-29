package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestLeversRoute(t *testing.T) {
	store, s := endorsementServer(t, board.Features{})
	w := get(s, "/api/levers", "")
	var body struct {
		OK   bool              `json:"ok"`
		Data board.LeverReport `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || !body.OK || len(body.Data.Pulled) != 0 || len(body.Data.Names) != len(board.LeverNames) {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	// Nothing pulled: /capabilities has no levers key.
	if w := get(s, "/capabilities", "application/json"); strings.Contains(w.Body.String(), `"levers"`) {
		t.Fatal("levers in capabilities with nothing pulled")
	}
	if _, err := store.PullLever(context.Background(), board.LeverPull{Name: board.LeverBlockPrefix, Args: []string{"198.51.100.0/24"}, Reason: "flood from one network"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PullLever(context.Background(), board.LeverPull{Name: board.LeverSignedOnly, Reason: "anonymous flood"}); err != nil {
		t.Fatal(err)
	}
	w = get(s, "/api/levers", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || strings.Join(body.Data.Pulled, ",") != "block-prefix,signed-only" || len(body.Data.Log) != 2 || strings.Contains(w.Body.String(), "198.51.100") {
		t.Fatalf("pulled: %d %s", w.Code, w.Body)
	}
	var caps map[string]any
	if w := get(s, "/capabilities", "application/json"); json.Unmarshal(w.Body.Bytes(), &caps) != nil || caps["levers"] == nil {
		t.Fatalf("capabilities: %s", w.Body)
	}
	// The refusal an agent meets points at the public log.
	req := httptest.NewRequest("POST", "/v1/command", strings.NewReader(`{"operation":"post","room":"lobby","text":"hi"}`))
	req.RemoteAddr = "192.0.2.8:12345"
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "signed_only") || !strings.Contains(w.Body.String(), "/api/levers") {
		t.Fatalf("anonymous post: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"/api/levers?x=1"} {
		if w := get(s, bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/levers", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}
}
