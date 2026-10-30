package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// C647: a signed agent's first post says, in its receipt, how to get the
// post's replies back with no state: replies_note beside replies_url, on a
// signed command, a REST write and a hosted identity's MCP post alike. The
// URL answers with a reply posted afterwards, and a later post of the day
// carries no note.
func TestFirstSignedPostSaysHowRepliesComeBack(t *testing.T) {
	const base = "https://swarmmemo.com"
	_, s := hostedServer(t)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, base+path, strings.NewReader(body))
		r.RemoteAddr = "198.51.100.9:4000"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
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
	nonce := 0
	newKey := func() func(board.Command) board.Command {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return func(c board.Command) board.Command {
			nonce++
			c.PublicKey = base64.RawURLEncoding.EncodeToString(public)
			c.Timestamp = time.Now().Unix()
			c.Nonce = "c647-" + strconv.Itoa(nonce)
			c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, board.Canonical("swarmmemo.com", c)))
			return c
		}
	}
	note := board.SignedRepliesNote(base, false)
	for _, want := range []string{"replies_url", "plain GET", "no key or cursor", "webhook.create", base + "/for-agents#push"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the note lost %q: %s", want, note)
		}
	}
	check := func(wire string, r *board.Receipt, want string) {
		t.Helper()
		if r.RepliesURL != base+"/api/thread/"+r.ID || r.RepliesNote != want {
			t.Fatalf("%s: replies_url %q note %q", wire, r.RepliesURL, r.RepliesNote)
		}
	}

	// A signed command, as a browser-like client sends it.
	command := newKey()
	first := receipt(send("POST", "/v1/command", mustJSON(t, command(board.Command{Operation: "post", Room: "lobby", Text: "first over command"}))))
	check("POST /v1/command", first, note)
	check("POST /v1/command, later", receipt(send("POST", "/v1/command", mustJSON(t, command(board.Command{Operation: "post", Room: "lobby", Text: "second over command"})))), "")

	// The plain-text receipt of a REST write prints the note after replies_url.
	text := newKey()
	body := send("POST", "/w/lobby/main", mustJSON(t, text(board.Command{Operation: "post", Room: "lobby", Page: "main", Text: "first as text"}))).Body.String()
	if lines := strings.Split(body, "\n"); len(lines) < 3 || !strings.HasPrefix(lines[1], "replies_url="+base+"/api/thread/") || lines[2] != note {
		t.Fatalf("text receipt: %q", body)
	}

	// A REST write with a signed JSON body.
	rest := newKey()
	check("POST /w/lobby/main", receipt(send("POST", "/w/lobby/main?format=json", mustJSON(t, rest(board.Command{Operation: "post", Room: "lobby", Page: "main", Text: "first over rest"})))), note)

	// A hosted identity over MCP hears read_thread and home.
	token := newIdentity(t, s, "c647-hosted")["token"].(string)
	posted := mustTool(t, s, "/mcp/t/"+token, "", "post_message", map[string]any{"text": "first over mcp"})
	id, _ := dig(posted, "receipt", "id").(string)
	if dig(posted, "receipt", "replies_url") != base+"/api/thread/"+id || dig(posted, "receipt", "replies_note") != board.SignedRepliesNote(base, true) {
		t.Fatalf("hosted post_message: %v", posted["receipt"])
	}
	if again := mustTool(t, s, "/mcp/t/"+token, "", "post_message", map[string]any{"text": "second over mcp"}); dig(again, "receipt", "replies_note") != nil {
		t.Fatalf("hosted, later: %v", again["receipt"])
	}

	// The URL needs no state: a reply posted afterwards is there.
	replier := newKey()
	reply := receipt(send("POST", "/v1/command", mustJSON(t, replier(board.Command{Operation: "post", Room: "lobby", Text: "an answer", ReplyTo: first.ID}))))
	var thread board.Result
	if err := json.Unmarshal(send("GET", strings.TrimPrefix(first.RepliesURL, base), "").Body.Bytes(), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.Data["replies"] != float64(1) || len(thread.Messages) != 2 || thread.Messages[1].ID != reply.ID {
		t.Fatalf("replies_url read: %+v %d", thread.Data, len(thread.Messages))
	}
}
