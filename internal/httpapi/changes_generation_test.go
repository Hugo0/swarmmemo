package httpapi

import (
	"context"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

type scopedChangesFixture struct {
	fakeService
	expected string
}

func (f *scopedChangesFixture) PublicUpdates(context.Context, int64) ([]board.Message, int64, error) {
	return []board.Message{}, 7, nil
}
func (f *scopedChangesFixture) PublicUpdatesGeneration(_ context.Context, after int64, expected string) ([]board.Message, int64, string, error) {
	f.expected = expected
	if expected != "" && expected != strings.Repeat("a", 32) {
		return nil, after, "", &board.Error{Status: 409, Code: "cursor_reset", Message: "reset"}
	}
	return []board.Message{}, 7, strings.Repeat("a", 32), nil
}

func TestChangesGenerationHTTP(t *testing.T) {
	f := &scopedChangesFixture{}
	s := New(f, nil, Config{})
	w := makeRequest(s, "GET", "/api/changes?after=-1", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"generation":"`+strings.Repeat("a", 32)+`"`) || !strings.Contains(w.Body.String(), `"service_id":"swarmmemo.com"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/api/changes?after=7&generation="+strings.Repeat("a", 32), "", "")
	if w.Code != 200 || f.expected != strings.Repeat("a", 32) {
		t.Fatal(w.Code, w.Body.String(), f.expected)
	}
	w = makeRequest(s, "GET", "/api/changes?after=7&generation="+strings.Repeat("b", 32), "", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "cursor_reset") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, query := range []string{"generation=", "generation=bad", "generation=" + strings.Repeat("A", 32), "generation=" + strings.Repeat("a", 32) + "&generation=" + strings.Repeat("a", 32), "after=7&after=8", "after=-2", "unrecognized=1"} {
		w = makeRequest(s, "GET", "/api/changes?"+query, "", "")
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
	}
}
