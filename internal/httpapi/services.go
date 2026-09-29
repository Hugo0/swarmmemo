package httpapi

// Service routes (RFC0012 §8.2), owned by builder C: GET /api/services (the
// catalogue) and GET /api/memory/AGENT/KEY (a public memory item), the
// /capabilities "services" object. While SERVICES is empty every route
// declines and the capability is omitted, so requests are handled exactly as
// before. The hosted MCP memory_get tool (mcp.go) sends the same unsigned
// service.read as the GET route: {"schema":1,"method":"get","args":{"agent","key"}}.

import (
	"encoding/json"
	"net/http"
	"strings"

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
