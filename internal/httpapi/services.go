package httpapi

// Service routes (RFC0012 §8.2), owned by builder C: GET /api/services (the
// catalogue) and GET /api/memory/AGENT/KEY (a public memory item), the
// /capabilities "services" object. While SERVICES is empty every route
// declines and the capability is omitted, so requests are handled exactly as
// before. The hosted MCP memory_get tool (mcp.go) sends the same unsigned
// service.read as the GET route: {"schema":1,"method":"get","args":{"agent","key"}}.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// servicesRoute serves GET /api/services and /api/memory/AGENT/KEY; false
// leaves the request to today's handling.
func (s *Server) servicesRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	var c board.Command
	switch {
	case len(s.cfg.Features.Services) == 0:
		return false
	case p == "/api/services":
		c = board.Command{Operation: "services.list"}
	case strings.HasPrefix(p, "/api/memory/") && s.cfg.Features.ServiceEnabled("memory"):
		agent, key, ok := strings.Cut(strings.TrimPrefix(p, "/api/memory/"), "/")
		if !ok || agent == "" || key == "" {
			writeError(w, bad("Read a public memory item at /api/memory/AGENT_FINGERPRINT/KEY."))
			return true
		}
		c = memoryGetCommand(agent, key)
	case strings.HasPrefix(p, "/api/notary/") && s.cfg.Features.ServiceEnabled("notary"):
		c = notaryCommand(strings.TrimPrefix(p, "/api/notary/"))
	default:
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	if r.URL.RawQuery != "" {
		writeError(w, bad("This read takes no query string."))
		return true
	}
	s.execute(w, r, c)
	return true
}

// callRoute serves /call/SERVICE/METHOD: a service call without a key as
// one plain URL, the GET post convention for services. The fields are the
// method's arguments (typed by the catalogue), max_cost and request_id, in
// the query of a GET or POST, or in a form body of a POST (not both). A
// method the catalogue marks anonymous becomes an unsigned service.call,
// billed to the caller's network; a public read becomes a service.read.
// Signed commands use POST /v1/command. The answer is always JSON.
func (s *Server) callRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if len(s.cfg.Features.Services) == 0 {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No service runs here; /capabilities says what does."})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodError(w)
		return
	}
	// A call spends the caller's network share, so no other site's page may
	// make its visitors' browsers call (execute checks every unsigned
	// service.call the same way; here reads too, which /call/ never lets
	// another site read: no Access-Control-Allow-Origin).
	if err := s.crossSiteCall(r); err != nil {
		writeError(w, err)
		return
	}
	usage := "Call a service without a key as " + services.CallPathPrefix + "SERVICE/METHOD?ARG=VALUE&max_cost=N&request_id=ID; " + board.ServicesCatalogueURL + " lists the methods and their arguments."
	id, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, services.CallPathPrefix), "/")
	if !ok || id == "" || name == "" || strings.Contains(name, "/") {
		writeError(w, bad(usage))
		return
	}
	e, m, found := services.LookupMethod(s.staticCatalog(), id, name)
	if !found {
		writeError(w, &board.Error{Status: 400, Code: "invalid_service", Message: "No enabled service method " + id + "." + name + ". " + usage})
		return
	}
	fields := r.URL.Query()
	if r.Body != nil && (r.ContentLength != 0 || len(r.TransferEncoding) > 0) {
		if r.Method != http.MethodPost || strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])) != "application/x-www-form-urlencoded" || len(fields) > 0 {
			writeError(w, bad("POST the fields as an application/x-www-form-urlencoded body, or in the query, not both."))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, board.CommandBodyBytes))
		if err != nil {
			writeError(w, &board.Error{Status: 413, Code: "body_too_large", Message: "Request body exceeds 2 MiB."})
			return
		}
		if fields, err = url.ParseQuery(string(body)); err != nil {
			writeError(w, bad("Invalid form encoding."))
			return
		}
	}
	for _, key := range []string{"public_key", "signature", "nonce", "timestamp", "delegation"} {
		if fields.Has(key) {
			writeError(w, bad("Signed calls are JSON commands to POST /v1/command; "+services.CallPathPrefix+" takes calls without a key."))
			return
		}
	}
	if f := fields["format"]; len(f) > 1 || len(f) == 1 && f[0] != "json" {
		writeError(w, bad("The answer is JSON; format may only be json."))
		return
	}
	data, requestID, err := services.CallData(m, fields, "format")
	if err != nil {
		writeError(w, bad(strings.TrimPrefix(err.Error(), services.ErrCallArgs.Error()+": ")+". "+usage))
		return
	}
	c := board.Command{Operation: m.Operation, Target: e.ID, Data: data, RequestID: requestID}
	if !m.Write() {
		if m.Signed || requestID != "" {
			writeError(w, bad("This read needs a key, or takes no request_id; use POST /v1/command."))
			return
		}
		c.RequestID = ""
	}
	// Never index a call's answer, and always answer in JSON.
	r.Header.Set("Accept", "application/json")
	s.execute(w, withVia(r, strings.ToLower(r.Method)), c)
}

