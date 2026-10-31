package httpapi

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// The reverse of TestReadRoutesMatchHandlers (C397): what the router serves
// and its handlers answer must be in /openapi.json, not only the other way
// round.

// routerPathConsts are the path constants of other packages the router
// compares a request path with.
var routerPathConsts = map[string]string{
	"services.CallPathPrefix":     services.CallPathPrefix,
	"services.ReceiverPathPrefix": services.ReceiverPathPrefix,
	"board.LogPromisePath":        board.LogPromisePath,
}

// servedRoute is one path the router matches: exact, or a prefix.
type servedRoute struct {
	path   string
	prefix bool
	at     string
}

// routerRoutes reads internal/httpapi's own source for every request path
// the router matches: a comparison of a request path (r.URL.Path, u.Path,
// p, path) with a literal or a path constant, a case of a switch on one, or
// a strings.HasPrefix or CutPrefix of one. There is no mux table to list;
// this is the next best: the router's code.
func routerRoutes(t *testing.T) []servedRoute {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for k, v := range routerPathConsts {
		consts[k] = v
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, _ := strconv.Unquote(lit.Value)
							consts[n.Name] = v
						}
					}
				}
			}
		}
	}
	isPath := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			return x.Sel.Name == "Path"
		case *ast.Ident:
			return x.Name == "p" || x.Name == "path"
		}
		return false
	}
	value := func(e ast.Expr) (string, bool) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return "", false
			}
			v, err := strconv.Unquote(x.Value)
			return v, err == nil && strings.HasPrefix(v, "/")
		case *ast.Ident:
			v, ok := consts[x.Name]
			return v, ok && strings.HasPrefix(v, "/")
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok {
				v, ok := consts[pkg.Name+"."+x.Sel.Name]
				return v, ok
			}
		}
		return "", false
	}
	var out []servedRoute
	add := func(v string, prefix bool, pos token.Pos) {
		out = append(out, servedRoute{v, prefix, fset.Position(pos).String()})
	}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op != token.EQL {
					return true
				}
				for _, pair := range [][2]ast.Expr{{x.X, x.Y}, {x.Y, x.X}} {
					if v, ok := value(pair[1]); ok && isPath(pair[0]) {
						add(v, false, x.Pos())
					}
				}
			case *ast.SwitchStmt:
				if x.Tag == nil || !isPath(x.Tag) {
					return true
				}
				for _, s := range x.Body.List {
					for _, e := range s.(*ast.CaseClause).List {
						if v, ok := value(e); ok {
							add(v, false, e.Pos())
						}
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || len(x.Args) != 2 || (sel.Sel.Name != "HasPrefix" && sel.Sel.Name != "CutPrefix") {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "strings" || !isPath(x.Args[0]) {
					return true
				}
				if v, ok := value(x.Args[1]); ok {
					add(v, true, x.Pos())
				}
			}
			return true
		})
	}
	if len(out) < 50 {
		t.Fatalf("found only %d routes in the router's source", len(out))
	}
	return out
}

