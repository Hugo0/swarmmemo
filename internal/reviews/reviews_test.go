package reviews

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/rubric"
)

var opt = Options{OwnHosts: []string{"swarmmemo.com"}}

func TestResolveCanonicalForms(t *testing.T) {
	for _, c := range []struct{ in, uri string }{
		{"x402:GET https://API.Example.com:443/v1/./search?q=1#frag", "x402:GET https://api.example.com/v1/search"},
		{"post https://api.example.com/v1/%7euser/a%2fb/", "x402:POST https://api.example.com/v1/~user/a%2Fb/"},
		{"GET https://api.example.com:8443", "x402:GET https://api.example.com:8443/"},
		{"https://Shop.Example.co.uk/deep/page?x=1", "site:https://shop.example.co.uk/"},
		{"domain:API.shop.example.co.uk", "domain:example.co.uk"},
		{"docs.foo.supabase.co", "domain:foo.supabase.co"},
		{"site:https://example.com", "site:https://example.com/"},
		{"mcp:https://mcp.example.com/sse/", "mcp:https://mcp.example.com/sse/"},
		{"mcp:io.GitHub.Owner/Server", "mcp:io.github.owner/server"},
		{strings.Repeat("ab", 32), "agent:" + strings.Repeat("ab", 32)},
		{"pkg:npm/@Scope/Name@1.2.3?x=y#sub", "pkg:npm/%40scope/name"},
		{"pkg:npm/%40scope/name", "pkg:npm/%40scope/name"},
		{"pkg:pypi/Zope.Interface_Foo@5", "pkg:pypi/zope-interface-foo"},
		{"pkg:github/Hugo0/SwarmMemo", "pkg:github/hugo0/swarmmemo"},
		{"pkg:golang/github.com/Foo/bar@v1", "pkg:golang/github.com/Foo/bar"},
	} {
		s, err := Resolve(c.in, opt)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if s.URI() != c.uri {
			t.Errorf("%q: got %q, want %q", c.in, s.URI(), c.uri)
		}
		again, err := Resolve(s.URI(), opt)
		if err != nil || again != s {
			t.Errorf("%q: not idempotent: %v %v", c.in, again, err)
		}
		if !ValidID(s.ID) || s.ID != SubjectID(s.Type, s.Key) {
			t.Errorf("%q: id %q", c.in, s.ID)
		}
	}
}

func TestResolveRefusals(t *testing.T) {
	for _, c := range []struct{ in, code string }{
		{"mailto:someone@example.com", "subject_refused"},
		{"someone@example.com", "subject_refused"},
		{"tel:+15550100", "subject_refused"},
		{"https://www.linkedin.com/in/someone", "invalid_subject"}, // site: the path is dropped, but…
		{"x402:GET https://social.example.com/@someone", "subject_refused"},
		{"x402:GET https://social.example.com/in/someone", "subject_refused"},
		{"x402:GET https://example.com/~bob/api", "subject_refused"},
		{"mcp:https://example.com/users/bob", "subject_refused"},
		{"x402:GET https://swarmmemo.com/agent/abc", "subject_refused"},
		{"x402:GET https://user:pw@example.com/x", "subject_refused"},
		{"x402:GET https://127.0.0.1/x", "subject_refused"},
		{"x402:GET https://[::1]/x", "subject_refused"},
		{"domain:printer.local", "subject_refused"},
		{"domain:foo.onion", "subject_refused"},
		{"domain:localhost", "invalid_subject"},
		{"x402:GET http://example.com/x", "invalid_subject"},
		{"x402:TRACE https://example.com/x", "invalid_subject"},
		{"x402:GET https://exa_mple.com/x", "invalid_subject"},
		{"x402:GET https://example.com:0/x", "invalid_subject"},
		{"x402:GET https://example.com:99999/x", "invalid_subject"},
		{"place:somewhere", "invalid_subject"},
		{"agent:ABC", "invalid_subject"},
		{"pkg:npm", "invalid_subject"},
		{"site:https://bücher.example/", "invalid_subject"},
		{"", "invalid_subject"},
	} {
		_, err := Resolve(c.in, opt)
		var e *Error
		if c.in == "https://www.linkedin.com/in/someone" {
			// A site is its origin: the profile path never reaches the key.
			if err != nil {
				t.Errorf("%q: %v", c.in, err)
			}
			continue
		}
		if !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%q: got %v, want %s", c.in, err, c.code)
		}
	}
	if _, err := Canonical("place", "x", opt); err == nil {
		t.Fatal("place is deferred")
	}
}

