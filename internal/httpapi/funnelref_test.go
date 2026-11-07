package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// C469: a ?ref= tag seen at first contact is the account's funnel entry, on
// every transport an outside agent arrives by.
func TestFunnelRefEveryTransport(t *testing.T) {
	store, s := hostedServer(t)
	send := func(method, path, addr, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = addr + ":4444"
		r.Header.Set("User-Agent", "curl/8.5.0")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	n := byte(0)
	newKey := func() ed25519.PrivateKey {
		n++
		seed := make([]byte, 32)
		seed[0], seed[1] = 0xc4, n
		return ed25519.NewKeyFromSeed(seed)
	}
	account := func(key ed25519.PrivateKey) string {
		h := sha256.Sum256(key.Public().(ed25519.PublicKey))
		return hex.EncodeToString(h[:])
	}
	signedPost := func(key ed25519.PrivateKey) []byte {
		c := board.Command{Operation: "post", Room: "lobby", Text: "hello, friend", RequestID: randomHex(), // short: one network's near-copies within an hour are a flood (repeat_flood)
			PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), Timestamp: time.Now().Unix(), Nonce: randomHex()}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		raw, _ := json.Marshal(c)
		return raw
	}
	post := func(path, addr string, key ed25519.PrivateKey) {
		t.Helper()
		if w := send("POST", path, addr, string(signedPost(key))); w.Code != 200 {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	get := func(path, addr string) { send("GET", path, addr, "") }

	// (1) HTTP: a ref on the command URL; a later different one never replaces it.
	explicit := newKey()
	post("/v1/command?ref=skitter-colony", "203.0.113.1", explicit)
	post("/v1/command?ref=skitter-other", "203.0.113.1", explicit)
	// (1) HTTP: the landing page's ref carried to the first write from its network.
	landed := newKey()
	get("/llms.txt?ref=skitter-agentboard", "203.0.113.2")
	get("/for-agents?ref=skitter-other", "203.0.113.2") // the first landing ref wins
	post("/v1/command", "203.0.113.2", landed)
	// A bad ref is ignored, on a landing page and on the command URL alike.
	bad := newKey()
	get("/llms.txt?ref=Skitter_Colony", "203.0.113.3")
	post("/v1/command", "203.0.113.3", bad)
	if w := send("POST", "/v1/command?ref=../etc", "203.0.113.3", string(signedPost(newKey()))); w.Code != 400 {
		t.Fatalf("bad ref on the command URL: %d", w.Code)
	}
	// (3) The web UI: the home page's ref, then the browser's first signed write.
	browser := newKey()
	get("/?ref=skitter-web", "203.0.113.6")
	post("/v1/command", "203.0.113.6", browser)
	// Another network's landing ref is not this one's.
	stranger := newKey()
	post("/v1/command", "203.0.113.4", stranger)
	// (1) A GET write link (base64url envelope) with &ref=.
	linked := newKey()
	if w := send("GET", "/c64/"+base64.RawURLEncoding.EncodeToString(signedPost(linked))+"?ref=skitter-colony", "203.0.113.5", ""); w.Code != 200 {
		t.Fatalf("GET write link: %d %s", w.Code, w.Body.String())
	}
	// (2) MCP: a ref on the server URL; a network's landing ref is not used.
	viaMCP, _ := mustTool(t, s, "/mcp?ref=skitter-colony", "", "create_identity", map[string]any{})["data"].(map[string]any)
	get("/llms.txt?ref=skitter-agentboard", "198.51.100.8") // mcpRequest's address
	mcpPlain, _ := mustTool(t, s, "/mcp", "", "create_identity", map[string]any{})["data"].(map[string]any)

	if err := store.FlushFunnel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FunnelRecompute(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.FunnelRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]board.FunnelRow{}
	for _, r := range rows {
		got[r.Account] = r
	}
	for _, c := range []struct {
		name, account, ref string
	}{
		{"explicit", account(explicit), "skitter-colony"},
		{"landed", account(landed), "skitter-agentboard"},
		{"bad", account(bad), ""},
		{"browser", account(browser), "skitter-web"},
		{"stranger", account(stranger), ""},
		{"linked", account(linked), "skitter-colony"},
		{"mcp", viaMCP["agent"].(string), "skitter-colony"},
		{"mcp without ref", mcpPlain["agent"].(string), ""},
	} {
		r, ok := got[c.account]
		if !ok {
			t.Fatalf("%s: no funnel row", c.name)
		}
		want := ""
		if c.ref != "" {
			want = "ref:" + c.ref
		}
		if r.EntryRef != c.ref || (want != "" && r.Entry != want) || (want == "" && strings.HasPrefix(r.Entry, "ref:")) {
			t.Errorf("%s: entry %q entry_ref %q, want ref %q", c.name, r.Entry, r.EntryRef, c.ref)
		}
	}
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// A ?ref=TAG landing followed by the first signed act from the same
// network records entry ref:TAG and entry_ref TAG, end to end through the
// funnel's flush and recompute: over IPv4 from the same address, over IPv6
// from another address of the same /64 (privacy addresses), never from
// another /64; and the landing's address is stored nowhere.
func TestFunnelRefLandingThenFirstAct(t *testing.T) {
	store, s := hostedServer(t)
	send := func(method, path, addr, body string) {
		t.Helper()
		r := httptest.NewRequest(method, "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = addr
		r.Header.Set("User-Agent", "python-requests/2.32")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s from %s: %d %s", method, path, addr, w.Code, w.Body.String())
		}
	}
	first := func(seed byte, addr string) string {
		t.Helper()
		key := ed25519.NewKeyFromSeed(append([]byte{0xf1, seed}, make([]byte, 30)...))
		c := board.Command{Operation: "post", Room: "lobby", Text: "hello, I came from a link", RequestID: randomHex(),
			PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), Timestamp: time.Now().Unix(), Nonce: randomHex()}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		raw, _ := json.Marshal(c)
		send("POST", "/v1/command", addr, string(raw))
		h := sha256.Sum256(key.Public().(ed25519.PublicKey))
		return hex.EncodeToString(h[:])
	}
	send("GET", "/llms.txt?ref=outreach-colony", "192.0.2.44:5000", "")
	v4 := first(1, "192.0.2.44:6000")
	send("GET", "/skill.md?ref=outreach-v6", "[2001:db8:1:2::10]:5000", "")
	v6 := first(2, "[2001:db8:1:2:aaaa::77]:6000")
	other := first(3, "[2001:db8:1:3::10]:6000")

	if err := store.FlushFunnel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FunnelRecompute(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.FunnelRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]board.FunnelRow{}
	for _, r := range rows {
		got[r.Account] = r
	}
	for _, c := range []struct{ name, account, ref string }{{"ipv4", v4, "outreach-colony"}, {"ipv6 same /64", v6, "outreach-v6"}, {"ipv6 other /64", other, ""}} {
		r := got[c.account]
		if c.ref == "" {
			if r.EntryRef != "" || strings.HasPrefix(r.Entry, "ref:") {
				t.Errorf("%s: entry %q entry_ref %q, want none", c.name, r.Entry, r.EntryRef)
			}
			continue
		}
		if r.Entry != "ref:"+c.ref || r.EntryRef != c.ref {
			t.Errorf("%s: entry %q entry_ref %q, want ref:%s", c.name, r.Entry, r.EntryRef, c.ref)
		}
	}
}
