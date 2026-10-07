package httpapi

import (
	"html"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// A call without a key is one plain URL: request_id and max_cost are
// optional. Left out, the board makes a random request_id and returns it as
// call.request_id, the key of a retry that is never charged twice; an
// example's placeholder pasted as it is counts as left out; a short one of
// the caller's own is refused with what to do instead (security review
// 1.21, L3 still holds: nobody can squat an id nobody chose).

func TestCallWithoutRequestID(t *testing.T) {
	_, s := anonCallServer(t, 2) // two notary stamps a day for this network
	get := func(path string) (int, map[string]any, string) {
		w := secReq(s, "GET", path, "", "198.51.100.40:1", nil)
		return w.Code, decodeResult(t, w.Body.Bytes()), w.Body.String()
	}
	// No request_id, no max_cost: the fewest fields work.
	code, first, raw := get("/call/notary/stamp?text=bare")
	id, _ := dig(first, "data", "call", "request_id").(string)
	if code != 200 || dig(first, "data", "call", "state") != "done" || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("no request_id: %d %s", code, raw)
	}
	if retry, _ := dig(first, "next", "retry").(string); !strings.Contains(retry, "request_id="+id) {
		t.Fatalf("next.retry: %s", raw)
	}
	if dig(first, "data", "call", "max_cost") != float64(1) {
		t.Fatalf("left out, max_cost is the quote: %s", raw)
	}
	// A retry with the returned id is the same call, charged once: the
	// network's share of 2 still has room for exactly one more call.
	for i := 0; i < 3; i++ {
		code, again, raw := get("/call/notary/stamp?text=bare&request_id=" + id)
		if code != 200 || dig(again, "data", "call", "id") != dig(first, "data", "call", "id") || dig(again, "data", "call", "request_id") != id || dig(again, "next", "retry") != nil {
			t.Fatalf("retry %d: %d %s", i, code, raw)
		}
	}
	// A pasted placeholder counts as left out: a new call with its own id.
	code, second, raw := get("/call/notary/stamp?text=bare&request_id=RANDOM_16_CHARS")
	if code != 200 || dig(second, "data", "call", "id") == dig(first, "data", "call", "id") || dig(second, "data", "call", "request_id") == "RANDOM_16_CHARS" {
		t.Fatalf("placeholder: %d %s", code, raw)
	}
	if code, _, raw := get("/call/notary/stamp?text=third"); code != 429 {
		t.Fatalf("the share is spent after two charged calls: %d %s", code, raw)
	}
	// A short id of the caller's own is refused, saying what to do.
	code, _, raw = get("/call/notary/stamp?text=bare&request_id=abc")
	if code != 400 || !strings.Contains(raw, "leave it out") || !regexp.MustCompile(`request_id=[0-9a-f]{32}`).MatchString(raw) || len(raw) > 600 {
		t.Fatalf("short request_id: %d %s", code, raw)
	}
	// An unsigned POST /v1/command without one works the same way.
	_, s2 := anonCallServer(t, 2000)
	w := secReq(s2, "POST", "/v1/command", `{"operation":"service.call","target":"notary","data":"{\"schema\":1,\"method\":\"stamp\",\"args\":{\"text\":\"json\"},\"max_cost\":1}"}`, "198.51.100.41:1", map[string]string{"Content-Type": "application/json"})
	if body := decodeResult(t, w.Body.Bytes()); w.Code != 200 || len(dig(body, "data", "call", "request_id").(string)) != 32 {
		t.Fatalf("unsigned /v1/command without request_id: %d %s", w.Code, w.Body.String())
	}
}

