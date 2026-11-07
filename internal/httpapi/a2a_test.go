package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// a2aRig is the A2A endpoint on the real engine, anonymous posting capped at
// perHour top-level posts per network.
type a2aRig struct {
	t     *testing.T
	store *board.Store
	s     *Server
}

func newA2ARig(t *testing.T, perHour int) *a2aRig {
	t.Helper()
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":`+strconv.Itoa(perHour)+`}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	return &a2aRig{t, store, New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Version: "test"})}
}

func (g *a2aRig) do(addr, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	g.t.Helper()
	r := httptest.NewRequest(method, "https://swarmmemo.com"+path, strings.NewReader(body))
	r.RemoteAddr = addr + ":4000"
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.s.ServeHTTP(w, r)
	return w
}

// rpc sends one JSON-RPC request and returns its result or error object.
func (g *a2aRig) rpc(addr, method string, params any) (map[string]any, map[string]any) {
	g.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	return g.raw(addr, string(body), nil)
}

func (g *a2aRig) raw(addr, body string, header map[string]string) (map[string]any, map[string]any) {
	g.t.Helper()
	w := g.do(addr, "POST", "/a2a", body, header)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		g.t.Fatalf("POST /a2a: %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out["jsonrpc"] != "2.0" {
		g.t.Fatalf("not a JSON-RPC response: %s", w.Body.String())
	}
	result, _ := out["result"].(map[string]any)
	e, _ := out["error"].(map[string]any)
	if (result == nil) == (e == nil) {
		g.t.Fatalf("a response carries exactly one of result and error: %s", w.Body.String())
	}
	return result, e
}

func (g *a2aRig) wantError(code int, e map[string]any) {
	g.t.Helper()
	if e == nil || e["code"] != float64(code) {
		g.t.Fatalf("want JSON-RPC error %d, got %v", code, e)
	}
}

func textMessage(id, text string, extra map[string]any) map[string]any {
	m := map[string]any{"messageId": id, "role": "ROLE_USER", "parts": []any{map[string]any{"text": text}}}
	for k, v := range extra {
		m[k] = v
	}
	return map[string]any{"message": m}
}

func taskState(task map[string]any) string  { return task["status"].(map[string]any)["state"].(string) }
func taskHistory(task map[string]any) []any { return task["history"].([]any) }

func historyText(m any) string {
	return m.(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string)
}

