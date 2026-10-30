package services

import (
	"bytes"
	"encoding/json"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// PublicDataSHA256How is the data_sha256 recipe, as the catalogue and the
// docs state it (C643, kosoyy): a digest of the item's data alone, so anyone
// re-fetching the same rows gets the same hash, whatever the call's ids and
// times.
const PublicDataSHA256How = "data_sha256 = lowercase hex SHA-256 of the UTF-8 bytes of Python json.dumps(data, sort_keys=True, separators=(',',':'), ensure_ascii=False), data being the item's data as parsed from the response"

// pdDataSHA256 is the data_sha256 of v (pdDataCanonical's bytes). Empty
// when v cannot be encoded.
func pdDataSHA256(v any) string {
	s, ok := pdDataCanonical(v)
	if !ok {
		return ""
	}
	return sha256Of([]byte(s))
}

// pdDataCanonical is v as the response encodes it, read back the way
// Python's json.loads would, then written as PublicDataSHA256How says.
func pdDataCanonical(v any) (string, bool) {
	raw := canonicalJSON(v)
	if raw == nil {
		return "", false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var parsed any
	if err := d.Decode(&parsed); err != nil {
		return "", false
	}
	var b strings.Builder
	if !pyJSON(&b, parsed) {
		return "", false
	}
	return b.String(), true
}

// pyJSON writes v exactly as Python's json.dumps(v, sort_keys=True,
// separators=(',',':'), ensure_ascii=False) would after json.loads. False
// for a value json.loads would not give back as written (a number out of
// float range).
func pyJSON(b *strings.Builder, v any) bool {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		s, ok := pyNumber(string(x))
		if !ok {
			return false
		}
		b.WriteString(s)
	case string:
		pyString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if !pyJSON(b, e) {
				return false
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys) // UTF-8 byte order is code point order, as Python sorts
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			pyString(b, k)
			b.WriteByte(':')
			if !pyJSON(b, x[k]) {
				return false
			}
		}
		b.WriteByte('}')
	default:
		return false
	}
	return true
}

// pyNumber is a JSON number literal as Python writes it back: an integer
// literal stays an int (arbitrary precision, -0 is 0), anything with a
// fraction or exponent becomes a float written by float.__repr__.
func pyNumber(lit string) (string, bool) {
	if !strings.ContainsAny(lit, ".eE") {
		n, ok := new(big.Int).SetString(lit, 10)
		if !ok {
			return "", false
		}
		return n.String(), true
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return "", false // out of range: Python would give inf
	}
	return pyFloatRepr(f), true
}

// pyFloatRepr is Python's repr of a finite float: the shortest round-trip
// digits, positional when the decimal exponent is in [-4, 16), else
// d.ddde±XX with at least two exponent digits; positional always has a
// fraction (1.0).
func pyFloatRepr(f float64) string {
	if f == 0 {
		if strconv.FormatFloat(f, 'g', -1, 64)[0] == '-' {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := strconv.Itoa(pdAbs(exp))
		if len(es) < 2 {
			es = "0" + es
		}
		if exp < 0 {
			return sign + m + "e-" + es
		}
		return sign + m + "e+" + es
	}
	var s string
	switch {
	case exp < 0:
		s = "0." + strings.Repeat("0", -exp-1) + digits
	case exp+1 >= len(digits):
		s = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	default:
		s = digits[:exp+1] + "." + digits[exp+1:]
	}
	return sign + s
}

func pdAbs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// pyString writes s as Python's json encoder does with ensure_ascii=False:
// ", \ and control characters below 0x20 escaped (\n \r \t \b \f short,
// the rest \u00xx), everything else as is.
func pyString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				const hexd = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hexd[r>>4])
				b.WriteByte(hexd[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
