package services

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Data is a parsed service.call or service.read data field:
//
//	{"schema":1,"method":M,"args":{...},"max_cost":N}   service.call
//	{"schema":1,"method":M,"args":{...}}                service.read
//
// The parser is strict: valid UTF-8, one JSON object and nothing after it,
// no unknown or duplicate keys at any depth, nesting at most JSONDepthMax,
// integers only where a number is expected.
type Data struct {
	Method  string
	Args    json.RawMessage // a JSON object; "{}" when absent
	MaxCost int64           // service.call only
}

const (
	// JSONDepthMax bounds the nesting of data and of args.
	JSONDepthMax = 16
	// MaxCostMax bounds max_cost, so no price arithmetic can overflow.
	MaxCostMax = 1 << 40
)

var (
	methodRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	integerRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,15})$`)
)

// Integer reads a JSON integer argument strictly: digits only, no sign,
// fraction, exponent or quotes, at most max.
func Integer(raw json.RawMessage, max int64) (int64, bool) {
	if !integerRE.Match(raw) {
		return 0, false
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	return n, err == nil && n <= max
}

// ParseData parses the data of service.call (call true) or service.read.
func ParseData(raw string, call bool) (Data, error) {
	var d Data
	var envelope struct {
		Schema  json.RawMessage `json:"schema"`
		Method  *string         `json:"method"`
		Args    json.RawMessage `json:"args"`
		MaxCost json.RawMessage `json:"max_cost"`
	}
	if err := StrictObject([]byte(raw), &envelope); err != nil {
		return d, err
	}
	if string(envelope.Schema) != "1" || envelope.Method == nil || !methodRE.MatchString(*envelope.Method) {
		return d, refusal("invalid_service_data")
	}
	d.Method = *envelope.Method
	d.Args = envelope.Args
	if len(d.Args) == 0 {
		d.Args = json.RawMessage("{}")
	}
	if d.Args[0] != '{' {
		return d, refusal("invalid_service_data")
	}
	switch {
	case call && envelope.MaxCost == nil, !call && envelope.MaxCost != nil:
		return d, refusal("invalid_service_data")
	case call:
		n, err := strconv.ParseInt(string(envelope.MaxCost), 10, 64)
		if err != nil || !integerRE.Match(envelope.MaxCost) || n > MaxCostMax {
			return d, refusal("invalid_service_data")
		}
		d.MaxCost = n
	}
	return d, nil
}

// StrictObject decodes one JSON object into dst (a pointer to a struct) and
// refuses anything a lenient decoder would accept silently: invalid UTF-8,
// unknown fields, duplicate keys, nesting beyond JSONDepthMax, a value that is
// not an object, and trailing data. Every refusal is invalid_service_data.
func StrictObject(raw []byte, dst any) error {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return refusal("invalid_service_data")
	}
	if err := checkStructure(raw); err != nil {
		return refusal("invalid_service_data")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return refusal("invalid_service_data")
	}
	return nil
}

var errStructure = errors.New("services: malformed JSON structure")

// checkStructure walks raw once: exactly one top-level object, no duplicate
// key in any object (case-insensitively), depth bounded, nothing after the value.
func checkStructure(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []*frame
	first := true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if first {
			if d, ok := tok.(json.Delim); !ok || d != '{' {
				return errStructure
			}
		} else if len(stack) == 0 {
			return errStructure // a second top-level value
		}
		first = false
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.key {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				if len(stack) > 0 {
					stack[len(stack)-1].valueDone()
				}
				continue
			}
			name, ok := tok.(string)
			// encoding/json matches field names case-insensitively, so keys
			// that differ only in case are duplicates too: "method" and
			// "METHOD" would let the last one win where another reader sees
			// the first (security review 1.20, L12).
			folded := strings.ToLower(strings.ToUpper(name))
			if !ok || top.keys[folded] {
				return errStructure
			}
			top.keys[folded] = true
			top.key = false
			continue
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{', '[':
				if len(stack) >= JSONDepthMax {
					return errStructure
				}
				stack = append(stack, &frame{object: d == '{', keys: map[string]bool{}, key: d == '{'})
			case ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 {
					stack[len(stack)-1].valueDone()
				}
			default:
				return errStructure
			}
		default:
			if top != nil {
				top.valueDone()
			}
		}
	}
	if first || len(stack) != 0 {
		return errStructure
	}
	return nil
}

// frame is one open object or array in checkStructure.
type frame struct {
	object bool
	keys   map[string]bool
	key    bool // the next token in this object is a key
}

// valueDone records that a value ended inside f: in an object, a key is next.
func (f *frame) valueDone() {
	if f.object {
		f.key = true
	}
}

// Encodings every provider shares: one implementation each.

// canonicalJSON is v as compact JSON without HTML escaping and without the
// encoder's trailing newline, so a record or an answer keeps text as sent;
// nil when v cannot be encoded.
func canonicalJSON(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

// sha256Of is the lowercase hex SHA-256 of b.
func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newCallID is a random 32-hex-digit id: a call's, a stored item's, or an
// unsigned call's request_id (NewRequestID).
func newCallID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// itoa is n in decimal, for copy that states a limit.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
