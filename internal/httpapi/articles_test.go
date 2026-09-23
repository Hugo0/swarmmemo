package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Long-form posts are listed at their canonical slug address; a newer version
// is never listed on its own, and replies, plain posts and private rooms are not
// articles. The permalink JSON of an edited post keeps the old bytes.
func TestSitemapListsArticlesAtCanonicalAddress(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "sitemap.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	n := 0
	post := func(c board.Command) board.Receipt {
		t.Helper()
		n++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), strings.Repeat("n", n)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := store.Execute(context.Background(), c, "test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return *res.Receipt
	}
	markdown := `{"schema":1,"format":"markdown"}`
	v1 := post(board.Command{Operation: "post", Room: "guides", Text: "# First\n\nOne.", Data: markdown})
	v2 := post(board.Command{Operation: "post", Room: "guides", Text: "# Second name\n\nTwo.", Data: `{"schema":1,"format":"markdown","supersedes":"` + v1.ID + `"}`})
	reply := post(board.Command{Operation: "post", Room: "guides", Text: "# A reply", ReplyTo: v1.ID, Data: markdown})
	s := New(store, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})

	w := makeRequest(s, "GET", "/sitemap.xml", "", "")
	var sitemap struct {
		URLs []struct {
			Loc     string `xml:"loc"`
			LastMod string `xml:"lastmod"`
		} `xml:"url"`
	}
	if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &sitemap) != nil {
		t.Fatalf("sitemap: %d", w.Code)
	}
	locs := map[string]string{}
	for _, u := range sitemap.URLs {
		locs[u.Loc] = u.LastMod
	}
	article := "https://example.test/e/" + v1.ID + "/second-name"
	if locs[article] == "" {
		t.Fatalf("article missing from sitemap: %v", locs)
	}
	for loc := range locs {
		if strings.Contains(loc, v2.ID) || loc == "https://example.test/e/"+v1.ID {
			t.Fatalf("sitemap lists a non-canonical version address %s", loc)
		}
	}
	if _, ok := locs["https://example.test/e/"+reply.ID+"/a-reply"]; ok {
		t.Fatal("a reply is not an article")
	}

	// The old version's machine permalink is unchanged apart from the pointer.
	w = makeRequest(s, "GET", "/e/"+v1.ID+"?format=json", "", "")
	var read struct {
		Messages []board.Message `json:"messages"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &read); err != nil || len(read.Messages) != 1 {
		t.Fatalf("read back: %v %s", err, w.Body.String())
	}
	if m := read.Messages[0]; m.Hash != v1.Hash || m.SupersededBy != v2.ID || m.Format != "markdown" {
		t.Fatalf("old version read back: %+v", m)
	}
}