// A browser's own navigation to a /call/ URL (typed, pasted, bookmarked:
// Sec-Fetch-Site none) is served, as JSON.
func TestCallFromABrowserAddressBar(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	w := secReq(s, "GET", "/call/notary/stamp?text=from+the+address+bar", "", "198.51.100.42:1", map[string]string{
		"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-User": "?1",
		"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"})
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || dig(decodeResult(t, w.Body.Bytes()), "data", "call", "state") != "done" {
		t.Fatalf("browser navigation: %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	// Readable in the tab: indented, one field a line.
	if !strings.Contains(w.Body.String(), "\n  \"data\": {\n") {
		t.Fatalf("browser navigation is not indented: %s", w.Body.String())
	}
	// An error reads the same way; an agent's request stays compact.
	w = secReq(s, "GET", "/call/notary/stamp?text=x&request_id=short", "", "198.51.100.42:1", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Dest": "document"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "\n  \"error\": {\n") {
		t.Fatalf("browser navigation, error: %d %s", w.Code, w.Body.String())
	}
	if w = secReq(s, "GET", "/call/notary/stamp?text=agent", "", "198.51.100.42:1", nil); w.Code != 200 || strings.Count(w.Body.String(), "\n") != 1 {
		t.Fatalf("an agent's answer: %d %s", w.Code, w.Body.String())
	}
}

// Every published copy-paste URL for a call without a key works verbatim:
// the ones in /capabilities, /api/services, /llms.txt, /for-agents and the
// TCP help's "one URL". A curl with --data is POSTed as its form.
func TestPublishedCallExamplesWorkVerbatim(t *testing.T) {
	store, api := anonCallServer(t, 20000, "notary", "screen")
	store.UseTextScreener(screenStub{})
	// The same store behind the web pages too, for /for-agents.
	s := New(store, web.Handler(store), api.cfg)
	urlRE := regexp.MustCompile(`https://swarmmemo\.com/call/[^\s'"<>\\]+`)
	dataRE := regexp.MustCompile(`--data '([^']*)'`)
	type example struct{ source, method, url, form string }
	var examples []example
	collect := func(source, text string) {
		for _, line := range strings.Split(text, "\n") {
			for _, u := range urlRE.FindAllString(line, -1) {
				ex := example{source: source, method: "GET", url: html.UnescapeString(u)}
				if m := dataRE.FindStringSubmatch(line); m != nil {
					ex.method, ex.form = "POST", m[1]
				}
				examples = append(examples, ex)
			}
		}
	}
	collect("/capabilities", makeRequest(s, "GET", "https://swarmmemo.com/capabilities", "", "").Body.String())
	// services.list states the example as a path on this origin.
	apiExample, _ := dig(decodeResult(t, makeRequest(s, "GET", "https://swarmmemo.com/api/services", "", "").Body.Bytes()), "data", "without_key", "example").(string)
	collect("/api/services", "https://swarmmemo.com"+apiExample)
	collect("/llms.txt", makeRequest(s, "GET", "https://swarmmemo.com/llms.txt", "", "").Body.String())
	collect("/for-agents", getHTML(t, s, "/for-agents"))
	sources := map[string]bool{}
	for _, ex := range examples {
		sources[ex.source] = true
	}
	for _, want := range []string{"/capabilities", "/api/services", "/llms.txt", "/for-agents"} {
		if !sources[want] {
			t.Fatalf("%s publishes no /call/ example", want)
		}
	}
	for _, ex := range examples {
		if strings.Contains(ex.url+ex.form, "request_id") || strings.Contains(ex.url+ex.form, "max_cost") {
			t.Errorf("%s: the example carries an optional field: %s %s", ex.source, ex.url, ex.form)
		}
		r := httptest.NewRequest(ex.method, ex.url, strings.NewReader(ex.form))
		r.RemoteAddr = "198.51.100.43:1"
		if ex.form != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		// A public read (tools search) answers with its result, not a call.
		service, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, services.CallPathPrefix), "/")
		if _, m, ok := services.LookupMethod(services.Catalog(services.Known()), service, method); ok && !m.Write() {
			if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "result") == nil {
				t.Errorf("%s: %s %s: %d %s", ex.source, ex.method, ex.url, w.Code, w.Body.String())
			}
			continue
		}
		if w.Code != 200 || dig(decodeResult(t, w.Body.Bytes()), "data", "call", "request_id") == nil {
			t.Errorf("%s: %s %s %s: %d %s", ex.source, ex.method, ex.url, ex.form, w.Code, w.Body.String())
		}
	}
}
