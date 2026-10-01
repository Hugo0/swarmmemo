package web

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestConnectPageAndJSON(t *testing.T) {
	code, _, body := getPage(t, "/connect")
	if code != 200 || !strings.Contains(body, "</html>") {
		t.Fatalf("/connect: %d, incomplete page", code)
	}
	var view connectView
	var twin string
	for _, route := range []struct{ path, accept string }{
		{"/connect.json", ""}, {"/connect?format=json", ""}, {"/connect", "application/json"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", route.path, nil)
		r.Header.Set("Accept", route.accept)
		Handler(&testService{}).ServeHTTP(w, r)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s (%s): %d, %s", route.path, route.accept, w.Code, w.Body.String())
		}
		if twin == "" {
			twin = w.Body.String()
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
		} else if w.Body.String() != twin {
			t.Errorf("%s (%s) differs from JSON twin", route.path, route.accept)
		}
	}
	for _, want := range []string{
		"<title>Connect the dots · SwarmMemo</title>", "<h1>Connect the dots</h1>",
		`name="description" content="` + html.EscapeString(view.Description) + `"`,
		`rel="canonical" href="https://swarmmemo.com/connect"`,
		`href="/messages"`, `href="/connect.json"`, `Default: <code>open</code>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/connect lacks %s", want)
		}
	}
	if strings.Count(body, "<h1>") != 1 || view.Policy.Default != "open" || len(view.Setup.Platforms) != len(platforms) {
		t.Fatalf("unexpected heading, default or platforms: %+v", view)
	}
	for i, p := range platforms {
		row := view.Setup.Platforms[i]
		if row.Slug != p.Slug || row.Name != p.Name || row.Page != "/for/"+p.Slug || row.Paste != p.Paste || row.Tested != p.Tested {
			t.Errorf("platform %s differs from setup source: %+v", p.Slug, row)
		}
		start := strings.Index(body, `<section id="platform-`+p.Slug+`">`)
		if start < 0 {
			t.Fatalf("missing platform row %s", p.Slug)
		}
		rowHTML, _, _ := strings.Cut(body[start:], "</section>")
		for _, want := range []string{`href="/for/` + p.Slug + `"`, html.EscapeString(p.Name), html.EscapeString(p.Paste), `data-copy-label="Copy ` + html.EscapeString(p.Name) + ` setup"`} {
			if !strings.Contains(rowHTML, want) {
				t.Errorf("%s row lacks %s", p.Slug, want)
			}
		}
		if (row.Status == "untested") != !p.Tested || strings.Contains(rowHTML, "(untested)") != !p.Tested {
			t.Errorf("%s tested flag does not match label", p.Slug)
		}
	}
	// Every content string in the JSON appears in the HTML, including the
	// policy descriptions, limitations and safety links.
	var checkStrings func(reflect.Value)
	checkStrings = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.String:
			if !strings.Contains(body, html.EscapeString(v.String())) {
				t.Errorf("HTML omits JSON content %q", v.String())
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				checkStrings(v.Field(i))
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				checkStrings(v.Index(i))
			}
		}
	}
	checkStrings(reflect.ValueOf(view))
	// Each explanatory link lands on a real section of the rendered guide.
	lines := append([]connectLine{view.Setup.Identity, view.Policy.Action}, view.Address.Lines...)
	lines = append(lines, view.Safety.Lines...)
	guide := legalPage("/messages")
	for _, line := range lines {
		_, anchor, ok := strings.Cut(line.Link, "#")
		if !ok || !strings.Contains(string(guide.Body), `id="`+anchor+`"`) {
			t.Errorf("missing doc section: %s", line.Link)
		}
	}
}

func TestConnectMethodsAndLinks(t *testing.T) {
	for _, path := range []string{"/connect", "/connect.json", "/connect?format=json"} {
		for method, status := range map[string]int{"HEAD": 200, "POST": 405} {
			w := httptest.NewRecorder()
			Handler(&testService{}).ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != status || (method == "HEAD" && w.Body.Len() != 0) {
				t.Errorf("%s %s: %d, %d bytes", method, path, w.Code, w.Body.Len())
			}
		}
	}
	for _, path := range []string{"/", "/for-agents", "/guides", "/connect"} {
		_, _, body := getPage(t, path)
		_, footer, _ := strings.Cut(body, `<nav aria-label="Footer">`)
		if !strings.Contains(footer, `href="/connect"`) {
			t.Errorf("%s footer omits /connect", path)
		}
		nav, _, _ := strings.Cut(body, `<main`)
		if strings.Contains(nav, `href="/connect"`) {
			t.Errorf("%s adds /connect to the top nav", path)
		}
		if path == "/" {
			main, _, _ := strings.Cut(body, `<footer`)
			if !strings.Contains(main, `href="/connect"`) {
				t.Error("home content omits /connect")
			}
		}
	}
}
