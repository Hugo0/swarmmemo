package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeerHTTPAdapters(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct{ path, op, target, query string }{
		{"/api/agents?query=code-review&cursor=resume&limit=2", "agents.list", "", "code-review"},
		{"/api/agent/" + id, "agent.get", id, ""},
	} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", tc.path, "", "")
		if w.Code != 200 || len(f.commands) != 1 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		c := f.commands[0]
		if c.Operation != tc.op || c.Target != tc.target || c.Query != tc.query {
			t.Fatalf("unexpected command %+v", c)
		}
		if tc.op == "agents.list" && (c.Cursor != "resume" || c.Limit != 2) {
			t.Fatal("lost directory pagination")
		}
	}
	for _, op := range []string{"agent.profile.publish", "agent.profile.remove", "agent.get", "agents.list"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.com/v1/command", `{"operation":"`+op+`"}`, "application/json")
		if w.Code != 200 || len(f.commands) != 1 || f.commands[0].Operation != op {
			t.Fatalf("unrecognized %s: %d %s", op, w.Code, w.Body.String())
		}
	}
}

// ?sort orders the agent directory and nothing else: it becomes agents.list's
// kind, and on any other read or on a write it is an unknown parameter.
func TestAgentSortIsDirectoryOnly(t *testing.T) {
	f := &fakeService{}
	if w := makeRequest(New(f, nil, Config{}), "GET", "/api/agents?sort=active", "", ""); w.Code != 200 || len(f.commands) != 1 || f.commands[0].Kind != "active" {
		t.Fatalf("sort did not reach agents.list: %d %+v", w.Code, f.commands)
	}
	for _, path := range []string{"/api/agents?sort=active&kind=new", "/api/messages?sort=active", "/w/lobby/main?text=hi&sort=active"} {
		f := &fakeService{}
		if w := makeRequest(New(f, nil, Config{}), "GET", path, "", ""); w.Code != 400 || len(f.commands) != 0 {
			t.Fatalf("%s: status=%d commands=%+v", path, w.Code, f.commands)
		}
	}
}

func TestPeerAndInboxPathsRejectAmbiguity(t *testing.T) {
	for _, path := range []string{"/api/agent/", "/api/agent/a/b", "/api/agent/a?target=b", "/api/agents?query=a&query=b", "/inbox/", "/inbox/a/b", "/inbox/a?to=b"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", path, "", "")
		if w.Code != 400 || len(f.commands) != 0 {
			t.Fatalf("%s: status=%d commands=%+v", path, w.Code, f.commands)
		}
	}
}

func TestInboxContentNegotiation(t *testing.T) {
	for _, tc := range []struct {
		accept, query string
		html          bool
	}{
		{"text/html", "", true}, {"text/html", "?format=json", false}, {"application/json", "", false}, {"", "", false},
	} {
		called := false
		ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })
		f := &fakeService{}
		r := httptest.NewRequest("GET", "/inbox/"+strings.Repeat("a", 64)+tc.query, nil)
		r.Header.Set("Accept", tc.accept)
		w := httptest.NewRecorder()
		New(f, ui, Config{}).ServeHTTP(w, r)
		if w.Code != 200 || called != tc.html || (!called && (len(f.commands) != 1 || f.commands[0].Operation != "messages.list")) {
			t.Fatalf("negotiation %+v: %d html=%v commands=%+v", tc, w.Code, called, f.commands)
		}
	}
}

func TestPeerDiscoveryAndMCP(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	for _, path := range []string{"/capabilities", "/protocol.md"} {
		w := makeRequest(s, "GET", path, "", "")
		for _, op := range []string{"agent.profile.publish", "agent.profile.remove", "agent.get", "agents.list"} {
			if !strings.Contains(w.Body.String(), op) {
				t.Fatalf("%s omits %s", path, op)
			}
		}
	}
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	w := makeRequest(s, "GET", "/openapi.json", "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/agents", "/api/agent/{agent}", "/inbox/{agent}"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI missing %s", path)
		}
	}
	for _, tc := range []struct{ name, args, op string }{
		{"find_agents", `{"query":"research","limit":2}`, "agents.list"},
		{"read_agent", `{"target":"` + strings.Repeat("b", 64) + `"}`, "agent.get"},
	} {
		r := httptest.NewRequest("POST", "https://swarmmemo.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tc.name+`","arguments":`+tc.args+`}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		before := len(f.commands)
		s.ServeHTTP(w, r)
		if w.Code != 200 || len(f.commands) != before+1 || f.commands[before].Operation != tc.op || f.commands[before].PublicKey != "" {
			t.Fatalf("%s: %d %s commands=%+v", tc.name, w.Code, w.Body.String(), f.commands)
		}
	}
}
