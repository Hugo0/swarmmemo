package services_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// TestFetchReadsEveryTextualType: every text/ type, the textual
// application/ types and an untyped or octet-stream body that is UTF-8 text
// come back as format "text"; binary bodies are refused, and the refusal
// names the media type the site sent and nothing else (C650).
func TestFetchReadsEveryTextualType(t *testing.T) {
	type served struct {
		ct   string // "" sends no Content-Type at all
		body string
	}
	pages := map[string]served{
		"/data.csv":     {"text/csv; charset=utf-8", "a,b\n1,2\n"},
		"/style.css":    {"text/css", "body { color: red }\n"},
		"/conf.yaml":    {"text/yaml", "key: value\n"},
		"/script.py":    {"text/x-python", "print('hi')\n"},
		"/app.js":       {"text/javascript", "let x = 1;\n"},
		"/rows.tsv":     {"text/tab-separated-values", "a\tb\n"},
		"/legacy.js":    {"application/javascript", "var y = 2;\n"},
		"/es.js":        {"application/ecmascript", "var z = 3;\n"},
		"/x.yaml":       {"application/x-yaml", "a: 1\n"},
		"/y.yaml":       {"application/yaml", "b: 2\n"},
		"/cfg.toml":     {"application/toml", "[a]\nb = 1\n"},
		"/run.sh":       {"application/x-sh", "#!/bin/sh\necho hi\n"},
		"/q.sql":        {"application/sql", "SELECT 1;\n"},
		"/log.ndjson":   {"application/x-ndjson", "{\"a\":1}\n{\"a\":2}\n"},
		"/log.jsonl":    {"application/jsonl", "{\"a\":1}\n"},
		"/latin.txt":    {"text/plain; charset=iso-8859-1", "caf\xe9\n"},
		"/raw.txt":      {"application/octet-stream", "plain words, ünïcode too\n"},
		"/raw2.txt":     {"binary/octet-stream", "more words\n"},
		"/untyped.txt":  {"", "no type sent, just words\n"},
		"/doc.pdf":      {"application/pdf", "%PDF-1.4\n"},
		"/form":         {"application/x-www-form-urlencoded", "a=1&b=2"},
		"/blob.bin":     {"application/octet-stream", "abc\x00def"},
		"/latin.bin":    {"application/octet-stream", "caf\xe9 au lait\n"},
		"/png.bin":      {"application/octet-stream", "\x89PNG\r\n\x1a\n\x00\x00"},
		"/untyped.bin":  {"", "abc\x00\x01\x02def"},
		"/archive.zip":  {"application/zip", "PK\x03\x04"},
		"/weird":        {"Application/X-Weird+Thing; boundary=<script>", "\x00\x01"},
		"/long":         {"application/" + strings.Repeat("x", 120), "\x00"},
		"/utf16.csv":    {"text/csv; charset=utf-16", "a,b"},
		"/image.png":    {"image/png", "\x89PNG\r\n"},
		"/feed.unknown": {"application/vnd.example", "\x00"},
	}
	handlers := map[string]http.HandlerFunc{}
	for path, p := range pages {
		p := p
		handlers[path] = func(w http.ResponseWriter, r *http.Request) {
			if p.ct == "" {
				w.Header()["Content-Type"] = nil // no sniffing by the test server
			} else {
				w.Header().Set("Content-Type", p.ct)
			}
			w.Write([]byte(p.body))
		}
	}
	s := newSite(t, false, handlers)
	r := newFetchRig(t, s, fetchOpts{})

	accepted := map[string]string{
		"/data.csv": "a,b\n1,2\n", "/style.css": "", "/conf.yaml": "", "/script.py": "", "/app.js": "", "/rows.tsv": "",
		"/legacy.js": "", "/es.js": "", "/x.yaml": "", "/y.yaml": "", "/cfg.toml": "", "/run.sh": "", "/q.sql": "",
		"/log.ndjson": "", "/log.jsonl": "", "/latin.txt": "café\n",
		"/raw.txt": "plain words, ünïcode too\n", "/raw2.txt": "", "/untyped.txt": "no type sent, just words\n",
	}
	for path, want := range accepted {
		out, err := r.fetch("alice", noScreen("http://site.test"+path))
		if err != nil {
			t.Errorf("%s (%s): refused: %v", path, pages[path].ct, err)
			continue
		}
		res := get(out, "result").(map[string]any)
		if res["format"] != "text" {
			t.Errorf("%s: format %v, want text", path, res["format"])
		}
		if want == "" {
			want = pages[path].body
		}
		if res["text"] != want {
			t.Errorf("%s: text %q, want %q", path, res["text"], want)
		}
	}

	// Refused, with the type the site sent as details.received_type (a clean
	// lowercase type/subtype), or no type at all when it sent none or a
	// malformed or overlong one.
	refused := map[string]string{
		"/doc.pdf": "application/pdf", "/form": "application/x-www-form-urlencoded",
		"/blob.bin": "application/octet-stream", "/latin.bin": "application/octet-stream", "/png.bin": "application/octet-stream",
		"/untyped.bin": "", "/archive.zip": "application/zip", "/weird": "application/x-weird+thing",
		"/long": "", "/utf16.csv": "text/csv", "/image.png": "image/png", "/feed.unknown": "application/vnd.example",
	}
	for path, want := range refused {
		for _, round := range []string{"fresh", "cached"} {
			_, err := r.fetch("alice", noScreen("http://site.test"+path))
			var e *allowance.Err
			if !errors.As(err, &e) || e.Code != "fetch_unsupported_type" {
				t.Errorf("%s (%s): got %v, want fetch_unsupported_type", path, round, err)
				continue
			}
			got := ""
			if d, ok := e.Details.(map[string]any); ok {
				got, _ = d["received_type"].(string)
				if len(d) != 1 {
					t.Errorf("%s: details carry more than received_type: %v", path, d)
				}
			}
			if got != want {
				t.Errorf("%s (%s): received_type %q, want %q", path, round, got, want)
			}
		}
	}
}
