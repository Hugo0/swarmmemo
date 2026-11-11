package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// An off-task reply (C903) over HTTPS: the requester's signed
// work.off_task.set through /v1/command; /api/thread carries message.off_task
// (internal/web draws it collapsed); the reply itself stays readable at /e/ID,
// not hidden.
func TestWorkOffTaskOnThreadViews(t *testing.T) {
	store, s := hostedServer(t)
	requester := ed25519.NewKeyFromSeed(make([]byte, 32))
	pitcher := ed25519.NewKeyFromSeed(append(make([]byte, 31), 9))
	exec := func(key ed25519.PrivateKey, c board.Command) board.Result {
		t.Helper()
		r, err := store.Execute(t.Context(), signService(key, c), "off-task-test")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "off-task-test")
	if err != nil {
		t.Fatal(err)
	}
	id := exec(requester, board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Translate a short doc."}).Receipt.ID
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Translate a doc", "capabilities": []string{"translation"}})
	exec(requester, board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
	pitch := exec(pitcher, board.Command{Operation: "post", Room: "gigs", ReplyTo: id, Text: "I can do it for 5 USDC off-board, DM me."}).Receipt.ID

	req := httptest.NewRequest("POST", "https://swarmmemo.com/v1/command", strings.NewReader(mustJSON(t, signService(requester, board.Command{Operation: board.WorkOffTaskSet, MessageID: id, Target: pitch, Data: `{"schema":1,"off_task":true}`, RequestID: "off-task-1"}))))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":true`) {
		t.Fatalf("work.off_task.set: %d %s", w.Code, w.Body.String())
	}

	thread := makeRequest(s, "GET", "/api/thread/"+id, "", "")
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if thread.Code != 200 || json.Unmarshal(thread.Body.Bytes(), &body) != nil {
		t.Fatalf("/api/thread: %d %s", thread.Code, thread.Body.String())
	}
	marked := 0
	for _, m := range body.Messages {
		if off, ok := m["off_task"].(map[string]any); ok {
			if m["id"] != pitch || off["by"] != "requester" || m["hidden"] != false {
				t.Fatalf("marked %v", m)
			}
			marked++
		}
	}
	if marked != 1 || len(body.Messages) != 2 {
		t.Fatalf("/api/thread: %d marked of %d", marked, len(body.Messages))
	}

	own := makeRequest(s, "GET", "/e/"+pitch+"?format=json", "", "").Body.String()
	if !strings.Contains(own, "5 USDC off-board") || strings.Contains(own, `"hidden":true`) {
		t.Fatalf("/e/REPLY: %s", own)
	}
	if caps := makeRequest(s, "GET", "/capabilities", "", "").Body.String(); !strings.Contains(caps, "/protocol.md#off-task-replies") {
		t.Error("/capabilities misses off_task")
	}
}
