package web

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

var (
	ldBlock       = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)
	canonicalLink = regexp.MustCompile(`<link rel="canonical" href="([^"]*)">`)
	metaContent   = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`<meta (?:name|property)="` + regexp.QuoteMeta(name) + `" content="([^"]*)">`)
	}
)

// ldGraph parses a page's one JSON-LD block into its @graph nodes by @type.
func ldGraph(t *testing.T, body string) map[string]map[string]any {
	t.Helper()
	blocks := ldBlock.FindAllStringSubmatch(body, -1)
	if len(blocks) != 1 {
		t.Fatalf("want one JSON-LD block, found %d", len(blocks))
	}
	var doc struct {
		Context string           `json:"@context"`
		Graph   []map[string]any `json:"@graph"`
	}
	if err := json.Unmarshal([]byte(blocks[0][1]), &doc); err != nil || doc.Context != "https://schema.org" {
		t.Fatalf("JSON-LD does not parse: %v %s", err, blocks[0][1])
	}
	nodes := map[string]map[string]any{}
	for _, node := range doc.Graph {
		nodes[node["@type"].(string)] = node
	}
	return nodes
}

const hostileText = "Look </script><script>alert(1)</script> & <!-- \"quotes\"   end"

func TestSEOIndexablePagesCarryMetadata(t *testing.T) {
	s, owner, mod := roomStore(t)
	root := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: hostileText}).Receipt.ID
	mod.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "A reply with </script> in it", ReplyTo: root})
	owner.run(t, s, board.Command{Operation: "post", Room: board.PersonalRoom(owner.id), Text: "My own room"})
	article := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "# A long read\n\nIt starts here.", Data: `{"schema":1,"format":"markdown"}`}).Receipt.ID

	for _, path := range []string{"/", "/r/lobby", "/e/" + root, "/e/" + article + "/a-long-read", "/agents", "/agent/" + owner.id, "/for-agents", "/docs", "/rooms", "/policy", "/limits", "/guides", "/@" + owner.id[:12]} {
		w := render(s, path)
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		// Indexable pages must not be noindexed by accident.
		if w.Header().Get("X-Robots-Tag") != "" || strings.Contains(body, `<meta name="robots"`) {
			t.Fatalf("%s is noindexed", path)
		}
		canonical := canonicalLink.FindStringSubmatch(body)
		if canonical == nil || !strings.HasPrefix(canonical[1], "https://swarmmemo.com/") || strings.Contains(canonical[1], "?") {
			t.Fatalf("%s canonical: %v", path, canonical)
		}
		for _, name := range []string{"description", "og:title", "og:description", "og:url", "og:image", "og:type", "og:site_name", "twitter:card"} {
			m := metaContent(name).FindStringSubmatch(body)
			if m == nil || strings.TrimSpace(m[1]) == "" {
				t.Fatalf("%s lacks %s", path, name)
			}
		}
		if og := metaContent("og:url").FindStringSubmatch(body)[1]; og != canonical[1] {
			t.Fatalf("%s: og:url %s differs from canonical %s", path, og, canonical[1])
		}
		if d := metaContent("description").FindStringSubmatch(body)[1]; len([]rune(d)) > 200 {
			t.Fatalf("%s: description of %d characters", path, len([]rune(d)))
		}
		// Message text never escapes into markup, in the head or anywhere else.
		if strings.Contains(body, "<script>alert(1)") || strings.Contains(body, "</script><script>") {
			t.Fatalf("%s: hostile text rendered as markup", path)
		}
	}

	home := ldGraph(t, render(s, "/").Body.String())
	site := home["WebSite"]
	if site == nil || home["Organization"] == nil || site["potentialAction"].(map[string]any)["target"].(map[string]any)["urlTemplate"] != "https://swarmmemo.com/?q={search_term_string}" {
		t.Fatalf("home JSON-LD: %v", home)
	}

	thread := render(s, "/e/"+root).Body.String()
	nodes := ldGraph(t, thread)
	posting := nodes["DiscussionForumPosting"]
	if posting == nil || nodes["BreadcrumbList"] == nil {
		t.Fatalf("thread JSON-LD: %v", nodes)
	}
	// The hostile text survives as data: escaped in the page, intact once parsed.
	if !strings.Contains(posting["text"].(string), "</script><script>alert(1)</script>") || posting["url"] != "https://swarmmemo.com/e/"+root {
		t.Fatalf("posting: %v", posting)
	}
	if author := posting["author"].(map[string]any); author["name"] != "writer" || author["url"] != "https://swarmmemo.com/agent/"+owner.id {
		t.Fatalf("author: %v", author)
	}
	comments := posting["comment"].([]any)
	if len(comments) != 1 || comments[0].(map[string]any)["author"].(map[string]any)["name"] != "keeper" {
		t.Fatalf("comments: %v", comments)
	}
	if stat := posting["interactionStatistic"].(map[string]any); stat["userInteractionCount"] != float64(1) {
		t.Fatalf("reply count: %v", stat)
	}
	if posting["datePublished"] == "" || posting["headline"] == "" {
		t.Fatalf("posting dates/headline: %v", posting)
	}

	room := ldGraph(t, render(s, "/r/lobby").Body.String())
	list := room["CollectionPage"]["mainEntity"].(map[string]any)["itemListElement"].([]any)
	urls := []string{}
	for _, item := range list {
		urls = append(urls, item.(map[string]any)["url"].(string))
	}
	if strings.Join(urls, " ") != "https://swarmmemo.com/e/"+article+"/a-long-read https://swarmmemo.com/e/"+root {
		t.Fatalf("room item list (roots only, canonical addresses): %v", urls)
	}
	if d := metaContent("description").FindStringSubmatch(render(s, "/r/lobby").Body.String())[1]; !strings.Contains(d, "public messages") {
		t.Fatalf("room description: %s", d)
	}
}

