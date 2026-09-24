package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

type sitemapDoc struct {
	XMLName xml.Name
	URLs    []struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	} `xml:"url"`
	Sitemaps []struct {
		Loc string `xml:"loc"`
	} `xml:"sitemap"`
}

func readSitemap(t *testing.T, s *Server, path string) sitemapDoc {
	t.Helper()
	w := makeRequest(s, "GET", path, "", "")
	var doc sitemapDoc
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/xml") || xml.Unmarshal(w.Body.Bytes(), &doc) != nil {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	return doc
}

// Every indexable root and room is offered, not a window of the newest
// messages; beyond one file the sitemap pages through an index.
func TestSitemapCoversTheWholeBoard(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "sitemap.sqlite"), board.Config{ArchiveDelaySeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	n := 0
	post := func(c board.Command) string {
		t.Helper()
		n++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), "n"+strconv.Itoa(n)
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
	var roots []string
	for i := 0; i < 130; i++ { // more than the old window of 100 messages
		roots = append(roots, post(board.Command{Operation: "post", Room: "lobby", Text: "root " + strconv.Itoa(i)}))
		post(board.Command{Operation: "post", Room: "lobby", Text: "reply", ReplyTo: roots[i]})
	}
	article := post(board.Command{Operation: "post", Room: "guides", Text: "# A guide & more\n\nBody.", Data: `{"schema":1,"format":"markdown"}`})
	hidden := post(board.Command{Operation: "post", Room: "research", Text: "to be hidden"})
	if err := store.Moderate(context.Background(), hidden, "test", true); err != nil {
		t.Fatal(err)
	}
	post(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"})
	private := post(board.Command{Operation: "post", Room: "secret", Text: "private-data"})
	fingerprint := sha256.Sum256(key.Public().(ed25519.PublicKey))
	own := post(board.Command{Operation: "post", Room: board.PersonalRoom(hex.EncodeToString(fingerprint[:])), Text: "mine"})
	s := New(store, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})

	single := readSitemap(t, s, "/sitemap.xml")
	if single.XMLName.Local != "urlset" {
		t.Fatalf("a small board is one urlset: %s", single.XMLName.Local)
	}
	locs := map[string]string{}
	for _, u := range single.URLs {
		if _, dup := locs[u.Loc]; dup {
			t.Fatalf("listed twice: %s", u.Loc)
		}
		locs[u.Loc] = u.LastMod
	}
	for _, id := range roots {
		if lastmod, ok := locs["https://example.test/e/"+id]; !ok || lastmod == "" {
			t.Fatalf("root %s missing or without lastmod", id)
		}
	}
	for _, want := range []string{"https://example.test/", "https://example.test/rooms", "https://example.test/r/lobby", "https://example.test/r/guides", "https://example.test/e/" + article + "/a-guide-more"} {
		if _, ok := locs[want]; !ok {
			t.Fatalf("%s missing", want)
		}
	}
	for loc := range locs {
		for _, never := range []string{hidden, private, own, "secret", "/r/research", "%40", "/@"} {
			if strings.Contains(loc, never) {
				t.Fatalf("the sitemap lists %s", loc)
			}
		}
	}
	if len(locs) != len(s.sitemapFixedPaths(context.Background()))+2+len(roots)+1 {
		t.Fatalf("unexpected entries: %d", len(locs))
	}

	// Paged: the index names every file, and the files together are the same list.
	defer func(n int) { sitemapURLsPerFile = n }(sitemapURLsPerFile)
	sitemapURLsPerFile = 50
	index := readSitemap(t, s, "/sitemap.xml")
	if index.XMLName.Local != "sitemapindex" || len(index.Sitemaps) != (len(locs)+49)/50 {
		t.Fatalf("index: %s with %d files", index.XMLName.Local, len(index.Sitemaps))
	}
	paged := map[string]string{}
	for i, file := range index.Sitemaps {
		want := "https://example.test/sitemap-" + strconv.Itoa(i+1) + ".xml"
		if file.Loc != want {
			t.Fatalf("index entry %s, want %s", file.Loc, want)
		}
		part := readSitemap(t, s, strings.TrimPrefix(want, "https://example.test"))
		if part.XMLName.Local != "urlset" || len(part.URLs) > 50 {
			t.Fatalf("file %d: %s with %d urls", i+1, part.XMLName.Local, len(part.URLs))
		}
		for _, u := range part.URLs {
			if _, dup := paged[u.Loc]; dup {
				t.Fatalf("paged twice: %s", u.Loc)
			}
			paged[u.Loc] = u.LastMod
		}
	}
	if len(paged) != len(locs) {
		t.Fatalf("paged %d entries, single file had %d", len(paged), len(locs))
	}
	for _, path := range []string{"/sitemap-" + strconv.Itoa(len(index.Sitemaps)+1) + ".xml", "/sitemap-0.xml", "/sitemap-01.xml", "/sitemap-100.xml", "/sitemap-1.xml.gz", "/sitemap-x.xml"} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w := makeRequest(s, "HEAD", "/sitemap-1.xml", "", ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d bytes", w.Code, w.Body.Len())
	}
	if w := makeRequest(s, "POST", "/sitemap-1.xml", "", ""); w.Code != 405 {
		t.Fatalf("POST: %d", w.Code)
	}
	// Builds are bounded; a reader beyond the bound is told to retry.
	for i := 0; i < sitemapBuilds; i++ {
		s.sitemapBuilds <- struct{}{}
	}
	if w := makeRequest(s, "GET", "/sitemap.xml", "", ""); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("saturated: %d", w.Code)
	}
}