// unsignedCall reports whether c is a service.call without a key: billed to
// the caller's network.
func unsignedCall(c board.Command) bool {
	return c.Operation == "service.call" && c.PublicKey == "" && c.Signature == ""
}

// crossSiteCall refuses a request a browser made for a page of another site
// (security review 1.21, M1): an unsigned service.call spends the network's
// free credit, so an <img>, a form, sendBeacon or a fetch from another site
// must not spend it for its visitors. A browser says where a request comes
// from in Sec-Fetch-Site, which only same-origin and none (typed or
// bookmarked) pass, whatever the mode; an older browser without Sec-Fetch
// headers is refused when it sends an Origin that is not this site's.
// Agents, curl and HTTP libraries send neither header.
func (s *Server) crossSiteCall(r *http.Request) *board.Error {
	refused := &board.Error{Status: 403, Code: "invalid_origin", Message: "A service call without a key spends your network's free credit, so it is not accepted from a web page on another site. Call it from a server or an agent (curl, an HTTP library, MCP or TCP), not from a browser page."}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site == "same-origin" || site == "none" {
			return nil
		}
		return refused
	}
	if origin := r.Header.Get("Origin"); origin != "" && !s.ownOrigin(r, origin) {
		return refused
	}
	return nil
}

// ownOrigin reports whether origin is this site: the public URL's, or the
// host the request was sent to.
func (s *Server) ownOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	primary, _ := url.Parse(s.cfg.PublicURL)
	return primary != nil && u.Scheme == primary.Scheme && strings.EqualFold(u.Host, primary.Host) || strings.EqualFold(u.Host, r.Host)
}

// noKeyTTL is how long the discovery surfaces reuse what they read of the
// no-key offer and the free credit offer (discoveryCache), and
// discoveryReadTimeout how long one read may take.
const (
	noKeyTTL             = time.Minute
	discoveryReadTimeout = 2 * time.Second
)

// discoveryOffers is what the discovery surfaces (/capabilities, /llms.txt,
// the hosted MCP server) say about free credit: the store's no-key offer and
// its free credit offer.
type discoveryOffers struct {
	noKey   services.NoKey
	noKeyOK bool
	credit  *board.FreeCredit
}

// discoveryCache keeps discoveryOffers for noKeyTTL, read with a
// discoveryReadTimeout (security review 1.21, L4): a discovery request never
// waits on the database for long, and a read that fails or times out keeps
// the last good answer instead of flapping to "nothing offered".
type discoveryCache struct {
	mu   sync.Mutex
	at   time.Time
	good bool
	val  discoveryOffers
}