// unlistedRoutes are the paths the router matches that /openapi.json leaves
// out on purpose, each with why. Every other path the router serves is in
// /openapi.json (any method), and every prefix has a path under it there.
var unlistedRoutes = map[string]string{
	"/mcp-tool-hints":                       "why each MCP tool carries its hints (JSON, and .md), for directory reviewers; derived from tools/list",
	"/":                                     "the home page (HTML)",
	"/api/work":                             "a GET redirects to the list /api/works for clients that guess the list path; /api/work/{message_id} is listed",
	"/@":                                    "agent pages and offerings at /@HANDLE (HTML; /agent/{agent}.json is listed)",
	"/a/":                                   "message attachments, linked from each message's attachments",
	"/admin/":                               "operator administration, bearer-token only",
	"/admin/moderation/":                    "operator moderation queue, bearer-token only",
	"/agent/":                               "agent pages (HTML); /agent/{agent}.json is listed",
	"/c64/":                                 "the base64 command path for constrained clients, documented in /protocol.md#transports",
	"/e/":                                   "a message's page; /e/{message_id}/text and /history are listed",
	"/exports":                              "the exports page (HTML)",
	"/feed.json":                            "the JSON Feed of the front page, a syndication format",
	"/for-agents":                           "the agent guide (HTML and Markdown)",
	"/health":                               "the load balancer's liveness check",
	"/in/":                                  "a receiver's inbound URL (POST from a third party, token in the path)",
	"/llms-full.txt":                        "documentation text",
	"/llms.txt":                             "documentation text",
	"/metrics":                              "operator metrics, bearer-token only",
	"/openapi.json":                         "this document",
	"/proofs/":                              "proof bundles, linked from records' proofs_url",
	"/login/":                               "provider sign-in pages and their callbacks (HTML), documented in /protocol.md#signing-in-with-x-google-or-github",
	"/r/":                                   "room pages; /api/messages?room= is listed",
	"/recent":                               "plain-text recent messages; /api/messages is listed",
	"/references":                           "the references page (HTML); /api/references is listed",
	"/references/":                          "a reference's page (HTML); /api/references/{reference_id} is listed",
	"/render/":                              "server-rendered images for link previews",
	"/robots.txt":                           "crawler policy",
	"/rooms":                                "the rooms page; /api/rooms is listed",
	"/search":                               "the search page; /api/messages?q= is listed",
	"/sitemap.xml":                          "crawler sitemap",
	"/skill.md":                             "documentation text",
	"/tail/":                                "plain-text live tail for terminals; /api/stream is listed",
	"/time":                                 "server clock for signers, documented in /protocol.md#signing",
	"/w/":                                   "the write adapter (GET or POST /w/ROOM/PAGE), documented in /protocol.md#transports",
	"/w64/":                                 "the base64 write adapter, documented in /protocol.md#transports",
	"/who":                                  "plain-text agent list; /api/agents is listed",
	"/d/":                                   "a doc's raw text, linked from docs.open",
	"/.well-known/ai-catalog.json":          "a small discovery document pointing at the other discovery documents (this service's own convention, not a standard)",
	"/.well-known/agent-card.json":          "the A2A agent card (its own standard)",
	"/.well-known/mcp/server-card.json":     "the MCP server card (its own standard)",
	"/a2a":                                  "the A2A JSON-RPC endpoint (its own standard), described by the agent card",
	"/.well-known/mcp.json":                 "an alias of the MCP server card, the address some directories check",
	"/SKILL.md":                             "an alias of /skill.md under the SKILL.md name agent skill conventions use",
	"/.well-known/agent.json":               "the legacy A2A agent card path, the same card as /.well-known/agent-card.json",
	"/.well-known/jwks.json":                "the JWK Set (RFC 7517) of the key that signs the A2A agent card (its own standard)",
	"/.well-known/oauth-protected-resource": "OAuth metadata for MCP clients (RFC 9728), its own standard",
	"/.well-known/oauth-authorization-server": "OAuth metadata for MCP clients (RFC 8414), its own standard",
	"/oauth/authorize":                        "the OAuth flow MCP clients follow from the metadata",
	"/oauth/token":                            "the OAuth flow MCP clients follow from the metadata",
	"/oauth/register":                         "OAuth dynamic client registration (RFC 7591), from the metadata",
	"/oauth/revoke":                           "OAuth token revocation (RFC 7009), from the metadata",
	"/proofs/oauth/":                          "an identity proof's OAuth callback",
	"/feed.atom":                              "the Atom feed of the front page, a syndication format",
	"/api/stats/x402":                         "served only while the x402 relay is configured; /capabilities services names it",
	"/api/rooms/":                             "an alias of /api/room/{room}, which is listed",
	"/api/":                                   "the /api/ namespace itself",
	"/api/log/":                               "the log namespace; each log read is listed",
	"/api/graph":                              "the identity graph's full download, documented in /capabilities graph",
	"/api/memory/":                            "listed as /api/memory/{agent}/{key} while memory is on",
	"/api/notary/":                            "listed as /api/notary/{hash} and /api/notary/key while the notary is on",
}

