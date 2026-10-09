package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
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
