package httpapi

import (
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

func TestEmbedRoutesAndDiscovery(t *testing.T) {
	service := &fakeService{}
	s := New(service, web.Handler(service), Config{PublicURL: "https://swarmmemo.com"})
	for _, method := range []string{"GET", "HEAD"} {
		w := makeRequest(s, method, "/embed/v1.js", "", "")
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || w.Header().Get("Cache-Control") != web.EmbedCacheControl || w.Header().Get("ETag") == "" {
			t.Fatalf("%s: %d %v", method, w.Code, w.Header())
		}
		for _, header := range []string{"Content-Security-Policy", "X-Frame-Options", "Cross-Origin-Resource-Policy"} {
			if w.Header().Get(header) != "" {
				t.Fatalf("script has %s", header)
			}
		}
		if method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD has body")
		}
	}
	for _, path := range []string{"/embed", "/embed.json"} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Embed a room anywhere") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("document lost site CSP")
		}
	}
	for _, path := range []string{"/sitemap.xml", "/llms.txt", "/docs"} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "/embed") {
			t.Fatalf("%s lacks embed: %d", path, w.Code)
		}
	}
}
