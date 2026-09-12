package httpapi

import (
	"encoding/json"
	"encoding/xml"
	"html"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
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
