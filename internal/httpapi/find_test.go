package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Find at parity (C188): GET /api/find, the MCP tool find and the /find
// page make one read and agree on its answer; the query names map onto
// its data; /capabilities, /llms.txt and openapi.json say how to use it.
func TestFindSurfaces(t *testing.T) {
	s := reviewServer(t, true)
	key := ed25519.NewKeyFromSeed(append(make([]byte, 31), 211))
	for i, c := range []board.Command{{Operation: "agent.register", Handle: "lisbonweather"},
		{Operation: "agent.profile.publish", Data: `{"schema":1,"description":"Weather for Lisbon, as a forecast.","capabilities":["forecast"],"availability":"available"}`}} {
		if w := topupRequest(s, signService(key, c), ""); w.Code != 200 {
			t.Fatalf("seed %d: %d %s", i, w.Code, w.Body)
		}
	}

	code, body := reviewGet(t, s, "/api/find?q="+url.QueryEscape("lisbon forecast agents")+"&max_price=any&explain=true")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	data := body["data"].(map[string]any)
	results := data["results"].([]any)
	first := results[0].(map[string]any)
	if first["url"] != "/@lisbonweather" || first["type"] != "agent" || first["why"] == "" || first["explain"] == nil {
		t.Fatalf("first %v", first)
	}
	filters := data["filters"].(map[string]any)
	if len(filters["type"].([]any)) != 1 || filters["max_price"] != "" || data["parsed_by"] != "rules" {
		t.Fatalf("filters %v %v", filters, data["parsed_by"])
	}
	if code, body = reviewGet(t, s, "/api/find?type=people"); code != 400 || body["error"].(map[string]any)["code"] != "invalid_find" {
		t.Fatalf("refusal %d %v", code, body)
	}
	if code, _ = reviewGet(t, s, "/api/find?type=agent&type=mcp"); code != 400 {
		t.Fatal("a name given twice is refused")
	}

	// MCP: the same first result.
	server := httptest.NewServer(s)
	defer server.Close()
	out := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find","arguments":{"query":"lisbon forecast agents"}}}`)
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"/@lisbonweather"`) || strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("mcp find %s", raw)
	}

	// The page: the same result, its filter chip, and Find current in the header.
	r := httptest.NewRequest("GET", "https://swarmmemo.com/find?q="+url.QueryEscape("lisbon forecast agents"), nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	page := w.Body.String()
	if w.Code != 200 || !strings.Contains(page, `href="/@lisbonweather"`) || !strings.Contains(page, `<a href="/find" aria-current="page">Find</a>`) || !strings.Contains(page, `class="find-chip is-current"`) {
		t.Fatalf("page %d", w.Code)
	}

	// Discovery.
	_, caps := reviewGet(t, s, capabilitiesFullPath)
	if f, _ := caps["find"].(map[string]any); f["http"] != "GET /api/find?q=TEXT" || f["mcp"] != "find" {
		t.Fatalf("capabilities find %v", caps["find"])
	}
	r = httptest.NewRequest("GET", "https://swarmmemo.com/llms-full.txt", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "/api/find?q=weather+under+a+cent+returns+JSON") {
		t.Fatal("llms-full.txt names /api/find with a verbatim example")
	}
	r = httptest.NewRequest("GET", "https://swarmmemo.com/openapi.json", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"/api/find"`) {
		t.Fatal("openapi.json documents /api/find")
	}
}

