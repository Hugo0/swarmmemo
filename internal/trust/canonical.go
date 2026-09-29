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
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		panic(err) // raw came from json.Marshal
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'})
}

// Canonical is v as canonical JSON.
func Canonical(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return canonicalize(raw)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
