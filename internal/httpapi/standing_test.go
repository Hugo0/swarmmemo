package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/bits"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

func signedStanding(key ed25519.PrivateKey, op, data string, n int) string {
	c := board.Command{Operation: op, Data: data, PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		Timestamp: time.Now().Unix(), Nonce: "standing-" + strconv.Itoa(n), RequestID: "standing-request-" + strconv.Itoa(n)}
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	raw, _ := json.Marshal(c)
	return string(raw)
}

// The whole path over HTTPS: the ways read (GET and the command endpoint
// agree), a proof-of-work challenge and solution, and a wallet linked with
// a Sign-In with Ethereum signature, whose link matches its published schema.
func TestStandingOverHTTP(t *testing.T) {
	features := board.Features{Trust: board.TrustShadow}
	store, err := board.Open(filepath.Join(t.TempDir(), "standing.db"), board.Config{ServiceID: "swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := New(store, web.Handler(store), Config{ServiceID: "swarmmemo.com", Features: features})
	validator := identityLinkValidator(t, s)
	seed := make([]byte, 32)
	seed[0] = 44
	key := ed25519.NewKeyFromSeed(seed)
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	fingerprint := hex.EncodeToString(sum[:])
	if w := tlsCommand(s, signedLinkCommand(key, "agent.register", "", 0)); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	type answer struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	read := func(body []byte) answer {
		var a answer
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return a
	}
	w := makeRequest(s, "GET", "/api/agent/"+fingerprint+"/standing", "", "")
	ways := read(w.Body.Bytes())
	if w.Code != 200 || !ways.OK || len(ways.Data["ways"].([]any)) != 5 || ways.Data["agent"] != fingerprint {
		t.Fatalf("ways: %d %s", w.Code, w.Body.String())
	}
	cmd := makeRequest(s, "POST", "/v1/command", `{"operation":"standing.ways","target":"`+fingerprint+`"}`, "application/json")
	if viaCommand := read(cmd.Body.Bytes()); !viaCommand.OK || len(viaCommand.Data["ways"].([]any)) != 5 {
		t.Fatalf("standing.ways via /v1/command: %d %s", cmd.Code, cmd.Body.String())
	}
	if w = makeRequest(s, "GET", "/api/agent/"+fingerprint+"/standing?target=other", "", ""); w.Code != 400 {
		t.Fatalf("conflicting target: %d", w.Code)
	}
	// Proof of work.
	w = tlsCommand(s, signedStanding(key, "standing.challenge", `{"schema":1,"kind":"pow","bits":20}`, 1))
	ch := read(w.Body.Bytes())
	if w.Code != 200 || ch.Data["prefix"] == nil {
		t.Fatalf("pow challenge: %d %s", w.Code, w.Body.String())
	}
	prefix, nonce := ch.Data["prefix"].(string), ch.Data["nonce"].(string)
	solution := ""
	for i := 0; ; i++ {
		h := sha256.Sum256([]byte(prefix + strconv.Itoa(i)))
		if z := bits.LeadingZeros32(uint32(h[0])<<24 | uint32(h[1])<<16 | uint32(h[2])<<8 | uint32(h[3])); z >= 20 {
			solution = strconv.Itoa(i)
			break
		}
	}
	w = tlsCommand(s, signedStanding(key, "standing.work", `{"schema":1,"nonce":"`+nonce+`","solution":"`+solution+`"}`, 2))
	if work := read(w.Body.Bytes()); w.Code != 200 || work.Data["work_units"] != float64(1) {
		t.Fatalf("work: %d %s", w.Code, w.Body.String())
	}
	// A wallet.
	walletKey := bytes.Repeat([]byte{0x33}, 32)
	addr, _, _ := services.SignPersonal(walletKey, []byte("x"))
	w = tlsCommand(s, signedStanding(key, "standing.challenge", `{"schema":1,"kind":"wallet","value":"`+addr.String()+`"}`, 3))
	ch = read(w.Body.Bytes())
	if w.Code != 200 {
		t.Fatalf("wallet challenge: %d %s", w.Code, w.Body.String())
	}
	_, sig, _ := services.SignPersonal(walletKey, []byte(ch.Data["message"].(string)))
	data, _ := json.Marshal(map[string]any{"schema": 1, "kind": "wallet", "value": addr.String(), "proof": sig, "nonce": ch.Data["nonce"]})
	if w = tlsCommand(s, signedStanding(key, "identity.link", string(data), 4)); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"verified"`) {
		t.Fatalf("wallet link: %d %s", w.Code, w.Body.String())
	}
	w = makeRequest(s, "GET", "/api/agent/"+fingerprint, "", "")
	var raw struct {
		Agent struct {
			Links []map[string]any `json:"links"`
		} `json:"agent"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &raw); err != nil || len(raw.Agent.Links) != 1 {
		t.Fatalf("agent: %s", w.Body.String())
	}
	if err = validator.Validate(raw.Agent.Links[0]); err != nil {
		t.Fatalf("wallet link drifted from its published schema: %v\n%v", err, raw.Agent.Links[0])
	}
	// The agent page lists the ways read-only, the same states the API
	// gives; /me carries the forms that add them.
	page := makeRequest(s, "GET", "/agent/"+fingerprint, "", "").Body.String()
	for _, want := range []string{`id="raise-standing"`, `id="standing-ways"`, `id="way-wallet" data-kind="wallet" data-state="verified" data-value="` + addr.String() + `"`,
		`id="way-pow" data-kind="pow" data-state="verified"`, `id="way-github" data-kind="github" data-state="none"`, `id="way-earned"`, `href="/api/agent/` + fingerprint + `/standing"`} {
		if !strings.Contains(page, want) {
			t.Errorf("agent page lacks %s", want)
		}
	}
	me := makeRequest(s, "GET", "/me", "", "").Body.String()
	for _, want := range []string{`id="standing"`, `id="standing-list"`, `id="standing-wallet-form"`, `id="standing-github-form"`, `id="standing-pow-form"`, `id="standing-status"`} {
		if !strings.Contains(me, want) {
			t.Errorf("/me lacks %s", want)
		}
	}
	// /capabilities lists the roots.
	caps := makeRequest(s, "GET", "/capabilities", "", "").Body.String()
	for _, want := range []string{`"raise":{`, `"standing.challenge {\"schema\":1,`, `"wallet":{`, `"cap_cents":600`, `"pow_open":4`} {
		if !strings.Contains(caps, want) {
			t.Errorf("/capabilities lacks %s", want)
		}
	}
}

// raise_standing makes exactly the signed commands the page and HTTPS use.
func TestRaiseStandingCommands(t *testing.T) {
	self := strings.Repeat("a", 64)
	for _, tc := range []struct {
		in       raiseStandingInput
		op, data string
		target   string
	}{
		{raiseStandingInput{}, "standing.ways", "", self},
		{raiseStandingInput{Action: "ways", Agent: "bob"}, "standing.ways", "", "bob"},
		{raiseStandingInput{Action: "challenge", Kind: "pow", Bits: 22}, "standing.challenge", `{"bits":22,"kind":"pow","schema":1}`, ""},
		{raiseStandingInput{Action: "challenge", Kind: "github", Value: "octo"}, "standing.challenge", `{"kind":"github","schema":1,"value":"octo"}`, ""},
		{raiseStandingInput{Action: "link", Kind: "wallet", Value: "0xA", Proof: "0xS", Nonce: "N"}, "identity.link", `{"kind":"wallet","nonce":"N","proof":"0xS","schema":1,"value":"0xA"}`, ""},
		{raiseStandingInput{Action: "work", Nonce: "N", Solution: "42"}, "standing.work", `{"nonce":"N","schema":1,"solution":"42"}`, ""},
	} {
		c, err := tc.in.command(self)
		if err != nil || c.Operation != tc.op || c.Data != tc.data || c.Target != tc.target {
			t.Errorf("%+v: %+v %v", tc.in, c, err)
		}
	}
	for _, in := range []raiseStandingInput{{Action: "link", Kind: "domain"}, {Action: "mint"}} {
		if _, err := in.command(self); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
}
