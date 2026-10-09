package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// C61 step 2: /capabilities says whether updates.get lists data.entries,
// and /llms.txt's inbox line says so only where it does, at no more bytes.
func TestInboxEntriesSurfaces(t *testing.T) {
	var sizes []int
	for _, mode := range []board.InboxMode{board.InboxShadow, board.InboxRead} {
		s, _ := catalogServer(board.Features{InboxEntries: mode})
		var caps struct {
			AgentReturn struct {
				Entries struct {
					Enabled bool     `json:"enabled"`
					Kinds   []string `json:"kinds"`
				} `json:"entries"`
			} `json:"agent_return"`
		}
		if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps); err != nil {
			t.Fatal(err)
		}
		if caps.AgentReturn.Entries.Enabled != (mode == board.InboxRead) || len(caps.AgentReturn.Entries.Kinds) != len(board.InboxKinds) {
			t.Fatalf("%v: agent_return.entries %+v", mode, caps.AgentReturn.Entries)
		}
		llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
		if strings.Contains(llms, "data.entries") != (mode == board.InboxRead) || !strings.Contains(llms, "updates.get, signed for yourself") {
			t.Fatalf("%v: /llms.txt inbox line", mode)
		}
		sizes = append(sizes, len(llms))
	}
	// The /api/updates line gave up 87 bytes (its conversations sentence) for it.
	if sizes[1]-sizes[0] > 60 {
		t.Fatalf("/llms.txt grows %d bytes with entries", sizes[1]-sizes[0])
	}
}

// C61 step 3 (C71): /capabilities inbox, the /llms.txt line and the MCP tool
// dispose_updates exist only under INBOX_ENTRIES=read; shadow lists none.
func TestInboxDispositionSurfaces(t *testing.T) {
	for _, mode := range []board.InboxMode{board.InboxShadow, board.InboxRead} {
		s, _ := catalogServer(board.Features{InboxEntries: mode})
		var caps struct {
			Inbox struct {
				Enabled bool `json:"enabled"`
				Dispose struct {
					Operation string   `json:"operation"`
					States    []string `json:"states"`
				} `json:"dispose"`
				Private bool `json:"dispositions_private"`
			} `json:"inbox"`
		}
		if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps); err != nil {
			t.Fatal(err)
		}
		if caps.Inbox.Enabled != (mode == board.InboxRead) || caps.Inbox.Dispose.Operation != "updates.dispose" || len(caps.Inbox.Dispose.States) != 5 || !caps.Inbox.Private {
			t.Fatalf("%v: inbox %+v", mode, caps.Inbox)
		}
		llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String()
		if strings.Contains(llms, "updates.dispose") != (mode == board.InboxRead) {
			t.Fatalf("%v: /llms.txt dispose line", mode)
		}
	}

	_, shadow := hostedServerWith(t, board.Features{InboxEntries: board.InboxShadow})
	if _, ok := listTools(t, shadow, "/mcp")["dispose_updates"]; ok {
		t.Fatal("shadow lists dispose_updates")
	}
	_, s := hostedServerWith(t, board.Features{InboxEntries: board.InboxRead})
	for _, path := range []string{"/mcp", web.AssistantMCPPath} {
		if _, ok := listTools(t, s, path)["dispose_updates"]; !ok {
			t.Fatalf("%s lacks dispose_updates", path)
		}
	}
	// A hosted identity with a message addressed to it: it waits, then it
	// is marked answered elsewhere and stops waiting.
	me := newIdentity(t, s, "")
	token, agent := me["token"].(string), me["agent"].(string)
	posted := mustTool(t, s, "/mcp", "", "post_message", map[string]any{"text": "a question for you", "to": agent})
	asked := dig(posted, "receipt", "id").(string)
	waiting := func() float64 {
		read := mustTool(t, s, "/mcp/t/"+token, "", "read_updates", map[string]any{})
		n, _ := dig(read, "data", "waiting").(float64)
		return n
	}
	if n := waiting(); n != 1 {
		t.Fatalf("waiting before: %v", n)
	}
	done := mustTool(t, s, "/mcp/t/"+token, "", "dispose_updates", map[string]any{"ids": []string{asked}, "state": "answered_elsewhere"})
	if dig(done, "data", "waiting") != float64(0) || waiting() != 0 {
		t.Fatalf("dispose_updates: %v", done)
	}
	if _, failure := callTool(t, s, "/mcp/t/"+token, "", "dispose_updates", map[string]any{"ids": []string{asked}, "state": "finished"}); !strings.HasPrefix(failure, "400 invalid_disposition") {
		t.Fatalf("a bad state: %q", failure)
	}
	if _, failure := callTool(t, s, "/mcp/t/"+token, "", "dispose_updates", map[string]any{"ids": []string{"no-such-entry"}, "state": "closure"}); !strings.HasPrefix(failure, "404 entry_not_found") {
		t.Fatalf("an unknown id: %q", failure)
	}
}
