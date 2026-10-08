package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C72: on a conversation page, an anonymous post that has a reply says its
// author never sees the reply in /api/updates. A signed post, and an
// anonymous post nobody answered, carry no note.
func TestThreadNotesRepliesTheAnonymousPosterNeverSees(t *testing.T) {
	const key = "9cf9c2894cac0f1c2d3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394"
	thread := []board.Message{
		{ID: "root", Room: "lobby", Page: "main", Kind: "note", Text: "a question", Author: "anonymous", Visibility: "public"},
		{ID: "answer", Room: "lobby", Page: "main", Kind: "note", Text: "an answer", Author: key, PublicKey: "pk", ReplyTo: "root", Visibility: "public"},
		{ID: "lonely", Room: "lobby", Page: "main", Kind: "note", Text: "unanswered", Author: "anonymous", ReplyTo: "answer", Visibility: "public"},
	}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "thread.get" {
			return board.Result{OK: true, Messages: thread, Data: map[string]any{"root_id": "root"}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/e/root", nil))
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	note := `<p class="anon-replies">` + tip("anon-replies")
	if strings.Count(body, `class="anon-replies"`) != 1 || !strings.Contains(body, strings.ReplaceAll(note, "'", "&#39;")) {
		t.Fatalf("want one note, under the answered anonymous post: %d", strings.Count(body, `class="anon-replies"`))
	}
	root, rest, _ := strings.Cut(body, `id="e-answer"`)
	if !strings.Contains(root[strings.Index(root, `id="e-root"`):], `class="anon-replies"`) || strings.Contains(rest, `class="anon-replies"`) {
		t.Fatal("the note is not under the answered anonymous post")
	}
	if tip("anon-replies") != "Posted anonymously: replies don't reach the poster's updates. Sign to get them." {
		t.Fatalf("glossary wording changed: %q", tip("anon-replies"))
	}
}
