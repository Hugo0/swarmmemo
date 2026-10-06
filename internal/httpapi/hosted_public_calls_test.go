package httpapi

// The public call tools (fetch_page, notary_stamp and every method that
// takes a call without a key) sign as the connection's hosted identity when
// it has one: the identity's allowance, caps and refusals apply, never the
// network's. Without one they stay anonymous. Real store, real engine, a
// stub site in place of the web.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// publicCallPage is the stub site's page: well over the 8 KiB a call
// without a key may read.
var publicCallPage = strings.Repeat("A line of page text. ", 2000)

// hostedPublicCallServer is a store with hosted identities, the ledger,
// fetch (served by a stub site at http://site.example/) and the notary; the
// anonymous tier, a hosted identity's until it is claimed, gets credit as
// its share of the day.
func hostedPublicCallServer(t *testing.T, credit int64) *Server {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kek := filepath.Join(dir, "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, publicCallPage)
	}))
	t.Cleanup(site.Close)
	fetch, err := services.ParseFetchConfig([]byte(`{"schema":1,"screen":"off"}`))
	if err != nil {
		t.Fatal(err)
	}
	fetch.UseTestUpstream(map[string][]net.IP{"site.example": {net.ParseIP("192.0.2.80")}}, site.Listener.Addr().String())
	features := board.Features{Services: []string{"fetch", "notary"}, Ledger: board.LedgerOn, AnonPrefix: true}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", Features: features, HostedKEKFile: kek, Fetch: fetch, NotaryKeyFile: filepath.Join(dir, "notary.key")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 10_000_000, 10_000_000, 10_000_000
	rp.Cap = []int64{1_000_000, 1_000_000, 1_000_000, credit}
	rp.Floor = []int64{100_000, 100_000, 100_000, credit}
	rp.RootCap = []int64{10_000_000, 10_000_000, 10_000_000, credit}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, 1_000_000}
	if _, err = store.SetAllowanceParams(t.Context(), ledger.AllowanceNamespace, p.Marshal(), "test", 0); err != nil {
		t.Fatal(err)
	}
	return New(store, nil, Config{Features: features, PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com"})
}

// fetch_page with a hosted identity reads more than a call without a key
// may, on the identity's allowance; without one it is the network's call,
// capped at 8 KiB, as before.
func TestPublicFetchSignsWithHostedIdentity(t *testing.T) {
	s := hostedPublicCallServer(t, 1_000_000)
	me := newIdentity(t, s, "")
	url, agent := "/mcp/t/"+me["token"].(string), me["agent"].(string)

	if _, failure := callTool(t, s, "/mcp", "", "fetch_page", map[string]any{"url": "http://site.example/page", "max_bytes": 32768}); !strings.Contains(failure, "32768/8192") {
		t.Fatalf("32 KiB without an identity: %q, want the anonymous cap's refusal", failure)
	}
	if used := creditsUsed(t, s, ""); used != 0 {
		t.Fatalf("a refused call charged the network %v", used)
	}

	for _, auth := range []struct{ path, header string }{{url, ""}, {"/mcp", "Bearer " + me["token"].(string)}} {
		before := creditsUsed(t, s, agent)
		got := mustTool(t, s, auth.path, auth.header, "fetch_page", map[string]any{"url": "http://site.example/page", "max_bytes": 32768})
		text, _ := dig(got, "data", "result", "text").(string)
		cost, _ := dig(got, "data", "call", "cost").(float64)
		if dig(got, "data", "call", "state") != "done" || len(text) <= services.FetchAnonymousTextMax || cost <= 0 {
			t.Fatalf("fetch_page as the identity (%s, bearer %v): %d bytes, %v", auth.path, auth.header != "", len(text), got)
		}
		if used := creditsUsed(t, s, agent) - before; used != cost {
			t.Fatalf("charged %v to the identity, want the call's %v", used, cost)
		}
	}
	if used := creditsUsed(t, s, ""); used != 0 {
		t.Fatalf("the identity's calls were billed to the network: %v", used)
	}

	got := mustTool(t, s, "/mcp", "", "fetch_page", map[string]any{"url": "http://site.example/other"})
	text, _ := dig(got, "data", "result", "text").(string)
	if len(text) == 0 || len(text) > services.FetchAnonymousTextMax || dig(got, "data", "call", "request_id") == nil {
		t.Fatalf("fetch_page without an identity: %d bytes, %v", len(text), got)
	}
	if used, cost := creditsUsed(t, s, ""), dig(got, "data", "call", "cost"); used != cost {
		t.Fatalf("the network was charged %v, want the call's %v", used, cost)
	}
}

// notary_stamp is signed as the identity too: its allowance is charged,
// not the network's.
func TestPublicNotarySignsWithHostedIdentity(t *testing.T) {
	s := hostedPublicCallServer(t, 1_000_000)
	me := newIdentity(t, s, "")
	url, agent := "/mcp/t/"+me["token"].(string), me["agent"].(string)
	got := mustTool(t, s, url, "", "notary_stamp", map[string]any{"text": "signed by the identity"})
	cost, _ := dig(got, "data", "call", "cost").(float64)
	if dig(got, "data", "call", "state") != "done" || cost <= 0 {
		t.Fatalf("notary_stamp: %v", got)
	}
	if used := creditsUsed(t, s, agent); used != cost {
		t.Fatalf("charged %v to the identity, want %v", used, cost)
	}
	if used := creditsUsed(t, s, ""); used != 0 {
		t.Fatalf("billed to the network: %v", used)
	}
}

// An identity whose share is spent gets the signed path's refusal, never
// a fallback to the network's free credit.
func TestPublicCallExhaustedIdentityIsRefused(t *testing.T) {
	s := hostedPublicCallServer(t, 3)
	me := newIdentity(t, s, "")
	url := "/mcp/t/" + me["token"].(string)
	_, failure := callTool(t, s, url, "", "fetch_page", map[string]any{"url": "http://site.example/page", "max_bytes": 32768})
	if !strings.Contains(failure, "quota_exhausted") || strings.Contains(failure, "without a key") {
		t.Fatalf("an exhausted identity: %q, want the signed quota_exhausted", failure)
	}
	if used := creditsUsed(t, s, ""); used != 0 {
		t.Fatalf("fell back to the network: %v", used)
	}
}

// The public call tools say they sign with the identity, and take sign-in
// or none; x402's call, which the identity makes through x402_tools_call,
// is not one of them.
func TestPublicCallToolsDescribeSigning(t *testing.T) {
	s := hostedPublicCallServer(t, 1_000_000)
	tools := listTools(t, s, "/mcp")
	for _, name := range []string{"fetch_page", "notary_stamp"} {
		if !strings.Contains(tools[name].Description, "Signed with your SwarmMemo identity when this connection has one") {
			t.Errorf("%s: %q", name, tools[name].Description)
		}
		if schemes := fmt.Sprint(securitySchemes(name)); schemes != "[map[type:noauth] map[scopes:[hosted] type:oauth2]]" {
			t.Errorf("%s securitySchemes %s", name, schemes)
		}
	}
	if isHostedSignedCall("x402_call") || isHostedSignedCall("notary_get") {
		t.Error("only the public call tools sign")
	}
}
