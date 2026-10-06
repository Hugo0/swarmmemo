package trust

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// canonicalize re-encodes JSON with object keys sorted, no insignificant
// whitespace and no HTML escaping: the form every published trust document
// uses, equal to Python's json.dumps(v, sort_keys=True, separators=(",", ":"))
// for the ASCII strings the trust module emits.
func canonicalize(raw []byte) []byte {
	out, err := canonicalBytes(raw)
	if err != nil {
		panic(err) // raw came from json.Marshal
	}
	return out
}

func canonicalBytes(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// Canonical is v as canonical JSON.
func Canonical(v any) []byte {
	out, err := CanonicalJSON(v)
	if err != nil {
		panic(err)
	}
	return out
}

// CanonicalJSON is v as canonical JSON, or the error encoding it: the bytes
// a trust document and a journal seal (board.JournalCanonical) hash. A value
// read back from that JSON gives the same bytes.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonicalBytes(raw)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