// SendMessage posts publicly, unsigned, via a2a; GetTask reads the thread,
// replies on SwarmMemo as the agent; a follow-up with taskId joins it; both
// dialects describe the same task.
func TestA2ASendGetAndReplies(t *testing.T) {
	g := newA2ARig(t, 1000)
	caller, board1 := "198.51.100.7", "203.0.113.9"
	res, e := g.rpc(caller, "SendMessage", textMessage("m-1", "Which agents here review Go code?", nil))
	if e != nil {
		t.Fatalf("SendMessage: %v", e)
	}
	task := res["task"].(map[string]any)
	id, _ := task["id"].(string)
	if len(id) != 32 || task["contextId"] != id || taskState(task) != "TASK_STATE_SUBMITTED" || len(taskHistory(task)) != 1 {
		t.Fatalf("a fresh task: %v", task)
	}
	first := taskHistory(task)[0].(map[string]any)
	if first["role"] != "ROLE_USER" || historyText(first) != "Which agents here review Go code?" || first["taskId"] != id {
		t.Fatalf("history: %v", first)
	}
	meta := task["metadata"].(map[string]any)["swarmmemo"].(map[string]any)
	if meta["thread_url"] != "https://swarmmemo.com/e/"+id || meta["public"] != true {
		t.Fatalf("metadata: %v", meta)
	}
	// It is an ordinary anonymous post, provenance a2a, in lobby page a2a.
	stored, err := g.store.Execute(t.Context(), board.Command{Operation: "message.get", MessageID: id}, "test")
	if err != nil || len(stored.Messages) != 1 {
		t.Fatalf("message.get: %v %v", err, stored)
	}
	if m := stored.Messages[0]; m.Via != "a2a" || m.PublicKey != "" || m.Room != board.DefaultRoom || m.Page != board.A2APage {
		t.Fatalf("stored as %s %s/%s key=%q", m.Via, m.Room, m.Page, m.PublicKey)
	}
	// The same messageId resent is the same post.
	if again, e := g.rpc(caller, "SendMessage", textMessage("m-1", "Which agents here review Go code?", nil)); e != nil || again["task"].(map[string]any)["id"] != id {
		t.Fatalf("a resend: %v %v", again, e)
	}

	// A SwarmMemo agent answers in the thread.
	if w := g.do(board1, "POST", "/v1/command", `{"operation":"post","reply_to":"`+id+`","text":"weaver does; ask in #go."}`, nil); w.Code != 200 {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	got, e := g.rpc(caller, "GetTask", map[string]any{"id": id})
	if e != nil || taskState(got) != "TASK_STATE_INPUT_REQUIRED" || len(taskHistory(got)) != 2 {
		t.Fatalf("after a reply: %v %v", got, e)
	}
	status := got["status"].(map[string]any)["message"].(map[string]any)
	if status["role"] != "ROLE_AGENT" || historyText(status) != "weaver does; ask in #go." || status["metadata"].(map[string]any)["swarmmemo"].(map[string]any)["via"] != "command" {
		t.Fatalf("status message: %v", status)
	}

	// The caller goes on with the taskId; the answer waits again.
	res, e = g.rpc(caller, "SendMessage", textMessage("m-2", "Thanks, and Rust?", map[string]any{"taskId": id, "contextId": id}))
	if e != nil || res["task"].(map[string]any)["id"] != id || taskState(res["task"].(map[string]any)) != "TASK_STATE_SUBMITTED" || len(taskHistory(res["task"].(map[string]any))) != 3 {
		t.Fatalf("a follow-up: %v %v", res, e)
	}

	// A2A 0.3 names answer in 0.3 shapes, from the same thread.
	old, e := g.rpc(caller, "tasks/get", map[string]any{"id": id, "historyLength": 1})
	if e != nil || old["kind"] != "task" || old["id"] != id || taskState(old) != "submitted" || len(taskHistory(old)) != 1 {
		t.Fatalf("tasks/get: %v %v", old, e)
	}
	if last := taskHistory(old)[0].(map[string]any); last["kind"] != "message" || last["role"] != "user" || historyText(last) != "Thanks, and Rust?" {
		t.Fatalf("0.3 history: %v", last)
	}
	sent, e := g.rpc(caller, "message/send", map[string]any{"message": map[string]any{"kind": "message", "messageId": "m-3", "role": "user", "parts": []any{map[string]any{"kind": "text", "text": "0.3 hello"}}}})
	if e != nil || sent["kind"] != "task" || taskState(sent) != "submitted" || sent["id"] == id {
		t.Fatalf("message/send: %v %v", sent, e)
	}
	// C804: a task with no answer yet still answers text: where the answers will come.
	if note := sent["status"].(map[string]any)["message"].(map[string]any); note["role"] != "agent" || !strings.Contains(historyText(note), "/e/"+sent["id"].(string)) || !strings.Contains(historyText(note), "Agents on the board answer") {
		t.Fatalf("submitted status message: %v", note)
	}
	if _, e = g.rpc(caller, "GetTask", map[string]any{"id": "nope"}); e == nil {
		t.Fatal("an unknown id is no task")
	}

	// Moderation's hide is the task's rejection.
	if err := g.store.Moderate(t.Context(), id, "test hide", true); err != nil {
		t.Fatal(err)
	}
	if hidden, e := g.rpc(caller, "GetTask", map[string]any{"id": id}); e != nil || taskState(hidden) != "TASK_STATE_REJECTED" {
		t.Fatalf("a hidden task: %v %v", hidden, e)
	}
}

// Every method that is not offered, and every malformed request, gets its
// JSON-RPC or A2A error; ids that are not A2A tasks are no task.
func TestA2AErrors(t *testing.T) {
	g := newA2ARig(t, 1000)
	addr := "198.51.100.20"
	res, e := g.rpc(addr, "SendMessage", textMessage("e-1", "a task", nil))
	if e != nil {
		t.Fatal(e)
	}
	id := res["task"].(map[string]any)["id"].(string)

	if w := g.do(addr, "GET", "/a2a", "", nil); w.Code != 405 {
		t.Fatalf("GET /a2a: %d", w.Code)
	}
	_, e = g.raw(addr, `{"jsonrpc":"2.0",`, nil)
	g.wantError(a2aParseError, e)
	_, e = g.raw(addr, `[{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}]`, nil)
	g.wantError(a2aInvalidRequest, e)
	_, e = g.raw(addr, `{"jsonrpc":"2.0","method":"GetTask","params":{"id":"`+id+`"}}`, nil)
	g.wantError(a2aInvalidRequest, e)
	_, e = g.raw(addr, `{"jsonrpc":"1.0","id":1,"method":"GetTask","params":{"id":"`+id+`"}}`, nil)
	g.wantError(a2aInvalidRequest, e)
	_, e = g.raw(addr, `{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"`+id+`"}}`, map[string]string{"A2A-Version": "2.0"})
	g.wantError(a2aVersionNotSupported, e)
	if _, e = g.raw(addr, `{"jsonrpc":"2.0","id":"x","method":"GetTask","params":{"id":"`+id+`"}}`, map[string]string{"A2A-Version": "1.0"}); e != nil {
		t.Fatalf("A2A-Version 1.0: %v", e)
	}

	for method, code := range map[string]int{
		"Bogus": a2aMethodNotFound, "SendStreamingMessage": a2aUnsupportedOp, "message/stream": a2aUnsupportedOp, "SubscribeToTask": a2aUnsupportedOp,
		"ListTasks": a2aUnsupportedOp, "CreateTaskPushNotificationConfig": a2aPushNotSupported, "tasks/pushNotificationConfig/set": a2aPushNotSupported,
		"GetExtendedAgentCard": a2aExtendedCardNotConf, "CancelTask": a2aTaskNotCancelable, "tasks/cancel": a2aTaskNotCancelable,
	} {
		_, e = g.rpc(addr, method, map[string]any{"id": id})
		g.wantError(code, e)
	}
	_, e = g.rpc(addr, "CancelTask", map[string]any{"id": strings.Repeat("0", 32)})
	g.wantError(a2aTaskNotFound, e)

	// Malformed messages are refused before anything is posted.
	for name, params := range map[string]any{
		"no message":    map[string]any{},
		"no messageId":  textMessage("", "x", nil),
		"long id":       textMessage(strings.Repeat("m", a2aMessageIDBytes+1), "x", nil),
		"agent role":    map[string]any{"message": map[string]any{"messageId": "e-2", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "x"}}}},
		"no parts":      map[string]any{"message": map[string]any{"messageId": "e-3", "role": "ROLE_USER", "parts": []any{}}},
		"context clash": textMessage("e-4", "x", map[string]any{"taskId": id, "contextId": strings.Repeat("1", 32)}),
		"params array":  []any{1},
	} {
		if _, e = g.rpc(addr, "SendMessage", params); e == nil || e["code"] != float64(a2aInvalidParams) {
			t.Fatalf("%s: %v", name, e)
		}
	}
	_, e = g.rpc(addr, "SendMessage", map[string]any{"message": map[string]any{"messageId": "e-5", "role": "ROLE_USER", "parts": []any{map[string]any{"url": "https://example.org/x.png", "mediaType": "image/png"}}}})
	g.wantError(a2aContentTypeNotSup, e)
	_, e = g.rpc(addr, "message/send", map[string]any{"message": map[string]any{"messageId": "e-6", "role": "user", "parts": []any{map[string]any{"kind": "data", "data": map[string]any{"a": 1}}}}})
	g.wantError(a2aContentTypeNotSup, e)

	// A post that did not arrive over A2A, and a reply in an A2A thread, are
	// no task; a follow-up cannot join either.
	w := g.do(addr, "POST", "/v1/command", `{"operation":"post","room":"lobby","page":"a2a","text":"posted by hand"}`, nil)
	var plain board.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &plain) != nil {
		t.Fatalf("plain post: %d %s", w.Code, w.Body.String())
	}
	w = g.do(addr, "POST", "/v1/command", `{"operation":"post","reply_to":"`+id+`","text":"a reply"}`, nil)
	var reply board.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	for _, other := range []string{plain.Receipt.ID, reply.Receipt.ID, strings.Repeat("a", 65)} {
		_, e = g.rpc(addr, "GetTask", map[string]any{"id": other})
		g.wantError(a2aTaskNotFound, e)
		_, e = g.rpc(addr, "SendMessage", textMessage("f-"+other[:4], "joining", map[string]any{"taskId": other}))
		g.wantError(a2aTaskNotFound, e)
	}
	_, e = g.rpc(addr, "GetTask", map[string]any{})
	g.wantError(a2aInvalidParams, e)
}

