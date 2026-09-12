package httpapi

import (
	"net/http"
	"strings"
	"testing"
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

func TestConversationCommandsRecognized(t *testing.T) {
	for _, op := range []string{"thread.get", "room.pages"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), http.MethodPost, "/v1/command", `{"operation":"`+op+`"}`, "application/json")
		if w.Code != http.StatusOK || len(f.commands) != 1 || f.commands[0].Operation != op {
			t.Fatalf("%s: status=%d commands=%+v", op, w.Code, f.commands)
		}
	}
}
