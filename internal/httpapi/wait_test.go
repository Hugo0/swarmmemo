package httpapi

// C596: a refusal the board times carries next_open_at and retry_after on
// every wire: the JSON error with the Retry-After header, and the MCP tool
// error's structured content.

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// Over HTTP: the third anonymous thread in an hour (rate 2) is 429
// anonymous_post_rate with next_open_at at the next hour, retry_after the
// seconds to it, and the same in Retry-After.
func TestWaitOverHTTP(t *testing.T) {
	g := newA2ARig(t, 2)
	var w *httptest.ResponseRecorder
	for i := range 3 {
		w = g.do("203.0.113.40", "POST", "/v1/command", `{"operation":"post","room":"lobby","text":"thread `+strconv.Itoa(i)+`"}`, nil)
	}
	var body struct {
		Error struct {
			Code       string `json:"code"`
			RetryAfter int    `json:"retry_after"`
			NextOpenAt int64  `json:"next_open_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	e := body.Error
	if w.Code != 429 || e.Code != "anonymous_post_rate" || e.NextOpenAt%3600 != 0 || e.NextOpenAt <= now || e.NextOpenAt > now+3600 || w.Header().Get("Retry-After") != strconv.Itoa(e.RetryAfter) {
		t.Fatalf("third thread: %d %s Retry-After %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	if d := e.NextOpenAt - int64(e.RetryAfter); d < now-5 || d > now {
		t.Fatalf("retry_after %d is not the seconds to next_open_at %d", e.RetryAfter, e.NextOpenAt)
	}
	// A retry hint has retry_after and the header, never next_open_at.
	rec := httptest.NewRecorder()
	writeError(rec, board.ErrBusy())
	if rec.Header().Get("Retry-After") != "1" || strings.Contains(rec.Body.String(), "next_open_at") {
		t.Fatalf("busy: %s %q", rec.Body.String(), rec.Header().Get("Retry-After"))
	}
}

// Over MCP: the same reply again is 409 duplicate_reply, whose structured
// error carries next_open_at a window after the first, retry_after and
// retry_after_seconds.
func TestWaitOverMCP(t *testing.T) {
	_, s := hostedServer(t)
	token := newIdentity(t, s, "")["token"].(string)
	path := "/mcp/t/" + token
	task := mustTool(t, s, path, "", "post_message", map[string]any{"room": "lobby", "text": "Who can review a draft?"})
	parent, _ := dig(task, "data", "receipt", "id").(string)
	if parent == "" {
		parent, _ = dig(task, "receipt", "id").(string)
	}
	if parent == "" {
		t.Fatalf("no receipt id in %v", task)
	}
	before := time.Now().Unix()
	mustTool(t, s, path, "", "post_message", map[string]any{"room": "lobby", "reply_to": parent, "text": "I can, send it over."})
	// The same reply as copies compare it; an identical one would answer as
	// the first (C789).
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "post_message", "arguments": map[string]any{"room": "lobby", "reply_to": parent, "text": "I can,  send it over."}}})
	out := mcpRequest(t, s, path, "", string(raw))
	e, _ := dig(out, "result", "structuredContent", "error").(map[string]any)
	next, _ := e["next_open_at"].(float64)
	retry, _ := e["retry_after"].(float64)
	if e["code"] != "duplicate_reply" || int64(next) < before+board.ReplyRepeatWindowSeconds || int64(next) > time.Now().Unix()+board.ReplyRepeatWindowSeconds || retry <= 0 || e["retry_after_seconds"] != retry {
		t.Fatalf("duplicate reply over MCP: %v", out)
	}
}
