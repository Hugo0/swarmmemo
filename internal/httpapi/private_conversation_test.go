package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// A private conversation end to end over GET /c64/ alone, the path for tools
// that can only fetch a URL: open, invite, join, post and read, all through
// the generic command dispatcher. Every answer is no-store, and HTTPS answers
// carry no cleartext label.
func TestPrivateConversationOverC64(t *testing.T) {
	_, s := viaFixture(t)
	owner, guest := newRoomKey(t), newRoomKey(t)
	send := func(c board.Command) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(c)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "https://swarmmemo.com"+c64(string(raw)), nil)
		r.RemoteAddr = "198.51.100.8:12345"
		s.ServeHTTP(w, r)
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s: Cache-Control %q", c.Operation, cc)
		}
		if strings.Contains(w.Body.String(), "not encrypted") {
			t.Fatalf("%s: an HTTPS answer carries the cleartext label", c.Operation)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out["ok"] != true {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return out
	}
	send(owner.sign(board.Command{Operation: "room.create", Room: "case-c64", Visibility: "private", RequestID: "open"}))
	invite := send(owner.sign(board.Command{Operation: "room.invite.create", Room: "case-c64", RequestID: "invite"}))["data"].(map[string]any)
	secret, _ := invite["secret"].(string)
	if invite["code"] != "case-c64."+secret || len(secret) != 43 {
		t.Fatalf("invite: %v", invite)
	}
	joined := send(guest.sign(board.Command{Operation: "room.invite.accept", Room: "case-c64", Data: secret, RequestID: "join"}))["data"].(map[string]any)
	if joined["member"] != guest.id {
		t.Fatalf("join: %v", joined)
	}
	send(guest.sign(board.Command{Operation: "post", Room: "case-c64", Visibility: "private", Text: "the failing test and its log", RequestID: "post"}))
	messages := send(owner.sign(board.Command{Operation: "messages.list", Room: "case-c64"}))["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("read: %v", messages)
	}
	m := messages[0].(map[string]any)
	if m["text"] != "the failing test and its log" || m["visibility"] != "private" || m["via"] != "c64" || m["author"] != guest.id {
		t.Fatalf("message: %v", m)
	}
}
