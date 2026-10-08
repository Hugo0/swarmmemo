package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// /e/ID/text is the posted bytes: their SHA-256 is the log leaf's
// text_sha256, sent as X-Content-SHA256 and the ETag; HEAD has the headers
// and no body; If-None-Match is a 304.
func TestPostTextHashesToLeaf(t *testing.T) {
	store, s, post := postTextFixture(t)
	text := "Exact bytes: café  \n\ttabbed ✓\n\ntrailing space "
	id := post(board.Command{Operation: "post", Room: "lobby", Text: text})
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := get(s, "/api/log/proof?message="+id, "")
	var proof board.LogInclusion
	if err := json.Unmarshal(w.Body.Bytes(), &proof); err != nil || w.Code != 200 {
		t.Fatalf("proof: %d %s", w.Code, w.Body)
	}
	var leaf struct {
		TextSHA256 string `json:"text_sha256"`
	}
	if err := json.Unmarshal([]byte(proof.Leaf.Data), &leaf); err != nil || leaf.TextSHA256 == "" {
		t.Fatalf("leaf: %s", proof.Leaf.Data)
	}

	for _, accept := range []string{"", "text/html", "application/json"} {
		w = get(s, "/e/"+id+"/text", accept)
		sum := sha256.Sum256(w.Body.Bytes())
		if w.Code != 200 || w.Body.String() != text || hex.EncodeToString(sum[:]) != leaf.TextSHA256 {
			t.Fatalf("Accept %q: %d %q, sha256 %x, leaf %s", accept, w.Code, w.Body, sum, leaf.TextSHA256)
		}
		h := w.Header()
		if h.Get("Content-Type") != "text/plain; charset=utf-8" || h.Get("X-Content-SHA256") != leaf.TextSHA256 ||
			h.Get("ETag") != `"`+leaf.TextSHA256+`"` || h.Get("Content-Length") != strconv.Itoa(len(text)) ||
			!strings.HasPrefix(h.Get("Cache-Control"), "public, max-age=") || !strings.Contains(h.Get("Access-Control-Expose-Headers"), "X-Content-SHA256") {
			t.Fatalf("Accept %q headers: %v", accept, h)
		}
	}

	head := request(s, "HEAD", "/e/"+id+"/text", "")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("X-Content-SHA256") != leaf.TextSHA256 || head.Header().Get("Content-Length") != strconv.Itoa(len(text)) {
		t.Fatalf("HEAD: %d %q %v", head.Code, head.Body, head.Header())
	}
	again := request(s, "GET", "/e/"+id+"/text", "", "If-None-Match", `"`+leaf.TextSHA256+`"`)
	if again.Code != 304 || again.Body.Len() != 0 {
		t.Fatalf("If-None-Match: %d %q", again.Code, again.Body)
	}
	if post := request(s, "POST", "/e/"+id+"/text", ""); post.Code != 405 {
		t.Fatalf("POST: %d", post.Code)
	}

	// Each version is its own address with its own bytes.
	v2text := "The edited words"
	v2 := post(board.Command{Operation: "post", Room: "lobby", Text: v2text, Data: `{"schema":1,"supersedes":"` + id + `"}`})
	if w = get(s, "/e/"+v2+"/text", ""); w.Code != 200 || w.Body.String() != v2text {
		t.Fatalf("version: %d %q", w.Code, w.Body)
	}
	if w = get(s, "/e/"+id+"/text", ""); w.Code != 200 || w.Body.String() != text {
		t.Fatalf("original after edit: %d %q", w.Code, w.Body)
	}

	// The post's page and its proof page link the text, nofollow; the page's
	// link names the version it shows.
	page := get(s, "/e/"+id, "text/html").Body.String()
	if !strings.Contains(page, `href="/e/`+v2+`/text" rel="nofollow"`) {
		t.Fatalf("post page lacks the text link: %s", page)
	}
	proofPage := get(s, "/e/"+id+"/proof", "text/html").Body.String()
	if !strings.Contains(proofPage, `href="/e/`+id+`/text" rel="nofollow"`) {
		t.Fatalf("proof page lacks the text link: %s", proofPage)
	}
}

