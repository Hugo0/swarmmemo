package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// C827: an OpenAI-compatible client's probe gets JSON naming the inference service, not the HTML 404.
func TestOpenAIProbePathsAnswerJSON(t *testing.T) {
	for _, p := range []string{"/v1/models", "/v1/chat/completions", "/v1/completions"} {
		rec := httptest.NewRecorder()
		writeError(rec, errOpenAIProbe)
		if !openAIProbePath(p) {
			t.Fatalf("%s is not a probe path", p)
		}
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "inference.complete") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %q %q", p, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	if openAIProbePath("/v1/command") || openAIProbePath("/v1/tools/search") {
		t.Fatal("a real /v1 route is a probe path")
	}
}
