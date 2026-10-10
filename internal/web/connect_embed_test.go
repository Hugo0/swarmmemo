package web

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testWorkerKey = "6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw"

func connectEmbedURL(values map[string]string) string {
	q := url.Values{}
	for k, v := range values {
		q.Set(k, v)
	}
	return ConnectEmbedPath + "?" + q.Encode()
}

// The sign-in window is never framed, sets no cookie, and asks in the
// reader's terms; scripts and requests stay on this origin.
func TestConnectEmbedWindowHeaders(t *testing.T) {
	for _, action := range []string{"", "signout"} {
		values := map[string]string{"room": "blog", "origin": "https://blog.example", "pub": testWorkerKey}
		if action != "" {
			values["action"] = action
		}
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", connectEmbedURL(values), nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body)
		}
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		if h.Get("X-Frame-Options") != "DENY" || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Fatalf("window can be framed or load foreign code: %v", h)
		}
		if h.Get("Set-Cookie") != "" || h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("headers %v", h)
		}
		body := w.Body.String()
		want := "Let <strong>https://blog.example</strong> comment and vote as"
		if action == "signout" {
			want = "Stop <strong>https://blog.example</strong> commenting and voting as"
		}
		for _, s := range []string{want, "#blog", `data-origin="https://blog.example"`, `data-pub="` + testWorkerKey + `"`, `src="/assets/connect-embed.js?v=` + assetVersion + `"`, `id="connect-embed-allow"`} {
			if !strings.Contains(body, s) {
				t.Fatalf("%s: missing %q in %s", action, s, body)
			}
		}
		if strings.Contains(body, "<script>") || strings.Contains(body, "style=") {
			t.Fatal("inline script or style would need a CSP exemption")
		}
	}
}

// Only an exact site origin, a valid room and a 32-byte key open the window;
// anything else, or anything extra, is refused, never ignored.
func TestConnectEmbedWindowValidatesOrigin(t *testing.T) {
	good := map[string]string{"room": "blog", "origin": "https://blog.example", "pub": testWorkerKey}
	with := func(k, v string) map[string]string {
		c := map[string]string{}
		for a, b := range good {
			c[a] = b
		}
		if v == "<delete>" {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	cases := []map[string]string{
		with("origin", "http://blog.example"), with("origin", "https://blog.example/"), with("origin", "https://Blog.example"),
		with("origin", "javascript:alert(1)"), with("origin", "null"), with("origin", "https://user@blog.example"), with("origin", "<delete>"),
		with("origin", `https://blog.example"><script>`), with("room", "Not A Room"), with("room", "<delete>"), with("pub", "short"),
		with("pub", testWorkerKey+"="), with("pub", "<delete>"), with("action", "delete"), with("extra", "1"),
	}
	for _, values := range cases {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", connectEmbedURL(values), nil))
		if w.Code != 400 || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%v: %d", values, w.Code)
		}
		body := w.Body.String()
		if strings.Contains(body, "connect-embed-allow") || strings.Contains(body, "data-origin=\"h") || strings.Contains(body, "<script>alert") {
			t.Fatalf("%v: refused window still offers Allow: %s", values, body)
		}
	}
	// A repeated parameter is refused too: the browser and the server could read different ones.
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", connectEmbedURL(good)+"&origin=https%3A%2F%2Fevil.example", nil))
	if w.Code != 400 {
		t.Fatalf("duplicate origin: %d", w.Code)
	}
}

// The sign-in module is served like the widget (CORS, short cache) but kept
// out of the widget's bundle and gzip budget.
func TestEmbedSigninModule(t *testing.T) {
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", EmbedSigninPath, nil))
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || w.Header().Get("Cache-Control") != EmbedCacheControl {
		t.Fatalf("module: %d %v", w.Code, w.Header())
	}
	body := w.Body.String()
	if !strings.Contains(body, "export default async function signin") || strings.Contains(body, "innerHTML") || strings.Contains(body, "document.cookie") {
		t.Fatal("module must export the sign-in and use text nodes, no cookies")
	}
	if strings.Contains(string(embedBundle), "export default") || !strings.Contains(string(embedBundle), "/embed/signin-v1.js") {
		t.Fatal("the widget imports the module on demand; it does not include it")
	}
	for _, want := range []string{"event.origin !== origin", "event.source !== win", "location.origin"} {
		if !strings.Contains(body, want) {
			t.Fatalf("module misses origin check %q", want)
		}
	}
	var z bytes.Buffer
	gz := gzip.NewWriter(&z)
	_, _ = gz.Write(w.Body.Bytes())
	_ = gz.Close()
	t.Logf("sign-in module: %d bytes, %d gzipped", w.Body.Len(), z.Len())
	popup, _ := files.ReadFile("assets/connect-embed.js")
	for _, want := range []string{"event.source !== opener", "event.origin !== origin", "postMessage(message, origin)"} {
		if !strings.Contains(string(popup), want) {
			t.Fatalf("window script misses %q", want)
		}
	}
}

func TestCompactScriptBlockComments(t *testing.T) {
	src := "/* header\n   more */\nconst a = 1; /* kept */\n  /* one line */\n/* a */ b();\nx = `\n/* css */\n`;\n/*\n*/ c();\n/*/ not closed\nd();\n*/\ne();\n"
	got := string(compactScript([]byte(src)))
	want := "const a = 1; /* kept */\n/* a */ b();\nx = `\n`;\nc();\ne();\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
