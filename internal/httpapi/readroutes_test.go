package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// readRouteFeatures turns on every feature a read route of the table
// depends on, so the rig serves (and /openapi.json declares) them all.
var readRouteFeatures = board.Features{Services: []string{"notary", "memory"}, Ledger: board.LedgerOn, AnonPrefix: true,
	Trust: board.TrustOn, Reviews: true, Moderation: true, VoteRecords: true, ExportEndorsements: true}

// recordingStore is the real store, recording each command it executes so
// a drift test can ask which operation a route ran (C397).
type recordingStore struct {
	*board.Store
	mu       sync.Mutex
	commands []board.Command
}

func (r *recordingStore) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	r.mu.Lock()
	r.commands = append(r.commands, c)
	r.mu.Unlock()
	return r.Store.Execute(ctx, c, peer)
}

// last is the command the latest request ran, if it ran one since mark.
func (r *recordingStore) since(mark int) (board.Command, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.commands) <= mark {
		return board.Command{}, false
	}
	return r.commands[len(r.commands)-1], true
}

func (r *recordingStore) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.commands)
}

// readRig is a real store with the transparency log signed, the notary and
// memory services and every feature of readRouteFeatures on: every route of
// the table is served.
type readRig struct {
	s     *Server
	store *recordingStore
	n     int
}

func newReadRig(t *testing.T) *readRig {
	t.Helper()
	store, base := anonCallServerWith(t, 2000, readRouteFeatures)
	if _, err := store.SignCheckpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec := &recordingStore{Store: store}
	return &readRig{s: New(rec, nil, base.cfg), store: rec}
}

