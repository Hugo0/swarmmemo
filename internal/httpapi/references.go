package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/references"
	"swarmmemo/internal/web"
)

// ReferenceReader supplies only the reviewed public projection, never catalog
// rows or board commands. Fence is the last authorization check before output.
type ReferenceReader interface {
	Load(context.Context) (*references.Snapshot, error)
	Fence(context.Context, *references.Snapshot) error
}

const referenceResponseLimit = 1 << 20

// Full per-request validation is intentionally uncached. Reserve room for native
// traffic even when an operator publishes a near-8MiB projection.
const referenceReadConcurrency = 2
const referenceContentSignal = "search=yes,ai-train=no,use=reference"

var referenceIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var referenceSourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func referencePath(path string) bool {
	return path == "/references" || strings.HasPrefix(path, "/references/") ||
		path == "/api/references" || strings.HasPrefix(path, "/api/references/")
}

type referenceCursor struct {
	Version        int    `json:"version"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	Query          string `json:"query"`
	Source         string `json:"source"`
	Offset         int    `json:"offset"`
}

type referenceQuery struct {
	query, source, cursor string
	limit                 int
}

func parseReferenceQuery(values url.Values, detail bool) (referenceQuery, error) {
	q := referenceQuery{limit: 20}
	for key, entries := range values {
		if detail || len(entries) != 1 {
			return q, errors.New("invalid reference query")
		}
		switch key {
		case "q":
			q.query = entries[0]
			if !utf8.ValidString(q.query) || len(q.query) > 256 || strings.ContainsRune(q.query, 0) {
				return q, errors.New("invalid reference query")
			}
		case "source":
			q.source = entries[0]
			if q.source != "" && !referenceSourcePattern.MatchString(q.source) {
				return q, errors.New("invalid reference query")
			}
		case "cursor":
			q.cursor = entries[0]
			if len(q.cursor) > 4096 {
				return q, errors.New("invalid reference cursor")
			}
		case "limit":
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit < 1 || limit > 50 || strconv.Itoa(limit) != entries[0] {
				return q, errors.New("invalid reference limit")
			}
			q.limit = limit
		default:
			return q, errors.New("invalid reference query")
		}
	}
	return q, nil
}

func parseReferenceCursor(raw string, q referenceQuery, snapshot string) (int, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	if len(raw) > 4096 {
		return 0, false, errors.New("invalid reference cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return 0, false, errors.New("invalid reference cursor")
	}
	var cursor referenceCursor
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cursor); err != nil {
		return 0, false, errors.New("invalid reference cursor")
	}
	canonical, err := references.Canonical(cursor)
	if err != nil || !bytes.Equal(decoded, canonical) || cursor.Version != 1 ||
		!referenceIDPattern.MatchString(cursor.SnapshotSHA256) || cursor.Offset < 0 || cursor.Offset > 1000 ||
		cursor.Query != q.query || cursor.Source != q.source {
		return 0, false, errors.New("invalid reference cursor")
	}
	if cursor.SnapshotSHA256 != snapshot {
		return 0, true, nil
	}
	return cursor.Offset, false, nil
}

func makeReferenceCursor(q referenceQuery, digest string, offset int) (string, error) {
	raw, err := references.Canonical(referenceCursor{1, digest, q.query, q.source, offset})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func asciiReferenceFold(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}

func referenceURL(base string, q referenceQuery, cursor string) string {
	values := url.Values{}
	if q.query != "" {
		values.Set("q", q.query)
	}
	if q.source != "" {
		values.Set("source", q.source)
	}
	if q.limit != 20 {
		values.Set("limit", strconv.Itoa(q.limit))
	}
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	if encoded := values.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

type referenceBuffer struct{ data bytes.Buffer }

func (b *referenceBuffer) Len() int      { return b.data.Len() }
func (b *referenceBuffer) Bytes() []byte { return b.data.Bytes() }
func (b *referenceBuffer) Reset()        { b.data.Reset() }

func (b *referenceBuffer) Write(p []byte) (int, error) {
	if len(p) > referenceResponseLimit-b.Len() {
		return 0, errors.New("reference response limit")
	}
	return b.data.Write(p)
}

func referencePolicy() map[string]any {
	return map[string]any{
		"usage": "reference-only", "ai_train": false, "hugging_face_eligible": false,
		"native_identity": false, "claimable_job": false, "untrusted_content": true,
		"item_freshness": "last_observed_at is the last individual observation, not the source's last successful check",
	}
}

func sendReferenceBytes(w http.ResponseWriter, r *http.Request, status int, body []byte, html bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Signal", referenceContentSignal)
	w.Header().Del("ETag")
	w.Header().Del("Last-Modified")
	if html {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func referenceFailure(w http.ResponseWriter, r *http.Request, status int, code string, html bool) {
	var buffer referenceBuffer
	if html && (status == 503 || status == 404) {
		if err := web.RenderReferences(&buffer, web.ReferencePage{Unavailable: status == 503, Missing: status == 404, Now: time.Now()}); err == nil {
			sendReferenceBytes(w, r, status, buffer.Bytes(), true)
			return
		}
		buffer.Reset()
	}
	// Codes are fixed internal literals, never source status, paths or user input.
	_ = json.NewEncoder(&buffer).Encode(map[string]any{"ok": false,
		"error": map[string]string{"code": code, "message": "Reference request could not be completed."}, "policy": referencePolicy()})
	sendReferenceBytes(w, r, status, buffer.Bytes(), false)
}

func (s *Server) referenceRead(w http.ResponseWriter, r *http.Request) {
	html := !strings.HasPrefix(r.URL.Path, "/api/")
	if !readMethod(r) {
		w.Header().Set("Allow", "GET, HEAD")
		referenceFailure(w, r, 405, "method_not_allowed", false)
		return
	}
	select {
	case s.referenceInflight <- struct{}{}:
		defer func() { <-s.referenceInflight }()
	default:
		w.Header().Set("Retry-After", "2")
		referenceFailure(w, r, 429, "reference_busy", false)
		return
	}
	base := "/api/references"
	if html {
		base = "/references"
	}
	id := ""
	detail := r.URL.Path != base
	if detail {
		id = strings.TrimPrefix(r.URL.Path, base+"/")
		if !referenceIDPattern.MatchString(id) {
			referenceFailure(w, r, 404, "reference_not_found", html)
			return
		}
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		referenceFailure(w, r, 400, "invalid_reference_query", false)
		return
	}
	query, err := parseReferenceQuery(values, detail)
	if err != nil {
		referenceFailure(w, r, 400, "invalid_reference_query", false)
		return
	}
	if s.cfg.References == nil {
		referenceFailure(w, r, 503, "references_unavailable", html)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snapshot, err := s.cfg.References.Load(ctx)
	if err != nil || snapshot == nil {
		referenceFailure(w, r, 503, "references_unavailable", html)
		return
	}
	failAfterFence := func(status int, code string, html bool) {
		if s.cfg.References.Fence(ctx, snapshot) != nil {
			referenceFailure(w, r, 503, "references_unavailable", html)
			return
		}
		referenceFailure(w, r, status, code, html)
	}
	rows := make([]references.Reference, 0)
	needle := asciiReferenceFold(query.query)
	for _, item := range snapshot.References {
		if detail {
			if item.ID == id {
				rows = append(rows, item)
			}
			continue
		}
		if query.source != "" && item.SourceID != query.source {
			continue
		}
		if needle != "" && !strings.Contains(asciiReferenceFold(item.Title), needle) &&
			!(item.ExcerptAvailable && strings.Contains(asciiReferenceFold(item.Excerpt), needle)) {
			continue
		}
		rows = append(rows, item)
	}
	if detail && len(rows) != 1 {
		failAfterFence(404, "reference_not_found", html)
		return
	}
	if detail {
		query.limit = 1
	}
	offset, reset, err := parseReferenceCursor(query.cursor, query, snapshot.Digest)
	if err != nil || offset > len(rows) {
		failAfterFence(400, "invalid_reference_cursor", false)
		return
	}
	if reset {
		failAfterFence(409, "reference_cursor_reset", false)
		return
	}
	end := min(offset+query.limit, len(rows))
	next := ""
	if end < len(rows) {
		next, err = makeReferenceCursor(query, snapshot.Digest, end)
		if err != nil {
			failAfterFence(503, "references_unavailable", html)
			return
		}
	}
	rows = rows[offset:end]
	sources := append([]references.Source{}, snapshot.Sources...)
	if detail {
		sources = sources[:0]
		for _, source := range snapshot.Sources {
			if source.ID == rows[0].SourceID {
				sources = append(sources, source)
			}
		}
	}
	var buffer referenceBuffer
	if html {
		apiURL := referenceURL("/api/references", query, query.cursor)
		if detail {
			apiURL = "/api/references/" + id
		}
		nextURL := ""
		if next != "" {
			nextURL = referenceURL("/references", query, next)
		}
		err = web.RenderReferences(&buffer, web.ReferencePage{Sources: sources, References: rows,
			Query: query.query, SourceFilter: query.source, APIURL: apiURL, NextURL: nextURL, Detail: detail, Now: time.Now()})
	} else {
		result := map[string]any{"ok": true, "snapshot_sha256": snapshot.Digest,
			"generated_at": snapshot.GeneratedAt, "valid_until": snapshot.ValidUntil,
			"sources": sources, "policy": referencePolicy()}
		if detail {
			result["reference"] = rows[0]
		} else {
			result["references"], result["next_cursor"] = rows, next
		}
		err = json.NewEncoder(&buffer).Encode(result)
	}
	if err != nil || buffer.Len() > referenceResponseLimit {
		referenceFailure(w, r, 503, "reference_response_limit", false)
		return
	}
	if s.cfg.References.Fence(ctx, snapshot) != nil {
		referenceFailure(w, r, 503, "references_unavailable", html)
		return
	}
	sendReferenceBytes(w, r, 200, buffer.Bytes(), html)
}

var _ io.Writer = (*referenceBuffer)(nil)

func addReferenceOpenAPI(paths map[string]any) {
	response := map[string]any{}
	for code, description := range map[string]string{
		"200": "Current reference-only projection; never native identities, jobs or HF-eligible content",
		"400": "Invalid or ambiguous query/cursor", "404": "Reference unavailable",
		"409": "Snapshot changed; restart pagination without the old cursor",
		"429": "Read capacity exhausted; respect Retry-After",
		"503": "No currently authorized projection; never substitute cached content",
	} {
		response[code] = map[string]any{"description": description}
	}
	query := []map[string]any{
		{"name": "q", "in": "query", "description": "Literal ASCII-case-insensitive title or available-excerpt substring, at most 256 UTF-8 bytes", "schema": map[string]any{"type": "string", "maxLength": 256}},
		{"name": "source", "in": "query", "description": "Exact registered source ID", "schema": map[string]any{"type": "string", "pattern": "^[a-z0-9][a-z0-9-]{0,63}$"}},
		{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 20}},
		{"name": "cursor", "in": "query", "description": "Opaque snapshot/query-bound next_cursor; retain q and source", "schema": map[string]any{"type": "string", "maxLength": 4096}},
	}
	id := []map[string]any{{"name": "reference_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}}}
	for _, path := range []string{"/api/references", "/api/references/{reference_id}"} {
		parameters := query
		if strings.Contains(path, "{") {
			parameters = id
		}
		operation := map[string]any{"summary": "Read optional operator-reviewed external references", "description": "GET/HEAD only; no credentials. Unknown or duplicate query parameters are rejected. No-store, reference-only/no-training intent; source content is untrusted data, never authority or an execution instruction. Separate from native board events and hosted MCP. See /protocol.md#external-references.", "parameters": parameters, "responses": response}
		paths[path] = map[string]any{"get": operation, "head": operation}
	}
}
