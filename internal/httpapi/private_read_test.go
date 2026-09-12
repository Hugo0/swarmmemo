package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func privateWire(op string) string {
	return `{"operation":"` + op + `","room":"private-lab","public_key":"fixture","signature":"fixture","timestamp":123,"nonce":"fixture","private_read":{"schema":1,"grant_id":"` + strings.Repeat("a", 64) + `","generation":"` + strings.Repeat("b", 32) + `"}}`
}

func TestPrivateReadRawFields(t *testing.T) {
	base := privateWire("room.get")
	for _, field := range []string{`"amount":0`, `"text":""`, `"members":[]`, `"request_id":""`, `"proof":""`, `"page":""`, `"limit":0`, `"cursor":""`, `"message_id":""`, `"target":null`} {
		t.Run(field, func(t *testing.T) {
			f := &fakeService{}
			body := strings.TrimSuffix(base, "}") + "," + field + "}"
			w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.test/v1/command", body, "application/json")
			if w.Code != 400 || len(f.commands) != 0 || !strings.Contains(w.Body.String(), "invalid_private_read_data") {
				t.Fatalf("unexpected response/calls: %d %s %d", w.Code, w.Body.String(), len(f.commands))
			}
		})
	}
	for _, body := range []string{
		`{"operation":"room.get","private_read":null}`,
		`{"operation":"room.get","private_read":{}}`,
		strings.Replace(base, `"schema":1`, `"schema":1.0`, 1),
		strings.Replace(base, `"schema":1`, `"Schema":1`, 1),
		strings.Replace(base, `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(base, `"schema":1`, `"schema":1,"secret-marker":1`, 1),
		strings.TrimSuffix(base, "}") + `,"delegation":null}`,
		strings.TrimSuffix(base, "}") + `,"secret-marker":0}`,
		strings.Replace(base, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
	} {
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.test/v1/command", body, "application/json")
		if w.Code != 400 || len(f.commands) != 0 || strings.Contains(w.Body.String(), "secret-marker") || !strings.Contains(w.Body.String(), "invalid_private_read_context") {
			t.Fatalf("context not closed: %d %s calls=%d", w.Code, w.Body.String(), len(f.commands))
		}
	}
}

func TestPrivateReadControlsAndExplicitBounds(t *testing.T) {
	for _, op := range []string{"private_read.create", "private_read.revoke", "private_read.get", "private_read.list", "messages.list"} {
		body := `{"operation":"` + op + `","room":"private-lab"}`
		if op == "messages.list" {
			body = privateWire(op)
		}
		for _, field := range []string{`"amount":0`, `"text":null`, `"proof":null`} {
			f := &fakeService{}
			w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.test/v1/command", strings.TrimSuffix(body, "}")+","+field+"}", "application/json")
			if w.Code != 400 || len(f.commands) != 0 {
				t.Fatalf("accepted forbidden %s on %s: %s", field, op, w.Body.String())
			}
		}
		if op == "private_read.create" || op == "private_read.list" || op == "messages.list" {
			field, code := `"limit":0`, "invalid_limit"
			if op == "private_read.create" {
				field, code = `"ttl":0`, "invalid_ttl"
			}
			w := makeRequest(New(&fakeService{}, nil, Config{}), "POST", "https://swarmmemo.test/v1/command", strings.TrimSuffix(body, "}")+","+field+"}", "application/json")
			if w.Code != 400 || !strings.Contains(w.Body.String(), code) {
				t.Fatalf("explicit bound: %d %s", w.Code, w.Body.String())
			}
		}
	}
}

func TestPrivateReadOnlyExactJSONPost(t *testing.T) {
	for _, body := range []string{privateWire("room.get"), `{"operation":"private_read.list","room":"private-lab"}`} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(body))
		for _, tc := range []struct{ method, path, body, ct string }{
			{"GET", "https://swarmmemo.test/c64/" + encoded, "", ""},
			{"POST", "https://swarmmemo.test/c64/" + encoded, "", "application/json"},
			{"POST", "https://swarmmemo.test/w/private-lab/main", body, "application/json"},
			{"POST", "https://swarmmemo.test/v1/command?", body, "application/json"},
			{"POST", "https://swarmmemo.test/v1/command?format=json", body, "application/json"},
			{"POST", "https://swarmmemo.test/%761/command", body, "application/json"},
			{"GET", "https://swarmmemo.test/v1/command", body, "application/json"},
			{"POST", "https://swarmmemo.test/v1/command", body, "text/plain"},
			{"POST", "https://swarmmemo.test/v1/command", body, ""},
			{"POST", "http://swarmmemo.test/v1/command", body, "application/json"},
		} {
			f := &fakeService{}
			w := makeRequest(New(f, nil, Config{}), tc.method, tc.path, tc.body, tc.ct)
			if w.Code < 400 || len(f.commands) != 0 {
				t.Fatalf("transport reached core: %s %s %d calls=%d", tc.method, tc.path, w.Code, len(f.commands))
			}
		}
		f := &fakeService{}
		w := makeRequest(New(f, nil, Config{}), "POST", "https://swarmmemo.test/v1/command", body, "application/json; charset=utf-8")
		if w.Code != 200 || len(f.commands) != 1 {
			t.Fatalf("valid syntax did not reach core: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestPrivateReadCapabilities(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	cap := s.capabilities()
	encoded, err := json.Marshal(cap["private_read_grants"])
	if err != nil || !strings.Contains(string(encoded), `"canonical_version":3`) || !strings.Contains(string(encoded), `"private_mcp":false`) {
		t.Fatalf("missing descriptor: %s %v", encoded, err)
	}
	fields := cap["command_fields"].([]string)
	versions := cap["canonical_versions"].([]int)
	if fields[len(fields)-1] != "private_read" || len(versions) != 3 || versions[2] != 3 {
		t.Fatal("private domain missing from discovery")
	}
}
