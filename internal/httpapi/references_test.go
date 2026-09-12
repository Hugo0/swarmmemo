package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/references"
)

type referenceFixtureReader struct {
	snapshot          *references.Snapshot
	loadErr, fenceErr error
	loads, fences     atomic.Int64
}

func (f *referenceFixtureReader) Load(context.Context) (*references.Snapshot, error) {
	f.loads.Add(1)
	return f.snapshot, f.loadErr
}
func (f *referenceFixtureReader) Fence(context.Context, *references.Snapshot) error {
	f.fences.Add(1)
	return f.fenceErr
}

func referenceFixture(t *testing.T) (*Server, *referenceFixtureReader, *fakeService) {
	t.Helper()
	now := time.Now().Unix()
	var snapshot references.Snapshot
	row := func(id, title, excerpt string) map[string]any {
		return map[string]any{"id": id, "source_id": "fixture", "external_id": id,
			"url": "https://example.org/original/" + id, "title": title, "excerpt": excerpt,
			"excerpt_available": excerpt != "", "excerpt_truncated": false,
			"authors":             []map[string]string{{"name": "External author", "url": "https://example.org/about"}},
			"source_published_at": "2026-09-01T00:00:00Z", "source_publication_timezone_known": true,
			"first_observed_at": now - 200, "last_observed_at": now - 100, "content_hash": strings.Repeat("a", 64),
			"native_identity": false, "claimable_job": false, "hugging_face_eligible": false, "untrusted_content": true}
	}
	fixture := map[string]any{"version": 1, "state": "ready", "registry_sha256": strings.Repeat("a", 64), "suppression_sha256": strings.Repeat("b", 64),
		"generated_at": now, "valid_until": now + 300,
		"sources": []map[string]any{{"id": "fixture", "name": "Fixture reference source", "feed_url": "https://example.org/feed.json", "adapter": "jsonfeed-1.1",
			"last_attempted_at": now - 10, "last_successful_at": now - 10, "status": "ok", "attribution_basis": "source_declared"}},
		"references": []map[string]any{row(strings.Repeat("1", 64), "First & <reference>", "SOURCE_EXCERPT_SENTINEL"), row(strings.Repeat("2", 64), "Second metadata only", ""), row(strings.Repeat("3", 64), "Third café", "Available final excerpt")}}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Digest = strings.Repeat("c", 64)
	reader := &referenceFixtureReader{snapshot: &snapshot}
	service := &fakeService{err: errors.New("reference reads must not execute board commands")}
	return New(service, nil, Config{References: reader}), reader, service
}

