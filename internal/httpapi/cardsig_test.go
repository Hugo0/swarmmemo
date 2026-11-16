package httpapi

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/jcs"
)

// The agent card is signed with the notary key (C581), as an A2A verifier
// checks it: fetch the card, take signatures out, JCS-canonicalise the rest,
// fetch the jku's JWK Set and verify the JWS (EdDSA) over the protected
// header and that payload. The version stays the content hash of the card
// without version and signatures.
func TestAgentCardSignatureVerifies(t *testing.T) {
	dir := t.TempDir()
	features := board.Features{Services: []string{"notary"}}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features, NotaryKeyFile: filepath.Join(dir, "notary.key")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})

	w := makeRequest(s, "GET", "/.well-known/agent-card.json", "", "")
	if w.Code != 200 {
		t.Fatalf("card: %d", w.Code)
	}
	var card map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	sigs, ok := card["signatures"].([]any)
	if !ok || len(sigs) != 1 {
		t.Fatalf("signatures: %v", card["signatures"])
	}
	sig := sigs[0].(map[string]any)
	protected, _ := sig["protected"].(string)
	signature, _ := sig["signature"].(string)
	delete(card, "signatures")
	if v := agentCardVersion(card); v != card["version"] {
		t.Fatalf("version %v is not the content hash %s", card["version"], v)
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := jcs.Canonical(raw)
	if err != nil {
		t.Fatal(err)
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(protected)
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]string
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	pub, id := store.NotaryPublicKey()
	if header["alg"] != "EdDSA" || header["kid"] != id || header["jku"] != "https://swarmmemo.com"+agentCardKeysPath {
		t.Fatalf("header: %v", header)
	}

	k := makeRequest(s, "GET", agentCardKeysPath, "", "")
	var set struct {
		Keys []agentJWK `json:"keys"`
	}
	if k.Code != 200 || json.Unmarshal(k.Body.Bytes(), &set) != nil || len(set.Keys) != 1 {
		t.Fatalf("jwks: %d %s", k.Code, k.Body.String())
	}
	jwk := set.Keys[0]
	if jwk.Kid != header["kid"] || jwk.Kty != "OKP" || jwk.Crv != "Ed25519" || jwk.Alg != "EdDSA" || jwk.X != pub {
		t.Fatalf("jwk: %+v", jwk)
	}
	key, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatalf("jwk x: %v", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		t.Fatal(err)
	}
	input := func(p []byte) []byte { return []byte(protected + "." + base64.RawURLEncoding.EncodeToString(p)) }
	if !ed25519.Verify(key, input(payload), sigBytes) {
		t.Fatal("the card's signature does not verify")
	}
	card["description"] = "tampered"
	raw, _ = json.Marshal(card)
	if tampered, _ := jcs.Canonical(raw); ed25519.Verify(key, input(tampered), sigBytes) {
		t.Fatal("a changed card still verifies")
	}
	if caps := makeRequest(s, "GET", capabilitiesFullPath, "", ""); !json.Valid(caps.Body.Bytes()) || !containsJSON(caps.Body.Bytes(), "a2a_agent_card_keys", agentCardKeysPath) {
		t.Fatalf("/capabilities does not name the card's keys")
	}

	// Without the notary key: no signatures and no JWK Set.
	plain := New(&fakeService{}, nil, Config{PublicURL: "https://swarmmemo.com"})
	if _, ok := plain.agentCard()["signatures"]; ok {
		t.Fatal("a card signed without a key")
	}
	if w := makeRequest(plain, "GET", agentCardKeysPath, "", ""); w.Code == 200 {
		t.Fatalf("jwks without a key: %d", w.Code)
	}
}

func containsJSON(body []byte, key, want string) bool {
	var m map[string]any
	return json.Unmarshal(body, &m) == nil && m[key] == want
}
