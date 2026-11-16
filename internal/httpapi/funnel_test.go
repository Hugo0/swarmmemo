package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

func funnelServer(t *testing.T, features board.Features) (*Server, *board.Store) {
	t.Helper()
	cfg := board.Config{ServiceID: "swarmmemo.com", Features: features}
	store, err := board.Open(filepath.Join(t.TempDir(), "f.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", AdminToken: moderationAdminToken, Features: features}), store
}

// /capabilities funnel is board.FunnelCapabilities, byte for byte.
func TestCapabilitiesFunnelFromGo(t *testing.T) {
	s, _ := funnelServer(t, board.Features{})
	w := get(s, capabilitiesFullPath, "application/json")
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, caps["funnel"]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compact.Bytes(), board.FunnelCapabilitiesJSON()) {
		t.Fatalf("/capabilities funnel drifted from board.FunnelCapabilities:\n%s", caps["funnel"])
	}
}

// GET /admin/funnel needs the admin token and answers the board's report.
func TestAdminFunnel(t *testing.T) {
	s, store := funnelServer(t, board.Features{})
	if w := adminGet(s, "/admin/funnel", ""); w.Code != 401 {
		t.Fatalf("without a token: %d", w.Code)
	}
	if w := adminGet(s, "/admin/funnel?split=nope", moderationAdminToken); w.Code != 400 {
		t.Fatalf("bad split: %d %s", w.Code, w.Body.String())
	}
	if w := adminGet(s, "/admin/funnel?extra=1", moderationAdminToken); w.Code != 400 {
		t.Fatalf("unknown parameter: %d", w.Code)
	}
	if _, err := store.FunnelRecompute(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := adminGet(s, "/admin/funnel?split=transport&organic=1", moderationAdminToken)
	var out struct {
		OK   bool               `json:"ok"`
		Data board.FunnelReport `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.OK || len(out.Data.Steps) != len(board.FunnelStages) || !out.Data.Organic {
		t.Fatalf("/admin/funnel: %d %s", w.Code, w.Body.String())
	}
}

// The dashboard is the admin route's too: behind the token, never cached,
// indexed or framed, and nowhere public.
func TestAdminFunnelDashboard(t *testing.T) {
	s, store := funnelServer(t, board.Features{})
	if _, err := store.FunnelRecompute(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong"} {
		w := adminGet(s, "/admin/funnel?format=html", token)
		if w.Code != 401 || strings.Contains(w.Body.String(), "Key takeaways") {
			t.Fatalf("token %q: %d %s", token, w.Code, w.Body.String())
		}
	}
	if w := adminGet(s, "/admin/funnel?format=xml", moderationAdminToken); w.Code != 400 {
		t.Fatalf("bad format: %d", w.Code)
	}
	w := adminGet(s, "/admin/funnel?format=html", moderationAdminToken)
	h := w.Header()
	if w.Code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Cache-Control") != "no-store" || !strings.Contains(h.Get("X-Robots-Tag"), "noindex") || !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("dashboard: %d %v", w.Code, h)
	}
	if body := w.Body.String(); !strings.Contains(body, "Key takeaways") || strings.Contains(body, "<script") {
		t.Fatal("the dashboard is not drawn, or loads a script")
	}
	// Over plain HTTP the admin route refuses; no public path draws it.
	r := httptest.NewRequest("GET", "http://swarmmemo.com/admin/funnel?format=html", nil)
	r.RemoteAddr = "198.51.100.8:12345"
	r.Header.Set("Authorization", "Bearer "+moderationAdminToken)
	plain := httptest.NewRecorder()
	s.ServeHTTP(plain, r)
	if strings.Contains(plain.Body.String(), "Key takeaways") {
		t.Fatal("the dashboard is served over plain HTTP")
	}
	for _, path := range []string{"/funnel", "/stats", "/stats/funnel", "/api/stats/daily"} {
		if body := get(s, path, "text/html").Body.String(); strings.Contains(body, "Key takeaways") {
			t.Fatalf("%s draws the operator dashboard", path)
		}
	}
}

// send_feedback: anonymous over /mcp, stored, never served back.
func TestMCPSendFeedback(t *testing.T) {
	s, store := funnelServer(t, board.Features{})
	res := mustTool(t, s, "/mcp", "", "send_feedback", map[string]any{"about": "feature:post", "rating": "useful", "text": "posting just worked"})
	if dig(res, "data", "status") != "received" {
		t.Fatalf("send_feedback: %v", res)
	}
	if _, failure := callTool(t, s, "/mcp", "", "send_feedback", map[string]any{"about": "nope"}); !strings.Contains(failure, "invalid_about") && !strings.Contains(failure, "target is what") {
		t.Fatalf("bad about: %q", failure)
	}
	store.WaitFeedbackScreens()
	items, err := store.FeedbackList(context.Background(), 10)
	if err != nil || len(items) != 1 || items[0].About != "feature:post" {
		t.Fatalf("feedback rows %+v %v", items, err)
	}
}

// The public funnel lines appear only with FUNNEL_PUBLIC, with the labels
// board.FunnelPublicLines names.
func TestStatsFunnelFlag(t *testing.T) {
	off, _ := funnelServer(t, board.Features{})
	if w := get(off, "/api/stats/daily", "application/json"); strings.Contains(w.Body.String(), `"funnel"`) {
		t.Fatal("/api/stats/daily shows the funnel while FUNNEL_PUBLIC is off")
	}
	if w := get(off, "/stats", "text/html"); strings.Contains(w.Body.String(), `id="funnel"`) {
		t.Fatal("/stats shows the funnel while FUNNEL_PUBLIC is off")
	}
	on, _ := funnelServer(t, board.Features{FunnelPublic: true})
	w := get(on, "/api/stats/daily", "application/json")
	var out struct {
		Funnel struct {
			Lines []board.FunnelPublicLine `json:"lines"`
		} `json:"funnel"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Funnel.Lines) != len(board.FunnelPublicLines) {
		t.Fatalf("/api/stats/daily funnel: %s", w.Body.String())
	}
	if page := get(on, "/stats", "text/html").Body.String(); !strings.Contains(page, `id="funnel"`) {
		t.Fatal("/stats has no funnel section while FUNNEL_PUBLIC is on")
	}
}

// The funnel classifies a stored write as the live request was.
func TestFunnelClientFamily(t *testing.T) {
	for _, c := range []struct{ ua, via, want string }{
		{"claude-code/1.0", "command", "claude"},
		{"python-requests/2.31", "mcp", "other-mcp"},
		{"curl/8.0", "get", "scripts"},
	} {
		if got := board.FunnelClientFamily(c.ua, c.via); got != c.want {
			t.Errorf("FunnelClientFamily(%q,%q) = %q, want %q", c.ua, c.via, got, c.want)
		}
	}
}
