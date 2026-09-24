package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"swarmmemo/internal/board"
)

type dailyResponse struct {
	OK          bool     `json:"ok"`
	Timezone    string   `json:"timezone"`
	Days        int      `json:"days"`
	MaximumDays int      `json:"maximum_days"`
	Notes       []string `json:"notes"`
	Daily       []struct {
		Day   string                      `json:"day"`
		Reads map[string]map[string]int64 `json:"reads"`
		Posts map[string]int64            `json:"posts"`
	} `json:"daily"`
}

func readDaily(t *testing.T, s *Server, query string) dailyResponse {
	t.Helper()
	w := makeRequest(s, "GET", "/api/stats/daily"+query, "", "")
	var body dailyResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("daily stats: %d %s", w.Code, w.Body.String())
	}
	return body
}

func TestReaderCountersIncrementOnEachCountedRoute(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "readers.db")
	store, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	s.readers.interval = time.Hour // flush explicitly below
	fingerprint := strings.Repeat("b", 64)
	const secretUA = "SecretAgentUA/9.9 (+contact-me)"
	get := func(path, ua string) {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "203.0.113.77:4444"
		r.Header.Set("User-Agent", ua)
		r.Header.Set("Referer", "https://referrer.example/secret-ref")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code >= 500 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	get("/llms.txt", secretUA)
	get("/llms.txt", "GPTBot/1.0")
	get("/llms-full.txt", secretUA)
	get("/skill.md", "Googlebot")
	get("/api/updates?agent="+fingerprint+"&cursor=secret-cursor-value", secretUA)
	get("/api/updates", secretUA)
	get("/api/updates", secretUA)
	// HEAD probes and uncounted pages do not count.
	if w := makeRequest(s, "HEAD", "/llms.txt", "", ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	get("/robots.txt", secretUA)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`
	for _, body := range []string{init, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.77:4444"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("User-Agent", secretUA)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		// The MCP handler must still receive the full body after inspection.
		if strings.Contains(body, "initialize") && (w.Code != 200 || !strings.Contains(w.Body.String(), "protocolVersion")) {
			t.Fatalf("initialize was not served intact: %d %s", w.Code, w.Body.String())
		}
	}

	// /for-agents is served by the UI handler.
	withUI := New(store, uiStub{}, Config{ServiceID: "swarmmemo.com"})
	withUI.readers = s.readers
	get2 := func(path string) {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "203.0.113.77:4444"
		withUI.ServeHTTP(httptest.NewRecorder(), r)
	}
	get2("/for-agents")
	get2("/for-agents")
	get2("/agents")

	check := func(body dailyResponse) {
		t.Helper()
		today := body.Daily[len(body.Daily)-1]
		want := map[string]map[string]int64{
			"llms_txt":              {"crawler": 1, "other": 1},
			"llms_full_txt":         {"crawler": 0, "other": 1},
			"skill_md":              {"crawler": 1, "other": 0},
			"for_agents":            {"crawler": 0, "other": 2},
			"updates_with_agent":    {"crawler": 0, "other": 1},
			"updates_without_agent": {"crawler": 0, "other": 2},
			"mcp_initialize":        {"crawler": 0, "other": 1},
		}
		for metric, split := range want {
			for class, n := range split {
				if today.Reads[metric][class] != n {
					t.Fatalf("%s/%s = %d, want %d (%v)", metric, class, today.Reads[metric][class], n, today.Reads)
				}
			}
		}
	}
	// Unflushed counts are visible, then identical after the flush to storage.
	check(readDaily(t, s, "?days=1"))
	s.readers.flush(store)
	if len(s.readers.pending) != 0 {
		t.Fatal("a successful flush must clear pending counts")
	}
	check(readDaily(t, s, "?days=1"))
	s.referrers.flush(store)

	// Nothing identifying reached storage: dump every text value in the database.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tables, err := raw.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		_ = tables.Scan(&name)
		names = append(names, name)
	}
	tables.Close()
	for _, table := range names {
		rows, err := raw.Query("SELECT * FROM \"" + table + "\"")
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			_ = rows.Scan(pointers...)
			for _, v := range values {
				text := ""
				switch v := v.(type) {
				case string:
					text = v
				case []byte:
					text = string(v)
				}
				for _, secret := range []string{"203.0.113.77", "SecretAgentUA", "contact-me", "GPTBot/1.0", "secret-ref", "https://referrer.example", fingerprint, "secret-cursor-value", "probe"} {
					if strings.Contains(text, secret) {
						t.Fatalf("table %s stored identifying value %q", table, secret)
					}
				}
				// The operator-only referrer counters (referrers.go) keep a bare
				// domain and a family name, and nowhere else is either stored.
				for _, name := range []string{"referrer.example", "GPTBot", "Googlebot"} {
					if strings.Contains(text, name) && (table != "counters" || !strings.HasPrefix(text, "referrer:") || !strings.HasSuffix(text, ":"+name)) {
						t.Fatalf("table %s stored %q outside a referrer counter: %q", table, name, text)
					}
				}
			}
		}
		rows.Close()
	}
}

type uiStub struct{}

func (uiStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }

// failingStatsService is a service whose counter storage hangs and then fails.
type failingStatsService struct {
	*fakeService
	release chan struct{}
	calls   chan struct{}
}

func (f *failingStatsService) AddReaderCounts(ctx context.Context, _ string, _ map[string]int64) error {
	select {
	case f.calls <- struct{}{}:
	default:
	}
	<-f.release
	return errors.New("disk on fire")
}
func (f *failingStatsService) ReadDailyStats(_ context.Context, end time.Time, days int) ([]board.DailyStats, error) {
	return nil, errors.New("disk on fire")
}

func TestFailingCounterStoreNeverFailsOrDelaysRequests(t *testing.T) {
	f := &failingStatsService{fakeService: &fakeService{}, release: make(chan struct{}), calls: make(chan struct{}, 1)}
	s := New(f, nil, Config{})
	s.readers.interval = 0 // every counted request is due for a flush
	started := time.Now()
	for i := 0; i < 20; i++ {
		if w := makeRequest(s, "GET", "/llms.txt", "", ""); w.Code != 200 {
			t.Fatalf("request %d failed: %d", i, w.Code)
		}
		if w := makeRequest(s, "GET", "/api/updates", "", ""); w.Code != 200 {
			t.Fatalf("updates request %d failed: %d", i, w.Code)
		}
	}
	select {
	case <-f.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("a flush should have been attempted")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("requests waited on a hung counter store: %v", elapsed)
	}
	close(f.release)
	deadline := time.Now().Add(2 * time.Second)
	for s.readers.flushing.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Failed counts are kept for a later attempt, bounded to a few days.
	s.readers.mu.Lock()
	var total int64
	for _, counts := range s.readers.pending {
		for _, n := range counts {
			total += n
		}
	}
	s.readers.mu.Unlock()
	if total != 40 {
		t.Fatalf("failed flush lost counts: %d", total)
	}
	for i := 0; i < 10; i++ {
		s.readers.pending[time.Date(2020, 1, 1+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02")] = map[string]int64{"llms_txt:other": 1}
	}
	s.readers.flush(f)
	if len(s.readers.pending) > readerMaxPendingDay {
		t.Fatalf("pending days unbounded: %d", len(s.readers.pending))
	}
	// A counter panic is swallowed.
	s.readers.pending = nil
	if w := makeRequest(s, "GET", "/llms.txt", "", ""); w.Code != 200 {
		t.Fatalf("a panicking counter failed the request: %d", w.Code)
	}
	if w := makeRequest(s, "GET", "/api/stats/daily", "", ""); w.Code != 503 {
		t.Fatalf("unavailable stats storage must be 503: %d", w.Code)
	}
}

func TestDailyStatsEndpointShapeAndBound(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "daily.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	body := readDaily(t, s, "")
	if !body.OK || body.Timezone != "UTC" || body.Days != 14 || body.MaximumDays != 90 || len(body.Daily) != 14 || len(body.Notes) != 3 {
		t.Fatalf("default shape: %+v", body)
	}
	if body.Daily[13].Day != time.Now().UTC().Format("2006-01-02") || body.Daily[0].Day >= body.Daily[13].Day {
		t.Fatalf("days must run oldest first and end today: %s..%s", body.Daily[0].Day, body.Daily[13].Day)
	}
	for _, day := range body.Daily {
		if len(day.Reads) != len(board.ReaderMetrics) || len(day.Posts) != 2 {
			t.Fatalf("incomplete day: %+v", day)
		}
	}
	if got := readDaily(t, s, "?days=90"); len(got.Daily) != 90 {
		t.Fatalf("days=90: %d", len(got.Daily))
	}
	for _, query := range []string{"?days=0", "?days=91", "?days=-1", "?days=abc", "?days=1&days=2", "?agent=x", "?days=100000"} {
		if w := makeRequest(s, "GET", "/api/stats/daily"+query, "", ""); w.Code != 400 {
			t.Fatalf("%s: %d", query, w.Code)
		}
	}
	if w := makeRequest(s, "POST", "/api/stats/daily", "", ""); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	joined := strings.Join(body.Notes, " ")
	for _, phrase := range []string{"include crawlers", "cannot distinguish operators", "do not know which keys the operator runs", "No identifying data is stored"} {
		if !strings.Contains(joined, phrase) {
			t.Fatalf("notes omit %q", phrase)
		}
	}
	caps := s.capabilities()["daily_stats"].(map[string]any)
	if caps["url"] != "/api/stats/daily" || caps["identifying_data_stored"] != false || caps["distinguishes_operators"] != false {
		t.Fatalf("capabilities: %v", caps)
	}
	if _, ok := s.openapi()["paths"].(map[string]any)["/api/stats/daily"]; !ok {
		t.Fatal("OpenAPI omits /api/stats/daily")
	}
}

// A restart must not lose counts from the last unflushed interval: main calls
// FlushReaderCounts after the HTTP server stops accepting requests.
func TestFlushReaderCountsPersistsTheFinalInterval(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "readers.db")
	store, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	s.readers.interval = time.Hour // no background flush during the test
	// lastFlush starts at the zero time, so the first read after start is otherwise
	// due at once and flushes in the background before the control check below.
	s.readers.lastFlush = time.Now()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/llms.txt", nil))
	if w.Code >= 500 {
		t.Fatalf("llms.txt: %d", w.Code)
	}
	stored := func(srv *Server) int64 {
		body := readDaily(t, srv, "?days=1")
		return body.Daily[len(body.Daily)-1].Reads["llms_txt"]["other"]
	}
	if got := stored(New(store, nil, Config{ServiceID: "swarmmemo.com"})); got != 0 {
		t.Fatalf("count reached storage before any flush: %d", got)
	}
	s.FlushReaderCounts()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := board.Open(dbPath, board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := stored(New(reopened, nil, Config{ServiceID: "swarmmemo.com"})); got != 1 {
		t.Fatalf("after flush and reopen, llms_txt other = %d, want 1", got)
	}
}
