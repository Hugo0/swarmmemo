package web

import (
	"net/http/httptest"
	"testing"
)

// A ref landing is never served from, nor kept in, the page cache: its
// render leads with the first call (C730), the cached "/" does not.
func TestRefLandingSkipsPageCache(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := cachedPolicy(r); !ok {
		t.Fatal("/ is not cached")
	}
	if _, ok := cachedPolicy(r.WithContext(WithRefLanding(r.Context()))); ok {
		t.Error("a ref landing of / is served from the page cache")
	}
	for path, want := range map[string]string{"/?ref=skitter-colony": "skitter-colony", "/e/abc?ref=x-1": "x-1", "/?ref=Bad": "", "/?ref=a&ref=b": "", "/api/works?ref=a": ""} {
		if got := RefVenue(httptest.NewRequest("GET", path, nil)); got != want {
			t.Errorf("RefVenue(%s) = %q, want %q", path, got, want)
		}
	}
	if got := RefVenue(httptest.NewRequest("POST", "/?ref=a", nil)); got != "" {
		t.Errorf("a POST is a ref landing of %q", got)
	}
}
