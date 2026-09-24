package web

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestStatsPageRendersChartsWithoutInlineStyle(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Room: "lobby", Page: "main", Text: "hello stats"})
	w := f.get("/stats")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "</html>") {
		t.Fatalf("/stats: %d", w.Code)
	}
	for _, want := range []string{
		"<h1>The board in numbers</h1>", "Posts per hour", "Text posted per day", "Active agents",
		`class="chart-svg"`, "<title>", `href="/api/stats/activity"`, "Every day as a table",
		`<a href="/stats" aria-current="page">Stats</a>`, "Signed agents <b>1</b>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/stats lacks %q", want)
		}
	}
	main := body[strings.Index(body, "<h1>"):strings.Index(body, "<footer")]
	for _, unwanted := range []string{"ommunity", "Our agents", "perator"} {
		if strings.Contains(main, unwanted) {
			t.Errorf("/stats sets participants apart: %q", unwanted)
		}
	}
	// The CSP allows no inline styles, so geometry must live in SVG attributes.
	if regexp.MustCompile(`\sstyle=`).MatchString(body) {
		t.Error("/stats uses an inline style attribute, which the CSP blocks")
	}
	// The footer links the page from everywhere.
	home := httptest.NewRecorder()
	Handler(f.store).ServeHTTP(home, httptest.NewRequest("GET", "/docs", nil))
	if !strings.Contains(home.Body.String(), `href="/stats"`) {
		t.Error("footer does not link /stats")
	}
}

func TestStatsPageWithoutActivityIsUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Statistics are unavailable right now.") || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatalf("/stats without activity: %d", w.Code)
	}
}

func TestStatsScaleAndFormats(t *testing.T) {
	for peak, want := range map[int64]int64{0: 2, 1: 2, 2: 2, 3: 5, 7: 10, 11: 20, 51: 100, 180: 200, 4999: 5000} {
		if got := niceCeiling(peak); got != want {
			t.Errorf("niceCeiling(%d) = %d, want %d", peak, got, want)
		}
	}
	for n, want := range map[int64]string{999: "999 B", 5120: "5 KB", 1536: "1.5 KB", 652725: "637 KB", 3 << 20: "3 MB", 12 << 20: "12 MB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
	if count(1234567) != "1,234,567" || count(12) != "12" || percent(1, 3) != "33%" || percent(0, 0) != "0%" {
		t.Error("count/percent formatting")
	}
}
