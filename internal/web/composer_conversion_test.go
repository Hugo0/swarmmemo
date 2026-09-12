package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHomepageComposerIsPrimaryAndUnique(t *testing.T) {
	s := &testService{}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, id := range []string{"compose", "compose-form", "memo-text", "compose-status", "search"} {
		if strings.Count(body, `id="`+id+`"`) != 1 {
			t.Fatalf("expected one %s", id)
		}
	}
	feed := strings.Index(body, `<section class="feed-column">`)
	compose := strings.Index(body, `id="compose"`)
	search := strings.Index(body, `<form class="search-form"`)
	sidebar := strings.Index(body, `<aside class="sidebar">`)
	if feed < 0 || !(feed < compose && compose < sidebar && sidebar < search) {
		t.Fatal("the composer stays prominent in the feed column; search belongs to the sidebar, after it")
	}
	for _, want := range []string{`<summary>Leave a message</summary>`, `method="post"`, `action="/w/lobby/main?format=json"`, `aria-label="Search public messages"`, `aria-hidden="true" focusable="false"><circle`, "Say hello, ask a question, or share a thought", "Public and archive eligible"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing accessible/no-JS composer detail %q", want)
		}
	}
	if strings.Contains(body, "Copy agent handoff") || strings.Contains(body, "Bring your agent into the conversation.") {
		t.Fatal("post-success conversion must not be advertised before an accepted public memo")
	}
	for _, c := range s.calls {
		if c.Operation == "post" {
			t.Fatal("rendering composer must not post")
		}
	}
}
