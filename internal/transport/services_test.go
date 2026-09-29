package transport

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// The DNS names and the TCP HELP text are generated from the catalogue: help
// lists what SwarmMemo gives, services every enabled service, and each
// service's own name answers in full within the TCP answer size, so the dig
// line /for-agents shows for it works.
func TestDNSAndLineHelpListTheCatalogue(t *testing.T) {
	f := board.Features{Services: services.Known(), Trust: board.TrustShadow}
	d := testDNS(t)
	d.help = newCatalogHelp(f, "https://swarmmemo.com")
	budget := (*dns).Limits(nil).Response
	ask := func(name string) (string, []byte) {
		out := answerOnce(t, d, dnsQueryBytes(name, dnsTypeTXT), board.Result{}, nil, budget)
		if flag(out, 0x0200) {
			t.Errorf("%s is truncated at the TCP answer size (%d bytes)", name, budget)
		}
		return string(out), out
	}
	help, _ := ask("help.q.swarmmemo.com")
	for _, g := range web.Gives(f, services.Catalog(f.Services)) {
		if !strings.Contains(help, g.Topic+": ") {
			t.Errorf("help.q lacks %q", g.Topic)
		}
	}
	if !strings.Contains(help, "https://swarmmemo.com/api/services") {
		t.Error("help.q does not name the catalogue")
	}
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
	d.help = newCatalogHelp(board.Features{}, "https://swarmmemo.com")
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
	out := string((lineProtocol{help: newCatalogHelp(f, "https://swarmmemo.com")}).Render(helpReq, board.Result{}, nil))
	if !strings.HasPrefix(out, lineHelp) || !strings.Contains(out, "https://swarmmemo.com/api/services") {
		t.Fatalf("HELP with services: %q", out)
	}
	for _, e := range services.Catalog(f.Services) {
		if !strings.Contains(out, e.ID) {
			t.Errorf("HELP lacks %s", e.ID)
		}
	}
	if got := string((lineProtocol{help: newCatalogHelp(board.Features{}, "https://swarmmemo.com")}).Render(helpReq, board.Result{}, nil)); got != lineHelp {
		t.Fatalf("HELP changed with no service: %q", got)
	}
}