// get answers path; a stream's request ends after a moment.
func (g *readRig) get(path string) (int, string) {
	// One address per request: the network rate limit is not under test.
	g.n++
	r := httptest.NewRequest("GET", "https://swarmmemo.com"+path, nil)
	r.RemoteAddr = fmt.Sprintf("198.51.%d.%d:1234", g.n/250, g.n%250+1)
	if strings.HasPrefix(path, "/api/stream") {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	g.s.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func readRouteRig(t *testing.T) (get func(path string) (int, string)) {
	t.Helper()
	return newReadRig(t).get
}

// probe is the route's URL with every path parameter and every query
// parameter that has an example filled in, taking only the first of each
// exactly-one-of group, minus the names in drop and plus extra.
func (rt readRoute) probe(drop []string, extra url.Values) string {
	path := rt.Path
	q := url.Values{}
	grouped := map[string]bool{}
	for _, g := range rt.OneOf {
		for _, name := range g[1:] {
			grouped[name] = true
		}
	}
	for _, p := range rt.Params {
		switch {
		case p.In == "path":
			path = strings.Replace(path, "{"+p.Name+"}", url.PathEscape(p.Example), 1)
		case p.Example != "" && !grouped[p.Name] && !slices.Contains(drop, p.Name):
			q.Set(p.Name, p.Example)
		}
	}
	for k, v := range extra {
		q[k] = v
	}
	if len(q) > 0 {
		return path + "?" + q.Encode()
	}
	return path
}

// TestReadRoutesMatchHandlers asks each table route's handler what the
// table says (C389): a full probe answers 404 exactly when the table lists
// 404, else 200; dropping a required query parameter, or every member of an
// exactly-one-of group, is a 400, as is giving two members; a strict route
// refuses an unknown parameter and accepts each declared one.
func TestReadRoutesMatchHandlers(t *testing.T) {
	rig := newReadRig(t)
	get := rig.get
	for _, rt := range readRoutes() {
		if !rig.s.routeFeatureOn(rt.Feature) {
			t.Fatalf("%s: the rig leaves its feature %s off", rt.Path, rt.Feature)
		}
		t.Run(rt.Path, func(t *testing.T) {
			_, has404 := rt.Errors[404]
			code, body := get(rt.probe(nil, nil))
			if want := map[bool]int{true: 404, false: 200}[has404]; code != want {
				t.Fatalf("%s: %d, the table says %d: %s", rt.probe(nil, nil), code, want, body)
			}
			for _, p := range rt.Params {
				if p.In != "query" || !p.Required {
					continue
				}
				u := rt.probe([]string{p.Name}, nil)
				if code, body := get(u); code != 400 {
					t.Errorf("%s without required %s: %d %s", u, p.Name, code, body)
				}
			}
			for _, g := range rt.OneOf {
				u := rt.probe(g, nil)
				if code, body := get(u); code != 400 {
					t.Errorf("%s with none of %v: %d %s", u, g, code, body)
				}
				ex := ""
				for _, p := range rt.Params {
					if p.Name == g[1] {
						ex = p.Example
					}
				}
				u = rt.probe(nil, url.Values{g[1]: {ex}})
				if code, body := get(u); code != 400 {
					t.Errorf("%s with two of %v: %d %s", u, g, code, body)
				}
			}
			if !rt.Strict {
				return
			}
			u := rt.probe(nil, url.Values{"zz_unknown": {"1"}})
			if code, body := get(u); code != 400 {
				t.Errorf("%s: an unknown parameter answered %d %s", u, code, body)
			}
			for _, p := range rt.Params {
				if p.In != "query" || p.Example == "" {
					continue
				}
				u := rt.Path + "?" + url.Values{p.Name: {p.Example}}.Encode()
				for _, pp := range rt.Params {
					if pp.In == "path" {
						u = strings.Replace(u, "{"+pp.Name+"}", url.PathEscape(pp.Example), 1)
					}
				}
				if _, body := get(u); strings.Contains(body, "Unknown query parameter") || strings.Contains(body, "takes no query string") {
					t.Errorf("%s: the declared parameter %s is refused: %s", u, p.Name, body)
				}
			}
		})
	}
}

// TestReadRoutesOpenAPI: /openapi.json carries each table route as the
// table says: its parameters, and exactly 200, its errors and 429.
func TestReadRoutesOpenAPI(t *testing.T) {
	rig := newReadRig(t)
	get := rig.get
	code, raw := get("/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || code != 200 {
		t.Fatalf("%d %v %.300s", code, err, raw)
	}
	for _, rt := range readRoutes() {
		op, _ := dig(spec, "paths", rt.Path, "get").(map[string]any)
		if op == nil {
			t.Errorf("%s: not in /openapi.json", rt.Path)
			continue
		}
		want := []string{"200", "429"}
		for status := range rt.Errors {
			want = append(want, strconv.Itoa(status))
		}
		got := []string{}
		for status := range op["responses"].(map[string]any) {
			got = append(got, status)
		}
		sort.Strings(want)
		want = slices.Compact(want)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s responses: %v, want %v", rt.Path, got, want)
		}
		params, _ := op["parameters"].([]any)
		declared := rt.params(rig.s.routeFeatureOn)
		if len(params) != len(declared) {
			t.Errorf("%s: %d parameters, the table has %d", rt.Path, len(params), len(declared))
			continue
		}
		for i, p := range declared {
			m := params[i].(map[string]any)
			if m["name"] != p.Name || m["in"] != p.In || (m["required"] == true) != (p.Required || p.In == "path") {
				t.Errorf("%s parameter %d: %v, want %+v", rt.Path, i, m, p)
			}
		}
		for _, g := range rt.OneOf {
			if d, _ := op["description"].(string); !strings.Contains(d, "exactly one of "+strings.Join(g, ", ")) {
				t.Errorf("%s: the description does not say exactly one of %v: %q", rt.Path, g, d)
			}
		}
	}
	// The reported omissions (C389): log query parameters and the service
	// reads' 404.
	for path, names := range map[string][]string{
		"/api/log/proof": {"message", "leaf", "notary", "size"}, "/api/log/promise": {"message", "leaf"},
		"/api/log/consistency": {"from", "to"}, "/api/log/leaves": {"start", "end"}, "/api/log/checkpoint": {"size"},
	} {
		params, _ := dig(spec, "paths", path, "get", "parameters").([]any)
		got := []string{}
		for _, p := range params {
			got = append(got, p.(map[string]any)["name"].(string))
		}
		if !slices.Equal(got, names) {
			t.Errorf("%s parameters %v, want %v", path, got, names)
		}
	}
	for _, path := range []string{"/api/notary/{hash}", "/api/memory/{agent}/{key}"} {
		if dig(spec, "paths", path, "get", "responses", "404") == nil {
			t.Errorf("%s: no 404", path)
		}
	}
}

// TestHandBuiltReadRoutes404 covers the read routes built outside the table
// whose handlers answer 404 for an unknown ID (C390): each declares it, and
// each handler does answer 404 for an ID that names nothing.
func TestHandBuiltReadRoutes404(t *testing.T) {
	get := readRouteRig(t)
	code, raw := get("/openapi.json")
	var spec map[string]any
	if err := json.Unmarshal([]byte(raw), &spec); err != nil || code != 200 {
		t.Fatalf("%d %v %.300s", code, err, raw)
	}
	for path, probe := range map[string]string{
		"/api/work/{message_id}":         "/api/work/" + exampleMessageID,
		"/api/work/{message_id}/history": "/api/work/" + exampleMessageID + "/history",
		"/api/room/{room}":               "/api/room/no-such-room",
		"/api/room/{room}/modlog":        "/api/room/no-such-room/modlog",
	} {
		if dig(spec, "paths", path, "get", "responses", "404") == nil {
			t.Errorf("%s: /openapi.json declares no 404", path)
		}
		if code, body := get(probe); code != 404 {
			t.Errorf("%s: %d, want the 404 /openapi.json declares: %s", probe, code, body)
		}
	}
}

// GET /api/notary/00 names the hash rule (with the prefix form, C912), not
// the service.call envelope.
func TestNotaryMalformedHashMessage(t *testing.T) {
	get := readRouteRig(t)
	code, body := get("/api/notary/00")
	if code != 400 || !strings.Contains(body, "hash must be a SHA-256 (64 hex digits, either case) or a prefix of one of at least 8 hex digits.") || strings.Contains(body, "max_cost") || strings.Contains(body, "strict JSON") {
		t.Fatalf("%d %s", code, body)
	}
}

// C934: a full hash in uppercase is read as the lowercase one (a miss is
// 404, never the 400 the prefix form never got).
func TestNotaryUppercaseHash(t *testing.T) {
	get := readRouteRig(t)
	if code, body := get("/api/notary/" + strings.Repeat("AB", 32)); code != 404 {
		t.Fatalf("%d %s", code, body)
	}
}