// offers is the cached discoveryOffers, read again after noKeyTTL.
func (s *Server) offers() discoveryOffers {
	c := &s.offerCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < noKeyTTL {
		return c.val
	}
	ctx, cancel := context.WithTimeout(context.Background(), discoveryReadTimeout)
	defer cancel()
	var v discoveryOffers
	v.credit = web.FreeCreditFor(ctx, s.service)
	if store, ok := s.service.(interface {
		NoKey(context.Context) services.NoKey
	}); ok && len(s.cfg.Features.Services) > 0 {
		v.noKey, v.noKeyOK = store.NoKey(ctx), true
	}
	c.at = time.Now()
	if ctx.Err() != nil && c.good {
		// Timed out: keep the last good answer for another minute.
		return c.val
	}
	c.val, c.good = v, ctx.Err() == nil
	return v
}

// noKey is what an agent without a key can call today, with the example as
// a full URL on this deployment (cached, see offers).
func (s *Server) noKey() (services.NoKey, bool) {
	o := s.offers()
	n := o.noKey
	if n.Example != "" {
		n.Example = s.cfg.PublicURL + n.Example
	}
	return n, o.noKeyOK
}

// memoryGetCommand is the unsigned service.read of one agent's public item.
func memoryGetCommand(agent, key string) board.Command {
	data, _ := json.Marshal(map[string]any{"schema": 1, "method": "get", "args": map[string]string{"agent": agent, "key": key}})
	return board.Command{Operation: "service.read", Target: "memory", Data: string(data)}
}

// servicesCapabilities is the /capabilities "services" object; nil omits it.
// entries is the catalogue (services.list), each entry with its examples on
// every wire: the same values /for-agents and /llms.txt render.
func (s *Server) servicesCapabilities(catalog []services.Entry) map[string]any {
	f := s.cfg.Features
	if len(f.Services) == 0 {
		return nil
	}
	caps := map[string]any{
		"enabled": f.Services, "call": "service.call", "read": "service.read", "list": "services.list", "catalogue": "/api/services",
		"data":   `{"schema":1,"method":METHOD,"args":{...},"max_cost":N}; max_cost only on service.call, which is refused with price_exceeds_max, spending nothing, when the current price is higher`,
		"status": `service.read {"schema":1,"method":"status","args":{"call":CALL_ID}} reads a remote or async call you made`,
		// inference (its configured upstreams), public_data (its catalogue's
		// fixed hosts), x402 (allowlisted resources) and runs (its loader)
		// call out; memory, wakeup, notary and echo never do.
		"network": f.ServiceEnabled("inference") || f.ServiceEnabled("public_data") || f.ServiceEnabled("x402") || f.ServiceEnabled("runs"),
		"entries": s.catalogWithExamples(catalog),
	}
	noKey, _ := s.noKey()
	if f.ServiceEnabled("public_data") {
		caps["public_data"] = map[string]any{
			"methods": []string{"fetch", "bulk", "datasets"}, "bulk_maximum": services.PublicDataBulkMax,
			"catalogue":   `service.read public_data {"schema":1,"method":"datasets"}`,
			"rate_limits": "per caller, by tier; see the datasets read",
		}
	}
	if f.ServiceEnabled("memory") {
		caps["memory"] = map[string]any{
			"e2ee": false, "ttl": false, "private_by_default": true, "server_readable": true,
			"public_read": "/api/memory/AGENT/KEY", "methods": []string{"put", "delete", "get", "list"},
			"key_bytes": board.MemoryKeyBytes, "key_pattern": "letters, digits, . _ / -; no .., no empty or . segments",
			"value_bytes": board.MemoryValueBytes, "keys": board.MemoryKeysMax, "bytes": board.MemoryBytesMax,
			"list_page_maximum": board.MemoryListPageMax, "reads_per_minute": board.MemoryReadsPerMinute,
			"resource": "memory_bytes",
		}
	}
	if len(noKey.Methods) > 0 {
		caps["without_key"] = noKey
	}
	if !noKey.Available {
		for _, e := range caps["entries"].([]services.Entry) {
			if ex, ok := e.Extra["examples"].(web.Examples); ok {
				ex.NoKey = ""
				e.Extra["examples"] = ex
			}
		}
	}
	providerCapabilities(caps, f)
	return caps
}

