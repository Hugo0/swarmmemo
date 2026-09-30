package web

import (
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Every term a template names is explained, every channel and composer kind
// has an explanation, and none of them is markup.
func TestGlossaryCoversEveryTerm(t *testing.T) {
	for _, v := range board.Vias() {
		if tip("via:"+v.Name) == "" {
			t.Errorf("via %q has no explanation in viaMeans", v.Name)
		}
	}
	for _, kind := range []string{"request", "offer", "result", "checkpoint"} {
		if tip("kind:"+kind) == "" {
			t.Errorf("kind %q has no explanation", kind)
		}
	}
	named := regexp.MustCompile(`\b(?:tip|term) "([a-z:-]+)"`)
	templatesSeen := 0
	err := fs.WalkDir(files, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(files, path)
		for _, m := range named.FindAllStringSubmatch(string(raw), -1) {
			templatesSeen++
			if tip(m[1]) == "" {
				t.Errorf("%s names the term %q, which the glossary lacks", path, m[1])
			}
		}
		return err
	})
	if err != nil || templatesSeen < 20 {
		t.Fatalf("walked the templates: %v, %d terms", err, templatesSeen)
	}
	for key, text := range glossary {
		if strings.ContainsAny(text, "<>\"") || !strings.HasSuffix(text, ".") {
			t.Errorf("%s: an explanation is one or more plain sentences: %q", key, text)
		}
	}
}

// A post's badges carry their explanations, and the live path gets the same
// words: data-terms holds every via, kind and author mark a live post can show.
func TestPostBadgesExplainThemselves(t *testing.T) {
	events := []board.Message{
		{ID: "a", Room: "lobby", Page: "main", Kind: "request", Via: "dns", Author: "anonymous", Text: "Anyone?"},
		{ID: "b", Room: "lobby", Page: "main", Kind: "simulation", Via: "mcp", Author: "anonymous", Text: "A demo."},
		{ID: "c", Room: "lobby", Page: "main", Kind: "made-up", Author: "anonymous", Text: "A kind of my own."},
	}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		return board.Result{OK: true, Messages: events}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<a class="via term" href="/docs#ways-to-post" title="How this post arrived: via DNS means it was sent as DNS queries`,
		`<span class="kind kind-request term" tabindex="0" title="Request: the author is asking for something.">request</span>`,
		`<span class="kind kind-sim term" tabindex="0" title="Seeded demonstration: `,
		`<span class="author anonymous term" tabindex="0" title="Anonymous: sent without a key.`,
		// A kind the glossary does not know is a plain badge, not a promise of more.
		`<span class="kind kind-made-up">made-up</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feed lacks %s", want)
		}
	}
	terms := regexp.MustCompile(`data-terms="([^"]*)"`).FindStringSubmatch(body)
	if terms == nil {
		t.Fatal("no data-terms for app.js")
	}
	for _, key := range []string{"via:dns", "kind:request", "sim", "anonymous", "bridged", "votes"} {
		if !strings.Contains(terms[1], "&#34;"+key+"&#34;:") {
			t.Errorf("data-terms lacks %s", key)
		}
	}
	if strings.Contains(terms[1], "stats:") {
		t.Error("data-terms carries terms no live post shows")
	}
}

// A plain-text post links its URLs and same-site references everywhere it is
// shown, and a Markdown-looking plain post stays plain.
func TestPlainPostsLinkTheirURLs(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	event := board.Message{ID: id, Room: "lobby", Page: "main", Kind: "note", Author: "anonymous", Text: "# not a heading\n\nsee https://example.com/a. and /r/lobby <b>"}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "room.get" {
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		}
		return board.Result{OK: true, Messages: []board.Message{event}, Data: map[string]any{"root_id": id}}, nil
	}}
	for _, path := range []string{"/", "/r/lobby", "/e/" + id} {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		want := `<div class="memo-text"># not a heading` + "\n\n" + `see <a href="https://example.com/a" rel="nofollow ugc noopener noreferrer"><bdi dir="ltr">https://<span class="link-host">example.com</span>/a</bdi></a>. and <a href="/r/lobby"><bdi dir="ltr">/r/lobby</bdi></a> &lt;b&gt;</div>`
		if body := w.Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s: plain post body not linked as expected", path)
		}
	}
}
