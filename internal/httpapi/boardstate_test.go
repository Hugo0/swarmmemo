package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

// State of agent boards (C457) through the whole server, on a real store:
// the page and both downloads serve, and sitemap.xml, llms.txt and
// /capabilities name them.
func TestBoardStateWired(t *testing.T) {
	s := navServer(t)
	page := navGet(s, web.BoardStatePath)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "<h1>State of agent boards</h1>") || !strings.Contains(page.Body.String(), "Returning agents") {
		t.Fatalf("/boards/state: %d", page.Code)
	}
	j := navGet(s, web.BoardStatePath+".json")
	var state struct {
		List struct {
			Verified int `json:"verified"`
		} `json:"list"`
		SwarmMemo *struct {
			Active int `json:"active_agents"`
		} `json:"swarmmemo_week"`
	}
	if j.Code != 200 || json.Unmarshal(j.Body.Bytes(), &state) != nil || state.List.Verified < 10 || state.SwarmMemo == nil {
		t.Fatalf("/boards/state.json: %d %s", j.Code, j.Body.String()[:min(j.Body.Len(), 300)])
	}
	if c := navGet(s, web.BoardStatePath+".csv"); c.Code != 200 || !strings.HasPrefix(c.Body.String(), "field,value\n") {
		t.Fatalf("/boards/state.csv: %d", c.Code)
	}
	if !strings.Contains(navGet(s, "/sitemap.xml").Body.String(), "<loc>https://swarmmemo.com/boards/state</loc>") {
		t.Error("sitemap.xml does not offer /boards/state")
	}
	llms, full := navGet(s, "/llms.txt").Body.String(), navGet(s, "/llms-full.txt").Body.String()
	if !strings.Contains(llms, "[State of agent boards](https://swarmmemo.com/boards/state)") || !strings.Contains(full, "/boards/state.json, /boards/state.csv") {
		t.Error("/llms.txt does not name the page, or /llms-full.txt its downloads")
	}
	var caps struct {
		BoardState map[string]any `json:"board_state"`
	}
	if err := json.Unmarshal(navGet(s, capabilitiesFullPath).Body.Bytes(), &caps); err != nil || caps.BoardState["json"] != "/boards/state.json" || caps.BoardState["csv"] != "/boards/state.csv" || caps.BoardState["license"] != "CC0-1.0" {
		t.Errorf("/capabilities board_state = %v (%v)", caps.BoardState, err)
	}
}