// Big social sites name people by their own path rules (security review
// C186 L2): any path on x.com and the like, a single segment on the code
// hosts; a repository (OWNER/REPO) stays a subject.
func TestResolveSocialHostPeople(t *testing.T) {
	for _, in := range []string{
		"x402:GET https://x.com/someone", "x402:GET https://twitter.com/someone/status/1", "mcp:https://mobile.twitter.com/someone",
		"x402:GET https://www.linkedin.com/company/acme", "x402:GET https://facebook.com/someone", "mcp:https://m.facebook.com/someone/",
		"x402:GET https://www.instagram.com/someone/", "x402:GET https://www.tiktok.com/someone", "x402:GET https://x.com:8443/someone",
		"x402:GET https://github.com/someone", "mcp:https://github.com/someone/", "x402:GET https://github.com/sponsors/someone", "x402:GET https://gitlab.com/someone",
		"x402:GET https://www.youtube.com/@someone", "x402:GET https://youtube.com/channel/UC123", "x402:GET https://youtube.com/c/someone",
		"x402:GET https://www.reddit.com/u/someone", "x402:GET https://reddit.com/user/someone/comments", "x402:GET https://medium.com/@someone/a-post",
	} {
		var e *Error
		if _, err := Resolve(in, opt); !errors.As(err, &e) || e.Code != "subject_refused" {
			t.Errorf("%q: got %v, want subject_refused", in, err)
		}
	}
	for _, in := range []string{
		"mcp:https://github.com/owner/repo", "x402:GET https://github.com/owner/repo/releases", "x402:GET https://www.youtube.com/watch",
		"x402:GET https://reddit.com/r/golang", "x402:GET https://api.x.com/2/tweets", "site:https://x.com/someone", "x402:GET https://medium.com/topics",
	} {
		if _, err := Resolve(in, opt); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestSubjectIDVector(t *testing.T) {
	// The id is base32 of SHA-256 over ["swarmmemo.subject.v1",type,key];
	// any other implementation must reproduce this vector.
	got := SubjectID("x402", "GET https://api.example.com/v1/search")
	if len(got) != 26 || !ValidID(got) {
		t.Fatalf("id %q", got)
	}
	if got != SubjectID("x402", "GET https://api.example.com/v1/search") || got == SubjectID("site", "GET https://api.example.com/v1/search") {
		t.Fatal("ids must be stable and typed")
	}
	s, _ := Resolve("x402:GET https://api.example.com/v1/search", opt)
	p, ok := s.Parent()
	if !ok || p.URI() != "domain:example.com" || s.Host() != "api.example.com" {
		t.Fatalf("parent %v %v host %q", p, ok, s.Host())
	}
}

func x402Rubric(t *testing.T) *Rubric {
	r := LookupRubric("x402-endpoint", "")
	if r == nil {
		t.Fatal("x402-endpoint rubric missing")
	}
	return r
}

func reviewJSON(t *testing.T, fields map[string]any) []byte {
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseReview(t *testing.T) {
	r := x402Rubric(t)
	subject := "x402:GET https://api.example.com/v1/search"
	rubric := map[string]any{"id": r.ID, "version": r.Version, "sha256": r.SHA256}
	ok := []map[string]any{
		{"subject": subject, "verdict": "good"},
		{"subject": subject, "verdict": "mixed", "dimensions": map[string]any{"works": 90, "safe": 100}, "relationship": "customer", "evidence": []string{"call:" + strings.Repeat("a", 32)}},
		{"subject": subject, "verdict": "good", "relationship": "lab", "rubric": rubric, "score": 84, "band": []int{78, 89}, "grade": "B", "flags": []string{"injection"},
			"evidence":     []string{"call:" + strings.Repeat("a", 32), "stamp:" + strings.Repeat("b", 64)},
			"observations": []any{map[string]any{"check": "R4", "pass": 3, "fail": 0, "evidence": []int{0}}, map[string]any{"check": "R3", "value": 420}, map[string]any{"check": "H4", "na": true}},
			"record":       map[string]any{"sha256": strings.Repeat("c", 64), "doc": strings.Repeat("d", 32)}},
		{"subject": subject, "verdict": "good", "relationship": "outside_source", "evidence": []string{"fetch:" + strings.Repeat("e", 32)},
			"source": map[string]any{"name": "x402 Trust", "url": "https://x402trust.example/grade", "terms": "public grade, attribution", "grade": "A"}},
		{"subject": subject, "verdict": "unverified", "evidence": []string{"tx:eip155:8453:0x" + strings.Repeat("f", 64)}},
	}
	for i, f := range ok {
		if _, err := Parse(reviewJSON(t, f), opt); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	bad := []string{
		`{"subject":"x402:GET https://api.example.com/v1/search"}`,
		`{"subject":"x402:get https://API.example.com/v1/search","verdict":"good"}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"great"}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","verdict":"bad"}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","rating":4}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","dimensions":{"works":101}}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","dimensions":{"speed":50}}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","dimensions":{"works":5.5}}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","dimensions":null}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","score":80}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","relationship":"lab"}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","evidence":["call:xyz"]}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","evidence":["call:` + strings.Repeat("a", 32) + `","call:` + strings.Repeat("a", 32) + `"]}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","observations":[{"check":"R4","pass":1}]}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","relationship":"outside_source","source":{"name":"X","url":"https://x.example/","grade":"A"}}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","source":{"name":"X","url":"https://x.example/","grade":"A"}}`,
		`{"subject":"domain:example.com","verdict":"good","rubric":{"id":"x402-endpoint","version":"` + r.Version + `","sha256":"` + r.SHA256 + `"}}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","rubric":{"id":"x402-endpoint","version":"` + r.Version + `","sha256":"` + strings.Repeat("0", 64) + `"}}`,
		`{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","rubric":{"id":"x402-endpoint","version":"9.0.0","sha256":"` + r.SHA256 + `"}}`,
		`[]`, `"x"`, `{}`, `{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good"} {}`,
	}
	for _, raw := range bad {
		if _, err := Parse([]byte(raw), opt); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	labBad := func(mut func(map[string]any)) {
		f := map[string]any{"subject": subject, "verdict": "good", "relationship": "lab", "rubric": rubric, "score": 84, "band": []int{78, 89}, "evidence": []string{"call:" + strings.Repeat("a", 32)}}
		mut(f)
		if _, err := Parse(reviewJSON(t, f), opt); err == nil {
			t.Errorf("accepted lab %v", f)
		}
	}
	labBad(func(f map[string]any) { f["band"] = []int{85, 89} })
	labBad(func(f map[string]any) { f["grade"] = "E+" })
	labBad(func(f map[string]any) { f["flags"] = []string{"scam"} })
	labBad(func(f map[string]any) { f["observations"] = []any{map[string]any{"check": "Z9", "pass": 1}} })
	labBad(func(f map[string]any) { f["observations"] = []any{map[string]any{"check": "R3", "pass": 1}} })
	labBad(func(f map[string]any) { f["observations"] = []any{map[string]any{"check": "R4", "value": 1}} })
	labBad(func(f map[string]any) {
		f["observations"] = []any{map[string]any{"check": "R4", "pass": 1, "evidence": []int{1}}}
	})
	labBad(func(f map[string]any) {
		f["observations"] = []any{map[string]any{"check": "R4", "pass": 1}, map[string]any{"check": "R4", "fail": 1}}
	})
	labBad(func(f map[string]any) { f["observations"] = []any{map[string]any{"check": "R4", "pass": 0, "fail": 0}} })
	labBad(func(f map[string]any) { f["observations"] = []any{map[string]any{"check": "H4", "na": false}} })
}

func TestRubricFiles(t *testing.T) {
	r := x402Rubric(t)
	if r.Version != "1.0.0-draft.1" || len(r.Checks) != 27 || r.Path() != "/rubrics/x402-endpoint/1.0.0-draft.1.json" || r.Doc.Title != "x402 endpoint" || r.CheckDoc("R6").Validity == "" {
		t.Fatalf("rubric %s %s %d %+v", r.ID, r.Version, len(r.Checks), r.Doc)
	}
	want := map[string]float64{"works": 45, "as_described": 30, "safe": 15, "good": 10}
	for _, d := range r.Dimensions {
		if want[d.ID] != d.Weight {
			t.Fatalf("dimension %s weighs %v", d.ID, d.Weight)
		}
	}
	// Each dimension's weighted checks sum to 100, so a check's weight reads
	// as its share of its dimension.
	sums := map[string]float64{}
	for _, c := range r.Checks {
		sums[c.Dimension] += c.Weight
	}
	for d, s := range sums {
		if s != 100 {
			t.Errorf("checks of %s weigh %v", d, s)
		}
	}
	for _, c := range r.Checks {
		dim := map[byte]string{'R': "works", 'C': "works", 'H': "as_described", 'S': "safe"}[c.ID[0]]
		if c.ID == "R6" {
			dim = "good"
		}
		if c.Dimension != dim {
			t.Errorf("%s is on %s, want %s", c.ID, c.Dimension, dim)
		}
	}
	// A file internal/rubric refuses, or one with a dimension beyond the
	// four, is never served.
	var m map[string]any
	_ = json.Unmarshal(r.Raw, &m)
	m["surprise"] = 1
	raw, _ := json.Marshal(m)
	if _, err := LoadRubric(raw); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	delete(m, "surprise")
	m["dimensions"] = []any{map[string]any{"id": "speed", "weight": 1}}
	m["checks"] = []any{map[string]any{"id": "R1", "dimension": "speed", "weight": 1, "kind": "free", "metric": "rate"}}
	m["gates"], m["verdict"] = []any{}, map[string]any{"probe_check": "R1"}
	raw, _ = json.Marshal(m)
	if _, err := LoadRubric(raw); err == nil || !strings.Contains(err.Error(), "speed") {
		t.Fatalf("a fifth dimension: %v", err)
	}
	// The served bytes score with internal/rubric (one format, one parser).
	obs := `{"schema":"swarmmemo.observations/1","rubric":"x402-endpoint","subject":{"type":"x402","key":"GET https://api.example.com/v1/search"},"as_of":"2026-10-11T12:00:00Z","checks":{"R1":{"events":[{"at":"2026-10-10T12:00:00Z","pass":10,"fail":0}]}}}`
	out, err := rubric.ScoreFiles(r.Raw, []byte(obs))
	if err != nil || !strings.Contains(string(out), `"rubric":{"id":"x402-endpoint","version":"1.0.0-draft.1"`) {
		t.Fatalf("score: %v %s", err, out)
	}
}

func TestReviewByteSizeExample(t *testing.T) {
	// The RFC's full lab example fits the 1,024-byte post data cap.
	r := x402Rubric(t)
	data := `{"schema":1,"review":{"subject":"x402:GET https://api.example.com/v1/search","verdict":"good","dimensions":{"works":88,"as_described":95,"safe":100,"good":70},"relationship":"lab","rubric":{"id":"x402-endpoint","version":"` + r.Version + `","sha256":"` + r.SHA256 + `"},"score":84,"band":[78,89],"grade":"B","evidence":["call:3f2a9c0e5b7d41a2a8e6c1f09d3b7e54","stamp:9b1d4e0c7a2f68e3d5b9c0a14f7e2d6c8b3a5f1e0d9c7b6a4e2f1d0c9b8a7e6f"],"observations":[{"check":"R4","pass":3,"fail":0,"evidence":[0]},{"check":"R3","value":420},{"check":"H4","na":true}],"record":{"sha256":"9b1d4e0c7a2f68e3d5b9c0a14f7e2d6c8b3a5f1e0d9c7b6a4e2f1d0c9b8a7e6f"}}}`
	if len(data) > 1024 {
		t.Fatalf("the example is %d bytes", len(data))
	}
	var outer struct {
		Review json.RawMessage `json:"review"`
	}
	if err := json.Unmarshal([]byte(data), &outer); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(outer.Review, opt); err != nil {
		t.Fatal(err)
	}
	t.Logf("lab example: %d bytes", len(data))
}

// An agent offering on the board's own host (RFC 0017) is the one narrow
// exception to the people rule (C200): POST /@ALIAS/NAME, lower-cased.
// Every other own-host @ path, another method, another host or a deeper
// path stays refused.
func TestOfferingSubjects(t *testing.T) {
	s, err := Resolve("POST https://SwarmMemo.com/@Weaver/Echo-1", opt)
	if err != nil || s.URI() != "x402:POST https://swarmmemo.com/@weaver/echo-1" {
		t.Fatalf("offering: %v %v", s, err)
	}
	if again, err := Resolve(s.URI(), opt); err != nil || again != s {
		t.Fatalf("not idempotent: %v %v", again, err)
	}
	alias, name, ok := OfferingPath(s, opt)
	if !ok || alias != "weaver" || name != "echo-1" {
		t.Fatalf("OfferingPath: %q %q %v", alias, name, ok)
	}
	fp := strings.Repeat("ab", 32)
	if s, err := Resolve("x402:POST https://swarmmemo.com/@"+fp+"/echo", opt); err != nil {
		t.Fatalf("fingerprint alias: %v", err)
	} else if a, _, ok := OfferingPath(s, opt); !ok || a != fp {
		t.Fatalf("fingerprint alias path: %q %v", a, ok)
	}
	for _, in := range []string{
		"x402:GET https://swarmmemo.com/@weaver/echo", "x402:POST https://swarmmemo.com/@weaver", "x402:POST https://swarmmemo.com/@weaver/echo/calls",
		"x402:POST https://swarmmemo.com/%40weaver/echo", "x402:POST https://other.example.com/@weaver/echo", "mcp:https://swarmmemo.com/@weaver/echo",
		"x402:POST https://swarmmemo.com/@we.aver/echo", "x402:POST https://swarmmemo.com/@weaver/echo_x", "x402:POST https://swarmmemo.com/agent/" + fp,
	} {
		var e *Error
		if _, err := Resolve(in, opt); !errors.As(err, &e) || e.Code != "subject_refused" {
			t.Errorf("%q: got %v, want subject_refused", in, err)
		}
	}
	for _, sub := range []Subject{{Type: TypeX402, Key: "POST https://other.example.com/@weaver/echo"}, {Type: TypeX402, Key: "GET https://swarmmemo.com/@weaver/echo"}, {Type: TypeSite, Key: "https://swarmmemo.com/"}} {
		if _, _, ok := OfferingPath(sub, opt); ok {
			t.Errorf("OfferingPath(%v) = ok", sub)
		}
	}
	for id, want := range map[string]bool{"call:oc_" + strings.Repeat("a", 32): true, "call:oc_" + strings.Repeat("a", 31): false, "work:oc_" + strings.Repeat("a", 32): false, "call:" + strings.Repeat("a", 32): true} {
		if ValidEvidenceID(id) != want {
			t.Errorf("ValidEvidenceID(%q) != %v", id, want)
		}
	}
}

// One of SwarmMemo's own tools is a subject (C301): tool:swarmmemo:SERVICE.METHOD,
// the id tools search and Find give it.
func TestToolSubjects(t *testing.T) {
	for _, in := range []string{"tool:swarmmemo:fetch.page", "TOOL:SwarmMemo:Fetch.Page", "swarmmemo:fetch.page", " tool:fetch.page "} {
		s, err := Resolve(in, opt)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if s.Type != TypeTool || s.Key != "swarmmemo:fetch.page" || s.URI() != "tool:swarmmemo:fetch.page" || s.ID != SubjectID(TypeTool, "swarmmemo:fetch.page") {
			t.Fatalf("%q: %+v", in, s)
		}
		if again, err := Resolve(s.URI(), opt); err != nil || again != s {
			t.Fatalf("%q: not idempotent: %v %v", in, again, err)
		}
		if svc, m, ok := s.ToolMethod(); !ok || svc != "fetch" || m != "page" {
			t.Fatalf("ToolMethod: %q %q %v", svc, m, ok)
		}
		if _, ok := s.Parent(); ok || s.Host() != "" || s.Siblings() != nil {
			t.Fatalf("a tool has no host, parent or siblings: %+v", s)
		}
	}
	c, err := Canonical(TypeTool, "swarmmemo:public_data.fetch", opt)
	if err != nil || c.Key != "swarmmemo:public_data.fetch" {
		t.Fatalf("Canonical: %+v %v", c, err)
	}
	for _, in := range []string{"tool:exa_search", "tool:swarmmemo:fetch", "tool:swarmmemo:fetch.page.x", "tool:swarmmemo:.page", "tool:other:fetch.page", "tool:swarmmemo:fe tch.page"} {
		var se *Error
		if _, err := Resolve(in, opt); !errors.As(err, &se) || se.Code != "invalid_subject" {
			t.Errorf("%q: %v", in, err)
		}
	}
	if _, _, ok := (Subject{Type: TypeX402, Key: "GET https://example.com/"}).ToolMethod(); ok {
		t.Fatal("ToolMethod of an x402 subject")
	}
	if !strings.Contains(TypesText(), "tool") {
		t.Fatal(TypesText())
	}
}

// C752: a lab grade shows only while every dimension is measured; else no
// letter and a marker naming the unmeasured dimensions.
func TestShowGrade(t *testing.T) {
	all := map[string]int{"works": 91, "as_described": 88, "safe": 90, "good": 85}
	if g := ShowGrade("B", all); g.Grade != "B" || g.Note != "" || g.Unmeasured != nil {
		t.Fatalf("all measured: %+v", g)
	}
	noGood := map[string]int{"works": 71, "as_described": 67, "safe": 77}
	if g := ShowGrade("D", noGood); g.Grade != "" || g.Note != "no grade (good not measured)" || len(g.Unmeasured) != 1 || g.Unmeasured[0] != "good" {
		t.Fatalf("good unmeasured: %+v", g)
	}
	if g := ShowGrade("F", map[string]int{"works": 50}); g.Note != "no grade (as_described, safe, good not measured)" {
		t.Fatalf("three unmeasured: %+v", g)
	}
	if g := ShowGrade("", noGood); g.Grade != "" || g.Note != "" || g.Unmeasured != nil {
		t.Fatalf("no stored grade shows nothing: %+v", g)
	}
}
