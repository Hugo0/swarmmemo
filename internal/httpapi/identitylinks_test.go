package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"swarmmemo/internal/board"
)

func identityLinkValidator(t *testing.T, s *Server) *jsonschema.Resolved {
	t.Helper()
	var spec struct {
		Components struct{ Schemas map[string]json.RawMessage }
	}
	if err := json.Unmarshal(makeRequest(s, "GET", "/openapi.json", "", "").Body.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(spec.Components.Schemas["IdentityLink"], &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func signedLinkCommand(key ed25519.PrivateKey, op, data string, nonce int) string {
	c := board.Command{Operation: op, Data: data, PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		Timestamp: time.Now().Unix(), Nonce: "link-" + string(rune('a'+nonce))}
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	raw, _ := json.Marshal(c)
	return string(raw)
}

func tlsCommand(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/command", strings.NewReader(body))
	r.RemoteAddr = "198.51.100.8:12345"
	r.TLS = &tls.ConnectionState{}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// The whole path an agent uses: a signed command through /v1/command, then the
// public profile read, with every returned link valid against the published
// schema. The schema itself forbids a claimed link carrying check evidence.
func TestIdentityLinksEndToEnd(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "links.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	validator := identityLinkValidator(t, s)
	seed := make([]byte, 32)
	seed[0] = 7
	ours := ed25519.NewKeyFromSeed(seed)
	seed[0] = 8
	theirs := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256(ours.Public().(ed25519.PublicKey))
	fingerprint := hex.EncodeToString(sum[:])
	theirKey := base64.RawURLEncoding.EncodeToString(theirs.Public().(ed25519.PublicKey))
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(theirs, []byte(board.LinkStatement("swarmmemo.com", fingerprint, theirKey))))

	if w := tlsCommand(s, signedLinkCommand(ours, "agent.register", "", 0)); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	for i, data := range []string{
		`{"schema":1,"kind":"domain","value":"Example.ORG"}`,
		`{"schema":1,"kind":"ed25519","value":"` + theirKey + `","proof":"` + proof + `"}`,
		`{"schema":1,"kind":"url","value":"https://example.org/agents/me"}`,
	} {
		w := tlsCommand(s, signedLinkCommand(ours, "identity.link", data, i+1))
		if w.Code != 200 || strings.Contains(w.Body.String(), "unknown_operation") {
			t.Fatalf("identity.link unreachable: %d %s", w.Code, w.Body.String())
		}
	}
	// Plain HTTP refuses identity management before it reaches the store.
	if w := makeRequest(s, "POST", "/v1/command", signedLinkCommand(ours, "identity.link", `{"schema":1,"kind":"domain","value":"example.net"}`, 9), "application/json"); w.Code != 400 || !strings.Contains(w.Body.String(), "https_required") {
		t.Fatalf("plaintext link: %d %s", w.Code, w.Body.String())
	}
	w := makeRequest(s, "GET", "/api/agent/"+fingerprint, "", "")
	var raw struct {
		Agent struct {
			DomainHandle string            `json:"domain_handle"`
			Links        []json.RawMessage `json:"links"`
		} `json:"agent"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &raw) != nil {
		t.Fatalf("agent read: %d %s", w.Code, w.Body.String())
	}
	if len(raw.Agent.Links) != 3 || raw.Agent.DomainHandle != "" {
		t.Fatalf("agent links: %s", w.Body.String())
	}
	states := map[string]string{}
	for _, item := range raw.Agent.Links {
		var link map[string]any
		if err := json.Unmarshal(item, &link); err != nil {
			t.Fatal(err)
		}
		if err := validator.Validate(link); err != nil {
			t.Fatalf("link drifted from its published schema: %v\n%s", err, item)
		}
		states[link["kind"].(string)] = link["state"].(string)
	}
	if states["domain"] != "claimed" || states["ed25519"] != "proof_attached" || states["url"] != "claimed" {
		t.Fatalf("states: %v", states)
	}
	// The published schema refuses the shapes that would let a claim pass for proof.
	for _, forged := range []map[string]any{
		{"kind": "domain", "value": "example.org", "state": "claimed", "linked_at": 1, "checked_at": 2},
		{"kind": "domain", "value": "example.org", "state": "claimed", "linked_at": 1, "method": "dns-txt"},
		{"kind": "domain", "value": "example.org", "state": "verified", "linked_at": 1},
		{"kind": "ed25519", "value": "x", "state": "proof_attached", "linked_at": 1, "method": "ed25519-signature"},
		{"kind": "domain", "value": "example.org", "state": "lapsed", "linked_at": 1, "method": "dns-txt"},
		{"kind": "domain", "value": "example.org", "state": "trusted", "linked_at": 1},
		{"kind": "email", "value": "a@example.org", "state": "claimed", "linked_at": 1},
	} {
		if validator.Validate(forged) == nil {
			t.Fatalf("schema accepts a misleading link: %v", forged)
		}
	}
	w = tlsCommand(s, signedLinkCommand(ours, "identity.unlink", `{"schema":1,"kind":"url","value":"https://example.org/agents/me"}`, 10))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unlinked":true`) {
		t.Fatalf("unlink: %d %s", w.Code, w.Body.String())
	}
}

func TestIdentityLinkDiscovery(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	var capabilities struct {
		Operations []string       `json:"operations"`
		Links      map[string]any `json:"identity_links"`
	}
	if json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &capabilities) != nil || capabilities.Links == nil {
		t.Fatal("capabilities do not describe identity links")
	}
	listed := map[string]bool{}
	for _, op := range capabilities.Operations {
		listed[op] = true
	}
	for _, op := range []string{"identity.link", "identity.unlink"} {
		if !listed[op] || !knownOperation(op) {
			t.Fatalf("%s is not both advertised and reachable", op)
		}
	}
	for key, want := range map[string]any{"signed_only": true, "anonymous": false, "delegated": false, "attestations": false, "links_on_behalf": false, "maximum_links": float64(board.IdentityLinkMaxPerKey)} {
		if capabilities.Links[key] != want {
			t.Fatalf("identity_links.%s = %v, want %v", key, capabilities.Links[key], want)
		}
	}
	if capabilities.Links["domain"].(map[string]any)["checks_enabled"] != false {
		t.Fatal("rechecks reported enabled on a service that never started them")
	}
	if !strings.Contains(makeRequest(New(&fakeService{}, nil, Config{IdentityChecks: true}), "GET", "/capabilities", "", "").Body.String(), `"checks_enabled":true`) {
		t.Fatal("started rechecks are not reported")
	}
	if !strings.Contains(makeRequest(s, "GET", "/llms.txt", "", "").Body.String(), "identity.link") {
		t.Fatal("llms.txt does not mention identity links")
	}
	for _, path := range []string{"/api/identity/link", "/api/links", "/w/identity.link"} {
		if w := makeRequest(s, "GET", path+"?operation=identity.link", "", ""); w.Code < 400 {
			t.Fatalf("%s answered %d; links are signed commands only", path, w.Code)
		}
	}
}
