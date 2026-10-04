package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestSwarmchasingDataDownloads(t *testing.T) {
	for path, name := range map[string]string{
		"/swarmchasing/data/universe.json": "swarmchasing-universe.json",
		"/swarmchasing/data/agents.json":   "swarmchasing-agents.json",
	} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if !json.Valid(w.Body.Bytes()) || w.Body.Len() == 0 {
			t.Errorf("%s: body is not valid JSON", path)
		}
		h := w.Header()
		if got := h.Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type %q", path, got)
		}
		if got, want := h.Get("Content-Disposition"), `attachment; filename="`+name+`"`; got != want {
			t.Errorf("%s: Content-Disposition %q, want %q", path, got, want)
		}
		if got := h.Get("Cache-Control"); got != "public, max-age=3600" {
			t.Errorf("%s: Cache-Control %q", path, got)
		}
		etag := h.Get("ETag")
		if len(etag) != 66 {
			t.Errorf("%s: ETag %q is not a quoted sha256", path, etag)
		}

		head := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(head, httptest.NewRequest("HEAD", path, nil))
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("ETag") != etag || head.Header().Get("Content-Disposition") == "" {
			t.Errorf("%s HEAD: %d, %d body bytes, ETag %q", path, head.Code, head.Body.Len(), head.Header().Get("ETag"))
		}

		cond := httptest.NewRequest("GET", path, nil)
		cond.Header.Set("If-None-Match", etag)
		nm := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(nm, cond)
		if nm.Code != 304 {
			t.Errorf("%s If-None-Match: %d, want 304", path, nm.Code)
		}
	}
}
