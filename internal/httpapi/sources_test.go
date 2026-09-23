package httpapi

// Drift guards for served copy: every internal link on a served page or
// document resolves, links into the source repository point at files the
// public snapshot publishes, known-wrong claims stay gone, and pages state
// limits through the board constants rather than as literals.
// See docs/project/SOURCES.md.

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

func realServer(t *testing.T) *Server {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "links.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com"})
}

func get(s *Server, path, accept string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "198.51.100.8:12345"
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// servedSurfaces is every page and document an agent or a person is sent to.
func servedSurfaces() []string {
	pages := []string{"/", "/for-agents", "/docs", "/policy", "/limits", "/agents", "/me", "/rooms", "/migration",
		"/llms.txt", "/llms-full.txt", "/skill.md", "/protocol.md",
		"/docs/INBOX.md", "/docs/OUTBOX.md", "/docs/DATASET.md", "/docs/CURATION.md", "/docs/SOURCE_SYNC.md",
		"/clients/python/README.md", "/clients/python/PRIVATE_INBOX.md", "/clients/python/FIRST_PUBLIC_WORK.md",
		"/clients/javascript/README.md", "/clients/mcp/README.md", "/clients/mcp/BOOTSTRAP.md"}
	return append(pages, web.PublicGuidePaths()...)
}

var (
	htmlLink     = regexp.MustCompile(`(?:href|src)="([^"]+)"`)
	markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	repoLink     = regexp.MustCompile(`https://github\.com/Hugo0/swarmmemo/blob/main/([^\s)"#?]+)`)
)

// markdownAnchors returns the GitHub-style anchors of a document's headings.
func markdownAnchors(doc string) map[string]bool {
	anchors := map[string]bool{}
	strip := regexp.MustCompile(`[^a-z0-9 _-]`)
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "#") {
			title := strings.ReplaceAll(strings.TrimSpace(strings.TrimLeft(line, "#")), "`", "")
			anchors[strings.ReplaceAll(strip.ReplaceAllString(strings.ToLower(title), ""), " ", "-")] = true
		}
	}
	return anchors
}

func TestServedLinksResolve(t *testing.T) {
	s := realServer(t)
	published := publicSnapshotFiles(t)
	bodies := map[string]string{}
	fetch := func(path string) (int, string) {
		if body, ok := bodies[path]; ok {
			return 200, body
		}
		w := get(s, path, "text/html")
		if w.Code < 400 {
			bodies[path] = w.Body.String()
		}
		return w.Code, w.Body.String()
	}
	checked := map[string]bool{}
	for _, page := range servedSurfaces() {
		status, body := fetch(page)
		if status != 200 {
			t.Errorf("%s answered %d", page, status)
			continue
		}
		base, _ := url.Parse("https://swarmmemo.com" + page)
		var targets []string
		for _, m := range htmlLink.FindAllStringSubmatch(body, -1) {
			targets = append(targets, strings.ReplaceAll(m[1], "&amp;", "&"))
		}
		if strings.HasSuffix(page, ".md") || strings.HasSuffix(page, ".txt") {
			for _, m := range markdownLink.FindAllStringSubmatch(body, -1) {
				targets = append(targets, m[1])
			}
		}
		for _, m := range repoLink.FindAllStringSubmatch(body, -1) {
			if !published[m[1]] {
				t.Errorf("%s links to %s in the source repository, which the public snapshot does not publish", page, m[1])
			}
		}
		for _, target := range targets {
			ref, err := base.Parse(target)
			if err != nil || ref.Host != "swarmmemo.com" || strings.Contains(target, "{{") {
				continue
			}
			path := ref.Path
			// Write URLs are commands, not links; message, room and agent
			// pages depend on data this empty store does not have.
			if strings.HasPrefix(path, "/w/") || strings.HasPrefix(path, "/w64/") || strings.HasPrefix(path, "/c64/") ||
				strings.HasPrefix(path, "/e/") || strings.HasPrefix(path, "/r/") || strings.HasPrefix(path, "/agent/") {
				continue
			}
			key := path + "#" + ref.Fragment
			if checked[key] {
				continue
			}
			checked[key] = true
			if ref.RawQuery != "" {
				path += "?" + ref.RawQuery
			}
			status, targetBody := fetch(path)
			switch {
			case ref.Path == "/references" && status == 503, ref.Path == "/mcp" && status < 500:
				continue // optional feature off in tests; a POST-only endpoint
			case status >= 400:
				t.Errorf("%s links to %s, which answers %d", page, target, status)
				continue
			}
			if ref.Fragment == "" {
				continue
			}
			if strings.HasSuffix(ref.Path, ".md") || strings.HasSuffix(ref.Path, ".txt") {
				if !markdownAnchors(targetBody)[ref.Fragment] {
					t.Errorf("%s links to %s, which has no heading #%s", page, ref.Path, ref.Fragment)
				}
			} else if !strings.Contains(targetBody, `id="`+ref.Fragment+`"`) {
				t.Errorf("%s links to %s, which has no id %q", page, ref.Path, ref.Fragment)
			}
		}
	}
	if len(checked) < 60 {
		t.Fatalf("checked only %d links; the extractor is broken", len(checked))
	}
}

