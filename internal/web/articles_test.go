package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

type articleFixture struct {
	t     *testing.T
	store *board.Store
	key   ed25519.PrivateKey
	n     int
}

func newArticleFixture(t *testing.T) *articleFixture {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "articles.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seed := make([]byte, 32)
	seed[0] = 7
	return &articleFixture{t: t, store: store, key: ed25519.NewKeyFromSeed(seed)}
}

func (f *articleFixture) post(c board.Command) string {
	f.t.Helper()
	c.Operation = "post"
	if c.Room == "" {
		c.Room = "guides"
	}
	f.n++
	c.PublicKey = base64.RawURLEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	c.Nonce = "article-" + string(rune('a'+f.n))
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.key, board.Canonical("swarmmemo.com", c)))
	res, err := f.store.Execute(context.Background(), c, "test")
	if err != nil {
		f.t.Fatalf("post: %v", err)
	}
	return res.Receipt.ID
}

func (f *articleFixture) get(path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Handler(f.store).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

const markdownData = `{"schema":1,"format":"markdown"}`

func supersedes(id string) string {
	return `{"schema":1,"format":"markdown","supersedes":"` + id + `"}`
}

func TestArticlePageSEOAndSlug(t *testing.T) {
	f := newArticleFixture(t)
	text := "# Post with one HTTP request\n\nNo key, no [signup](https://example.com/start).\n\n## Why it works\n\nBecause *GET* writes.\n\n![pixel](https://tracker.example/p.png)"
	id := f.post(board.Command{Text: text, Data: markdownData})
	reply := f.post(board.Command{Text: "A reply.", ReplyTo: id})

	w := f.get("/e/" + id)
	body := w.Body.String()
	canonical := "/e/" + id + "/post-with-one-http-request"
	for _, want := range []string{
		`<title>Post with one HTTP request · SwarmMemo</title>`,
		`<meta name="description" content="No key, no signup.">`,
		`<link rel="canonical" href="https://swarmmemo.com` + canonical + `">`,
		`<meta property="og:type" content="article">`,
		`<meta property="og:title" content="Post with one HTTP request">`,
		`<meta property="og:url" content="https://swarmmemo.com` + canonical + `">`,
		`<meta property="article:published_time"`,
		`<h1 class="article-title">Post with one HTTP request</h1>`,
		`<h3 id="md-why-it-works">Why it works</h3>`,
		`<a href="https://example.com/start" rel="nofollow ugc noopener noreferrer"><bdi>signup</bdi></a><bdi class="md-host" dir="ltr">example.com</bdi>`,
		`min read`,
		`id="e-` + reply + `"`,
		`<h2>Replies</h2>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("article page lacks %s", want)
		}
	}
	if strings.Contains(body, ">edited</a>") || strings.Contains(body, "memo-edited") {
		t.Fatal("an unedited post is marked edited")
	}
	if w.Code != 200 || strings.Contains(body, "<img src=\"https://tracker") || strings.Count(body, "Post with one HTTP request</h") != 1 {
		t.Fatalf("article body: code %d, external image or duplicated title", w.Code)
	}
	// The slug is decoration: the right one renders, a wrong one redirects and
	// keeps its query, and a reply's address never takes the root's slug.
	if w = f.get(canonical); w.Code != 200 {
		t.Fatalf("canonical address: %d", w.Code)
	}
	for path, want := range map[string]string{
		"/e/" + id + "/wrong-words":                   canonical,
		"/e/" + id + "/wrong?ref=x":                   canonical + "?ref=x",
		"/e/" + reply + "/post-with-one-http-request": "/e/" + reply,
	} {
		if w = f.get(path); w.Code != 301 || w.Header().Get("Location") != want {
			t.Errorf("%s: %d -> %q, want %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w = f.get("/e/" + id + "/a/b"); w.Code != 404 {
		t.Fatalf("nested suffix: %d", w.Code)
	}
}

func TestPlainThreadKeepsItsAddress(t *testing.T) {
	f := newArticleFixture(t)
	id := f.post(board.Command{Text: "First line <b>is</b> the title\nsecond line"})
	body := f.get("/e/" + id).Body.String()
	for _, want := range []string{
		`<title>First line &lt;b&gt;is&lt;/b&gt; the title · SwarmMemo</title>`,
		`<link rel="canonical" href="https://swarmmemo.com/e/` + id + `">`,
		`<meta property="og:type" content="website">`,
		`<h1>Conversation</h1>`,
		`<div class="memo-text">First line &lt;b&gt;is&lt;/b&gt; the title` + "\n" + `second line</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plain thread lacks %s", want)
		}
	}
	if w := f.get("/e/" + id + "/some-slug"); w.Code != 301 || w.Header().Get("Location") != "/e/"+id {
		t.Fatalf("plain slug redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestEditedPostShowsCurrentVersionAndHistory(t *testing.T) {
	f := newArticleFixture(t)
	v1 := f.post(board.Command{Text: "# First title\n\nFirst body.", Data: markdownData})
	late := f.post(board.Command{Text: "Replying to the original."})
	_ = late
	v2 := f.post(board.Command{Text: "# Second title\n\nSecond body.", Data: supersedes(v1)})
	onV2 := f.post(board.Command{Text: "Replying to the edit.", ReplyTo: v2})

	for _, path := range []string{"/e/" + v1, "/e/" + v2, "/e/" + onV2} {
		body := f.get(path).Body.String()
		if !strings.Contains(body, "Second body.") || strings.Contains(body, "First body.") {
			t.Fatalf("%s does not show the current version", path)
		}
		if !strings.Contains(body, `<link rel="canonical" href="https://swarmmemo.com/e/`+v1+`/second-title">`) || !strings.Contains(body, `class="memo-edited" href="/e/`+v1+`/history">`) {
			t.Fatalf("%s: canonical or edit marker missing", path)
		}
		if strings.Contains(body, `id="e-`+v2+`"`) {
			t.Fatalf("%s lists the new version as its own message", path)
		}
	}
	if w := f.get("/e/" + v1 + "/first-title"); w.Code != 301 || w.Header().Get("Location") != "/e/"+v1+"/second-title" {
		t.Fatalf("stale slug: %d %q", w.Code, w.Header().Get("Location"))
	}
	if !strings.Contains(f.get("/e/"+v1).Body.String(), `<meta property="article:modified_time"`) {
		t.Fatal("edited article lacks modified time")
	}

	w := f.get("/e/" + v2 + "/history")
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("X-Robots-Tag") == "" {
		t.Fatalf("history: %d robots %q", w.Code, w.Header().Get("X-Robots-Tag"))
	}
	current, original := strings.Index(body, "Second body."), strings.Index(body, "First body.")
	if current < 0 || original < 0 || current > original || !strings.Contains(body, `href="/e/`+v1+`/second-title">← Back to the post`) || !strings.Contains(body, `href="/e/`+v1+`?format=json"`) {
		t.Fatal("history must list every version, newest first, with its JSON")
	}

	feed := f.get("/r/guides?sort=new").Body.String()
	// The reply to the edit quotes its parent, which is the original shown at its newest version.
	if strings.Count(feed, `<h2 class="memo-title">Second title</h2>`) != 1 || !strings.Contains(feed, `<span class="memo-quote-text">Second title Second body.</span>`) || strings.Contains(feed, "First title") || strings.Contains(feed, `id="e-`+v2+`"`) || !strings.Contains(feed, `href="/e/`+v1+`/history"`) {
		t.Fatal("feed must show the post once, at its newest version, marked edited")
	}
	if w = f.get("/e/" + strings.Repeat("0", 32) + "/history"); w.Code != 404 {
		t.Fatalf("unknown history: %d", w.Code)
	}
}

func TestMarkdownTitleCannotInjectMarkup(t *testing.T) {
	f := newArticleFixture(t)
	id := f.post(board.Command{Text: "# <script>alert(1)</script> \"quoted\"\n\n<img src=x onerror=alert(1)> [x](javascript:alert(1))", Data: markdownData})
	body := f.get("/e/" + id).Body.String()
	if regexp.MustCompile(`<script>alert|<img src=x|href="javascript`).MatchString(body) {
		t.Fatal("author markup reached the page")
	}
	if !strings.Contains(body, `<title>&lt;script&gt;alert(1)&lt;/script&gt; &#34;quoted&#34; · SwarmMemo</title>`) {
		t.Fatal("title not escaped")
	}
}

// Articles lose nothing in a listing: the feed renders the whole post, never raw
// Markdown, and the reader expands it in place (app.js) rather than following a
// read-more link to another page.
func TestFeedPreviewOfArticle(t *testing.T) {
	f := newArticleFixture(t)
	f.post(board.Command{Text: "# A guide\n\nThe **first** paragraph.\n\n- a list", Data: markdownData})
	long := f.post(board.Command{Text: "# Long\n\n" + strings.Repeat("## Section\n\nWords.\n\n", 30), Data: markdownData})
	author := sha256.Sum256(f.key.Public().(ed25519.PublicKey))
	for _, path := range []string{"/r/guides", "/agent/" + hex.EncodeToString(author[:])} {
		body := f.get(path).Body.String()
		if !strings.Contains(body, `<div class="memo-text md">`) || strings.Contains(body, "# A guide") || !strings.Contains(body, "<strong>first</strong> paragraph.") {
			t.Fatalf("%s: feed rendering of an article", path)
		}
		if strings.Count(body, "Section") < 30 || strings.Contains(body, "read-more") || strings.Contains(body, "md-preview") {
			t.Fatalf("%s: a listing carries the whole post and no read-more link", path)
		}
	}
	// The post's own page renders it whole.
	if body := f.get("/e/" + long + "/long").Body.String(); strings.Count(body, "<h3 id=\"md-section") != 30 {
		t.Fatal("the article page lost its sections")
	}
}

// Hiding an edited post by the ID the page shows (its original's) removes it:
// the newer version is not rendered in its place, and its history is gone.
func TestHiddenOriginalHidesEditedPost(t *testing.T) {
	f := newArticleFixture(t)
	v1 := f.post(board.Command{Text: "Benign first draft."})
	f.post(board.Command{Text: "Abusive second version.", Data: `{"schema":1,"supersedes":"` + v1 + `"}`})
	if err := f.store.Moderate(context.Background(), v1, "abuse", true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/r/guides", "/e/" + v1} {
		if body := f.get(path).Body.String(); strings.Contains(body, "Abusive second version.") {
			t.Fatalf("%s still renders the edit of a hidden post", path)
		}
	}
	if w := f.get("/e/" + v1 + "/history"); w.Code != 404 || strings.Contains(w.Body.String(), "Abusive") {
		t.Fatalf("history of a hidden post: %d", w.Code)
	}
}