// A post /e/ID would not show the words of is refused, with the status /e/ID
// gives (404 for a private room or an unknown ID) or 410 for a removed post,
// whose /e/ID is a tombstone; no refusal carries the text.
func TestPostTextRefusesWhatEventHides(t *testing.T) {
	store, s, post := postTextFixture(t)
	hidden := post(board.Command{Operation: "post", Room: "lobby", Text: "Doomed secret words"})
	edited := post(board.Command{Operation: "post", Room: "lobby", Text: "Original then removed"})
	later := post(board.Command{Operation: "post", Room: "lobby", Text: "Later version words", Data: `{"schema":1,"supersedes":"` + edited + `"}`})
	post(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"})
	private := post(board.Command{Operation: "post", Room: "secret", Text: "Private room words"})
	for _, id := range []string{hidden, edited} {
		if err := store.Moderate(context.Background(), id, "spam", true); err != nil {
			t.Fatal(err)
		}
	}

	// /e/ID for the same reader: the private post is not there; the hidden
	// one is a tombstone with no text.
	if w := get(s, "/e/"+private+"?format=json", "application/json"); w.Code != 404 || strings.Contains(w.Body.String(), "Private room") {
		t.Fatalf("/e/ private: %d %s", w.Code, w.Body)
	}
	if w := get(s, "/e/"+hidden+"?format=json", "application/json"); strings.Contains(w.Body.String(), "Doomed") || !strings.Contains(w.Body.String(), "tombstone") {
		t.Fatalf("/e/ hidden: %d %s", w.Code, w.Body)
	}

	for path, code := range map[string]int{
		"/e/" + private + "/text":                 404,
		"/e/" + strings.Repeat("0", 32) + "/text": 404,
		"/e/nope/text":                            404,
		"/e/" + hidden + "/text":                  410,
		"/e/" + edited + "/text":                  410,
		"/e/" + later + "/text":                   410,
	} {
		for _, method := range []string{"GET", "HEAD"} {
			w := request(s, method, path, "")
			body := w.Body.String()
			if w.Code != code || strings.Contains(body, "words") || w.Header().Get("X-Content-SHA256") != "" || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: %d (want %d) %s %v", method, path, w.Code, code, body, w.Header())
			}
		}
	}

	// Neither page offers a text link for the removed post.
	if page := get(s, "/e/"+hidden, "text/html").Body.String(); strings.Contains(page, "/text\"") {
		t.Fatal("hidden post page links its text")
	}
	if page := get(s, "/e/"+hidden+"/proof", "text/html").Body.String(); strings.Contains(page, hidden+"/text") {
		t.Fatal("hidden post's proof page links its text")
	}
}

// An article titled "Text" never takes /e/ID/text as its slug.
func TestPostTextArticleSlug(t *testing.T) {
	_, s, post := postTextFixture(t)
	id := post(board.Command{Operation: "post", Room: "lobby", Text: "# Text\n\nAn article.", Data: `{"schema":1,"format":"markdown"}`})
	w := get(s, "/e/"+id, "text/html")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `/e/`+id+`/text-1"`) {
		t.Fatalf("article canonical: %d", w.Code)
	}
	if w = get(s, "/e/"+id+"/text", "text/html"); w.Code != 200 || w.Body.String() != "# Text\n\nAn article." {
		t.Fatalf("article text: %d %q", w.Code, w.Body)
	}
}

func postTextFixture(t *testing.T) (*board.Store, *Server, func(board.Command) string) {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "posttext.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	n := 0
	post := func(c board.Command) string {
		t.Helper()
		n++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), "txt"+strconv.Itoa(n)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := store.Execute(context.Background(), c, "test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		if res.Receipt == nil {
			return ""
		}
		return res.Receipt.ID
	}
	s := New(store, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})
	return store, s, post
}
