package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// GET /d/ID.txt is docs.open with format=text at a path with no query: the
// same bytes, inline, its ETag the bytes' SHA-256; a private doc is the same
// doc_not_found as an unknown id.
func TestDocRawURL(t *testing.T) {
	_, h := anonCallServer(t, 2000, "paste", "docs")
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	text := "<!doctype html><script>alert(1)</script>\nline two\r\n\t é"
	n := 0
	create := func(service string, args map[string]any) string {
		t.Helper()
		n++
		data, _ := json.Marshal(map[string]any{"schema": 1, "method": "create", "args": args, "max_cost": 10})
		body, _ := json.Marshal(signService(key, board.Command{Operation: "service.call", Target: service, Data: string(data), RequestID: "raw-create-" + strconv.Itoa(n)}))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		out := decodeResult(t, w.Body.Bytes())
		if id, ok := dig(out, "data", "result", "doc", "id").(string); ok {
			return id
		}
		id, _ := dig(out, "data", "result", "paste", "id").(string)
		return id
	}
	unlisted := create("docs", map[string]any{"title": "Raw", "text": text, "visibility": "unlisted"})
	private := create("docs", map[string]any{"title": "Mine", "text": text})
	paste := create("paste", map[string]any{"text": text, "visibility": "unlisted"})
	get := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://swarmmemo.com"+path, nil)
		r.RemoteAddr = "198.51.100.8:12345"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	sum := sha256.Sum256([]byte(text))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	for _, id := range []string{unlisted, paste} {
		want := get("GET", "/call/docs/open?id="+id+"&format=text", nil)
		w := get("GET", "/d/"+id+".txt", map[string]string{"Accept": "text/html"})
		hd := w.Header()
		if w.Code != 200 || want.Code != 200 || w.Body.String() != want.Body.String() || w.Body.String() != text {
			t.Fatalf("%s: %d %q, docs.open %d %q", id, w.Code, w.Body.String(), want.Code, want.Body.String())
		}
		if hd.Get("Content-Type") != "text/plain; charset=utf-8" || hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("ETag") != etag ||
			hd.Get("Content-Disposition") != `inline; filename="`+id+`.txt"` || hd.Get("Content-Security-Policy") != "sandbox; default-src 'none'" ||
			!strings.Contains(hd.Get("X-Robots-Tag"), "noindex") || hd.Get("Access-Control-Allow-Origin") != "" || hd.Get("X-Doc-Screen") == "" {
			t.Fatalf("%s headers: %v", id, hd)
		}
		for _, match := range []string{etag, `W/` + etag, `"other", ` + etag, "*"} {
			if w := get("GET", "/d/"+id+".txt", map[string]string{"If-None-Match": match}); w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag {
				t.Fatalf("If-None-Match %s: %d %q", match, w.Code, w.Body.String())
			}
		}
		if w := get("GET", "/d/"+id+".txt", map[string]string{"If-None-Match": `"stale"`}); w.Code != 200 || w.Body.String() != text {
			t.Fatalf("stale If-None-Match: %d", w.Code)
		}
	}
	// A private doc and an unknown id are one answer: no existence oracle.
	priv := get("GET", "/d/"+private+".txt", nil)
	unknown := get("GET", "/d/"+strings.Repeat("0", 32)+".txt", nil)
	if priv.Code != 404 || unknown.Code != 404 || strings.Contains(priv.Body.String(), text) ||
		dig(decodeResult(t, priv.Body.Bytes()), "error", "code") != "doc_not_found" || dig(decodeResult(t, unknown.Body.Bytes()), "error", "code") != "doc_not_found" ||
		dig(decodeResult(t, priv.Body.Bytes()), "error", "message") != dig(decodeResult(t, unknown.Body.Bytes()), "error", "message") {
		t.Fatalf("private %d %s, unknown %d %s", priv.Code, priv.Body.String(), unknown.Code, unknown.Body.String())
	}
	// Anything else under /d/ is a JSON 404; a write method is refused; another
	// site's page cannot make a browser open (and pay for) it.
	for _, path := range []string{"/d/", "/d/" + unlisted, "/d/" + strings.ToUpper(unlisted) + ".txt", "/d/" + unlisted + ".txt?v=1", "/d/" + unlisted + ".txt/x"} {
		if w := get("GET", path, nil); w.Code != 404 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, method := range []string{"POST", "HEAD"} {
		if w := get(method, "/d/"+unlisted+".txt", nil); w.Code != 405 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	if w := get("GET", "/d/"+unlisted+".txt", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 || w.Body.String() == text {
		t.Fatalf("cross-site: %d %s", w.Code, w.Body.String())
	}
}
