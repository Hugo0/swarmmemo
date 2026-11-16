package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// capabilitiesCompactBudget is the compact /capabilities' ceiling (C1032):
// what an LLM fetcher or the MCP bridge's frame takes in one read, with room.
const capabilitiesCompactBudget = 64 << 10

// The compact document stays under budget with every feature on; past it,
// the failure names the largest inline sections, one of which joins
// linkedCapabilitySections.
func TestCapabilitiesCompactBudget(t *testing.T) {
	full := prodLikeServer().capabilities()
	compact := capabilityJSON(compactCapabilities(full))
	t.Logf("compact /capabilities %d B (full %d B, budget %d B)", len(compact), len(capabilityJSON(full)), capabilitiesCompactBudget)
	if len(compact) <= capabilitiesCompactBudget {
		return
	}
	type section struct {
		name  string
		bytes int
	}
	var inline []section
	for k, v := range full {
		if !slices.Contains(linkedCapabilitySections, k) {
			inline = append(inline, section{k, len(capabilityJSON(v))})
		}
	}
	sort.Slice(inline, func(i, j int) bool { return inline[i].bytes > inline[j].bytes })
	t.Errorf("compact /capabilities is %d B, over %d B; link one of the largest inline sections in linkedCapabilitySections: %v", len(compact), capabilitiesCompactBudget, inline[:min(8, len(inline))])
}

// The compact document is derived from the full one: the same top-level
// keys (plus sections and full), every other key byte-identical, each
// linked section a stub of its scalar children plus more.
func TestCapabilitiesCompactDerived(t *testing.T) {
	full := prodLikeServer().capabilities()
	for _, k := range []string{"sections", "full"} {
		if _, ok := full[k]; ok {
			t.Fatalf("the full document has a top-level %q, which the compact one adds", k)
		}
	}
	compact := compactCapabilities(full)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(capabilityJSON(compact), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(full)+2 {
		t.Errorf("compact has %d top-level keys, full %d (+ sections, full)", len(got), len(full))
	}
	var sections map[string]struct {
		URL   string `json:"url"`
		Bytes int    `json:"bytes"`
		ETag  string `json:"etag"`
	}
	if err := json.Unmarshal(got["sections"], &sections); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains([]string{`"` + capabilitiesFullPath + `"`}, string(got["full"])) {
		t.Errorf("full = %s", got["full"])
	}
	for k, v := range full {
		body := capabilityJSON(v)
		if !slices.Contains(linkedCapabilitySections, k) {
			if !bytes.Equal(bytes.TrimSpace(body), got[k]) {
				t.Errorf("inline %s differs from the full document", k)
			}
			continue
		}
		var children, stub map[string]json.RawMessage
		if json.Unmarshal(body, &children) != nil {
			t.Errorf("linked %s is not an object", k)
			continue
		}
		if _, ok := children["more"]; ok {
			t.Errorf("linked %s has its own more", k)
		}
		if err := json.Unmarshal(got[k], &stub); err != nil {
			t.Fatalf("%s stub: %v", k, err)
		}
		if string(stub["more"]) != `"`+capabilitySectionPrefix+k+`"` {
			t.Errorf("%s stub more = %s", k, stub["more"])
		}
		for c, raw := range stub {
			if c != "more" && (!bytes.Equal(raw, children[c]) || !capabilityScalar(raw)) {
				t.Errorf("%s.%s in the stub is %s, in the section %s", k, c, raw, children[c])
			}
		}
		for c, raw := range children {
			if _, kept := stub[c]; kept != capabilityScalar(raw) {
				t.Errorf("%s.%s: kept %v, scalar %v", k, c, kept, capabilityScalar(raw))
			}
		}
		if s := sections[k]; s.URL != capabilitySectionPrefix+k || s.Bytes != len(body) || s.ETag != capabilityETag(body) {
			t.Errorf("sections.%s = %+v", k, s)
		}
	}
	for k := range sections {
		if _, ok := full[k]; !ok {
			t.Errorf("sections lists %s, which the full document lacks", k)
		}
	}
	if len(sections) < len(linkedCapabilitySections)/2 {
		t.Errorf("only %d of %d linked sections are served with every feature on", len(sections), len(linkedCapabilitySections))
	}
	// In-repo readers' keys stay in the compact document (C1032 design).
	for _, path := range [][]string{{"service_id"}, {"canonical_versions"}, {"delegation", "hosted_key_custody"}, {"private_read_grants"}, {"public_corrections", "url"},
		{"work_coordination", "schema"}, {"work_coordination", "signed_transitions"}, {"work_coordination", "generation_bound"}, {"work_coordination", "automatic_execution"}, {"inbox", "enabled"}} {
		if dig(decodeCapabilities(t, capabilityJSON(compact)), path...) == nil {
			t.Errorf("compact /capabilities lacks %s", strings.Join(path, "."))
		}
	}
}

func decodeCapabilities(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// capabilitiesGet is a GET of path with the given headers (name, value pairs).
func capabilitiesGet(s http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "198.51.100.8:12345"
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// The routes, rendered (a service without Revisions) and kept (the hot
// cache): ?full=1 is the whole document, /capabilities/NAME one key of it,
// each with an ETag that a matching If-None-Match turns into a 304.
func TestCapabilitiesRoutes(t *testing.T) {
	hot := newHotFixture(t)
	for _, tc := range []struct {
		name string
		s    *Server
	}{{"rendered", prodLikeServer()}, {"kept", hot.s}} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			live := s.capabilitiesWith(s.liveCatalog(httptest.NewRequest("GET", capabilitiesPath, nil)))
			full := capabilityJSON(live)
			whole := capabilitiesGet(s, capabilitiesFullPath)
			if whole.Code != 200 || !bytes.Equal(whole.Body.Bytes(), full) {
				t.Fatalf("?full=1: %d, equal %v", whole.Code, bytes.Equal(whole.Body.Bytes(), full))
			}
			compact := capabilitiesGet(s, capabilitiesPath)
			if compact.Code != 200 || !bytes.Equal(compact.Body.Bytes(), capabilityJSON(compactCapabilities(live))) {
				t.Fatalf("/capabilities: %d, not the compact document", compact.Code)
			}
			for _, w := range []*httptest.ResponseRecorder{whole, compact} {
				if w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") != capabilityETag(w.Body.Bytes()) || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
					t.Errorf("headers %v", w.Header())
				}
			}
			var caps map[string]json.RawMessage
			_ = json.Unmarshal(full, &caps)
			for _, name := range []string{"limits", "trust", "services", "service_id"} {
				one := capabilitiesGet(s, capabilitySectionPrefix+name)
				if _, ok := caps[name]; !ok {
					continue
				}
				if one.Code != 200 || !bytes.Equal(bytes.TrimSpace(one.Body.Bytes()), caps[name]) {
					t.Errorf("/capabilities/%s: %d", name, one.Code)
				}
				etag := one.Header().Get("ETag")
				again := capabilitiesGet(s, capabilitySectionPrefix+name)
				if again.Header().Get("ETag") != etag {
					t.Errorf("%s ETag moved: %s, %s", name, etag, again.Header().Get("ETag"))
				}
				nm := capabilitiesGet(s, capabilitySectionPrefix+name, "If-None-Match", etag)
				if nm.Code != http.StatusNotModified || nm.Body.Len() != 0 || nm.Header().Get("ETag") != etag {
					t.Errorf("%s If-None-Match: %d, %d B", name, nm.Code, nm.Body.Len())
				}
			}
			nm := capabilitiesGet(s, capabilitiesPath, "If-None-Match", `"stale", `+compact.Header().Get("ETag"))
			if nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
				t.Errorf("/capabilities If-None-Match: %d", nm.Code)
			}
			if w := capabilitiesGet(s, capabilitiesPath, "If-None-Match", `"stale"`); w.Code != 200 || w.Body.Len() == 0 {
				t.Errorf("a stale If-None-Match: %d", w.Code)
			}
			missing := capabilitiesGet(s, capabilitySectionPrefix+"no_such_section")
			if missing.Code != 404 || !strings.Contains(missing.Body.String(), `"not_found"`) || !strings.Contains(missing.Body.String(), `"sections"`) {
				t.Errorf("unknown section: %d %s", missing.Code, missing.Body.String())
			}
			if w := makeRequest(s, "POST", capabilitySectionPrefix+"limits", "", ""); w.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST a section: %d", w.Code)
			}
			if w := makeRequest(s, "HEAD", capabilitySectionPrefix+"limits", "", ""); w.Code != 200 || w.Header().Get("ETag") == "" {
				t.Errorf("HEAD a section: %d", w.Code)
			}
		})
	}
	// The kept full document and the compact one are kept apart.
	before := hot.store.count("services.list")
	capabilitiesGet(hot.s, capabilitiesFullPath)
	capabilitiesGet(hot.s, capabilitiesPath)
	if hot.store.count("services.list") != before {
		t.Errorf("a second read of ?full=1 or /capabilities rendered again")
	}
}

