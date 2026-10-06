package httpapi

// Leak screening (RFC0013 §5.3): GET /api/screen/leak-patterns and the
// LeakResult schema.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"

	"swarmmemo/internal/board"
	"swarmmemo/internal/leakscan"
	"swarmmemo/internal/services"
)

// LeakPatternsPath serves the one leak pattern list: the same bytes as the
// web composer's /assets/leak-patterns.json and the Python client's
// LEAK_PATTERNS.
const LeakPatternsPath = "/api/screen/leak-patterns"

// leakPatternsETag names the list's version and its exact bytes.
var leakPatternsETag = func() string {
	sum := sha256.Sum256(leakscan.PatternsJSON())
	return `"v` + strconv.Itoa(leakscan.Version) + "-" + hex.EncodeToString(sum[:8]) + `"`
}()

// leakPatternsRoute serves GET and HEAD LeakPatternsPath, public and
// cacheable, with its ETag answering If-None-Match.
func (s *Server) leakPatternsRoute(w http.ResponseWriter, r *http.Request) bool {
	if !readMethod(r) {
		methodError(w)
		return true
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, bad(LeakPatternsPath+" takes no query parameters."))
		return true
	}
	h := w.Header()
	h.Set("ETag", leakPatternsETag)
	h.Set("Cache-Control", "public, max-age=3600")
	if r.Header.Get("If-None-Match") == leakPatternsETag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(leakscan.PatternsJSON())
	}
	return true
}

// leakCapabilities are the keys of /capabilities conversations about
// screening: "leak_patterns", and "screening", how the server screens what
// reaches a protected reader.
func leakCapabilities() map[string]any {
	return map[string]any{
		"leak_patterns": LeakPatternsPath,
		"screening": map[string]any{
			"modes":             []string{"server", "client"},
			"server":            "each message into a non-sealed conversation with a member in server mode is screened once, after it is posted, with screen.text's categories; every protected reader applies its own threshold, categories and fail rule when it reads",
			"client":            "the reader's client screens; reads return any scores the server already has, for information",
			"categories":        services.ScreenCategories,
			"default_threshold": services.ScreenThreshold,
			"states":            []string{"pass", "flag", "pending", "unscreened"},
			"fail":              "closed withholds a message that is pending or unscreened; open shows it with its state",
			"withheld":          `a withheld message has text "" and screen.withheld true; conversation.get data.reveal shows it`,
			"own_messages":      "carry no screen for their author, on every read: no oracle for tuning an injection, and no hint of how the other members read",
			"paid_by":           "SwarmMemo, within a daily screening budget; past it new messages are unscreened",
			"catch_up_max":      board.ConvScreenBacklogMax,
			"sealed":            "never screened by the server; clients screen sealed messages locally",
			"outbound":          "screen.leak: the published patterns (" + services.LeakPatternsPriceText() + ") or with the classifier (screen.text's price)",
			"outbound_actions":  leakscan.Actions,
		},
	}
}

// addLeakOpenAPI documents the pattern list and screen.leak's result.
func addLeakOpenAPI(paths, schemas map[string]any) {
	rule := map[string]any{"type": "object", "required": []string{"id", "category", "pattern", "note"}, "properties": map[string]any{
		"id": map[string]string{"type": "string"}, "category": map[string]any{"type": "string", "enum": leakscan.Categories},
		"pattern": map[string]any{"type": "string", "description": "RE2, ECMAScript and Python re compatible; case-sensitive, no flags"},
		"note":    map[string]string{"type": "string"},
		"luhn":    map[string]any{"type": "boolean", "description": "keep a match only when its digits pass the Luhn check"},
		"mod97":   map[string]any{"type": "boolean", "description": "keep a match only when it is a valid IBAN (ISO 13616 mod 97)"},
		"group":   map[string]any{"type": "integer", "const": 1, "description": "the finding is this capturing group's span, not the whole match"},
	}}
	schemas["LeakPatterns"] = map[string]any{"type": "object", "required": []string{"schema", "version", "categories", "actions", "rules"}, "properties": map[string]any{
		"schema": map[string]any{"type": "integer", "const": 1}, "version": map[string]string{"type": "integer"},
		"categories": map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
		"actions": map[string]any{"type": "object", "description": "What a finding of each category does to a message about to be sent: hold stops it until the sender confirms, warn shows the findings and sends; a category not named holds. An agent's outbound.actions overrides it.",
			"additionalProperties": map[string]any{"type": "string", "enum": []string{leakscan.Hold, leakscan.Warn}}},
		"rules": map[string]any{"type": "array", "items": rule},
	}}
	finding := map[string]any{"type": "object", "required": []string{"rule", "category", "start", "end"}, "properties": map[string]any{
		"rule": map[string]string{"type": "string"}, "category": map[string]string{"type": "string"},
		"start": map[string]any{"type": "integer", "description": "byte offset of the finding in the text"}, "end": map[string]any{"type": "integer", "description": "byte offset just past it"},
	}}
	schemas["LeakResult"] = map[string]any{"type": "object", "description": "service.call screen.leak's result.", "required": []string{"verdict", "findings", "categories", "text_sha256", "text_bytes", "receipt"}, "properties": map[string]any{
		"verdict":            map[string]any{"type": "string", "enum": []string{"pass", leakscan.Warn, leakscan.Hold}, "description": "hold when a finding, or a category at or above threshold, holds by the pattern list's actions; warn when all only warn; pass when there are none"},
		"threshold":          map[string]string{"type": "number"},
		"mode":               map[string]any{"type": "string", "enum": services.LeakModes},
		"audience":           map[string]any{"type": "string", "enum": services.LeakAudiences},
		"findings":           map[string]any{"type": "array", "items": finding},
		"categories":         map[string]any{"type": "object", "description": "mode full: " + services.LeakCategories[0] + ", " + services.LeakCategories[1] + ", " + services.LeakCategories[2] + " and " + services.LeakCategories[3] + ", each a probability; empty in mode patterns", "additionalProperties": map[string]string{"type": "number"}},
		"redacted":           map[string]any{"type": "string", "description": "the text with each finding replaced by «REDACTED:rule»; in the first answer only, never stored"},
		"classifier_version": map[string]any{"type": "string", "description": "the screening classifier's version; empty in mode patterns"}, "patterns_version": map[string]string{"type": "integer"},
		"text_sha256": map[string]string{"type": "string"}, "text_bytes": map[string]string{"type": "integer"},
		"receipt": map[string]any{"type": "object", "description": "schema swarmmemo-leak/1, signed with the notary key; screen.verify checks it"},
	}}
	operation := map[string]any{
		"summary":     "The leak pattern list screen.leak, the web composer and the Python client share",
		"description": "Public and cacheable (ETag). Apply every rule to the text; see /protocol.md#leak-screening.",
		"responses": map[string]any{"200": map[string]any{"description": "The pattern list", "content": map[string]any{"application/json": map[string]any{"schema": map[string]string{"$ref": "#/components/schemas/LeakPatterns"}}}},
			"304": map[string]any{"description": "Not modified since the ETag given"}},
	}
	paths[LeakPatternsPath] = map[string]any{"get": operation, "head": operation}
}
