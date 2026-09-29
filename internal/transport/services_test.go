package transport

import (
	"context"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// The DNS names and the TCP HELP text are generated from the catalogue: help
// lists what SwarmMemo gives besides the services and names every service,
// services lists each one's line, and each service's own name answers in
// full within the TCP answer size, so the dig line /for-agents shows for it
// works.
func TestDNSAndLineHelpListTheCatalogue(t *testing.T) {
	f := board.Features{Services: services.Known(), Trust: board.TrustShadow}
	d := testDNS(t)
	d.help = newCatalogHelp(f, "https://swarmmemo.com", nil)
	budget := (*dns).Limits(nil).Response
	ask := func(name string) (string, []byte) {
		out := answerOnce(t, d, dnsQueryBytes(name, dnsTypeTXT), board.Result{}, nil, budget)
		if flag(out, 0x0200) {
			t.Errorf("%s is truncated at the TCP answer size (%d bytes)", name, budget)
		}
		return string(out), out
	}
	help, _ := ask("help.q.swarmmemo.com")
	assertHelpNamesAll(t, help, f)
	list, _ := ask("services.q.swarmmemo.com")
	for _, e := range services.Catalog(f.Services) {
		if !strings.Contains(list, e.ID+": ") {
			t.Errorf("services.q lacks %s", e.ID)
		}
		web.SetDNSZone("q.swarmmemo.com")
		name := strings.TrimPrefix(web.ServiceExamples("https://swarmmemo.com", e).DNS, "dig +short TXT ")
		one, raw := ask(name)
		if rcode(raw) != rcodeOK || !strings.Contains(one, "Docs: https://swarmmemo.com"+e.Docs) || !strings.Contains(one, e.Title+": ") {
			t.Errorf("%s: %q", name, one)
		}
	}
	web.SetDNSZone("")
	if _, raw := ask("nothing.services.q.swarmmemo.com"); rcode(raw) != rcodeNXDomain {
		t.Error("an unknown service must be NXDOMAIN")
	}
	// With no service enabled, services names do not exist; help still answers.
	d.help = newCatalogHelp(board.Features{}, "https://swarmmemo.com", nil)
	if _, raw := ask("services.q.swarmmemo.com"); rcode(raw) != rcodeNXDomain {
		t.Error("services.q while no service runs")
	}
	if _, raw := ask("memory.services.q.swarmmemo.com"); rcode(raw) != rcodeNXDomain {
		t.Error("memory.services.q while no service runs")
	}
	if off, _ := ask("help.q.swarmmemo.com"); !strings.Contains(off, "Voice everywhere: ") || strings.Contains(off, "/api/services") {
		t.Errorf("help.q with no service: %q", off)
	}

	helpReq := Request{Route: "help"}
	out := string((lineProtocol{help: newCatalogHelp(f, "https://swarmmemo.com", nil)}).Render(helpReq, board.Result{}, nil))
	if !strings.HasPrefix(out, lineHelp) || !strings.Contains(out, "https://swarmmemo.com/api/services") {
		t.Fatalf("HELP with services: %q", out)
	}
	for _, e := range services.Catalog(f.Services) {
		if !strings.Contains(out, e.ID) {
			t.Errorf("HELP lacks %s", e.ID)
		}
	}
	if got := string((lineProtocol{help: newCatalogHelp(board.Features{}, "https://swarmmemo.com", nil)}).Render(helpReq, board.Result{}, nil)); got != lineHelp {
		t.Fatalf("HELP changed with no service: %q", got)
	}
}

// While the store offers free credit, TCP HELP and help.ZONE lead with the
// same line /llms.txt does, within the TCP answer size. The zone's own usage
// answer stays small enough for plain UDP and points at help.ZONE.
func TestHelpLeadsWithTheFreeCredit(t *testing.T) {
	f := board.Features{Ledger: board.LedgerOn, Services: services.Known(), Trust: board.TrustShadow}
	offer := board.FreeCreditOffer(100_000, f.Services)
	line := offer.LineAt("https://swarmmemo.com")
	help := newCatalogHelp(f, "https://swarmmemo.com", nil)
	help.offer = func() *board.FreeCredit { return offer }

	out := string((lineProtocol{allowance: true, help: help}).Render(Request{Route: "help"}, board.Result{}, nil))
	if !strings.HasPrefix(out, line+"\n"+board.SigningLine+"\n"+lineHelp) {
		t.Fatalf("HELP does not lead with the offer: %q", out)
	}
	d := testDNS(t)
	d.allowance, d.help = true, help
	budget := (*dns).Limits(nil).Response
	raw := answerOnce(t, d, dnsQueryBytes("help.q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, budget)
	if flag(raw, 0x0200) || !strings.Contains(string(raw), line) {
		t.Errorf("help.q lacks the offer or is truncated: %q", raw)
	}
	if usage := answerOnce(t, d, dnsQueryBytes("q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, 512); flag(usage, 0x0200) {
		t.Error("the zone's usage no longer fits plain UDP")
	}
	// No offer, no line.
	help.offer = func() *board.FreeCredit { return nil }
	if out := string((lineProtocol{help: help}).Render(Request{Route: "help"}, board.Result{}, nil)); !strings.HasPrefix(out, lineHelp) {
		t.Fatalf("HELP without an offer: %q", out)
	}
}

// assertHelpNamesAll fails unless help.ZONE's answer has every give that is
// not a service's, names every service and the catalogue, and points to
// each service's own lines.
func assertHelpNamesAll(t *testing.T, help string, f board.Features) {
	t.Helper()
	catalog := services.Catalog(f.Services)
	service := map[string]bool{}
	for _, e := range catalog {
		service[e.Topic] = true
		if !strings.Contains(help, e.ID) {
			t.Errorf("help.q does not name %s", e.ID)
		}
	}
	for _, g := range web.Gives(f, catalog) {
		if !service[g.Topic] && !strings.Contains(help, g.Topic+": ") {
			t.Errorf("help.q lacks %q", g.Topic)
		}
	}
	for _, want := range []string{"https://swarmmemo.com/api/services", "TXT services.q.swarmmemo.com", "TXT ID.services.q.swarmmemo.com"} {
		if !strings.Contains(help, want) {
			t.Errorf("help.q lacks %q: %q", want, help)
		}
	}
}

// help.ZONE fits the TCP answer size in the worst case: every production
// service, each trust mode, card images, the free credit offer and calls
// without a key at a large allowance (security review screen, M2).
func TestHelpFitsTheTCPAnswerInTheWorstCase(t *testing.T) {
	web.SetCardImages(true)
	defer web.SetCardImages(false)
	production := []string{"memory", "wakeup", "notary", "inference", "x402", "public_data", "runs", "screen"}
	budget := (*dns).Limits(nil).Response
	for _, trust := range []board.TrustMode{board.TrustOff, board.TrustShadow, board.TrustAllocation} {
		for _, noKey := range []bool{false, true} {
			f := board.Features{Ledger: board.LedgerOn, Services: production, Trust: trust}
			offer := board.FreeCreditOffer(100_000_000, f.Services)
			help := newCatalogHelp(f, "https://swarmmemo.com", nil)
			help.offer = func() *board.FreeCredit { return offer }
			if noKey {
				nk := services.NoKeyFor("https://swarmmemo.com", help.catalog, 100_000_000, 1_000_000_000, "")
				help.noKeyOf = &noKeyCache{read: func(context.Context) services.NoKey { return nk }}
			}
			d := testDNS(t)
			d.allowance, d.help = true, help
			raw := answerOnce(t, d, dnsQueryBytes("help.q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, budget)
			if flag(raw, 0x0200) {
				full := answerOnce(t, d, dnsQueryBytes("help.q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, 1<<16)
				t.Fatalf("trust %s, no key %v: help.q is %d bytes, over the %d-byte TCP answer", trust, noKey, len(full), budget)
			}
			assertHelpNamesAll(t, string(raw), f)
			if noKey != strings.Contains(string(raw), "No key needed for") || !strings.Contains(string(raw), offer.LineAt("https://swarmmemo.com")) {
				t.Errorf("trust %s, no key %v: %q", trust, noKey, raw)
			}
		}
	}
}