// Every "/capabilities NAME[.x.y]" pointer in the docs and served
// instructions names a top-level key, which the compact document keeps (as
// itself or as a stub whose more is /capabilities/NAME).
func TestCapabilitiesPointersResolve(t *testing.T) {
	s := prodLikeServer()
	full := s.capabilities()
	texts := map[string]string{
		"/llms.txt":      makeRequest(s, "GET", "/llms.txt", "", "").Body.String(),
		"/llms-full.txt": makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String(),
	}
	for _, f := range []string{"../../docs/PROTOCOL.md", "../../docs/GLOSSARY.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		texts[f] = string(b)
	}
	// A key written as code (`/capabilities` `trust`, /capabilities `funnel`,
	// /capabilities trust.layer): words after a bare /capabilities are prose.
	pointer := regexp.MustCompile("/capabilities`? `([a-z][a-z0-9_]*)[.`]|/capabilities ([a-z][a-z0-9]*_[a-z0-9_]*|[a-z][a-z0-9_]*\\.[a-z])")
	// Keys a configuration the fixture does not run serves.
	configured := map[string]bool{"transports": true, "reviews": true, "find": true, "offerings": true, "subjects": true, "allowance": true,
		"hosted": true, "moderation": true, "memory": true, "notary": true, "public_data": true, "tools": true, "events": true, "extensions": true,
		"stats": true, "free_credit": true, "images": true, "wakeup": true, "without_key": true, "key_id": true, "verifier_key": true, "a2a_agent_card_keys": true, "levers": true}
	seen := 0
	for where, text := range texts {
		for _, m := range pointer.FindAllStringSubmatch(text, -1) {
			name := m[1]
			if name == "" {
				name, _, _ = strings.Cut(m[2], ".")
			}
			seen++
			if _, ok := full[name]; !ok && !configured[name] {
				t.Errorf("%s: %q names no top-level /capabilities key", where, m[0])
			}
		}
	}
	if seen == 0 {
		t.Error("no /capabilities pointer found; the pattern no longer matches the docs")
	}
}