// catalogWithExamples adds each entry's examples (web.ServiceExamples) to
// its JSON object.
func (s *Server) catalogWithExamples(catalog []services.Entry) []services.Entry {
	out := make([]services.Entry, 0, len(catalog))
	for _, e := range catalog {
		extra := map[string]any{"examples": web.ServiceExamples(s.cfg.PublicURL, e)}
		for k, v := range e.Extra {
			extra[k] = v
		}
		e.Extra = extra
		out = append(out, e)
	}
	return out
}

// addServicesOpenAPI documents the enabled services in /openapi.json, from
// the catalogue: GET /api/services, the services' own GET routes, and
// ServiceData, the data string of service.call and service.read, one schema
// per method with its documented arguments.
func addServicesOpenAPI(paths, schemas, commandProps map[string]any, catalog []services.Entry, response map[string]any) {
	ids := make([]string, 0, len(catalog))
	variants := []any{}
	for _, e := range catalog {
		ids = append(ids, e.ID)
		for _, m := range e.Methods {
			required := []string{"schema", "method"}
			props := map[string]any{"schema": map[string]any{"const": 1}, "method": map[string]any{"const": m.Name}, "args": argsSchema(m.Args)}
			if m.Write() {
				required = append(required, "max_cost")
				props["max_cost"] = map[string]any{"type": "integer", "minimum": 0, "description": "your ceiling; a higher current price is refused and nothing is spent"}
			}
			variants = append(variants, map[string]any{"title": e.ID + "." + m.Name, "description": m.Access() + " target " + e.ID + ". " + m.Line + " Price: " + m.PriceText() + ".",
				"type": "object", "required": required, "properties": props, "additionalProperties": false})
		}
	}
	schemas["ServiceData"] = map[string]any{"description": "The data of service.call and service.read, as a JSON string; target names the service. Each variant is one method of the catalogue at /api/services.", "oneOf": variants}
	commandProps["data"] = map[string]any{"type": "string", "description": "Operation data as a JSON string. For service.call and service.read it is components/schemas/ServiceData."}
	get := func(summary string, params ...map[string]any) map[string]any {
		op := map[string]any{"summary": summary, "responses": response}
		if len(params) > 0 {
			op["parameters"] = params
		}
		return map[string]any{"get": op}
	}
	pathParam := func(name, pattern string) map[string]any {
		schema := map[string]any{"type": "string"}
		if pattern != "" {
			schema["pattern"] = pattern
		}
		return map[string]any{"name": name, "in": "path", "required": true, "schema": schema}
	}
	paths["/api/services"] = get("The service catalogue (services.list): " + strings.Join(ids, ", ") + ", each with its methods, current prices, arguments, limits and an example.")
	if methods := services.AnonymousMethods(catalog); len(methods) > 0 {
		query := func(name, note string) map[string]any {
			return map[string]any{"name": name, "in": "query", "required": false, "schema": map[string]any{"type": "string"}, "description": note}
		}
		op := get("A service call without a key, as one URL: the method's arguments as query fields (strings as text, numbers and booleans as literals, objects and arrays as JSON), max_cost and request_id. Methods: "+strings.Join(methods, ", ")+"; public reads also work. Billed to your network's free daily credit; the answer is the same JSON as POST /v1/command.",
			pathParam("service", ""), pathParam("method", ""), query("max_cost", "your ceiling, in the method's resource; required for a call"), query("request_id", "required for a call; an exact retry returns the first answer and is never charged twice"))
		post := map[string]any{}
		for k, v := range op["get"].(map[string]any) {
			post[k] = v
		}
		op["post"] = post
		paths[services.CallPathPrefix+"{service}/{method}"] = op
	}
	for _, e := range catalog {
		switch e.ID {
		case "memory":
			paths["/api/memory/{agent}/{key}"] = get("Read one public memory item (memory get, unsigned).", pathParam("agent", "^[a-f0-9]{64}$"), pathParam("key", ""))
		case "notary":
			paths["/api/notary/key"] = get("The notary's public key (notary key).")
			paths["/api/notary/{hash}"] = get("The notary receipt for a SHA-256 hash (notary get).", pathParam("hash", "^[a-f0-9]{64}$"))
		}
	}
}
