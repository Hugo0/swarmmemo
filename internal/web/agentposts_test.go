package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// The hot directory pages on with its own order, and an agent's page lists
// its posts newest first with a link to older ones, a search, and a restart
// when a page link no longer applies.
func TestHotDirectoryAndAgentPostsPage(t *testing.T) {
	id := strings.Repeat("e", 64)
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "agents.list":
			return board.Result{OK: true, Agents: []board.Agent{{ID: id, Handle: "echo"}}, NextCursor: "next-hot", Data: map[string]any{"has_more": true, "sort": "hot"}}, nil
		case "agent.get":
			return board.Result{OK: true, Agent: &board.Agent{ID: id, Handle: "echo"}}, nil
		case "agent.posts":
			if c.Cursor == "expired" {
				return board.Result{}, &board.Error{Status: 409, Code: "cursor_expired", Message: "expired"}
			}
			return board.Result{OK: true, Messages: []board.Message{{ID: "m1", Type: "message", Room: "lobby", Text: "a public post", Author: id, Visibility: "public"}}, NextCursor: "older-1", Data: map[string]any{"has_more": true}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	get := func(path string) string {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		return w.Body.String()
	}
	body := get("/agents")
	if !strings.Contains(body, `href="/agents?sort=hot&amp;cursor=next-hot">More agents`) {
		t.Fatal("the hot directory has no next page")
	}
	get("/agents?sort=hot&cursor=next-hot")
	if c := s.calls[len(s.calls)-1]; c.Kind != "hot" || c.Cursor != "next-hot" {
		t.Fatalf("hot page two read %+v", c)
	}

	body = get("/agent/" + id)
	c := s.calls[len(s.calls)-1]
	if c.Operation != "agent.posts" || c.Target != id || c.Cursor != "" {
		t.Fatalf("posts read %+v", c)
	}
	for _, want := range []string{`<h2>Posts</h2>`, "a public post", `href="/agent/` + id + `?cursor=older-1#posts">Older posts`, `name="q"`, `/inbox/` + id} {
		if !strings.Contains(body, want) {
			t.Errorf("agent page lacks %q", want)
		}
	}
	if strings.Contains(body, ">Newest posts<") {
		t.Error("the first page offers itself as the newest posts")
	}
	body = get("/agent/" + id + "?cursor=older-1&q=post")
	if c := s.calls[len(s.calls)-1]; c.Cursor != "older-1" || c.Query != "post" {
		t.Fatalf("posts page two read %+v", c)
	}
	if !strings.Contains(body, ">Newest posts<") || !strings.Contains(body, `?cursor=older-1&amp;q=post#posts`) {
		t.Error("a later page lacks its way back or its search")
	}
	body = get("/agent/" + id + "?cursor=expired")
	if c := s.calls[len(s.calls)-1]; c.Cursor != "" || !strings.Contains(body, "start again from the newest") {
		t.Error("an unusable posts cursor must restart from the newest posts")
	}
}
