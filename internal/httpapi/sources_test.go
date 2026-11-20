package httpapi

// Drift guards for served copy: every internal link on a served page or
// document resolves, links into the source repository point at files the
// public snapshot publishes, known-wrong claims stay gone, and pages state
// limits through the board constants rather than as literals.
// See docs/SOURCES.md.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	return getFrom(s, path, accept, "198.51.100.8:12345")
}

// getFrom is get from a chosen client address.
func getFrom(s *Server, path, accept, addr string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = addr
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// servedSurfaces is every page and document an agent or a person is sent to.
func servedSurfaces() []string {
	pages := []string{"/", "/for-agents", "/docs", "/agents", "/me", "/rooms", "/swarmchasing",
		"/privacy", "/terms", "/privacy.md", "/terms.md", "/messages", "/messages.md", "/verify", "/verify.md", "/glossary", "/glossary.md", "/trust-model", "/trust-model.md",
		"/llms.txt", "/llms-full.txt", "/skill.md", "/protocol.md",
		"/docs/INBOX.md", "/docs/OUTBOX.md", "/docs/DATASET.md", "/docs/CURATION.md", "/docs/SOURCE_SYNC.md",
		"/clients/python/README.md", "/clients/python/PRIVATE_INBOX.md", "/clients/python/FIRST_PUBLIC_WORK.md",
		"/clients/javascript/README.md", "/clients/mcp/README.md", "/clients/mcp/BOOTSTRAP.md"}
	// Every page of the one table of pages (C349), so no page of the nav,
	// the footer or the Docs hub links a moved or missing address.
	for _, n := range web.SiteNav {
		// Pages whose links follow feature flags this default server leaves
		// off are covered by TestSiteNavNoStrayLinks with every flag on.
		if n.Path != "" && !n.API && !slices.Contains(pages, n.Path) && !slices.Contains([]string{"/s", "/trust/penalties", "/tools", "/faq", "/trust", web.AgentsNetworkPath}, n.Path) {
			pages = append(pages, n.Path)
		}
	}
	return append(append(pages, web.PublicGuidePaths()...), append(web.PlatformPaths(), web.FrameworkPaths()...)...)
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
		// Every page fetched from one address trips the per-client read
		// limit as the surfaces grow; a 429 is the limiter, not a broken
		// link, so ask again from another documentation address.
		for i := 1; w.Code == http.StatusTooManyRequests && i < 250; i++ {
			w = getFrom(s, path, "text/html", fmt.Sprintf("198.51.100.%d:12345", i))
		}
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
			if path == "" {
				path = "/"
			}
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
			case status == http.StatusMovedPermanently && ref.Path != "/cases":
				// C349: a page that moved keeps its old address working for
				// links elsewhere, but our own copy links the new home.
				t.Errorf("%s links to %s, which moved (301)", page, target)
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

// Every file internal/web embeds (assets/, templates/) is in the public
// snapshot, or the published source builds a site that 404s on it.
func TestEmbeddedWebFilesArePublic(t *testing.T) {
	published := publicSnapshotFiles(t)
	for _, dir := range []string{"assets", "templates"} {
		root := filepath.Join("..", "web", dir)
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel := filepath.ToSlash(filepath.Join("internal/web", dir, strings.TrimPrefix(path, root+string(filepath.Separator))))
				if !published[rel] {
					t.Errorf("%s is embedded but not in release/public-files.json", rel)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// publicSnapshotFiles is the set of paths the public snapshot publishes: the
// release allowlist in the private tree, or, inside a published snapshot
// (which has no release/ directory), the SOURCE_MANIFEST.json it was cut with.
func publicSnapshotFiles(t *testing.T) map[string]bool {
	t.Helper()
	files := map[string]bool{"README.md": true}
	raw, err := os.ReadFile("../../release/public-files.json")
	if os.IsNotExist(err) {
		raw, err = os.ReadFile("../../SOURCE_MANIFEST.json")
		if err != nil {
			t.Fatalf("neither release/public-files.json nor SOURCE_MANIFEST.json: %v", err)
		}
		var manifest struct {
			Files []struct{ Path string } `json:"files"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Files) < 20 {
			t.Fatalf("SOURCE_MANIFEST.json: %v", err)
		}
		for _, f := range manifest.Files {
			files[f.Path] = true
		}
		return files
	}
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files []struct{ Destination string } `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest.Files) < 20 {
		t.Fatalf("public-files.json: %v", err)
	}
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
		if file == "release/PUBLIC_README.md" && os.IsNotExist(err) {
			continue // the public snapshot: it is README.md there
		}
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
	// The 1.24 docs audit (T47).
	{`(?i)bulletin\s+board|message\s+board\s+for\s+AI\s+agents|durable\s+communication\s+service`, "1.24 positioning: the hub line (web.Tagline)"},
	// Variants of the positioning lines; the one source is the glossary's
	// positioning block (web.Positioning, TestPositioningSingleSourced).
	{`(?i)hub\s+where\s+AI\s+agents\s+talk|public\s+square\s+and\s+paid-task\s+market|message\s+board\s+(?:and\s+identity\s+hub|for\s+agent\s+swarms)|(?:public|identity)\s+(?:board|hub)\s+for\s+AI\s+agents`, "1.24 positioning: the glossary's positioning block (web.Positioning)"},
	{`(?i)both\s+MCP\s+adapters\s+remain\s+public-only|public\s+reads\s+and\s+public\s+anonymous\s+posting\s+only|key\s+custody\s+are\s+deliberately\s+not\s+exposed|public-only\s+tools;\s+use\s+signed\s+HTTP`, "a hosted identity holds an inbox and private conversations over MCP"},
	{`(?i)or\s+end-to-end\s+encryption\s+is\s+currently\s+implemented`, "sealed conversations are end-to-end encrypted"},
	{`(?i)no\s+anonymous,\s+browser\s+or\s+delegated\s+form,\s+and\s+nobody\s+can\s+link`, "/me signs identity links with the browser's own key (friction 1095)"},
	{`(?i)no\s+escrow\s+or\s+reward\s+is\s+promised`, "say that bounties are a separate paid program (friction 1094)"},
	{"and `community`", "activity buckets carry native (friction 1088)"},
	// The connect-URL rule (web.ConnectRule): assistant apps add
	// /mcp/assistant with no sign-in to start; developers add /mcp.
	{`(?i)/mcp` + "`?" + `\s+(?:\(or[^)]*\)\s+)?as\s+a\s+connector,?\s+(?:and\s+)?sign\s+in`, "the connect-URL rule: assistant apps add /mcp/assistant and need no sign-in to start (web.ConnectRule)"},
	{`(?i)navigation\s+and\s+forms\s+dropped`, "fetch keeps a form's text and drops buttons and menus (services.FetchHTMLDropped)"},
	{`Any MCP client:\s+https?://\S+/mcp/core`, "the connect-URL rule (web.ConnectRule): /mcp/assistant for assistant apps, /mcp for your own code"},
	{`(?i)has\s+no\s+mail\s+address|email\s+(?:only\s+)?(?:for\s+)?(?:a\s+)?private\s+rooms?\s+with\s+a\s+plain\s+name|signing\s+wire\s+but\s+email`, "email carries conversations at ~NAME@ or _NAME@"},
}

func TestStalePhrasesStayGone(t *testing.T) {
	for source, text := range publicCopy(t) {
		if source == "/migration" || source == "/glossary" || source == "/glossary.md" {
			continue // the old-to-new table and the glossary name retired terms on purpose
		}
		for _, stale := range stalePhrases {
			if source == "/guides/agent-board-map" && strings.HasPrefix(stale.why, "1.24 positioning") {
				continue // generated from the awesome-agent-boards list, updated there
			}
			if m := regexp.MustCompile(stale.pattern).FindString(text); m != "" {
				t.Errorf("%s says %q: %s", source, m, stale.why)
			}
		}
	}
}

// Templates and the generated agent text state a limit through the board
// constants (the limit/limitText template funcs, board.LimitText in Go), so a
// changed limit cannot leave a page promising the old one.
//
// Every template and every script under web/assets is scanned (opt-out, not
// opt-in): a limit of 1000 or more may not appear as a bare number, a smaller
// one may not appear next to its unit ("500 bytes", "64 characters"), and no
// form field states its own maxlength: it reads {{limit}}/{{inputMax}} in a
// template, page-data.js's limits (or a data- attribute the template filled)
// in a script.
func TestCopyStatesLimitsThroughConstants(t *testing.T) {
	var sources []string
	templates, _ := filepath.Glob("../web/templates/*.html")
	scripts, _ := filepath.Glob("../web/assets/*.js")
	if len(templates) < 20 || len(scripts) < 10 {
		t.Fatalf("found %d templates and %d scripts; the globs are broken", len(templates), len(scripts))
	}
	sources = append(sources, templates...)
	sources = append(sources, scripts...)
	sources = append(sources, "discovery.go", "mcp.go", "receipts.go")
	forbidden := map[string]string{}
	for _, l := range board.PublicLimits() {
		if l.Value >= 1000 {
			forbidden[`\b`+strconv.FormatInt(l.Value, 10)+`\b`] = l.Key
		}
		if l.Unit == "bytes" && l.Value >= 1024 {
			forbidden[`\b`+regexp.QuoteMeta(l.Text())+`\b`] = l.Key
		}
		// A smaller limit is stated next to its unit.
		if l.Value >= 10 && l.Value < 1000 {
			unit := map[string]string{"bytes": `(?:UTF-8 )?bytes`, "seconds": `seconds`}[l.Unit]
			if strings.HasSuffix(l.Key, "_chars") {
				unit = `(?:characters|chars)`
			}
			if unit != "" {
				forbidden[`\b`+strconv.FormatInt(l.Value, 10)+`[ -]`+unit+`\b`] = l.Key
			}
		}
	}
	// A form field's own number: maxlength="64" in a template, maxLength = 500
	// or maxLength: 80 in a script.
	forbidden[`(?i)maxlength(?:="|\s*[:=]\s*)\d`] = "the field's bound"
	// Numbers that equal a limit by coincidence, by file and match.
	coincidences := map[string]string{
		"graph.js|604800":  "seconds in a week, for the weekly timeline",
		"graph-gl.js|2048": "the camera's world coordinates",
		"embed-v1.js|2048": "a sanity bound on the host page's data-url, which is never sent",
		// webhook_host_requests_per_hour is 1200 (C1108b); these are not it.
		"page.html|1200":        "the Open Graph image's width in pixels",
		"app.js|1200":           "the g-key chord's timeout in milliseconds",
		"connect-embed.js|1200": "the delay before the sign-in window closes, in milliseconds",
		"graph.js|1200":         "the camera animation's duration in milliseconds",
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
					if m == "1000" && strings.HasSuffix(path, ".js") {
						continue // milliseconds per second in a script, not the 1,000 limits
					}
					if coincidences[filepath.Base(path)+"|"+m] != "" {
						continue
					}
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
	"agent.links": true, "agent.domain_handle": true, "agent.profile": true, "agent.avatar": true,
	"agents.json": true, // the Swarmchasing dataset download, a file name
	// MCP Events names (board.MCPEventNames), not operations.
	"room.post": true, "work.open": true, "work.update": true, "identity.witnessed": true,
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
		if source == "/openapi.json" || source == "/capabilities" {
			continue // JSON keys, not prose
		}
		if source == "/glossary" {
			// The 1.0 rename table (#renamed) names the old operations on purpose.
			if i := strings.Index(text, `id="renamed"`); i >= 0 {
				text = text[:i]
			}
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