// The door filter at parity (C228): /api/find?door=agent, the MCP find
// tool's door and the /find page keep the same results (the agent that
// declares signup-key, not the one that declares nothing); the page shows
// the "Agent can sign up" chip and each result's door badge with its
// source; openapi.json, /capabilities and /llms.txt name it.
func TestFindDoorSurfaces(t *testing.T) {
	s := reviewServer(t, true)
	for i, c := range []struct {
		seed byte
		cmds []board.Command
	}{
		{212, []board.Command{{Operation: "agent.register", Handle: "doorkeybot"}, {Operation: "agent.profile.publish", Data: `{"schema":1,"description":"Lighthouse keeper, registers with a key.","capabilities":["lighthouse","signup-key"],"availability":"available"}`}}},
		{213, []board.Command{{Operation: "agent.register", Handle: "doorplainbot"}, {Operation: "agent.profile.publish", Data: `{"schema":1,"description":"Lighthouse keeper.","capabilities":["lighthouse"],"availability":"available"}`}}},
	} {
		key := ed25519.NewKeyFromSeed(append(make([]byte, 31), c.seed))
		for _, cmd := range c.cmds {
			if w := topupRequest(s, signService(key, cmd), ""); w.Code != 200 {
				t.Fatalf("seed %d: %d %s", i, w.Code, w.Body)
			}
		}
	}
	urls := func(results []any) []string {
		var out []string
		for _, r := range results {
			out = append(out, r.(map[string]any)["url"].(string))
		}
		return out
	}
	code, body := reviewGet(t, s, "/api/find?q=lighthouse&door=agent")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	data := body["data"].(map[string]any)
	api := urls(data["results"].([]any))
	if len(api) != 1 || api[0] != "/@doorkeybot" || data["filters"].(map[string]any)["door"] != "agent" {
		t.Fatalf("api door=agent: %v %v", api, data["filters"])
	}
	door := data["results"].([]any)[0].(map[string]any)["door"].(map[string]any)
	if door["value"] != "key" || door["by"] != "self" || door["source"] == "" {
		t.Fatalf("api door %v", door)
	}
	if _, body = reviewGet(t, s, "/api/find?q=lighthouse"); len(body["data"].(map[string]any)["results"].([]any)) != 2 {
		t.Fatal("without the filter both agents match")
	}
	if code, _ = reviewGet(t, s, "/api/find?q=lighthouse&door=maybe"); code != 400 {
		t.Fatal("a door outside the list is refused")
	}

	server := httptest.NewServer(s)
	defer server.Close()
	out := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find","arguments":{"query":"lighthouse","door":"agent"}}}`)
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"/@doorkeybot"`) || strings.Contains(string(raw), `"/@doorplainbot"`) || strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("mcp find door %s", raw)
	}

	r := httptest.NewRequest("GET", "https://swarmmemo.com/find?q=lighthouse&door=agent", nil)
	r.Header.Set("Accept", "text/html")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	page := w.Body.String()
	if w.Code != 200 || !strings.Contains(page, `href="/@doorkeybot"`) || strings.Contains(page, `href="/@doorplainbot"`) ||
		!strings.Contains(page, `Door: key · self-reported`) || !strings.Contains(page, `find-door-chip is-current`) || !strings.Contains(page, `<option value="agent" selected>`) ||
		!strings.Contains(page, `sign-up door`) {
		t.Fatalf("page %d %s", w.Code, page)
	}

	_, caps := reviewGet(t, s, capabilitiesFullPath)
	if f, _ := caps["find"].(map[string]any); f["doors"] == nil || f["params"].(map[string]any)["door"] == nil {
		t.Fatalf("capabilities find %v", caps["find"])
	}
	r = httptest.NewRequest("GET", "https://swarmmemo.com/llms-full.txt", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "/api/find?q=sign+up+without+a+human: door=agent") {
		t.Fatal("llms-full.txt names the door filter")
	}
	r = httptest.NewRequest("GET", "https://swarmmemo.com/openapi.json", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"name":"door"`) {
		t.Fatal("openapi.json documents door")
	}
}

// SwarmMemo tools' doors follow the catalogue's Anonymous flag (C236): the
// notary takes a call without a key (open), memory.put needs one (key);
// door=agent keeps both.
func TestFindToolDoors(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	toolDoors := func(q string) map[string]string {
		_, body := reviewGet(t, s, "/api/find?"+q)
		out := map[string]string{}
		for _, r := range dig(body, "data", "results").([]any) {
			m := r.(map[string]any)
			if d, ok := m["door"].(map[string]any); ok {
				out[m["id"].(string)] = d["value"].(string) + " " + d["by"].(string)
			}
		}
		return out
	}
	for _, q := range []string{"q=notary+stamp", "q=notary+stamp&door=agent"} {
		if got := toolDoors(q)["swarmmemo:notary.stamp"]; got != "open measured" {
			t.Fatalf("%s: notary.stamp door %q", q, got)
		}
	}
	for _, q := range []string{"q=memory+put", "q=memory+put&door=agent"} {
		if got := toolDoors(q)["swarmmemo:memory.put"]; got != "key measured" {
			t.Fatalf("%s: memory.put door %q", q, got)
		}
	}
}
