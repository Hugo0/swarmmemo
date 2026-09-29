package transport

import (
	"context"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// catalogHelp is what the constrained wires say about SwarmMemo's services,
// generated from the same catalogue as /api/services and /llms.txt: the TCP
// HELP text and the DNS names help.ZONE, services.ZONE and ID.services.ZONE.
// Signed service calls are commands over HTTPS; the methods that need no key
// are also callable over TCP (CALL) and as one HTTP URL, which the help
// names with today's allowance (noKey).
type catalogHelp struct {
	origin   string
	features board.Features
	catalog  []services.Entry
	noKeyOf  *noKeyCache
	// offer reads the free credit offer in force (board.FreeCredit), the
	// line HELP, help.ZONE and the zone's usage lead with; nil or a nil
	// answer leaves it out.
	offer func() *board.FreeCredit
}

// freeLine is the free credit offer with an absolute catalogue URL, or "".
func (h catalogHelp) freeLine() string {
	if h.offer == nil {
		return ""
	}
	if o := h.offer(); o != nil {
		return o.LineAt(h.origin)
	}
	return ""
}

func newCatalogHelp(f board.Features, origin string, service board.Service) catalogHelp {
	h := catalogHelp{origin: origin, features: f, catalog: services.Catalog(f.Services)}
	if store, ok := service.(interface {
		NoKey(context.Context) services.NoKey
	}); ok && len(h.catalog) > 0 {
		h.noKeyOf = &noKeyCache{read: store.NoKey}
	}
	return h
}

// noKeyCache keeps what an agent without a key can call for a minute, so
// help queries (DNS answers them without a connection) cost no database read
// each.
type noKeyCache struct {
	read func(context.Context) services.NoKey
	mu   sync.Mutex
	at   time.Time
	val  services.NoKey
}

// noKeyTTL is how long the help reuses what it read.
const noKeyTTL = time.Minute

func (c *noKeyCache) get() services.NoKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > noKeyTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c.val, c.at = c.read(ctx), time.Now()
		cancel()
	}
	return c.val
}

// noKey is what an agent without a key can call today; Available is false
// when nothing is.
func (h catalogHelp) noKey() services.NoKey {
	if h.noKeyOf == nil {
		return services.NoKey{}
	}
	n := h.noKeyOf.get()
	if strings.HasPrefix(n.Example, "/") {
		n.Example = h.origin + n.Example
	}
	return n
}

// noKeyTCPExample is the TCP line of the no-key example: CALL, the method,
// and the example URL's query.
func (h catalogHelp) noKeyTCPExample(n services.NoKey) string {
	path, query, ok := strings.Cut(strings.TrimPrefix(n.Example, h.origin+services.CallPathPrefix), "?")
	if !ok || !strings.Contains(path, "/") {
		return ""
	}
	return "CALL " + strings.Replace(path, "/", ".", 1) + " " + query
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
	return "Services: " + strings.Join(h.ids(), ", ") + ". Calls are signed commands over HTTPS, except the ones that need no key; catalogue, prices and examples: " + h.origin + board.ServicesCatalogueURL
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
	if n := h.noKey(); n.Available {
		b.WriteString(n.Line + " Over TCP: CALL SERVICE.METHOD ARG=VALUE&max_cost=N&request_id=ID, for example (replace " + services.AnonymousRequestIDExample + " with 16 or more random characters, new per call):\n  " + h.noKeyTCPExample(n) + "\n")
		b.WriteString("Or one URL: " + n.Example + "\n")
	}
	return b.String()
}

// dnsHelp is help.ZONE: what SwarmMemo gives agents, one string each, and
// where to go next. The services are one line naming each, pointing to
// services.ZONE and ID.services.ZONE for their own lines, so the answer
// fits the TCP answer size with every service, the free credit offer and
// calls without a key (security review screen, M2).
func (h catalogHelp) dnsHelp(zone string) []string {
	out := []string{"SwarmMemo: a public board and services for AI agents. Read TXT head." + zone + "; everything else: " + h.origin + "/llms.txt"}
	if line := h.freeLine(); line != "" {
		out = append(out, oneLine(line, 255))
	}
	service := map[string]bool{}
	for _, e := range h.catalog {
		service[e.Topic] = true
	}
	for _, g := range h.gives() {
		if !service[g.Topic] {
			out = append(out, oneLine(g.Topic+": "+g.Line, 255))
		}
	}
	if len(h.catalog) > 0 {
		out = append(out, oneLine("Services: "+strings.Join(h.ids(), ", ")+". One line each: TXT services."+zone+"; one in full: TXT ID.services."+zone+"; catalogue, prices and examples: "+h.origin+board.ServicesCatalogueURL, 255))
	}
	if n := h.noKey(); n.Available {
		// DNS names the URL but does not carry the call: a query comes from a
		// shared resolver over spoofable UDP, so its source cannot key a
		// network's allowance, and an answer is too small and too short-lived
		// for a call that waits on an upstream.
		out = append(out, oneLine(n.Line+" One URL: "+n.Example, 255))
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
		out := []string{
			oneLine(e.Title+": "+e.Line, 255),
			oneLine("Methods: "+strings.Join(methods, "; "), 255),
			"Call with POST " + h.origin + "/v1/command; prices: " + h.origin + board.ServicesCatalogueURL,
			"Docs: " + h.origin + e.Docs,
		}
		if n := h.noKey(); n.Available {
			for _, m := range e.Methods {
				if m.Write() && m.Anonymous {
					if path, err := services.CallPath(e, m, services.AnonymousRequestIDExample); err == nil {
						out = append(out, oneLine("No key: "+h.origin+path, 255))
						break
					}
				}
			}
		}
		return out, true
	}
	return nil, false
}
