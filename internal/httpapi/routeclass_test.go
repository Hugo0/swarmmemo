package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The slow-request log names a route class, never an identifier or a query.
func TestRouteClassHasNoIdentifiers(t *testing.T) {
	for path, want := range map[string]string{
		"/":                   "/",
		"/capabilities":       "/capabilities",
		"/llms.txt":           "/llms.txt",
		"/api/agent/abc123":   "/api/agent",
		"/v1/command":         "/v1/command",
		"/v1/events/42":       "/v1/events",
		"/r/secret-room":      "/r",
		"/call/notary/stamp":  "/call",
		"/c64/eyJvcCI6InBvc3": "/c64",
	} {
		if got := routeClass(path); got != want {
			t.Errorf("routeClass(%q) = %q, want %q", path, got, want)
		}
	}
}

// statusWriter keeps the first status and still flushes.
func TestStatusWriterRecordsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusWriter{ResponseWriter: rec}
	w.WriteHeader(503)
	w.Flush()
	if w.status != 503 || rec.Code != 503 || !rec.Flushed {
		t.Fatalf("status %d, recorder %d, flushed %v", w.status, rec.Code, rec.Flushed)
	}
	if _, ok := any(w).(http.Flusher); !ok {
		t.Fatal("not a Flusher")
	}
}
