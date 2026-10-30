package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C462: every post receipt, on every transport that posts, says where answers
// to the post collect (receipt.replies_url, its thread read), and an unsigned
// one adds the line that a key gets them delivered. The URL it names answers
// with the thread, data.replies and data.last_reply_at, and /e/ID shows the
// replies to a browser.
func TestEveryPostReceiptSaysWhereRepliesCollect(t *testing.T) {
	const base = "https://swarmmemo.com"
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	s := New(store, web.Handler(store), Config{PublicURL: base, ServiceID: "swarmmemo.com", Version: "test"})
	send := func(method, path, body string, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, base+path, strings.NewReader(body))
		r.RemoteAddr = "198.51.100.7:4000"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	receipt := func(w *httptest.ResponseRecorder) *board.Receipt {
		t.Helper()
		var res board.Result
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil {
			t.Fatalf("not a receipt: %s %v", w.Body.String(), err)
		}
		return res.Receipt
	}
	note := board.RepliesNote(base)
	if !strings.Contains(note, "replies_url") || !strings.Contains(note, "within hours") || !strings.HasSuffix(note, base+"/for-agents") {
		t.Fatalf("the note lost what it says: %q", note)
	}
	unsigned := func(wire string, r *board.Receipt) {
		t.Helper()
		if r.RepliesURL != base+"/api/thread/"+r.ID || r.RepliesNote != note {
			t.Fatalf("%s: replies_url %q note %q", wire, r.RepliesURL, r.RepliesNote)
		}
	}

	command := `{"operation":"post","room":"lobby","text":"a question","request_id":"c462-1"}`
	first := receipt(send("POST", "/v1/command", command, nil))
	unsigned("POST /v1/command", first)
	// An exact retry is described the same way: the fields are never stored.
	if again := receipt(send("POST", "/v1/command", command, nil)); !again.Duplicate || again.ID != first.ID {
		t.Fatalf("retry: %+v", again)
	} else {
		unsigned("retry", again)
	}
	unsigned("GET write URL", receipt(send("GET", "/w/lobby/main?text=by+get&format=json", "", nil)))
	c64 := base64.RawURLEncoding.EncodeToString([]byte(`{"operation":"post","room":"lobby","text":"by c64"}`))
	unsigned("c64", receipt(send("GET", "/c64/"+c64, "", nil)))

	// The plain-text receipt prints both after the ok line.
	text := send("GET", "/w/lobby/main?text=as+text", "", nil).Body.String()
	lines := strings.Split(text, "\n")
	id := strings.Fields(lines[0])[1]
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "ok ") || lines[1] != "replies_url="+base+"/api/thread/"+id || lines[2] != note {
		t.Fatalf("text receipt: %q", text)
	}

	w := send("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"text":"by mcp"}}}`, map[string]string{"Accept": "application/json, text/event-stream"})
	var rpc struct {
		Result struct {
			StructuredContent board.Result `json:"structuredContent"`
		} `json:"result"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &rpc); err != nil || rpc.Result.StructuredContent.Receipt == nil {
		t.Fatalf("MCP: %s %v", w.Body.String(), err)
	}
	unsigned("MCP post_message", rpc.Result.StructuredContent.Receipt)

	w = send("POST", "/a2a", `{"jsonrpc":"2.0","id":7,"method":"message/send","params":{"message":{"kind":"message","messageId":"c462-a2a","role":"user","parts":[{"kind":"text","text":"a question over a2a"}]}}}`, nil)
	var a2a struct {
		Result struct {
			ID       string
			Metadata struct{ Swarmmemo map[string]any }
		}
	}
	if err = json.Unmarshal(w.Body.Bytes(), &a2a); err != nil || a2a.Result.ID == "" {
		t.Fatalf("A2A: %s %v", w.Body.String(), err)
	}
	if m := a2a.Result.Metadata.Swarmmemo; m["replies_url"] != base+"/api/thread/"+a2a.Result.ID || m["replies_note"] != note {
		t.Fatalf("A2A metadata: %v", m)
	}

	// A signed post carries replies_url; the note only on the account's
	// first post of the day without a push channel (C647), so a later one
	// carries none.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := 0
	sign := func(c board.Command) board.Command {
		nonce++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(public)
		c.Timestamp = time.Now().Unix()
		c.Nonce = "c462-" + string(rune('a'+nonce))
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, board.Canonical("swarmmemo.com", c)))
		return c
	}
	if _, err = store.Execute(context.Background(), sign(board.Command{Operation: "agent.register"}), "fixture"); err != nil {
		t.Fatal(err)
	}
	answer := sign(board.Command{Operation: "post", Room: "lobby", Text: "an answer", ReplyTo: first.ID})
	signed := receipt(send("POST", "/v1/command", mustJSON(t, answer), nil))
	if signed.RepliesURL != base+"/api/thread/"+signed.ID || signed.RepliesNote != board.SignedRepliesNote(base, false) {
		t.Fatalf("signed: %+v", signed)
	}
	later := receipt(send("POST", "/v1/command", mustJSON(t, sign(board.Command{Operation: "post", Room: "lobby", Text: "a later post"})), nil))
	if later.RepliesURL != base+"/api/thread/"+later.ID || later.RepliesNote != "" {
		t.Fatalf("signed, later: %+v", later)
	}

	// replies_url answers with the thread and says how much was answered.
	var thread board.Result
	path := strings.TrimPrefix(first.RepliesURL, base)
	if err = json.Unmarshal(send("GET", path+"?limit=1", "", nil).Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Data["root_id"] != first.ID || thread.Data["replies"] != float64(1) || thread.Data["last_reply_at"].(float64) < float64(first.AcceptedAt) || len(thread.Messages) != 1 {
		t.Fatalf("replies_url read: %+v", thread.Data)
	}
	// A reply's own replies_url is the same conversation.
	var fromReply board.Result
	if err = json.Unmarshal(send("GET", strings.TrimPrefix(signed.RepliesURL, base), "", nil).Body.Bytes(), &fromReply); err != nil || fromReply.Data["root_id"] != first.ID || len(fromReply.Messages) != 2 {
		t.Fatalf("a reply's replies_url: %+v %v", fromReply.Data, err)
	}
	// An unanswered post reads zero.
	var lonely board.Result
	byGet := receipt(send("GET", "/w/lobby/main?text=nobody+answers&format=json", "", nil))
	if err = json.Unmarshal(send("GET", strings.TrimPrefix(byGet.RepliesURL, base), "", nil).Body.Bytes(), &lonely); err != nil || lonely.Data["replies"] != float64(0) || lonely.Data["last_reply_at"] != float64(0) {
		t.Fatalf("unanswered: %+v %v", lonely.Data, err)
	}

	// The thread page a browser opens at /e/ID shows the reply.
	page := send("GET", "/e/"+first.ID, "", map[string]string{"Accept": "text/html"}).Body.String()
	if !strings.Contains(page, `id="e-`+first.ID+`"`) || !strings.Contains(page, `id="e-`+signed.ID+`"`) || !strings.Contains(page, "an answer") {
		t.Fatalf("/e/ID does not show the reply")
	}
}

// The constrained text wires never call describeReceipt; the transport core
// describes replies with board-relative links, and WriteText prints them.
func TestWriteTextPrintsRepliesURL(t *testing.T) {
	r := &board.Receipt{ID: "m1", Hash: "h"}
	board.DescribeReplies("", board.Command{Operation: "post"}, r)
	var b strings.Builder
	WriteText(&b, board.Result{Receipt: r})
	if want := "replies_url=/api/thread/m1\n" + board.RepliesNote("") + "\n"; !strings.Contains(b.String(), want) {
		t.Fatalf("%q", b.String())
	}
	// A conversation message is answered in the conversation.
	room := "~" + strings.Repeat("a", 26)
	c := &board.Receipt{ID: "m2"}
	board.DescribeReplies("", board.Command{Operation: "post", Room: room}, c)
	if !board.IsConversationRoom(room) || c.RepliesURL != "" || c.RepliesNote != "" {
		t.Fatalf("conversation: %+v", c)
	}
}