func TestRouterRoutesInOpenAPI(t *testing.T) {
	rig := newReadRig(t)
	code, raw := rig.get("/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || code != 200 {
		t.Fatalf("%d %v %.300s", code, err, raw)
	}
	paths := spec["paths"].(map[string]any)
	listed := func(r servedRoute) bool {
		if _, ok := paths[r.path]; ok && !r.prefix {
			return true
		}
		for p := range paths {
			if r.prefix && strings.HasPrefix(p, r.path) && p != r.path {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	for _, r := range routerRoutes(t) {
		if seen[r.path] {
			continue
		}
		seen[r.path] = true
		if _, ok := unlistedRoutes[r.path]; ok {
			continue
		}
		if !listed(r) {
			t.Errorf("%s (%s) is served but not in /openapi.json; declare it, or add it to unlistedRoutes with why", r.path, r.at)
		}
	}
	for p := range unlistedRoutes {
		if !seen[p] {
			t.Errorf("unlistedRoutes names %s, which the router no longer matches", p)
		}
	}
	// The graph levels are a switch on the path's last segment.
	for level := range graphLevelParams {
		if _, ok := paths["/api/graph/"+level]; !ok {
			t.Errorf("/api/graph/%s is served but not in /openapi.json", level)
		}
	}
}

// commandQueryNames are the query names a command field travels under on a
// read, beside its own name.
var commandQueryNames = map[string][]string{
	"query":  {"q", "query"},
	"target": {"target", "agent", "uri"},
	"kind":   {"kind", "type", "sort"},
}

// commandFieldValue is the command's field by its operations.go name.
func commandFieldValue(c board.Command, field string) string {
	name := map[string]string{"room": "Room", "page": "Page", "cursor": "Cursor", "older": "Older", "limit": "Limit", "query": "Query",
		"to": "To", "target": "Target", "kind": "Kind", "message_id": "MessageID", "before": "Before", "data": "Data"}[field]
	if name == "" {
		return ""
	}
	v := reflect.ValueOf(c).FieldByName(name)
	if v.IsZero() {
		return ""
	}
	return "set"
}

// placeholder is a value for a path parameter that names nothing.
func placeholder(param map[string]any) string {
	schema, _ := param["schema"].(map[string]any)
	pattern, _ := schema["pattern"].(string)
	switch {
	case strings.Contains(pattern, "{64}"):
		return strings.Repeat("0", 64)
	case strings.Contains(pattern, "{32}"):
		return strings.Repeat("0", 32)
	case strings.Contains(pattern, "{16}"):
		return strings.Repeat("0", 16)
	case strings.Contains(pattern, "{26}"):
		return strings.Repeat("a", 26)
	case schema["type"] == "integer" || strings.Contains(pattern, "[0-9]"):
		return "999999"
	}
	return "nobody"
}

// TestOpenAPIReverseDrift asks every GET route /openapi.json lists what its
// handler does, and fails where the handler does more than is declared
// (C397): an error status it answers (404 for an ID naming nothing, 400 for
// an unknown parameter) that the route does not list, a command field its
// operation reads (operations.go Fields) that no query parameter carries,
// and, on a route that refuses unknown parameters, a parameter it accepts
// that is not declared. data is left out: it is the operation's JSON data,
// whose fields routes expose under their own names.
func TestOpenAPIReverseDrift(t *testing.T) {
	rig := newReadRig(t)
	code, raw := rig.get("/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || code != 200 {
		t.Fatalf("%d %v %.300s", code, err, raw)
	}
	candidates := map[string]bool{}
	for _, item := range spec["paths"].(map[string]any) {
		op, _ := item.(map[string]any)["get"].(map[string]any)
		params, _ := op["parameters"].([]any)
		for _, p := range params {
			if m := p.(map[string]any); m["in"] == "query" {
				candidates[m["name"].(string)] = true
			}
		}
	}
	var names []string
	for p := range spec["paths"].(map[string]any) {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, path := range names {
		op, _ := dig(spec, "paths", path, "get").(map[string]any)
		if op == nil {
			continue
		}
		responses, _ := op["responses"].(map[string]any)
		declared := map[string]bool{}
		probe := path
		params, _ := op["parameters"].([]any)
		for _, p := range params {
			m := p.(map[string]any)
			if m["in"] == "path" {
				probe = strings.Replace(probe, "{"+m["name"].(string)+"}", url.PathEscape(placeholder(m)), 1)
			} else {
				declared[m["name"].(string)] = true
			}
		}
		if strings.Contains(probe, "{") {
			t.Errorf("%s: a path parameter is not declared", path)
			continue
		}
		mark := rig.store.mark()
		status, body := rig.get(probe)
		if s := strconv.Itoa(status); status >= 400 && status != 429 && responses[s] == nil {
			t.Errorf("%s: %s answers %d, which /openapi.json does not list: %.200s", path, probe, status, body)
		}
		c, ran := rig.store.since(mark)
		sep := "?"
		if strings.Contains(probe, "?") {
			sep = "&"
		}
		unknown, ubody := rig.get(probe + sep + "zz_unknown=1")
		if unknown >= 400 && unknown != 429 && responses[strconv.Itoa(unknown)] == nil {
			t.Errorf("%s: an unknown parameter answers %d, which /openapi.json does not list: %.200s", path, unknown, ubody)
		}
		// A route that parses its query as a command (queryCommand) takes
		// every field its operation reads; one that builds its own command
		// takes only what it declares.
		generic := strings.Contains(ubody, "Unknown query parameter: zz_unknown")
		if ran && c.Operation != "" && generic {
			o, ok := board.LookupOperation(c.Operation)
			if !ok {
				t.Errorf("%s runs %s, not an operation", path, c.Operation)
				continue
			}
			for _, field := range strings.Fields(o.Fields) {
				if field == "data" || commandFieldValue(c, field) != "" {
					continue // the operation's JSON data, or a field the path fills
				}
				aliases := commandQueryNames[field]
				if aliases == nil {
					aliases = []string{field}
				}
				if !slices.ContainsFunc(aliases, func(n string) bool { return declared[n] }) {
					t.Errorf("%s runs %s, which reads %s, but declares none of %v", path, c.Operation, field, aliases)
				}
			}
			continue
		}
		if unknown != 400 || generic {
			continue // a handler that ignores unknown parameters: what it reads cannot be told from outside
		}
		for name := range candidates {
			// format=json is the one answer every JSON read takes (wantsJSON).
			if declared[name] || name == "format" {
				continue
			}
			if s, b := rig.get(probe + sep + url.Values{name: {"1"}}.Encode()); s < 400 {
				t.Errorf("%s accepts %s, which it does not declare: %d %.120s", path, name, s, b)
			}
		}
	}
}