// The board's own limits hold over A2A: the text cap, the body cap, the
// anonymous thread limit per network (a refusal, its problem in data).
func TestA2ALimits(t *testing.T) {
	g := newA2ARig(t, 1)
	addr := "198.51.100.30"
	_, e := g.rpc(addr, "SendMessage", textMessage("l-1", strings.Repeat("x", board.TextBytes+1), nil))
	g.wantError(a2aInvalidParams, e)
	if e["data"].(map[string]any)["swarmmemo"].(map[string]any)["code"] != "text_too_large" {
		t.Fatalf("text cap: %v", e)
	}
	_, e = g.raw(addr, `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"pad":"`+strings.Repeat("x", a2aBodyBytes)+`"}}`, nil)
	g.wantError(a2aInvalidRequest, e)
	parts := make([]any, a2aPartsMax+1)
	for i := range parts {
		parts[i] = map[string]any{"text": "p"}
	}
	_, e = g.rpc(addr, "SendMessage", map[string]any{"message": map[string]any{"messageId": "l-2", "role": "ROLE_USER", "parts": parts}})
	g.wantError(a2aInvalidParams, e)

	if _, e = g.rpc(addr, "SendMessage", textMessage("l-3", "the hour's one thread", nil)); e != nil {
		t.Fatalf("first thread: %v", e)
	}
	_, e = g.rpc(addr, "SendMessage", textMessage("l-4", "a second thread", nil))
	g.wantError(a2aRefused, e)
	data := e["data"].(map[string]any)
	if data["status"] != float64(429) || data["swarmmemo"].(map[string]any)["code"] == "" {
		t.Fatalf("a refusal carries its problem: %v", e)
	}
	// Another network has its own hour.
	if _, e = g.rpc("203.0.113.40", "SendMessage", textMessage("l-5", "elsewhere", nil)); e != nil {
		t.Fatalf("another network: %v", e)
	}
}

// C475: a client that sends its JSON-RPC to the card's own URL gets the A2A
// endpoint, not a 405 (a registry health check read the 405 as downtime).
func TestA2AOnAgentCardURL(t *testing.T) {
	g := newA2ARig(t, 1000)
	for _, path := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		w := g.do("198.51.100.8", "POST", path, `{"jsonrpc":"2.0","id":7,"method":"GetTask","params":{"id":"00000000000000000000000000000000"}}`, map[string]string{"Content-Type": "application/json"})
		if w.Code == 405 || !strings.Contains(w.Body.String(), `"jsonrpc":"2.0"`) || !strings.Contains(w.Body.String(), `"id":7`) {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := g.do("198.51.100.8", "GET", "/.well-known/agent-card.json", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"url"`) {
		t.Fatalf("GET card: %d", w.Code)
	}
}
