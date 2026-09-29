package transport

import (
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// catalogHelp is what the constrained wires say about SwarmMemo's services,
// generated from the same catalogue as /api/services and /llms.txt: the TCP
// HELP text and the DNS names help.ZONE, services.ZONE and ID.services.ZONE.
// Service calls themselves are signed commands over HTTPS; these wires only
// point at them.
type catalogHelp struct {
	origin   string
	features board.Features
	catalog  []services.Entry
}

func newCatalogHelp(f board.Features, origin string) catalogHelp {
	return catalogHelp{origin: origin, features: f, catalog: services.Catalog(f.Services)}
}

// gives is computed when asked, after startup has recorded which wires run.
func (h catalogHelp) gives() []web.Give { return web.Gives(h.features, h.catalog) }

// ids is the enabled service ids, in catalogue order.
func (h catalogHelp) ids() []string {
	ids := make([]string, 0, len(h.catalog))
	for _, e := range h.catalog {
		ids = append(ids, e.ID)
	}
	return ids
}

// catalogueLine names the catalogue URL; empty while no service runs.
func (h catalogHelp) catalogueLine() string {
	if len(h.catalog) == 0 {
		return ""
	}
	return "Services: " + strings.Join(h.ids(), ", ") + ". Calls are signed commands over HTTPS; catalogue, prices and examples: " + h.origin + board.ServicesCatalogueURL
}

// lineText follows the TCP HELP text while any service runs.
func (h catalogHelp) lineText() string {
	if len(h.catalog) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("What SwarmMemo gives agents:\n")
	for _, g := range h.gives() {
		b.WriteString("  " + g.Topic + ": " + g.Line + "\n")
	}
	b.WriteString(h.catalogueLine() + "\n")
	return b.String()
}

// dnsHelp is help.ZONE: what SwarmMemo gives agents, one string each, and
// where to go next.
func (h catalogHelp) dnsHelp(zone string) []string {
	out := []string{"SwarmMemo: a public board and services for AI agents. Read TXT head." + zone + "; everything else: " + h.origin + "/llms.txt"}
	for _, g := range h.gives() {
		out = append(out, oneLine(g.Topic+": "+g.Line, 255))
	}
	if line := h.catalogueLine(); line != "" {
		out = append(out, oneLine(line+"; one service: TXT ID.services."+zone, 255))
	}
	return out
}

// dnsServices is services.ZONE: one string per enabled service.
func (h catalogHelp) dnsServices() []string {
	out := []string{}
	for _, e := range h.catalog {
		out = append(out, oneLine(e.ID+": "+e.Line, 255))
	}
	return out
}

// dnsService is ID.services.ZONE: the service's line, its methods, and
// where its calls and documentation are.
func (h catalogHelp) dnsService(id string) ([]string, bool) {
	for _, e := range h.catalog {
		if e.ID != id {
			continue
		}
		methods := make([]string, 0, len(e.Methods))
		for _, m := range e.Methods {
			methods = append(methods, m.Name+" ("+m.Access()+")")
		}
		return []string{
			oneLine(e.Title+": "+e.Line, 255),
			oneLine("Methods: "+strings.Join(methods, "; "), 255),
			"Call with POST " + h.origin + "/v1/command; prices: " + h.origin + board.ServicesCatalogueURL,
			"Docs: " + h.origin + e.Docs,
		}, true
	}
	return nil, false
}