func publicSnapshotFiles(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../release/public-files.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files []struct{ Destination string } `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Files) < 20 {
		t.Fatalf("public-files.json: %v", err)
	}
	files := map[string]bool{"README.md": true}
	for _, f := range manifest.Files {
		files[f.Destination] = true
	}
	return files
}

// publicCopy is the text of every surface a reader sees: served pages and
// documents, discovery JSON, and the repository READMEs.
func publicCopy(t *testing.T) map[string]string {
	t.Helper()
	s := realServer(t)
	copy := map[string]string{}
	for _, path := range append(servedSurfaces(), "/capabilities", "/openapi.json", "/.well-known/mcp/server-card.json") {
		copy[path] = get(s, path, "text/html").Body.String()
	}
	for _, file := range []string{"README.md", "release/PUBLIC_README.md", "scripts/dataset_card.md", "clients/javascript/README.md", "clients/python/README.md", "clients/mcp/README.md"} {
		raw, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		copy[file] = string(raw)
	}
	return copy
}

// Claims that were true once, or never, and must not come back. Add a line
// here whenever a wrong claim is corrected.
var stalePhrases = []struct{ pattern, why string }{
	{`(?i)attachments?\s+(?:expire|are\s+deleted|are\s+removed)\s+after`, "files are kept unless the uploader sets a ttl"},
	{`(?i)(?:links|files)\s+may\s+expire`, "files are kept unless the uploader sets a ttl"},
	{`(?i)profiles?\s+(?:expires?|is\s+hidden|are\s+hidden)\s+(?:after|when)`, "profiles are never hidden for age; fresh turns false"},
	{`(?i)unexpired\s+(?:self-described\s+)?profile`, "profiles are never hidden for age"},
	{`(?i)capability\s+cards?`, "retired in 1.0: a profile"},
	{`(?i)public\s+copy\s+of\s+this\s+list`, "there is no separate public copy"},
	{`(?i)identity\s+fingerprint`, "retired in 1.0: an agent fingerprint"},
	{`\bEVENT_ID\b|(?i)\bevent\s+ID\b`, "retired in 1.0: a posted item is a message (MESSAGE_ID)"},
	{`six\s+attempts\s+with\s+backoff,\s+then\s+the\s+subscription\s+disables`, "a subscription disables after 5 failed deliveries in a row"},
	{`(?i)deliveries\s+an\s+hour\s+per\s+key`, "webhook caps are per agent"},
	{`(?i)see\s+/docs\s+for\s+supported\s+commands`, "the operation list is /capabilities"},
	{`(?i)setting\s+it\s+is\s+not\s+open\s+yet|arrive\s+with\s+room\s+ownership`, "room.style.set is wired"},
}

func TestStalePhrasesStayGone(t *testing.T) {
	for source, text := range publicCopy(t) {
		if source == "/migration" {
			continue // the old-to-new table names retired terms on purpose
		}
		for _, stale := range stalePhrases {
			if m := regexp.MustCompile(stale.pattern).FindString(text); m != "" {
				t.Errorf("%s says %q: %s", source, m, stale.why)
			}
		}
	}
}

// Templates and the generated agent text state a limit through the board
// constants (the limit/limitText template funcs, board.LimitText in Go), so a
// changed limit cannot leave a page promising the old one.
func TestCopyStatesLimitsThroughConstants(t *testing.T) {
	var sources []string
	templates, _ := filepath.Glob("../web/templates/*.html")
	sources = append(sources, templates...)
	sources = append(sources, "discovery.go", "mcp.go", "receipts.go", "../web/assets/app.js")
	forbidden := map[string]string{}
	for _, l := range board.PublicLimits() {
		if l.Value >= 1000 {
			forbidden[`\b`+strconv.FormatInt(l.Value, 10)+`\b`] = l.Key
		}
		if l.Unit == "bytes" && l.Value >= 1024 {
			forbidden[`\b`+regexp.QuoteMeta(l.Text())+`\b`] = l.Key
		}
	}
	patterns := make([]string, 0, len(forbidden))
	for pattern := range forbidden {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, path := range sources {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "identity backup") {
				continue // a client-side sanity bound on key files, not a service limit
			}
			for _, pattern := range patterns {
				if m := regexp.MustCompile(pattern).FindString(line); m != "" {
					t.Errorf("%s:%d states %q literally; use the %s limit (limitText %q in templates, board.LimitText in Go)", path, i+1, m, forbidden[pattern], forbidden[pattern])
				}
			}
		}
	}
}

// Copy that names an operation names one the service accepts, so a renamed or
// removed operation cannot linger in a page or document. Words in an
// operation namespace that are JSON paths, not operations, are listed here.
var operationLookalikes = map[string]bool{
	"agent.links": true, "agent.domain_handle": true, "agent.profile": true,
}

func TestCopyNamesOnlyRealOperations(t *testing.T) {
	// Only code spans: `op` in Markdown and text, <code>op</code> in HTML.
	name := regexp.MustCompile("[`>]((?:messages|message|thread|updates|room|rooms|agent|agents|identity|blob|quota|credit|work|works|delegation|delegations|private_read|webhook|lease)\\.[a-z_]+(?:\\.[a-z_]+)*)[`<]")
	for op := range operationLookalikes {
		if board.KnownOperation(op) {
			t.Errorf("%s is an operation now; remove it from operationLookalikes", op)
		}
	}
	unknown, seen := map[string][]string{}, 0
	for source, text := range publicCopy(t) {
		if source == "/migration" || source == "/openapi.json" || source == "/capabilities" {
			continue // the rename table on purpose; JSON keys, not prose
		}
		for _, m := range name.FindAllStringSubmatch(text, -1) {
			seen++
			if !board.KnownOperation(m[1]) && !operationLookalikes[m[1]] {
				unknown[m[1]] = append(unknown[m[1]], source)
			}
		}
	}
	if seen < 100 {
		t.Fatalf("found only %d operation names in the copy; the pattern is broken", seen)
	}
	for op, sources := range unknown {
		sort.Strings(sources)
		t.Errorf("%q is not an operation (see internal/board/operations.go), in %v", op, sources)
	}
}
