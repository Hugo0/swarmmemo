package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/markdown"
	"swarmmemo/internal/web"
)

func TestGuidesInPublicDiscovery(t *testing.T) {
	f := &fakeService{}
	s := New(f, web.Handler(f), Config{PublicURL: "https://example.test"})
	w := makeRequest(s, "GET", "/sitemap.xml", "", "")
	var sitemap struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &sitemap) != nil {
		t.Fatal("invalid sitemap")
	}
	for _, path := range web.PublicGuidePaths() {
		count := 0
		for _, item := range sitemap.URLs {
			if item.Loc == "https://example.test"+path {
				count++
			}
		}
		if count != 1 {
			t.Errorf("sitemap contains %s %d times", path, count)
		}
		w = makeRequest(s, "GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "</html>") {
			t.Errorf("HTTP API did not dispatch guide %s", path)
		}
	}
	for _, path := range []string{"/llms.txt", "/skill.md"} {
		w = makeRequest(s, "GET", path, "", "")
		if !strings.Contains(w.Body.String(), "[Agent communication guides and related projects](https://example.test/guides)") {
			t.Errorf("guide missing from %s", path)
		}
	}
}

// A legacy guide that now redirects to its post leaves the sitemap; the post
// is listed at its article address instead. An impostor's post changes nothing.
func TestSitemapDropsMovedGuides(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "moved.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operator, impostor := ed25519.NewKeyFromSeed(make([]byte, 32)), ed25519.NewKeyFromSeed(append([]byte{1}, make([]byte, 31)...))
	previous := web.GuideAuthors()
	sum := sha256.Sum256(operator.Public().(ed25519.PublicKey))
	if err = web.SetGuideAuthors(hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = web.SetGuideAuthors(strings.Join(previous, ",")) })
	n := 0
	post := func(key ed25519.PrivateKey, text string) string {
		t.Helper()
		n++
		c := board.Command{Operation: "post", Room: "guides", Text: text, Data: `{"schema":1,"format":"markdown"}`}
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), strings.Repeat("n", n)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := store.Execute(context.Background(), c, "test")
		if err != nil {
			t.Fatal(err)
		}
		return res.Receipt.ID
	}
	s := New(store, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})
	locs := func() map[string]bool {
		var sitemap struct {
			URLs []struct {
				Loc string `xml:"loc"`
			} `xml:"url"`
		}
		if xml.Unmarshal(makeRequest(s, "GET", "/sitemap.xml", "", "").Body.Bytes(), &sitemap) != nil {
			t.Fatal("invalid sitemap")
		}
		found := map[string]bool{}
		for _, u := range sitemap.URLs {
			found[u.Loc] = true
		}
		return found
	}
	post(impostor, "# 4chan for agents\n\nNot the operator.")
	if !locs()["https://example.test/guides/4chan-for-agents"] {
		t.Fatal("an impostor's post removed a legacy guide from the sitemap")
	}
	id := post(operator, "# 4chan for agents\n\nThe guide, as a post.")
	found := locs()
	if found["https://example.test/guides/4chan-for-agents"] || !found["https://example.test/e/"+id+"/4chan-for-agents"] {
		t.Fatalf("moved guide not swapped for its post: %v", found)
	}
	for _, path := range []string{"/guides", "/guides/agent-board-map", "/guides/http-agent-messaging"} {
		if !found["https://example.test"+path] {
			t.Errorf("sitemap lost %s", path)
		}
	}
	if w := makeRequest(s, "GET", "/guides/4chan-for-agents", "", ""); w.Code != 301 || w.Header().Get("Location") != "/e/"+id+"/4chan-for-agents" {
		t.Fatalf("legacy address: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// The guide drafts under deploy/ are posted as they are, so they meet the same
// drift rules as served copy: each title slugs to the legacy address it takes
// over, each fits in one message, internal links resolve, repository links
// point at published files, and no retired claim or term comes back.
func TestGuidePostDrafts(t *testing.T) {
	manifests, _ := filepath.Glob("../../deploy/guides-*/manifest.json")
	if len(manifests) == 0 {
		t.Skip("no guide drafts")
	}
	s := realServer(t)
	published := publicSnapshotFiles(t)
	legacy := map[string]bool{}
	for _, path := range web.PublicGuidePaths() {
		legacy[path] = true
	}
	for _, manifestPath := range manifests {
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]struct {
			File, Persona string
			RequestID     string `json:"request_id"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest) == 0 {
			t.Fatalf("%s: %v", manifestPath, err)
		}
		for slug, entry := range manifest {
			path := filepath.Join(filepath.Dir(manifestPath), entry.File)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.TrimSpace(string(raw))
			if got := markdown.Slug(markdown.Title(text), 60); got != slug || !legacy["/guides/"+slug] || slug == "agent-board-map" {
				t.Errorf("%s: title slugs to %q, want the legacy guide %q", path, got, slug)
			}
			if int64(len(text)) > board.LimitValue("text_bytes") {
				t.Errorf("%s: %d bytes, over the %s message limit", path, len(text), board.LimitText("text_bytes"))
			}
			if (entry.Persona != "weaver" && entry.Persona != "khepri") || entry.RequestID != "guide-"+slug+"-v1" {
				t.Errorf("%s: persona %q, request_id %q", path, entry.Persona, entry.RequestID)
			}
			for _, stale := range stalePhrases {
				if m := regexp.MustCompile(stale.pattern).FindString(text); m != "" {
					t.Errorf("%s says %q: %s", path, m, stale.why)
				}
			}
			for _, m := range repoLink.FindAllStringSubmatch(text, -1) {
				if !published[m[1]] {
					t.Errorf("%s links to %s, which the public snapshot does not publish", path, m[1])
				}
			}
			for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
				ref, err := url.Parse(m[1])
				if err != nil || ref.Host != "" || !strings.HasPrefix(ref.Path, "/") {
					continue
				}
				// Message pages need data this empty store does not have.
				if strings.HasPrefix(ref.Path, "/e/") || strings.HasPrefix(ref.Path, "/r/") || strings.HasPrefix(ref.Path, "/agent/") {
					continue
				}
				target := ref.Path
				if ref.RawQuery != "" {
					target += "?" + ref.RawQuery
				}
				w := get(s, target, "text/html")
				if w.Code >= 400 && !(ref.Path == "/references" && w.Code == 503) {
					t.Errorf("%s links to %s, which answers %d", path, m[1], w.Code)
					continue
				}
				if ref.Fragment == "" {
					continue
				}
				if strings.HasSuffix(ref.Path, ".md") || strings.HasSuffix(ref.Path, ".txt") {
					if !markdownAnchors(w.Body.String())[ref.Fragment] {
						t.Errorf("%s links to %s, which has no heading #%s", path, ref.Path, ref.Fragment)
					}
				} else if !strings.Contains(w.Body.String(), `id="`+ref.Fragment+`"`) {
					t.Errorf("%s links to %s, which has no id %q", path, ref.Path, ref.Fragment)
				}
			}
		}
	}
}

func TestRenderedGuideWriteExamples(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "guides.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com"})
	// The guide renders the shared quickstart block; the examples it shows must
	// still be executable exactly as printed.
	rendered := makeRequest(s, "GET", "/guides/http-agent-messaging", "", "").Body.String()
	writes := []url.Values{}
	targets := []string{}
	reads := []string{}
	for _, code := range regexp.MustCompile(`<pre><code>([\s\S]*?)</code></pre>`).FindAllStringSubmatch(rendered, -1) {
		text := html.UnescapeString(code[1])
		match := regexp.MustCompile(`curl -sS --get 'https://swarmmemo.com([^']+)'`).FindStringSubmatch(text)
		if len(match) != 2 {
			continue
		}
		// The form style also encodes read parameters, such as the return read's
		// opaque cursor. Only write endpoints belong in the executed-write set.
		if !strings.HasPrefix(match[1], "/w/") {
			reads = append(reads, text)
			continue
		}
		fields := url.Values{}
		for _, field := range regexp.MustCompile(`--data-urlencode '([^']+)'`).FindAllStringSubmatch(text, -1) {
			key, value, ok := strings.Cut(field[1], "=")
			if !ok {
				t.Fatal("invalid rendered form example")
			}
			fields.Set(key, value)
		}
		writes, targets = append(writes, fields), append(targets, match[1])
	}
	if len(targets) != 2 || targets[0] != "/w/lobby/main" || targets[1] != "/w/ROOM/PAGE" {
		t.Fatalf("guide must render the canonical post and reply writes: %v", targets)
	}
	// The return read is shown in the same executable form and must be a read:
	// rendering it must not be able to publish anything.
	if len(reads) != 1 || !strings.Contains(reads[0], "/api/updates") {
		t.Fatalf("guide must render the return read exactly once: %v", reads)
	}
	if writes[0].Get("format") != "json" || writes[0].Get("request_id") == "" || writes[1].Get("reply_to") != "RECEIPT_ID" {
		t.Fatal("rendered examples lost JSON selection, retry identity or reply target")
	}
	// Placeholders are replaced the way a reader would: a fresh retry ID per
	// intended memo, and the accepted receipt as the reply target.
	writes[0].Set("request_id", "guide-post-fixture")
	writes[1].Set("request_id", "guide-reply-fixture")
	var rootID string
	for index, fields := range writes {
		target := targets[index]
		if index == 1 {
			target = "/w/lobby/main"
			fields.Set("reply_to", rootID)
		}
		var firstID string
		for attempt := 0; attempt < 2; attempt++ {
			w := makeRequest(s, "GET", target+"?"+fields.Encode(), "", "")
			var result board.Result
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.OK || result.Receipt == nil || result.Receipt.ID == "" {
				t.Fatalf("rendered example %d failed: %d %s", index, w.Code, w.Body.String())
			}
			if attempt == 0 {
				firstID = result.Receipt.ID
			} else if result.Receipt.ID != firstID || !result.Receipt.Duplicate {
				t.Fatal("retry was not idempotent")
			}
		}
		if index == 0 {
			rootID = firstID
		}
	}
	w := makeRequest(s, "GET", "/api/messages?room=lobby&page=main&limit=10", "", "")
	var feed board.Result
	if json.Unmarshal(w.Body.Bytes(), &feed) != nil || len(feed.Messages) != 2 {
		t.Fatal("examples did not produce exactly two memos")
	}
	if feed.Messages[1].ReplyTo != rootID {
		t.Fatal("rendered reply did not bind to the accepted receipt")
	}
	for _, event := range feed.Messages {
		if event.Text == "" || strings.Contains(event.Text, "%") {
			t.Fatal("example text did not survive encoding")
		}
	}
}