func referenceRequest(server http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func TestReferenceHTTPReadOnlyFiltersPaginationAndNativeIsolation(t *testing.T) {
	server, reader, service := referenceFixture(t)
	response := referenceRequest(server, "GET", "/api/references?limit=1", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var first struct {
		References []references.Reference `json:"references"`
		NextCursor string                 `json:"next_cursor"`
		Policy     map[string]any         `json:"policy"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.References) != 1 || first.NextCursor == "" || first.Policy["hugging_face_eligible"] != false {
		t.Fatal(first)
	}
	response = referenceRequest(server, "GET", "/api/references?limit=1&cursor="+url.QueryEscape(first.NextCursor), nil)
	var second struct {
		References []references.Reference `json:"references"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil || len(second.References) != 1 || second.References[0].ID == first.References[0].ID {
		t.Fatal(response.Body.String(), err)
	}
	for target, want := range map[string]string{
		"/api/references?q=source_excerpt":           "SOURCE_EXCERPT_SENTINEL",
		"/api/references?q=METADATA":                 "Second metadata only",
		"/api/references?q=CAF%C3%89":                `"references":[]`, // ASCII-only, no invented Unicode fold.
		"/api/references?source=unknown":             `"references":[]`,
		"/api/references/" + strings.Repeat("1", 64): "SOURCE_EXCERPT_SENTINEL",
	} {
		response = referenceRequest(server, "GET", target, nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
			t.Errorf("%s: %d %s", target, response.Code, response.Body.String())
		}
	}
	for _, method := range []string{"POST", "PUT", "MKCOL", "DELETE", "OPTIONS"} {
		before := reader.loads.Load()
		response = referenceRequest(server, method, "/api/references", nil)
		if response.Code != 405 || response.Header().Get("Allow") != "GET, HEAD" || reader.loads.Load() != before {
			t.Errorf("%s allowed mutation", method)
		}
	}
	if len(service.commands) != 0 {
		t.Fatal("reference route touched board service")
	}
	if reader.loads.Load() != reader.fences.Load() {
		t.Fatal("successful reads missed final fence")
	}
}

func TestReferenceCursorRejectsAmbiguousInputAndResetsOnProjectionChange(t *testing.T) {
	query := referenceQuery{query: "snow 雪", source: "fixture", limit: 20}
	digest := strings.Repeat("c", 64)
	valid, err := makeReferenceCursor(query, digest, 2)
	if err != nil {
		t.Fatal(err)
	}
	if offset, reset, err := parseReferenceCursor(valid, query, digest); err != nil || reset || offset != 2 {
		t.Fatal(offset, reset, err)
	}
	if _, reset, err := parseReferenceCursor(valid, query, strings.Repeat("d", 64)); err != nil || !reset {
		t.Fatal(reset, err)
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(valid)
	for _, raw := range []string{string(decoded) + "\n", strings.Replace(string(decoded), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(decoded), `"offset":2`, `"offset":2.0`, 1), strings.Replace(string(decoded), `"offset":2`, `"offset":1001`, 1)} {
		if _, _, err := parseReferenceCursor(base64.RawURLEncoding.EncodeToString([]byte(raw)), query, digest); err == nil {
			t.Fatal("accepted noncanonical cursor", raw)
		}
	}
	server, reader, _ := referenceFixture(t)
	for _, target := range []string{"/api/references?q=a&q=b", "/api/references?limit=01", "/api/references?limit=51", "/api/references?unknown=x", "/api/references?source=../x", "/api/references?q=%00", "/api/references?q=%zz", "/api/references?q=a;b", "/api/references/" + strings.Repeat("1", 64) + "?q=x"} {
		if response := referenceRequest(server, "GET", target, nil); response.Code != 400 {
			t.Fatal(target, response.Code)
		}
	}
	noFilter := referenceQuery{limit: 20}
	old, _ := makeReferenceCursor(noFilter, strings.Repeat("e", 64), 1)
	response := referenceRequest(server, "GET", "/api/references?cursor="+old, nil)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "reference_cursor_reset") || strings.Contains(response.Body.String(), "SOURCE_EXCERPT_SENTINEL") {
		t.Fatal(response.Code, response.Body.String())
	}
	if reader.fences.Load() != 1 {
		t.Fatal("reset missed fence")
	}
}

func TestReferenceFinalFenceDiscardsPreparedJSONHTMLHeadAndMissing(t *testing.T) {
	for _, target := range []string{"/references", "/api/references", "/references/" + strings.Repeat("1", 64), "/api/references/" + strings.Repeat("f", 64)} {
		for _, method := range []string{"GET", "HEAD"} {
			server, reader, _ := referenceFixture(t)
			reader.fenceErr = errors.New("DO_NOT_REFLECT_PRIVATE_PATH_OR_REASON")
			response := referenceRequest(server, method, target, map[string]string{"If-None-Match": "*", "If-Modified-Since": time.Now().Format(http.TimeFormat)})
			if response.Code != 503 || reader.fences.Load() != 1 || strings.Contains(response.Body.String(), "SOURCE_EXCERPT_SENTINEL") || strings.Contains(response.Body.String(), "DO_NOT_REFLECT") || strings.Contains(response.Body.String(), "External author") {
				t.Fatal(target, method, response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Signal") != referenceContentSignal || response.Header().Get("ETag") != "" {
				t.Fatal("unsafe caching")
			}
			if method == "HEAD" && response.Body.Len() != 0 {
				t.Fatal("HEAD returned body")
			}
		}
	}
}

func TestReferenceUnavailableEmptyMissingAndOverloadAreDistinct(t *testing.T) {
	server, reader, _ := referenceFixture(t)
	reader.snapshot.References = []references.Reference{}
	if response := referenceRequest(server, "GET", "/api/references", nil); response.Code != 200 || !strings.Contains(response.Body.String(), `"references":[]`) {
		t.Fatal(response.Body.String())
	}
	if response := referenceRequest(server, "GET", "/api/references/"+strings.Repeat("1", 64), nil); response.Code != 404 {
		t.Fatal(response.Code)
	}
	reader.loadErr = errors.New("private diagnostic must not be returned")
	if response := referenceRequest(server, "GET", "/references", nil); response.Code != 503 || strings.Contains(response.Body.String(), "private diagnostic") {
		t.Fatal(response.Code, response.Body.String())
	}
	server.cfg.References = nil
	if response := referenceRequest(server, "GET", "/api/references", nil); response.Code != 503 {
		t.Fatal(response.Code)
	}
	for i := 0; i < referenceReadConcurrency; i++ {
		server.referenceInflight <- struct{}{}
	}
	if response := referenceRequest(server, "GET", "/api/references", nil); response.Code != 429 || response.Header().Get("Retry-After") != "2" {
		t.Fatal(response.Code)
	}
}

func TestReferenceResponseBufferCannotBypassLimitViaStringWriter(t *testing.T) {
	var buffer referenceBuffer
	if _, err := buffer.Write([]byte(strings.Repeat("x", referenceResponseLimit))); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("x")); err == nil || buffer.Len() != referenceResponseLimit {
		t.Fatal("buffer overflow")
	}
	if _, ok := any(&buffer).(interface{ WriteString(string) (int, error) }); ok {
		t.Fatal("promoted StringWriter bypasses bound")
	}
}

func TestReferenceHTTPDiscardsOversizedPreparedResponse(t *testing.T) {
	for _, path := range []string{"/api/references?limit=50", "/references?limit=50"} {
		server, reader, service := referenceFixture(t)
		item := reader.snapshot.References[0]
		item.Authors = make([]references.Author, 20)
		for i := range item.Authors {
			item.Authors[i] = references.Author{Name: strings.Repeat("n", 512), URL: "https://example.org/" + strings.Repeat("a", 4000)}
		}
		reader.snapshot.References = make([]references.Reference, 50)
		for i := range reader.snapshot.References {
			reader.snapshot.References[i] = item
			reader.snapshot.References[i].ID = fmt.Sprintf("%064x", i+1)
		}
		response := referenceRequest(server, "GET", path, nil)
		if response.Code != 503 || response.Body.Len() > 2048 || !strings.Contains(response.Body.String(), "reference_response_limit") || strings.Contains(response.Body.String(), "SOURCE_EXCERPT_SENTINEL") {
			t.Fatal(path, response.Code, response.Body.Len())
		}
		if len(service.commands) != 0 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("oversized response escaped reference isolation")
		}
	}
}

func TestReferenceDiscoveryAndTrainingRulesDoNotExpandNativeSurface(t *testing.T) {
	server, reader, service := referenceFixture(t)
	for _, configured := range []bool{true, false} {
		server.cfg.References = nil
		if configured {
			server.cfg.References = reader
		}
		response := referenceRequest(server, "GET", "/capabilities", nil)
		var capabilities struct {
			References map[string]any `json:"external_references"`
		}
		if json.Unmarshal(response.Body.Bytes(), &capabilities) != nil || capabilities.References["configured"] != configured || capabilities.References["mcp"] != false || capabilities.References["automatic_execution"] != false {
			t.Fatal(response.Body.String())
		}
	}
	for _, path := range []string{"/llms.txt", "/skill.md"} {
		response := referenceRequest(server, "GET", path, nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "/references") {
			t.Fatal(path, response.Body.String())
		}
	}
	response := referenceRequest(server, "GET", "/openapi.json", nil)
	var api struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/references", "/api/references/{reference_id}"} {
		if len(api.Paths[path]) != 2 || api.Paths[path]["get"] == nil || api.Paths[path]["head"] == nil {
			t.Fatal(path, api.Paths[path])
		}
	}
	response = referenceRequest(server, "GET", "/robots.txt", nil)
	groups := strings.Split(response.Body.String(), "\n\n")
	if len(groups) < 2 || strings.Contains(groups[0], "Disallow: /references") || !strings.Contains(groups[1], "User-agent: GPTBot") || !strings.Contains(groups[1], "Disallow: /references") {
		t.Fatal(response.Body.String())
	}
	for _, group := range groups[:2] {
		for _, route := range []string{"/w/", "/w64/", "/c64/", "/v1/", "/mcp", "/admin/"} {
			if !strings.Contains(group, "Disallow: "+route+"\n") {
				t.Fatal("specific crawler group dropped native exclusion", route)
			}
		}
	}
	if reader.loads.Load() != 0 || len(service.commands) != 0 {
		t.Fatal("discovery unexpectedly accessed references or board data")
	}
}
