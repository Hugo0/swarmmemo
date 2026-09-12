package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

var workOperations = []string{"work.create", "work.claim", "work.renew", "work.submit", "work.accept", "work.reject", "work.cancel", "work.get", "works.list", "work.history"}

func TestWorkReadAdapters(t *testing.T) {
	for _, tc := range []struct{ path, op, id string }{
		{"/api/works?room=lab&kind=open&query=research&cursor=resume&limit=2", "works.list", ""},
		{"/api/work/memo", "work.get", "memo"},
		{"/api/work/memo/history?cursor=resume&limit=2", "work.history", "memo"},
	} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", tc.path, "", "")
		if w.Code != 200 || len(f.commands) != 1 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		c := f.commands[0]
		if c.Operation != tc.op || c.MessageID != tc.id || c.PublicKey != "" {
			t.Fatalf("unexpected command %+v", c)
		}
		if tc.op != "work.get" && (c.Cursor != "resume" || c.Limit != 2) {
			t.Fatal("lost work pagination")
		}
		if tc.op == "works.list" && (c.Room != "lab" || c.Kind != "open" || c.Query != "research") {
			t.Fatal("lost work scope")
		}
	}
	for _, path := range []string{"/api/work/", "/api/work/a/", "/api/work/a/claim", "/api/work/a/history/b", "/api/work/a?message_id=b", "/api/work/a/history?message_id=b", "/api/works?kind=open&kind=claimed"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", path, "", "")
		if w.Code != 400 || len(f.commands) != 0 {
			t.Fatalf("ambiguous %s: %d commands=%+v", path, w.Code, f.commands)
		}
	}
	for _, method := range []string{"POST", "PUT", "MKCOL", "DELETE"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), method, "/api/work/a", "", "")
		if w.Code != 405 || len(f.commands) != 0 {
			t.Fatalf("method %s: %d commands=%+v", method, w.Code, f.commands)
		}
	}
}

func TestWorkCommandTransports(t *testing.T) {
	for _, op := range workOperations {
		for _, transport := range []string{"json", "c64"} {
			f := &fakeService{}
			body := `{"operation":"` + op + `","message_id":"memo","data":"{\"schema\":1,\"generation\":\"` + strings.Repeat("a", 32) + `\"}","public_key":"client-key","signature":"client-signature","request_id":"stable"}`
			method, path, contentType := "POST", "https://swarmmemo.com/v1/command", "application/json"
			if transport == "c64" {
				method, path, body, contentType = "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString([]byte(body)), "", ""
			}
			w := makeRequest(New(f, nil, Config{}), method, path, body, contentType)
			if w.Code != 200 || len(f.commands) != 1 || f.commands[0].Operation != op || f.commands[0].Signature != "client-signature" || f.commands[0].RequestID != "stable" {
				t.Fatalf("%s %s: %d %s commands=%+v", op, transport, w.Code, w.Body.String(), f.commands)
			}
		}
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "POST", "http://swarmmemo.com/v1/command", `{"operation":"`+op+`","public_key":"key"}`, "application/json")
		if w.Code != 400 || len(f.commands) != 0 || !strings.Contains(w.Body.String(), "https_required") {
			t.Fatalf("insecure signed %s: %d %+v", op, w.Code, f.commands)
		}
	}
}

func TestWorkDiscoveryAndMCP(t *testing.T) {
	f := &fakeService{}
	s := New(f, nil, Config{})
	for _, path := range []string{"/capabilities", "/protocol.md"} {
		w := makeRequest(s, "GET", path, "", "")
		for _, op := range workOperations {
			if !strings.Contains(w.Body.String(), op) {
				t.Fatalf("%s omits %s", path, op)
			}
		}
	}
	var spec struct{ Paths map[string]any }
	w := makeRequest(s, "GET", "/openapi.json", "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/works", "/api/work/{message_id}", "/api/work/{message_id}/history"} {
		if spec.Paths[path] == nil {
			t.Fatalf("OpenAPI missing %s", path)
		}
	}
	for _, tc := range []struct{ name, args, op string }{
		{"find_work", `{"room":"lab","kind":"open","query":"research","limit":2}`, "works.list"},
		{"read_work", `{"message_id":"memo"}`, "work.get"},
		{"read_work_history", `{"message_id":"memo","cursor":"resume","limit":2}`, "work.history"},
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
