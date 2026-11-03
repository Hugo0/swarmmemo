package httpapi

// Drift tests for the one table of pages (web.SiteNav, C349): every row is
// served, every moved address lands, no indexable page is an orphan, no
// internal link names a moved or missing address (in the served pages,
// llms.txt and the public docs and templates), and every generated surface
// (top nav, footer, Docs hub, sitemap, refLanding, llms.txt, robots.txt)
// equals the table.

import (
	"html"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// navFeatures turn every page of the table on that a store without outside
// configuration can serve (the services that need an upstream stay off).
var navFeatures = board.Features{Services: slices.DeleteFunc(services.Known(), func(id string) bool {
	return slices.Contains([]string{"inference", "runs", "public_data", "fetch", services.CorroborateID}, id)
}), Ledger: board.LedgerOn, Trust: board.TrustOn, Reviews: true}

// navServer is a real store with every page on, the nav following it.
func navServer(t *testing.T) *Server {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "nav.db"), board.Config{ServiceID: "swarmmemo.com", Features: navFeatures})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	web.SetNavFeatures(navFeatures)
	t.Cleanup(func() { web.SetNavFeatures(board.Features{}) })
	return New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", Features: navFeatures})
}

// navGet reads path, from a fresh address each time so the read limit never
// answers instead of the page.
var navAddr = 0

func navGet(s *Server, path string) *httptest.ResponseRecorder {
	navAddr++
	return getFrom(s, path, "text/html", "198.51.100."+strconv.Itoa(navAddr%250+1)+":12345")
}

func rowServed(n web.NavPage) bool { return n.Served == nil || n.Served(navFeatures) }

// Every row serves (200), a row this deployment does not serve is a 404,
// and every page of a family serves.
func TestSiteNavRowsServe(t *testing.T) {
	s := navServer(t)
	for _, n := range web.SiteNav {
		if n.Path == "" {
			continue
		}
		w := navGet(s, n.Path)
		switch {
		case n.Path == "/references" && w.Code == 503: // optional source index, unconfigured here
		case rowServed(n) && w.Code != 200:
			t.Errorf("%s (%s) answered %d", n.Path, n.Label, w.Code)
		case !rowServed(n) && w.Code >= 500:
			// Not served by this deployment: never linked (the footer and
			// the hub skip it) and never offered to search.
			t.Errorf("%s is not served here but answered %d", n.Path, w.Code)
		}
	}
	family := web.IndexedNavPaths(t.Context(), nil, navFeatures)
	if len(family) < 60 {
		t.Fatalf("the table expands to only %d indexable pages", len(family))
	}
	for _, path := range family {
		if w := navGet(s, path); w.Code != 200 {
			t.Errorf("indexable %s answered %d", path, w.Code)
		}
	}
	// Nothing for the operator: no /admin row, no operator or community group.
	for _, n := range web.SiteNav {
		low := strings.ToLower(n.Label + " " + string(n.Foot) + " " + string(n.Hub))
		if strings.HasPrefix(n.Path, "/admin") || strings.Contains(low, "operator") || strings.Contains(low, "our agents") || strings.Contains(low, "community") {
			t.Errorf("row %q sets the operator apart", n.Path)
		}
	}
}

