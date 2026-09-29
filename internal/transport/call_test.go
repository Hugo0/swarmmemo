package transport

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// Over TCP, CALL makes a service call without a key: the /call/ URL's query
// on one line, billed to the TCP peer's network like an HTTP call. DNS only
// names the URL. Signed service calls stay on HTTPS.

func openAnonStore(t *testing.T, anonCap int64) *board.Store {
	t.Helper()
	f := board.Features{Services: []string{"notary", "memory"}, Ledger: board.LedgerOn, AnonPrefix: true}
	store, err := board.Open(filepath.Join(t.TempDir(), "t.db"), board.Config{ServiceID: testService, DailyBytes: 1 << 20, AnonymousDailyBytes: 1 << 20, GlobalDailyBytes: 1 << 24, MaxTextBytes: 16384, Features: f})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := ledger.DefaultAllowanceParams()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 1_600_000, 1_600_000, 1_600_000
	rp.Cap = []int64{400_000, 200_000, 100_000, anonCap}
	rp.Floor = []int64{1600, 1600, 1600, anonCap}
	rp.RootCap = []int64{1_600_000, 800_000, 400_000, anonCap}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, 100_000}
	if _, err = store.SetAllowanceParams(context.Background(), ledger.AllowanceNamespace, p.Marshal(), "test", 0); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestTCPCallWithoutAKey(t *testing.T) {
	store := openAnonStore(t, 2000)
	_, addrs := started(t, store, nil)
	tcp := addrs["tcp/tcp"]
	out := streamExchange(t, tcp, []byte("CALL notary.stamp text=over+netcat&max_cost=1&request_id=tcp-request-id-01\n"), false)
	var answer struct {
		OK   bool `json:"ok"`
		Data struct {
			Call struct {
				ID, State string
				Cost      int64
			} `json:"call"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil || !answer.OK || answer.Data.Call.State != "done" || answer.Data.Call.Cost != 1 {
		t.Fatalf("CALL: %v %s", err, out)
	}
	// The same line again is the same call.
	again := streamExchange(t, tcp, []byte("CALL notary.stamp text=over+netcat&max_cost=1&request_id=tcp-request-id-01\n"), false)
	if !strings.Contains(again, answer.Data.Call.ID) {
		t.Fatalf("retry over TCP: %s", again)
	}
	// A public read too.
	if out := streamExchange(t, tcp, []byte("CALL notary.key\n"), false); !strings.Contains(out, "public_key") {
		t.Fatalf("read over TCP: %s", out)
	}
	for line, want := range map[string]string{
		"CALL memory.put key=k&value=v&max_cost=300&request_id=m1": "signature_required",
		"CALL notary.stamp text=x&max_cost=1":                      "invalid_request", // no request_id
		"CALL notary.stamp text=x&max_cost=1&request_id=t1":        "at least 16 characters",
		"CALL notary.stamp text=x&request_id=r":                    "invalid_request", // no max_cost
		"CALL nope.x max_cost=1&request_id=r":                      "invalid_service",
		"CALL notary":                                              "invalid_request",
	} {
		if out := streamExchange(t, tcp, []byte(line+"\n"), false); !strings.Contains(out, want) {
			t.Errorf("%q: %s, want %s", line, out, want)
		}
	}
	help := streamExchange(t, tcp, []byte("HELP\n"), false)
	if !strings.Contains(help, "No key needed for the notary: 2,000 credits a day per network.") || !strings.Contains(help, "CALL notary.stamp text=") || !strings.Contains(help, "CALL <service.method>") {
		t.Fatalf("HELP: %s", help)
	}
	// DNS names the URL and never carries the call.
	d := testDNS(t)
	d.help = newCatalogHelp(store.Features(), "https://swarmmemo.com", store)
	lines := strings.Join(d.help.dnsHelp("q.swarmmemo.com"), "\n")
	if !strings.Contains(lines, "No key needed for the notary") || !strings.Contains(lines, "https://swarmmemo.com"+services.CallPathPrefix+"notary/stamp?") {
		t.Fatalf("help.q: %s", lines)
	}
	one, _ := d.help.dnsService("notary")
	if !strings.Contains(strings.Join(one, "\n"), "No key: https://swarmmemo.com/call/notary/stamp?") {
		t.Fatalf("notary.services.q: %v", one)
	}
}

func TestSignedServiceCallsStayOnHTTPS(t *testing.T) {
	if err := permitted("tcp", board.Command{Operation: "service.call", PublicKey: "k", Signature: "s"}); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("a signed service.call over a constrained wire: %v", err)
	}
	if err := permitted("tcp", board.Command{Operation: "service.call", Target: "notary"}); err != nil {
		t.Fatalf("an unsigned service.call: %v", err)
	}
	// Security review 1.21, L7: only the TCP wire, whose peer is a real
	// connection, takes an unsigned call; the other wires name no network.
	for _, wire := range []string{"dns", "email", "gemini", "nostr", ""} {
		if err := permitted(wire, board.Command{Operation: "service.call", Target: "notary"}); err == nil {
			t.Errorf("an unsigned service.call over %q was permitted", wire)
		}
		if err := permitted(wire, board.Command{Operation: "service.read", Target: "notary"}); err != nil {
			t.Errorf("a public service.read over %q: %v", wire, err)
		}
	}
}

func TestTCPCallHelpSilentWithoutCredit(t *testing.T) {
	store := openAnonStore(t, 0)
	h := newCatalogHelp(store.Features(), "https://swarmmemo.com", store)
	if strings.Contains(h.lineText(), "No key") || strings.Contains(strings.Join(h.dnsHelp("q.swarmmemo.com"), " "), "No key") {
		t.Fatal("help advertises calls without a key while there is no credit for them")
	}
}
