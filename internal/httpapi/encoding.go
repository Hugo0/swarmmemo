package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// DecodeCommand shares exact transport parsing with local canonical tooling.
// It rejects ambiguity before signed bytes can be normalized or downgraded.
func DecodeCommand(raw []byte) (board.Command, error) {
	var command board.Command
	err := strictJSON(raw, &command)
	return command, err
}

// Reject duplicate fields rather than allowing parsers to disagree about which
// signed operation/destination/payload the sender authorized.
func strictJSON(raw []byte, v any) (resultErr error) {
	var commandFields map[string]json.RawMessage
	if _, command := v.(*board.Command); command {
		_ = json.Unmarshal(raw, &commandFields)
		_, asserted := commandFields["private_read"]
		var op string
		_ = json.Unmarshal(commandFields["operation"], &op)
		if asserted || strings.HasPrefix(op, "private_read.") {
			// Never reflect attacker-provided names or parsing details on this
			// private plane, including failures in the generic recursive parser.
			defer func() {
				if resultErr != nil {
					if be, ok := resultErr.(*board.Error); ok && (be.Code == "invalid_ttl" || be.Code == "invalid_limit" || be.Code == "invalid_private_read_data") {
						return
					}
					resultErr = privateInputError(asserted)
				}
			}()
		}
	}
	if !utf8.Valid(raw) {
		return bad("JSON must be UTF-8.")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	fields := map[string]bool{}
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = true
			}
		}
	}
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return bad("JSON nesting is too deep.")
		}
		tok, e := d.Token()
		if e != nil {
			return bad("Invalid JSON.")
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return bad("Invalid JSON object.")
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return bad("Duplicate JSON field.")
				}
				seen[key] = true
				if depth == 0 && len(fields) > 0 && !fields[key] {
					return bad("Unknown or incorrectly cased JSON field: " + key)
				}
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		default:
			return bad("Invalid JSON delimiter.")
		}
		_, e = d.Token()
		if e != nil {
			return bad("Incomplete JSON.")
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return bad("Expected exactly one JSON object.")
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return bad("Expected a JSON object.")
	}
	if _, command := v.(*board.Command); command {
		// A nil *DelegationContext otherwise conflates explicit null with absence
		// and selects legacy canonical bytes. Asserted authority must fail closed.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return bad("Invalid JSON command.")
		}
		if context, exists := fields["delegation"]; exists && bytes.Equal(bytes.TrimSpace(context), []byte("null")) {
			return bad("Delegation context cannot be null; omit it only for ordinary commands.")
		}
	}
	d = json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return bad("Invalid or unknown JSON command fields.")
	}
	if c, command := v.(*board.Command); command {
		return privateRawFields(commandFields, *c)
	}
	return nil
}

// A path-only envelope is useful for fetch tools which discard query strings.
// It is a write surface and must never be linked, crawled, cached or prefetched.
func (s *Server) pathCommand(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if r.Method != "GET" && r.Method != "POST" {
		methodError(w)
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		writeError(w, bad("A c64 envelope cannot be combined with a query or body."))
		return
	}
	encoded := strings.TrimPrefix(r.URL.Path, "/c64/")
	raw, e := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if e != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		writeError(w, bad("Expected one unpadded base64url JSON command."))
		return
	}
	var cmd board.Command
	if e = strictJSON(raw, &cmd); e != nil {
		writeError(w, e)
		return
	}
	if !knownOperation(cmd.Operation) {
		writeError(w, bad("Unknown command operation."))
		return
	}
	// Never accidentally index sensitive command results, even for read operations.
	r.Header.Set("Accept", "application/json")
	s.execute(w, r, cmd)
}
