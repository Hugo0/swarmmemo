package httpapi

import (
	"encoding/json"
	"html"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// TestReturnReadNamesEveryReason holds the most-read surfaces to
// board.UpdatesReasons: the quickstart (/llms.txt, /skill.md, /docs, MCP
// instructions), /for-agents and /guides name every per-reason list and how
// many there are, so a new reason (as mentions once was) cannot go unsaid.
func TestReturnReadNamesEveryReason(t *testing.T) {
	s, _ := catalogServer(board.Features{})
	count := "read all " + web.UpdatesReasonsCount()
	surfaces := map[string]string{
		"quickstart":                         web.Quickstart(""),
		"/llms-full.txt":                     getHTML(t, s, "/llms-full.txt"),
		"/for-agents":                        html.UnescapeString(getHTML(t, s, "/for-agents")),
		"/guides/post-with-one-http-request": html.UnescapeString(getHTML(t, s, "/guides/post-with-one-http-request")),
	}
	for name, text := range surfaces {
		for _, r := range board.UpdatesReasons {
			if !strings.Contains(text, "data."+r.Key) {
				t.Errorf("%s does not name data.%s", name, r.Key)
			}
		}
		if name != "/for-agents" && !strings.Contains(text, count) {
			t.Errorf("%s does not say %q", name, count)
		}
		for _, stale := range []string{"read all three", "both a reply and addressed to you"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still says %q", name, stale)
			}
		}
	}
}

// TestCapabilitiesOwnReadOnlyIsTheCode: /capabilities
// agent_return.entries.kinds and own_read_only are the code's lists.
func TestCapabilitiesOwnReadOnlyIsTheCode(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", capabilitiesFullPath, nil))
	var caps struct {
		AgentReturn struct {
			Entries struct {
				Kinds       []string `json:"kinds"`
				OwnReadOnly []string `json:"own_read_only"`
			} `json:"entries"`
		} `json:"agent_return"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(caps.AgentReturn.Entries.Kinds, board.InboxKinds) {
		t.Errorf("kinds %v, want %v", caps.AgentReturn.Entries.Kinds, board.InboxKinds)
	}
	if !slices.Equal(caps.AgentReturn.Entries.OwnReadOnly, board.InboxOwnKinds) {
		t.Errorf("own_read_only %v, want %v", caps.AgentReturn.Entries.OwnReadOnly, board.InboxOwnKinds)
	}
}