// Removed and simulated conversations stay readable but are not offered to
// search engines; neither are resumed pages, and none carries structured data.
func TestSEONoindexWhereItBelongs(t *testing.T) {
	s, owner, _ := roomStore(t)
	hidden := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "soon removed"}).Receipt.ID
	if err := s.Moderate(t.Context(), hidden, "test", true); err != nil {
		t.Fatal(err)
	}
	simulation := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Kind: "simulation", Text: "operator fixture"}).Receipt.ID
	plain := owner.run(t, s, board.Command{Operation: "post", Room: "lobby", Text: "ordinary"}).Receipt.ID
	for _, path := range []string{"/e/" + hidden, "/e/" + simulation, "/e/" + plain + "?cursor=start", "/?q=ordinary", "/r/lobby?cursor=start", "/me", "/r/no-such-room", "/e/" + plain + "/history"} {
		w := render(s, path)
		body := w.Body.String()
		if !strings.HasPrefix(w.Header().Get("X-Robots-Tag"), "noindex") || !strings.Contains(body, `<meta name="robots" content="noindex">`) {
			t.Fatalf("%s: %d not noindexed (%q)", path, w.Code, w.Header().Get("X-Robots-Tag"))
		}
		if strings.Contains(body, "application/ld+json") {
			t.Fatalf("%s carries structured data", path)
		}
		if strings.Contains(body, "soon removed") {
			t.Fatalf("%s shows a removed body", path)
		}
	}
}

// A styled room keeps its structured data: the room's CSS is an external
// stylesheet and cannot reach the head, and a JSON-LD data block is never
// executed, so the styled page's script-src 'self' policy leaves it alone.
func TestSEOStructuredDataOnStyledRooms(t *testing.T) {
	s, owner, _ := roomStore(t)
	owner.run(t, s, board.Command{Operation: "room.create", Room: "garden"})
	owner.run(t, s, board.Command{Operation: "post", Room: "garden", Text: "a root"})
	css := `{"css":":scope{--trust-plate:#000;--trust-ink:#fff;--trust-muted:#ccc;background:#000}"}`
	owner.run(t, s, board.Command{Operation: "room.style.set", Room: "garden", Data: css})
	w := get(t, s, "/r/garden", "swarmmemo.com")
	body := w.Body.String()
	if !strings.Contains(body, `class="room-styled`) || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("not a styled page: %d", w.Code)
	}
	if ldGraph(t, body)["CollectionPage"] == nil {
		t.Fatal("styled room lost its structured data")
	}
	head := body[:strings.Index(body, "</head>")]
	if strings.Contains(head, "background:#000") || strings.Count(head, "<style") != 0 {
		t.Fatal("room CSS reached the head inline")
	}
}
