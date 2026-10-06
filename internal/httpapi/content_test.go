package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Paste text leaves only as JSON or as a sandboxed text/plain download, never
// as HTML, whatever the text holds and however it is asked for.
func TestPasteTextIsNeverHTML(t *testing.T) {
	_, h := anonCallServer(t, 2000, "paste", "docs")
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	text := "<!doctype html><html><body><script>alert(document.cookie)</script></body></html>"
	create := func(visibility string) string {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"schema": 1, "method": "create", "args": map[string]any{"text": text, "visibility": visibility}, "max_cost": 10})
		body, _ := json.Marshal(signService(key, board.Command{Operation: "service.call", Target: "paste", Data: string(data), RequestID: "create-" + visibility}))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		id, _ := dig(decodeResult(t, w.Body.Bytes()), "data", "result", "paste", "id").(string)
		return id
	}
	unlisted, private := create("unlisted"), create("private")
	notHTML := func(w *httptest.ResponseRecorder, what string) {
		t.Helper()
		if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "html") || ct == "" {
			t.Fatalf("%s served as %q", what, ct)
		}
	}
	get := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://swarmmemo.com"+path, nil)
		r.RemoteAddr = "198.51.100.8:12345"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// JSON, even for a browser tab asking for HTML.
	for _, headers := range []map[string]string{nil, {"Accept": "text/html", "Sec-Fetch-Dest": "document", "Sec-Fetch-Site": "none"}} {
		w := get("/call/paste/open?id="+unlisted, headers)
		notHTML(w, "an open")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || dig(decodeResult(t, w.Body.Bytes()), "data", "result", "text") != text {
			t.Fatalf("open: %d %s", w.Code, w.Body.String())
		}
	}
	// The download: the text alone, as an attachment a browser never renders.
	w := get("/call/paste/open?id="+unlisted+"&format=text", map[string]string{"Accept": "text/html"})
	notHTML(w, "a download")
	hd := w.Header()
	if w.Code != 200 || w.Body.String() != text || hd.Get("Content-Type") != "text/plain; charset=utf-8" || !strings.HasPrefix(hd.Get("Content-Disposition"), "attachment") ||
		hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Content-Security-Policy") != "sandbox" || !strings.Contains(hd.Get("X-Robots-Tag"), "noindex") ||
		hd.Get("Access-Control-Allow-Origin") != "" || hd.Get("X-Paste-Screen") == "" {
		t.Fatalf("download: %d %v %q", w.Code, hd, w.Body.String())
	}
	// Refusals are JSON: a private paste, an unknown id, another format or method.
	for path, code := range map[string]string{
		"/call/paste/open?id=" + private + "&format=text":              "paste_not_found",
		"/call/paste/open?id=" + strings.Repeat("0", 32):               "paste_not_found",
		"/call/paste/open?id=" + unlisted + "&format=html":             "invalid_request",
		"/call/paste/open?id=" + unlisted + "&format=text&format=text": "invalid_request",
	} {
		w := get(path, nil)
		notHTML(w, path)
		if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || dig(decodeResult(t, w.Body.Bytes()), "error", "code") != code {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	// Another site's page cannot make a visitor's browser open (and pay for) it.
	if w := get("/call/paste/open?id="+unlisted+"&format=text", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 || w.Body.String() == text {
		t.Fatalf("cross-site download: %d %s", w.Code, w.Body.String())
	}
	// The owner's signed read is JSON too.
	data, _ := json.Marshal(map[string]any{"schema": 1, "method": "get", "args": map[string]any{"id": private}})
	body, _ := json.Marshal(signService(key, board.Command{Operation: "service.read", Target: "paste", Data: string(data)}))
	w = makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
	notHTML(w, "a get")
	if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "result", "text") != text {
		t.Fatalf("owner get: %d %s", w.Code, w.Body.String())
	}
}
