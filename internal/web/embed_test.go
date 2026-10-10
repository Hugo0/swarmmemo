package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestEmbedDocuments(t *testing.T) {
	for _, path := range []string{"/embed", "/embed.json"} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if path == "/embed.json" {
			var doc embedDocument
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Snippet != embedSnippet || len(doc.Sections) != 9 {
				t.Fatalf("incomplete JSON: %+v", doc)
			}
		}
		// The widget never adds text to a comment; the docs must not say it does.
		if strings.Contains(w.Body.String(), "appended") {
			t.Fatalf("%s claims the widget appends text to comments", path)
		}
		for _, want := range []string{"Embed a room anywhere", "data-room", "data-page", "--sm-heading-font", "public dataset", "localStorage", "room policy", "webhook add https://your.site/hook", "X-SwarmMemo-Delivery", "hmac.compare_digest", "/api/updates?agent=FP"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
	}
}

func TestEmbedScript(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/embed/v1.js", nil))
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("script: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("Cache-Control") != EmbedCacheControl || w.Header().Get("ETag") == "" {
		t.Fatal(w.Header())
	}
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	_, _ = z.Write(w.Body.Bytes())
	_ = z.Close()
	// 13 KiB since the shared work line (memo-core.js workLine, C33); still
	// under the first widget's 15.
	if compressed.Len() >= 13*1024 {
		t.Fatalf("widget exceeds gzip budget: %d", compressed.Len())
	}
	t.Logf("widget: %d bytes, %d gzipped", w.Body.Len(), compressed.Len())
	// One shared core (memo-core.js, also loaded by app.js) inside one closure:
	// the host page gains no globals.
	core, _ := files.ReadFile("assets/memo-core.js")
	if !strings.HasPrefix(w.Body.String(), "(() => {\n") || !strings.HasSuffix(w.Body.String(), "})();\n") || !bytes.Contains(w.Body.Bytes(), compactScript(core)) {
		t.Fatal("embed bundle must wrap memo-core.js and embed-v1.js in one closure")
	}
	if strings.Contains(w.Body.String(), "innerHTML") {
		t.Fatal("widget must use text nodes")
	}
	if board.SlugMaxChars != 64 || board.HandleMaxChars != 32 {
		t.Fatal("update embed input validation to match server")
	}
}