// Every moved address answers 301 to its new home, the query kept, on GET
// and HEAD; no moved address is also a page or a retired (410) name.
func TestSiteNavMovesLand(t *testing.T) {
	s := navServer(t)
	rows := map[string]bool{}
	for _, n := range web.SiteNav {
		rows[n.Path] = true
	}
	fp := strings.Repeat("ab", 32)
	cases := map[string]string{
		"/limits":                 "/trust?x=1#allowance",
		"/pricing":                "/trust?x=1#allowance",
		"/prices":                 "/trust?x=1#allowance",
		"/policy":                 "/terms?x=1#board-rules",
		"/migration":              "/glossary?x=1#renamed",
		"/trust/network":          "/agents/network?x=1",
		"/graph":                  "/swarmchasing?x=1",
		"/graph/x":                "/swarmchasing?x=1",
		"/feed/tune":              "/me?x=1#feed",
		"/agent/" + fp + "/trust": "/agent/" + fp + "?x=1#standing",
	}
	for _, path := range web.MovedPaths() {
		if rows[path] {
			t.Errorf("%s is both a page and a moved address", path)
		}
		probe := path
		if strings.HasSuffix(path, "/") {
			probe += "x"
		}
		if _, ok := cases[probe]; !ok {
			t.Errorf("moved %s has no landing case here", path)
		}
	}
	for from, to := range cases {
		for _, method := range []string{"GET", "HEAD"} {
			r := httptest.NewRequest(method, from+"?x=1", nil)
			r.RemoteAddr = "198.51.100.9:1"
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 301 || w.Header().Get("Location") != to {
				t.Errorf("%s %s: %d %q, want 301 %q", method, from, w.Code, w.Header().Get("Location"), to)
			}
		}
		if w := navGet(s, from); w.Code == 410 {
			t.Errorf("%s is a retired name, not a move", from)
		}
	}
	if w := navGet(s, "/feed/tune?profile="+fp); w.Header().Get("Location") != "/me?tune="+fp+"#feed" {
		t.Errorf("/feed/tune?profile= lands on %q", w.Header().Get("Location"))
	}
}

// links are a page's same-site link paths (with their query and fragment).
var navHref = regexp.MustCompile(`(?:href|src)="(/[^"/][^"]*|/)"`)

func sameSiteLinks(body string) []string {
	var out []string
	for _, m := range navHref.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.ReplaceAll(m[1], "&amp;", "&"))
	}
	for _, m := range markdownLink.FindAllStringSubmatch(body, -1) {
		if u, err := url.Parse(m[1]); err == nil && (u.Host == "swarmmemo.com" || u.Host == "" && strings.HasPrefix(m[1], "/")) {
			out = append(out, u.RequestURI())
		}
	}
	return out
}

// isMoved reports whether path is an address that moved.
func isMoved(path string) bool {
	for _, from := range web.MovedPaths() {
		if path == from || strings.HasSuffix(from, "/") && strings.HasPrefix(path, from) {
			return true
		}
	}
	return regexp.MustCompile(`^/agent/[^/]+/trust$`).MatchString(path)
}

// No page of the table, no family page and no agent-facing text links an
// address that moved, and every link into the table's pages answers.
func TestSiteNavNoStrayLinks(t *testing.T) {
	s := navServer(t)
	pages := []string{"/llms.txt", "/llms-full.txt", "/skill.md", "/protocol.md", "/glossary.md", "/trust-model.md", "/privacy.md", "/terms.md", "/messages.md", "/verify.md", "/faq.md"}
	for _, n := range web.SiteNav {
		if n.Path != "" && rowServed(n) {
			pages = append(pages, n.Path)
		}
	}
	served := web.IndexedNavPaths(t.Context(), nil, navFeatures)
	pages = append(pages, served...)
	inTable := func(path string) bool {
		if strings.HasPrefix(path, "/tools/") || strings.HasPrefix(path, "/for/") {
			// A family page is in the table when this deployment serves it.
			return slices.Contains(served, path)
		}
		for _, n := range web.SiteNav {
			if n.Path == path || n.Prefix != "" && strings.HasPrefix(path, n.Prefix) {
				return true
			}
		}
		return false
	}
	checked := map[string]bool{}
	for _, page := range pages {
		w := navGet(s, page)
		if w.Code != 200 {
			continue // TestSiteNavRowsServe names it
		}
		for _, link := range sameSiteLinks(w.Body.String()) {
			u, err := url.Parse(link)
			if err != nil {
				continue
			}
			if isMoved(u.Path) {
				t.Errorf("%s links %s, an address that moved", page, link)
				continue
			}
			if !inTable(u.Path) || checked[u.Path] || u.Path == "/references" {
				continue
			}
			checked[u.Path] = true
			if code := navGet(s, u.Path).Code; code >= 400 {
				t.Errorf("%s links %s, which answers %d", page, link, code)
			}
		}
	}
	if len(checked) < 40 {
		t.Fatalf("checked only %d links into the table; the extractor is broken", len(checked))
	}
}

