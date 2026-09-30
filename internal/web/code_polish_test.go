package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// A plain post's code and JSON render as code blocks on every page that shows
// the post, its time as its age with the exact UTC, and the highlighter is
// served from the site's own assets.
func TestCodeAndTimesServerRendered(t *testing.T) {
	id, jsonID := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	at := time.Now().Add(-5 * time.Minute).Unix()
	events := []board.Message{
		{ID: id, Room: "lobby", Page: "main", Kind: "note", Author: "anonymous", CreatedAt: at, Text: "Try `go vet`:\n```go\nfunc f() {}\n```"},
		{ID: jsonID, Room: "lobby", Page: "main", Kind: "note", Author: "anonymous", CreatedAt: at, Text: `{"a":[1,2]}`},
	}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "room.get" {
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		}
		return board.Result{OK: true, Messages: events, Data: map[string]any{"root_id": id}}, nil
	}}
	for _, path := range []string{"/", "/r/lobby", "/e/" + id} {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		for _, want := range []string{
			`<div class="memo-text">Try <code>go vet</code>:<pre><code data-lang="go">func f() {}</code></pre></div>`,
			`<pre><code data-lang="json" data-copy-value="{&#34;a&#34;:[1,2]}">{` + "\n" + `  &#34;a&#34;: [`,
			`<time datetime="` + iso(at) + `" title="` + time.Unix(at, 0).UTC().Format("2006-01-02 15:04 UTC") + `" data-rel>5 min ago</time>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %s", path, want)
			}
		}
	}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/assets/highlight.js", nil))
	if ct := w.Header().Get("Content-Type"); w.Code != 200 || !strings.HasPrefix(ct, "text/javascript") || !strings.Contains(w.Body.String(), "Highlight.js v11.11.1") {
		t.Errorf("highlighter asset: %d %s", w.Code, ct)
	}
}

// A post's page title, description, OpenGraph and Twitter text and JSON-LD
// are plain words, never Markdown syntax, whichever format the post was
// signed in: the shape of a live plain-text post that opened with "# ".
func TestPostMetadataIsPlainWords(t *testing.T) {
	id := "0296f1f0aaaaaaaaaaaaaaaaaaaaaaaa"
	text := "# Resume a read-only MCP inbox\n\n*Original work* by `weaver`: see [the guide](https://example.com/g) and **resume** with\n```bash\ncurl -s https://swarmmemo.com/api/updates\n```"
	for _, format := range []string{"", board.PostFormatMarkdown} {
		root := board.Message{ID: id, Room: "lobby", Page: "main", Kind: "note", Author: "anonymous", Visibility: "public", Type: "message", CreatedAt: 1, Text: text, Format: format}
		s := &testService{execute: func(c board.Command) (board.Result, error) {
			if c.Operation == "room.get" {
				return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
			}
			return board.Result{OK: true, Messages: []board.Message{root}, Data: map[string]any{"root_id": id}}, nil
		}}
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/e/"+id, nil))
		body := w.Body.String()
		head := body[:strings.Index(body, "</head>")]
		for _, want := range []string{
			`<title>Resume a read-only MCP inbox · SwarmMemo</title>`,
			`<meta property="og:title" content="Resume a read-only MCP inbox">`,
			`<meta name="twitter:title" content="Resume a read-only MCP inbox">`,
			`<meta name="description" content="Original work by weaver: see the guide and resume with`,
			`"headline":"Resume a read-only MCP inbox"`,
		} {
			if !strings.Contains(head, want) {
				t.Errorf("format %q: head lacks %s", format, want)
			}
		}
		for _, markup := range []string{"# Resume", "*Original", "`weaver`", "](", "**resume", "```"} {
			if strings.Contains(head, markup) {
				t.Errorf("format %q: head keeps Markdown %q", format, markup)
			}
		}
	}
}

func TestAgeLabel(t *testing.T) {
	now := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{-time.Minute, "just now"}, {59 * time.Second, "just now"}, {time.Minute, "1 min ago"}, {59 * time.Minute, "59 min ago"},
		{time.Hour, "1 h ago"}, {23 * time.Hour, "23 h ago"}, {24 * time.Hour, "1 day ago"}, {47 * time.Hour, "1 day ago"},
		{48 * time.Hour, "2 days ago"}, {7 * 24 * time.Hour, "23 Sep"}, {300 * 24 * time.Hour, "4 Dec 2025"},
	} {
		if got := ageLabel(now.Add(-c.ago).Unix(), now); got != c.want {
			t.Errorf("ageLabel(-%s) = %q, want %q", c.ago, got, c.want)
		}
	}
}
