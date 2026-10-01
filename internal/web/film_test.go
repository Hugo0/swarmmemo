package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The launch film is on the home page (top of the sidebar, so the composer stays above the fold) and at the top of the /messages guide:
// both cuts, the poster, the transcript link and the script, without autoplay in the markup (the
// script starts it, unless the reader prefers reduced motion).
func TestFilmIsOnTheHomePageAndTheMessagesGuide(t *testing.T) {
	for _, tc := range []struct{ path, variant string }{{"/", "film-side"}, {"/messages", "film-guide"}} {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
		for _, want := range []string{
			`class="film ` + tc.variant + `"`, `src="/assets/film-16x9.mp4" type="video/mp4"`,
			`src="/assets/film-1x1.mp4" type="video/mp4" media="(max-width: 600px)"`,
			`poster="/assets/film-poster-16x9.jpg"`, `data-poster-square="/assets/film-poster-1x1.jpg"`,
			`href="/assets/film-transcript.txt"`, `<script defer src="/assets/film.js">`,
			` muted loop playsinline preload="metadata"`, `class="film-sound"`, `class="film-play"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %s", tc.path, want)
			}
		}
		if strings.Count(body, "data-film") != 1 || strings.Contains(body, " autoplay") {
			t.Errorf("%s: want one film and no autoplay attribute", tc.path)
		}
	}
	// On /messages it sits above the guide's body.
	w := httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/messages", nil))
	if b := w.Body.String(); strings.Index(b, "data-film") > strings.Index(b, `<article class="md article-body`) {
		t.Error("/messages: the film is not at the top of the guide")
	}
	// Other pages do not load it.
	w = httptest.NewRecorder()
	Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/privacy", nil))
	if b := w.Body.String(); strings.Contains(b, "data-film") || strings.Contains(b, "film.js") {
		t.Error("/privacy carries the film")
	}
}

// Every film file is served with its type and the assets' cache header, and the videos answer
// byte ranges (Safari will not play a video that does not).
func TestFilmAssetsAreServedWithTheirTypes(t *testing.T) {
	for name, typ := range FilmAssets {
		w := httptest.NewRecorder()
		Handler(&testService{}).ServeHTTP(w, httptest.NewRequest("GET", "/assets/"+name, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != typ || w.Header().Get("Cache-Control") != "public, max-age=3600" || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q %q (%d bytes)", name, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"), w.Body.Len())
		}
		if strings.HasSuffix(name, ".mp4") {
			r := httptest.NewRequest("GET", "/assets/"+name, nil)
			r.Header.Set("Range", "bytes=0-1023")
			w = httptest.NewRecorder()
			Handler(&testService{}).ServeHTTP(w, r)
			if w.Code != 206 || w.Body.Len() != 1024 {
				t.Errorf("%s range: %d, %d bytes", name, w.Code, w.Body.Len())
			}
		}
	}
}