// The public sources (templates, scripts, published docs and client READMEs)
// name no moved address: the pages that render from them are covered above,
// and these are read where they are published too.
func TestSiteNavSourcesNameNoMovedPage(t *testing.T) {
	roots := []string{"../web/templates", "../web/assets", "../../docs", "../../clients", "../../README.md", "../../release/PUBLIC_README.md", "../web/quickstart.md.tmpl"}
	var moved []*regexp.Regexp
	for _, from := range append(web.MovedPaths(), "/agent/FP/trust", "/agent/FINGERPRINT/trust", "/agent/AGENT/trust") {
		quoted := regexp.QuoteMeta(strings.TrimSuffix(from, "/"))
		// As a link: href="/x", ](/x), swarmmemo.com/x, `/x`; not /api/x.
		moved = append(moved, regexp.MustCompile(`(?:href="|\]\(|swarmmemo\.com|`+"`"+`|\s)`+quoted+`(?:["#?)/`+"`"+`\s.,]|$)`))
	}
	skip := func(path string) bool {
		// Dated history: RFC changelogs, rollout notes and the storyboard of a
		// page that no longer exists keep the address they had then.
		return strings.Contains(path, "/rfcs/") || strings.Contains(path, "node_modules") || strings.HasSuffix(path, "_test.go") ||
			strings.HasSuffix(path, "trust-explainer-storyboard.md") || strings.Contains(path, "/testdata/")
	}
	n := 0
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || skip(path) {
				return nil
			}
			ext := filepath.Ext(path)
			if !slices.Contains([]string{".html", ".js", ".md", ".tmpl", ".py", ".ts"}, ext) {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			n++
			for _, re := range moved {
				for _, line := range strings.Split(string(raw), "\n") {
					// A line that says the page moved (and where) may name it.
					said := false
					for _, word := range []string{"301", "moved", "once ", "which was", "absorbed", "formerly", "the old "} {
						said = said || strings.Contains(line, word)
					}
					if re.MatchString(line) && !strings.Contains(line, "/api/agent/") && !said {
						t.Errorf("%s names a moved page: %s", path, strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
	}
	if n < 50 {
		t.Fatalf("read only %d source files", n)
	}
}

// Every indexable row is in the footer or the Docs hub; the footer holds
// every footer row once, in table order, and nothing else of ours.
func TestSiteNavNoOrphans(t *testing.T) {
	s := navServer(t)
	for _, n := range web.SiteNav {
		if n.Index && n.Path != "" && n.Foot == "" && n.Hub == "" && !n.Top {
			t.Errorf("%s is indexable but in neither the footer nor the Docs hub", n.Path)
		}
	}
	body := navGet(s, "/").Body.String()
	_, footer, _ := strings.Cut(body, "<footer")
	var want []string
	for _, col := range web.FootCols {
		for _, n := range web.SiteNav {
			if n.Foot == col && rowServed(n) {
				want = append(want, n.Path)
			}
		}
		if col == web.FootAbout {
			want = append(want, "https://github.com/Hugo0/swarmmemo")
		}
	}
	var got []string
	for _, m := range regexp.MustCompile(`<a href="([^"]+)"`).FindAllStringSubmatch(footer[strings.Index(footer, `aria-label="Footer"`):], -1) {
		got = append(got, m[1])
	}
	if !slices.Equal(got, want) {
		t.Errorf("footer links\n%v\nwant the table's\n%v", got, want)
	}
	if len(want) < 28 {
		t.Errorf("the footer has only %d links", len(want))
	}
	// The Docs hub lists every hub row, and each hub page marks Docs current.
	docs := navGet(s, "/docs").Body.String()
	for _, n := range web.SiteNav {
		if n.Hub != "" && n.Path != "" && rowServed(n) && !strings.Contains(docs, `<li><a href="`+n.Path+`">`+html.EscapeString(n.Label)+`</a>`) {
			t.Errorf("the Docs hub lacks %s", n.Path)
		}
	}
	for _, path := range []string{"/gate", "/faq", "/for-agents", "/tools/board", "/privacy"} {
		if !strings.Contains(navGet(s, path).Body.String(), `<a href="/docs" aria-current="location">Docs</a>`) {
			t.Errorf("%s does not mark Docs current", path)
		}
	}
}

// The generated surfaces equal the table: the five top items, the sitemap's
// fixed part, refLanding, llms.txt's Pages line within its budget, and
// robots.txt's Disallow lines. Labels use the glossary's words.
func TestSiteNavGeneratedSurfaces(t *testing.T) {
	s := navServer(t)
	body := navGet(s, "/agents/network").Body.String()
	nav := body[strings.Index(body, `aria-label="Main navigation"`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	var top []string
	for _, m := range regexp.MustCompile(`<a href="([^"]+)"`).FindAllStringSubmatch(nav, -1) {
		top = append(top, m[1])
	}
	if !slices.Equal(top, []string{"/", "/rooms", "/agents", "/work", "/docs"}) {
		t.Errorf("top nav %v", top)
	}
	if !strings.Contains(nav, `<a href="/agents" aria-current="location">Agents</a>`) || !strings.Contains(body, `<form class="nav-find" action="/find"`) {
		t.Error("Agents is not current on /agents/network, or Find is not the search field")
	}
	// Sitemap: the fixed part, in table order, then rewarded work.
	sitemap := navGet(s, "/sitemap.xml").Body.String()
	var locs []string
	for _, m := range regexp.MustCompile(`<loc>https://swarmmemo\.com([^<]*)</loc>`).FindAllStringSubmatch(sitemap, -1) {
		locs = append(locs, m[1])
	}
	fixed := web.IndexedNavPaths(t.Context(), s.service, navFeatures)
	if len(locs) < len(fixed) || !slices.Equal(locs[:len(fixed)], fixed) {
		t.Errorf("sitemap fixed part differs from the table:\n%v\n%v", locs, fixed)
	}
	for _, gone := range []string{"/limits", "/policy", "/migration", "/me", "/trust/network"} {
		if slices.Contains(locs, gone) {
			t.Errorf("sitemap offers %s", gone)
		}
	}
	// refLanding: exactly the Landing rows and their families.
	for _, n := range web.SiteNav {
		if n.Path != "" && refLanding(n.Path) != n.Landing {
			t.Errorf("refLanding(%s) = %v, the table says %v", n.Path, refLanding(n.Path), n.Landing)
		}
	}
	for _, path := range []string{"/tools/board", "/for/claude", "/guides/agent-board-map", "/e/x", "/agent/x", "/r/lobby", "/work/x"} {
		if !refLanding(path) {
			t.Errorf("refLanding(%s) is false", path)
		}
	}
	// llms.txt: the Pages line, every LLMs row linked.
	llms := navGet(s, "/llms.txt").Body.String()
	if !strings.Contains(llms, web.LLMsPages("https://swarmmemo.com")) {
		t.Error("/llms.txt lacks the generated Pages line")
	}
	for _, n := range web.SiteNav {
		if n.LLMs != "" && !strings.Contains(llms, "["+n.LLMs+"](https://swarmmemo.com"+n.Path+")") {
			t.Errorf("/llms.txt does not link %s", n.Path)
		}
	}
	// robots.txt: every NoIndex row for everyone, NoAI rows for AI crawlers.
	robots := navGet(s, "/robots.txt").Body.String()
	everyone, _, _ := strings.Cut(robots, "\n\n")
	everyone += "\n"
	for _, n := range web.SiteNav {
		if n.NoIndex && !strings.Contains(everyone, "Disallow: "+n.Path+"\n") {
			t.Errorf("robots.txt does not disallow %s for every crawler", n.Path)
		}
		if n.NoAI && (strings.Contains(everyone, "Disallow: "+n.Path+"\n") || !strings.Contains(robots, "Disallow: "+n.Path+"\n")) {
			t.Errorf("robots.txt does not disallow %s for the AI crawlers alone", n.Path)
		}
	}
	// One vocabulary: no row label uses a banned phrase.
	for _, n := range web.SiteNav {
		for _, b := range glossaryBanned(t) {
			for _, label := range []string{n.Label, n.TopLabel, string(n.Foot), string(n.Hub), n.LLMs} {
				if b.re.MatchString(label) {
					t.Errorf("label %q uses %q; say %q", label, b.phrase, b.instead)
				}
			}
		}
	}
}
