package services_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return b.Bytes()
}

// C884: a JSON listing past 256 KiB is read whole, paged with offset (the
// windows join into the body exactly, next_offset is absent on the last),
// each window priced like the first for the same bytes and read upstream
// once; field reads a value near its end.
func TestFetchPagesALargeJSONBody(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	n := 0
	for ; b.Len() < 700<<10; n++ {
		if n > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"title":"listing %d €","price":%d}`, n, n, 1000+n)
	}
	b.WriteString(`]}`)
	body := b.String()
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/listings.json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		},
	})
	r := newFetchRig(t, s, fetchOpts{})

	var got strings.Builder
	offset, windows := 0, 0
	for i := 0; ; i++ {
		args := noScreen("http://site.test/listings.json")
		args["max_bytes"], args["offset"] = services.FetchTextMax, offset
		out := r.must("alice", args)
		res := get(out, "result").(map[string]any)
		text := res["text"].(string)
		if res["page_bytes"] != float64(len(body)) || res["text_bytes"] != float64(len(body)) || res["body_limit_bytes"] != float64(services.FetchBodyBytes) {
			t.Fatalf("window %d: %+v", i, res["truncated_by"])
		}
		// Every window costs what the first did for the same bytes: 5 + 1 per KiB.
		if c := get(out, "call", "cost"); c != float64(services.FetchPrice.For(int64(len(text)))) {
			t.Fatalf("window %d of %d bytes cost %v, want %d", i, len(text), c, services.FetchPrice.For(int64(len(text))))
		}
		windows++
		got.WriteString(text)
		next, ok := res["next_offset"].(float64)
		if !ok {
			break
		}
		offset = int(next)
	}
	if got.String() != body {
		t.Fatalf("windows join to %d bytes, want %d", got.Len(), len(body))
	}
	if windows < 7 {
		t.Fatalf("%d windows for %d bytes", windows, len(body))
	}
	if h := s.hit("/listings.json"); h != 1 {
		t.Fatalf("paging read upstream %d times", h)
	}

	// field past 256 KiB: the last item's price.
	args := noScreen("http://site.test/listings.json")
	args["field"] = fmt.Sprintf("/items/%d/price", n-1)
	res := get(r.must("bob", args), "result").(map[string]any)
	if res["field_value"] != float64(1000+n-1) {
		t.Fatalf("field past 256 KiB: %v", res["field_value"])
	}
}

// C884: a gzip sitemap is read decompressed as XML, typed as gzip or untyped
// from a .gz URL; raw_sha256 is of the gzip bytes as received. A body that
// inflates past the cap (a bomb) is cut there, never held whole, and a .gz
// that holds no text (a tar) is refused.
func TestFetchReadsGzipBodies(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "<url><loc>https://site.test/l/%d</loc></url>\n", i)
	}
	b.WriteString(`</urlset>`)
	sitemap := b.String()
	gz := gzipped(t, sitemap)
	bomb := gzipped(t, "<x>"+strings.Repeat("a", 6*services.FetchBodyBytes))
	tarball := gzipped(t, "file.txt\x00\x00\x00\x00\x00\x00\x00\x00"+strings.Repeat("\x00", 500))
	serve := func(ct string, body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Write(body)
		}
	}
	s := newSite(t, false, map[string]http.HandlerFunc{
		"/sitemap.xml.gz": serve("application/octet-stream", gz),
		"/sitemap":        serve("application/x-gzip", gz),
		"/bomb.xml.gz":    serve("application/gzip", bomb),
		"/files.tar.gz":   serve("application/gzip", tarball),
	})
	r := newFetchRig(t, s, fetchOpts{})
	sum := sha256.Sum256(gz)
	for _, path := range []string{"/sitemap.xml.gz", "/sitemap"} {
		args := noScreen("http://site.test" + path)
		args["max_bytes"] = services.FetchTextMax
		res := get(r.must("alice", args), "result").(map[string]any)
		if res["format"] != "xml" || res["gzip"] != true || res["raw_sha256"] != hex.EncodeToString(sum[:]) || res["raw_bytes"] != float64(len(gz)) || res["page_bytes"] != float64(len(sitemap)) {
			t.Fatalf("%s: format %v gzip %v raw %v page %v", path, res["format"], res["gzip"], res["raw_bytes"], res["page_bytes"])
		}
		if text := res["text"].(string); !strings.HasPrefix(sitemap, text) || res["next_offset"] == nil || res["text_bytes"] != float64(len(sitemap)) {
			t.Fatalf("%s: text %d bytes of %v", path, len(text), res["text_bytes"])
		}
	}
	res := get(r.must("alice", noScreen("http://site.test/bomb.xml.gz")), "result").(map[string]any)
	if res["truncated"] != true || !sameStrings(res["truncated_by"], "body", "text") || res["page_bytes"] != float64(services.FetchBodyBytes) || res["raw_bytes"] != float64(len(bomb)) {
		t.Fatalf("bomb: %+v page %v raw %v", res["truncated_by"], res["page_bytes"], res["raw_bytes"])
	}
	if _, err := r.fetch("alice", noScreen("http://site.test/files.tar.gz")); code(err) != "fetch_unsupported_type" {
		t.Fatalf("a tar inside gzip: %v", err)
	}
}

// C884: a page a script builds (an empty app shell with a bundle) answers
// js_rendered with a note pointing at the API, sitemap or RSS; it is not a
// refusal. A short static page and a page with text are not flagged.
func TestFetchFlagsJavaScriptShells(t *testing.T) {
	shell := `<!doctype html><html><head><title>Listings</title><script src="/app.js"></script><script>window.__STATE__=` +
		strings.Repeat(`{"k":"v"},`, 400) + `0</script></head><body><div id="root"></div><noscript>Enable JavaScript</noscript></body></html>`
	article := `<html><head><script>track()</script></head><body><h1>News</h1><p>` + strings.Repeat("Words of a real article. ", 40) + `</p></body></html>`
	short := `<html><body><p>` + strings.Repeat("<span></span>", 100) + `Short page.</p></body></html>`
	serve := func(doc string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, doc)
		}
	}
	s := newSite(t, false, map[string]http.HandlerFunc{"/shell": serve(shell), "/article": serve(article), "/short": serve(short)})
	r := newFetchRig(t, s, fetchOpts{})
	res := get(r.must("alice", noScreen("http://site.test/shell")), "result").(map[string]any)
	if res["js_rendered"] != true || !strings.Contains(res["note"].(string), services.FetchJSRenderedNote) || res["status"] != float64(200) {
		t.Fatalf("shell: %v %v", res["js_rendered"], res["note"])
	}
	for _, path := range []string{"/article", "/short"} {
		res = get(r.must("alice", noScreen("http://site.test"+path)), "result").(map[string]any)
		if _, ok := res["js_rendered"]; ok || strings.Contains(res["note"].(string), "JavaScript") {
			t.Fatalf("%s flagged: %v", path, res["note"])
		}
	}
}
