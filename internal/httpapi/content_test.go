package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strconv"
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

// An unlisted paste opens without its author unless it was created with
// show_author: true; then the open names the key's fingerprint and handle.
func TestPasteShowAuthorOverTheWire(t *testing.T) {
	_, h := anonCallServer(t, 2000, "paste")
	key, fingerprint := wireKey(9)
	send := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(signService(key, c))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	send(board.Command{Operation: "agent.register", Handle: "paste-author"})
	create := func(args map[string]any) string {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"schema": 1, "method": "create", "args": args, "max_cost": 10})
		id, _ := dig(send(board.Command{Operation: "service.call", Target: "paste", Data: string(data), RequestID: "create-" + args["text"].(string)}), "data", "result", "paste", "id").(string)
		return id
	}
	hidden := create(map[string]any{"text": "quiet", "visibility": "unlisted"})
	shown := create(map[string]any{"text": "signed", "visibility": "unlisted", "show_author": true})
	open := func(id string) (map[string]any, string) {
		t.Helper()
		r := httptest.NewRequest("GET", "https://swarmmemo.com/call/paste/open?id="+id, nil)
		r.RemoteAddr = "198.51.100.10:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("open: %d %s", w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes()), w.Body.String()
	}
	if out, body := open(hidden); dig(out, "data", "result", "paste", "author") != nil || strings.Contains(body, fingerprint) || strings.Contains(body, "paste-author") {
		t.Fatalf("the default open shows its author: %s", body)
	}
	if out, body := open(shown); dig(out, "data", "result", "paste", "author", "fingerprint") != fingerprint || dig(out, "data", "result", "paste", "author", "handle") != "paste-author" {
		t.Fatalf("a show_author open: %s", body)
	}
}

// /api/stats/daily counts paste and doc use by kind, today included: counts
// only, never an id, a title or text.
func TestDailyStatsCountPastesAndDocs(t *testing.T) {
	_, h := anonCallServer(t, 2000, "paste", "docs")
	owner, reader := ed25519.NewKeyFromSeed(make([]byte, 32)), ed25519.NewKeyFromSeed(append(make([]byte, 31), 7))
	n := 0
	call := func(key ed25519.PrivateKey, service, method string, args map[string]any) map[string]any {
		t.Helper()
		n++
		data, _ := json.Marshal(map[string]any{"schema": 1, "method": method, "args": args, "max_cost": 1000})
		body, _ := json.Marshal(signService(key, board.Command{Operation: "service.call", Target: service, Data: string(data), RequestID: "stats-" + strconv.Itoa(n)}))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s.%s: %d %s", service, method, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	unlisted, _ := dig(call(owner, "paste", "create", map[string]any{"text": "secret paste text", "visibility": "unlisted"}), "data", "result", "paste", "id").(string)
	call(owner, "paste", "create", map[string]any{"text": "private one"})
	call(owner, "paste", "create", map[string]any{"text": "private two", "title": "secret title"})
	call(reader, "paste", "open", map[string]any{"id": unlisted})
	r := httptest.NewRequest("GET", "https://swarmmemo.com/call/paste/open?id="+unlisted, nil)
	r.RemoteAddr = "198.51.100.9:12345"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("anonymous open: %d %s", w.Code, w.Body.String())
	}
	doc, _ := dig(call(owner, "docs", "create", map[string]any{"title": "Plan", "text": "v1"}), "data", "result", "doc", "id").(string)
	call(owner, "docs", "write", map[string]any{"id": doc, "base_version": 1, "text": "v2"})

	w = makeRequest(h, "GET", "/api/stats/daily?days=2", "", "")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("daily: %d %s", w.Code, body)
	}
	for _, leak := range []string{unlisted, doc, "secret", "Plan"} {
		if strings.Contains(body, leak) {
			t.Fatalf("daily stats carry %q: %s", leak, body)
		}
	}
	var got struct {
		Daily []struct {
			Day     string `json:"day"`
			Content struct {
				Pastes   map[string]int64 `json:"pastes_created"`
				Opens    map[string]int64 `json:"paste_opens"`
				Docs     map[string]int64 `json:"docs_created"`
				Versions int64            `json:"doc_versions"`
			} `json:"content"`
		} `json:"daily"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Daily) != 2 {
		t.Fatalf("daily: %v %s", err, body)
	}
	today, before := got.Daily[1].Content, got.Daily[0].Content
	if today.Pastes["private"] != 2 || today.Pastes["unlisted"] != 1 || today.Opens["signed"] != 1 || today.Opens["anonymous"] != 1 ||
		today.Docs["own"] != 1 || today.Docs["group"] != 0 || today.Versions != 2 {
		t.Fatalf("today's content counts: %+v", today)
	}
	if before.Pastes["private"] != 0 || before.Versions != 0 || before.Opens == nil {
		t.Fatalf("yesterday's content counts: %+v", before)
	}
}
