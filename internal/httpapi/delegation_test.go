package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestDelegationTransportAndStrictContext(t *testing.T) {
	context := `{"schema":1,"grant_id":"` + strings.Repeat("a", 64) + `","generation":"` + strings.Repeat("b", 32) + `"}`
	for _, op := range []string{"delegation.create", "delegation.revoke", "delegation.get", "delegations.list"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.com/v1/command", `{"operation":"`+op+`"}`, "application/json")
		if w.Code != 200 || len(f.commands) != 1 || f.commands[0].Operation != op {
			t.Fatalf("missing %s adapter: %d", op, w.Code)
		}
	}
	for _, raw := range []string{
		`{"operation":"post","delegation":null}`,
		`{"operation":"post","delegation":{}}`,
		`{"operation":"post","delegation":[]}`,
		`{"operation":"post","delegation":` + strings.Replace(context, `"schema":1`, `"schema":true`, 1) + `}`,
		`{"operation":"post","delegation":` + strings.Replace(context, `"schema":1`, `"schema":1,"schema":1`, 1) + `}`,
		`{"operation":"post","delegation":` + strings.Replace(context, `"schema":1`, `"Schema":1`, 1) + `}`,
		`{"operation":"post","delegation":` + strings.Replace(context, `"schema":1`, `"schema":1,"extra":1`, 1) + `}`,
	} {
		for _, transport := range []string{"json", "c64"} {
			f := &fakeService{}
			method, path, body := "POST", "https://swarmmemo.com/v1/command", raw
			if transport == "c64" {
				method, path, body = "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString([]byte(raw)), ""
			}
			w := makeRequest(New(f, nil, Config{}), method, path, body, "application/json")
			if w.Code != 400 || len(f.commands) != 0 {
				t.Fatalf("accepted malformed %s context: %d %s", transport, w.Code, w.Body.String())
			}
		}
	}
	command := `{"operation":"post","room":"lab","text":"hello","visibility":"public","public_key":"key","signature":"sig","delegation":` + context + `}`
	for _, transport := range []string{"json", "c64", "query"} {
		f := &fakeService{}
		method, path, body := "POST", "https://swarmmemo.com/v1/command", command
		if transport == "c64" {
			method, path, body = "GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString([]byte(command)), ""
		}
		if transport == "query" {
			method, path, body = "GET", "https://swarmmemo.com/w/lab/main?text=hello&visibility=public&public_key=key&signature=sig&delegation="+url.QueryEscape(context), ""
		}
		w := makeRequest(New(f, nil, Config{}), method, path, body, "application/json")
		if w.Code != 200 || len(f.commands) != 1 {
			t.Fatalf("context transport %s: %d %s", transport, w.Code, w.Body.String())
		}
		encoded, _ := json.Marshal(f.commands[0].Delegation)
		if string(encoded) != context {
			t.Fatal("context changed in transport")
		}
	}
	f := &fakeService{}
	w := makeRequest(New(f, nil, Config{}), "POST", "http://swarmmemo.com/v1/command", command, "application/json")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "https_required") {
		t.Fatal("delegated authenticated post accepted without TLS")
	}
	for _, path := range []string{"/api/delegation/", "/api/delegation/a/b", "/api/delegation/a?target=b", "/api/messages?delegation=null", "/api/messages?delegation=%7B%7D"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "GET", path, "", "")
		if w.Code != 400 || len(f.commands) != 0 {
			t.Fatalf("ambiguous delegation path %s: %d", path, w.Code)
		}
	}
}

func TestDelegationPublicProofAdapter(t *testing.T) {
	f := &fakeService{}
	id := strings.Repeat("a", 64)
	w := makeRequest(New(f, nil, Config{}), "GET", "/api/delegation/"+id, "", "")
	if w.Code != 200 || len(f.commands) != 1 || f.commands[0].Operation != "delegation.get" || f.commands[0].Target != id {
		t.Fatal("missing public proof adapter")
	}
	if f.commands[0].PublicKey != "" || f.commands[0].Delegation != nil {
		t.Fatal("public proof read inherited authority")
	}
}

func TestDelegationCommandNeverDiscardsQueryAuthority(t *testing.T) {
	for _, query := range []string{"?", "?delegation=null", "?delegation=%7B%7D", "?public_key=key&signature=sig", "?format=json", "?text=other", "?bad=%FF"} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.com/v1/command"+query, `{"operation":"post","room":"lobby","text":"local fixture"}`, "application/json")
		if w.Code != 400 || len(f.commands) != 0 || !strings.Contains(w.Body.String(), "ambiguous_command") {
			t.Fatalf("query intent silently discarded: %q status=%d commands=%d", query, w.Code, len(f.commands))
		}
	}
}
