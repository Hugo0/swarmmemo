package services

// fetch.page's field (C790): one value of a JSON answer, selected by an
// RFC 6901 JSON pointer, with the SHA-256 of its canonical JSON. A body
// with a per-call counter, timestamp or nonce has a new raw_sha256 on
// every fetch; field_sha256 of the value a claim is about stays the same,
// so two verifiers of one claim stamp the same hash with the notary.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
)

const (
	// FetchFieldBytes is the longest field (JSON pointer) a call takes.
	FetchFieldBytes = 256
	// FetchFieldValueBytes is the most canonical JSON field_value carries.
	FetchFieldValueBytes = 4 << 10
)

// parseFetchField checks a JSON pointer's syntax: "" (no field) or
// "/"-separated reference tokens, each ~ escaped as ~0 or ~1.
func parseFetchField(ptr string) ([]string, error) {
	rule := "field is an RFC 6901 JSON pointer such as \"/data/rate\" or \"/items/0/price\" (starting with /, ~ written ~0 and / inside a key ~1), up to " + itoa(FetchFieldBytes) + " bytes."
	if len(ptr) > FetchFieldBytes || !utf8.ValidString(ptr) || !strings.HasPrefix(ptr, "/") {
		return nil, badArg(rule)
	}
	tokens := strings.Split(ptr[1:], "/")
	for i, t := range tokens {
		for j := 0; j < len(t); j++ {
			if t[j] == '~' && (j+1 == len(t) || t[j+1] != '0' && t[j+1] != '1') {
				return nil, badArg(rule)
			}
		}
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

// fetchFieldValue resolves tokens in raw, a JSON body, and returns the
// value as canonical JSON: compact, object keys sorted, numbers as the
// site wrote them, strings as encoding/json writes them with < > & and
// U+2028/U+2029 escaped. That last part is why this is json.Marshal and
// not canonicalJSON: an answer passes through re-encoders with and without
// HTML escaping (withOnce, the transports), and only the escaped form comes
// out of both unchanged, so field_value as delivered hashes to field_sha256.
func fetchFieldValue(raw []byte, format, ptr string, tokens []string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	err := dec.Decode(&doc)
	if err == nil {
		if _, more := dec.Token(); !errors.Is(more, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		return nil, &allowance.Err{Code: "fetch_field_not_json", Details: map[string]any{"field": ptr, "format": format}}
	}
	v := doc
	for i, t := range tokens {
		var ok bool
		switch node := v.(type) {
		case map[string]any:
			v, ok = node[t]
		case []any:
			// RFC 6901: an array index is "0" or digits without a leading
			// zero; Atoi alone also takes "-0" and "+1".
			n, err := strconv.Atoi(t)
			if err == nil && n < len(node) && strings.Trim(t, "0123456789") == "" && (t == "0" || t[0] != '0') {
				v, ok = node[n], true
			}
		}
		if !ok {
			at := "/" + strings.Join(escapeFetchTokens(tokens[:i+1]), "/")
			return nil, &allowance.Err{Code: "fetch_field_not_found", Details: map[string]any{"field": ptr, "missing_at": at}}
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, refusal("upstream_failed")
	}
	if len(out) > FetchFieldValueBytes {
		return nil, &allowance.Err{Code: "fetch_field_too_large", Details: map[string]any{"field": ptr, "value_bytes": len(out), "limit_bytes": FetchFieldValueBytes}}
	}
	return out, nil
}

func escapeFetchTokens(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1")
	}
	return out
}
