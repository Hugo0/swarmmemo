package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestAgentDiscoveryHeadersDoNotRequireHTML(t *testing.T) {
	for _, path := range []string{"/health", "/api/messages", "/capabilities", "/llms.txt"} {
		w := makeRequest(New(&fakeService{}, nil, Config{}), http.MethodGet, path, "", "")
		for _, target := range []string{"</llms.txt>", "</openapi.json>", "</capabilities>"} {
			if !strings.Contains(w.Header().Get("Link"), target) {
				t.Fatalf("%s missing discoverable %s", path, target)
			}
		}
		if !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Link") {
			t.Fatal("cross-origin clients cannot read discovery links")
		}
	}
}

func TestConversationReadAdapters(t *testing.T) {
	for _, tc := range []struct{ path, operation, eventID, room, kind string }{
		{"/api/thread/memo123?limit=2&cursor=resume", "thread.get", "memo123", "", ""},
		{"/api/pages?room=lobby&limit=2&cursor=resume", "room.pages", "", "lobby", ""},
		{"/api/messages?kind=imported&limit=2&cursor=resume", "messages.list", "", "", "imported"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			f := &fakeService{}
			w := makeRequest(New(f, nil, Config{}), http.MethodGet, tc.path, "", "")
			if w.Code != http.StatusOK || len(f.commands) != 1 {
				t.Fatalf("status=%d body=%s commands=%+v", w.Code, w.Body.String(), f.commands)
			}
			c := f.commands[0]
			if c.Operation != tc.operation || c.MessageID != tc.eventID || c.Room != tc.room || c.Kind != tc.kind || c.Cursor != "resume" || c.Limit != 2 {
				t.Fatalf("unexpected command: %+v", c)
			}
		})
	}
}

func TestConversationPathRejectsAmbiguity(t *testing.T) {
	for _, path := range []string{"/api/thread/", "/api/thread/a/b", "/api/thread/a?message_id=b", "/api/thread/a?message_id=a&message_id=b", "/api/pages?room=a&room=b"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), http.MethodGet, path, "", "")
		if w.Code != http.StatusBadRequest || len(f.commands) != 0 {
			t.Fatalf("%s: status=%d commands=%+v", path, w.Code, f.commands)
		}
	}
}

// data is signed as a JSON-encoded string: an object is refused with the
// field named and the fix shown, and any other field of the wrong type is
// named too; nothing reaches the service.
func TestCommandFieldTypeErrorsNameTheField(t *testing.T) {
	for body, want := range map[string]string{
		`{"operation":"conversation.get","room":"~abc","data":{"schema":1}}`: `The field data must be a JSON-encoded string, not a JSON object: send "data":"{\"schema\":1,...}"`,
		`{"operation":"messages.list","limit":"5"}`:                          "The field limit must be a JSON number, not a JSON string.",
		`{"operation":"conversation.open","members":"abc"}`:                  "The field members must be a JSON array, not a JSON string.",
	} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), http.MethodPost, "/v1/command", body, "application/json")
		var answer struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		if w.Code != http.StatusBadRequest || len(f.commands) != 0 || !strings.HasPrefix(answer.Error.Message, want) {
			t.Fatalf("%s: status=%d body=%s", body, w.Code, w.Body.String())
		}
	}
}

// The text lines a wire without JSON prints: an encrypted-only conversation
// says where to read it, a room policy says what it applied, and a handle
// claim says it applied.
func TestTextSaysWhatWasApplied(t *testing.T) {
	var b strings.Builder
	WriteText(&b, board.Result{Data: map[string]any{"conversations": []board.Conversation{{Room: "~x", Kind: "group", State: "open", WriteVia: []string{"encrypted"}}}}})
	WriteText(&b, board.Result{Data: map[string]any{"room": "~x", "policy": board.RoomPolicy{Write: "members", Reply: "members", MaxMessages: 50, WriteVia: []string{"encrypted"}}}})
	WriteText(&b, board.Result{Receipt: &board.Receipt{ID: "r", HandleApplied: "neat-agent"}})
	for _, want := range []string{
		"conversation ~x kind=group state=open sealed=false my_state= members=0 unread=0 write_via=encrypted (posts and reads only over encrypted channels (HTTPS and MCP); or use a sealed conversation)\n",
		"policy ~x write=members reply=members closed=false closes_at=0 max_messages=50 write_via=encrypted (posts arrive only via encrypted channels (HTTPS and MCP))\n",
		"handle applied: neat-agent\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}

func TestConversationCommandsRecognized(t *testing.T) {
	for _, op := range []string{"thread.get", "room.pages"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), http.MethodPost, "/v1/command", `{"operation":"`+op+`"}`, "application/json")
		if w.Code != http.StatusOK || len(f.commands) != 1 || f.commands[0].Operation != op {
			t.Fatalf("%s: status=%d commands=%+v", op, w.Code, f.commands)
		}
	}
}
