package board

import (
	"math"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Identity link values are attacker-chosen strings that end up in a DNS query,
// an HTML page and a JSON attestation, so each kind has exactly one canonical
// form and anything that does not normalise to it is refused, not repaired.

const (
	linkValueMaxBytes = 512
	linkDomainMaxLen  = 253
)

var (
	ldhLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tldLabel = regexp.MustCompile(`^[a-z]{2,63}$`)
	// Special-use names (RFC 6761, 6762, 7686, 8375, 9476) never belong to one
	// agent on the public internet.
	specialUseTLDs = map[string]bool{"localhost": true, "local": true, "test": true, "invalid": true, "example": true, "onion": true, "arpa": true, "internal": true, "alt": true}
)

// normalizeLinkDomain returns the lowercase A-label form of a public DNS name.
// Unicode labels are converted to punycode and never rendered back: showing
// only the A-label is what keeps a lookalike name from passing for another.
func normalizeLinkDomain(raw string) (string, bool) {
	if raw == "" || len(raw) > linkDomainMaxLen+1 || !utf8.ValidString(raw) {
		return "", false
	}
	name := strings.TrimSuffix(raw, ".")
	if net.ParseIP(name) != nil {
		return "", false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", false
	}
	for i, label := range labels {
		ascii, ok := normalizeLinkLabel(label)
		if !ok {
			return "", false
		}
		labels[i] = ascii
	}
	tld := labels[len(labels)-1]
	// A numeric or otherwise non-alphabetic final label is an address in
	// disguise (127.1, 0x7f.1) or a name no registry issues.
	if !tldLabel.MatchString(tld) && !strings.HasPrefix(tld, "xn--") || specialUseTLDs[tld] {
		return "", false
	}
	out := strings.Join(labels, ".")
	if len(out) > linkDomainMaxLen {
		return "", false
	}
	return out, true
}

func normalizeLinkLabel(label string) (string, bool) {
	if label == "" || len(label) > 63*4 {
		return "", false
	}
	ascii := true
	for i := 0; i < len(label); i++ {
		if label[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		lower := strings.ToLower(label)
		if !ldhLabel.MatchString(lower) {
			return "", false
		}
		// Labels with hyphens in positions 3-4 are reserved; the only one in use
		// is the IDNA prefix, and it must decode to a valid Unicode label.
		if len(lower) >= 4 && lower[2:4] == "--" {
			if !strings.HasPrefix(lower, "xn--") {
				return "", false
			}
			decoded, ok := punyDecode(lower[4:])
			if !ok || !validULabel(decoded) {
				return "", false
			}
			if again, ok := punyEncode(decoded); !ok || "xn--"+again != lower {
				return "", false
			}
		}
		return lower, true
	}
	lower := strings.Map(unicode.ToLower, label)
	if !validULabel(lower) {
		return "", false
	}
	encoded, ok := punyEncode(lower)
	if !ok {
		return "", false
	}
	out := "xn--" + encoded
	if len(out) > 63 || !ldhLabel.MatchString(out) {
		return "", false
	}
	return out, true
}

// validULabel is deliberately narrower than IDNA2008: letters, decimal digits
// and interior hyphens only, already lowercase, with at least one non-ASCII
// rune. Combining marks are refused, so there is exactly one encoding per label
// without a normalisation table; a name that needs them can be linked by its
// A-label, which is checked the same way after decoding.
func validULabel(label string) bool {
	if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
		return false
	}
	nonASCII := false
	for _, r := range label {
		switch {
		case r == '-':
		case r < utf8.RuneSelf:
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				return false
			}
		case unicode.IsLetter(r) || unicode.Is(unicode.Nd, r):
			if unicode.ToLower(r) != r || compatibilityForm(r) {
				return false
			}
			nonASCII = true
		default:
			return false
		}
	}
	return nonASCII
}

// compatibilityForm covers the blocks whose letters IDNA maps onto other
// letters (fullwidth, mathematical, letterlike, enclosed, ligatures). Without
// a mapping table they would encode to names no registry issues, so they are
// refused rather than silently kept.
func compatibilityForm(r rune) bool {
	return r >= 0xFF00 && r <= 0xFFEF || r >= 0x2100 && r <= 0x214F || r >= 0x2460 && r <= 0x24FF ||
		r >= 0x3300 && r <= 0x33FF || r >= 0xFB00 && r <= 0xFB4F || r >= 0x1D400 && r <= 0x1D7FF || r >= 0x1F100 && r <= 0x1F1FF
}

// Punycode, RFC 3492. Both directions are bounded by the label length the
// callers enforce, and every arithmetic step checks for overflow.
const (
	punyBase        int32 = 36
	punyTMin        int32 = 1
	punyTMax        int32 = 26
	punySkew        int32 = 38
	punyDamp        int32 = 700
	punyInitialBias int32 = 72
	punyInitialN    int32 = 128
)

func punyAdapt(delta, points int32, first bool) int32 {
	if first {
		delta /= punyDamp
	} else {
		delta /= 2
	}
	delta += delta / points
	k := int32(0)
	for delta > ((punyBase-punyTMin)*punyTMax)/2 {
		delta /= punyBase - punyTMin
		k += punyBase
	}
	return k + (punyBase-punyTMin+1)*delta/(delta+punySkew)
}

func punyThreshold(k, bias int32) int32 {
	t := k - bias
	if t < punyTMin {
		return punyTMin
	}
	if t > punyTMax {
		return punyTMax
	}
	return t
}

func punyDigit(d int32) byte {
	if d < 26 {
		return byte('a' + d)
	}
	return byte('0' + d - 26)
}

func punyEncode(s string) (string, bool) {
	runes := []rune(s)
	if len(runes) > 63 {
		return "", false
	}
	out := make([]byte, 0, len(s)+8)
	var basic, remaining int32
	for _, r := range runes {
		if r < 0x80 {
			basic++
			out = append(out, byte(r))
		} else {
			remaining++
		}
	}
	if basic > 0 {
		out = append(out, '-')
	}
	n, bias, delta, handled := punyInitialN, punyInitialBias, int32(0), basic
	for remaining > 0 {
		m := int32(math.MaxInt32)
		for _, r := range runes {
			if r >= n && r < m {
				m = r
			}
		}
		step := int64(m-n) * int64(handled+1)
		if int64(delta)+step > math.MaxInt32 {
			return "", false
		}
		delta += int32(step)
		n = m
		for _, r := range runes {
			if r < n {
				if delta == math.MaxInt32 {
					return "", false
				}
				delta++
				continue
			}
			if r > n {
				continue
			}
			q := delta
			for k := punyBase; ; k += punyBase {
				t := punyThreshold(k, bias)
				if q < t {
					break
				}
				out = append(out, punyDigit(t+(q-t)%(punyBase-t)))
				q = (q - t) / (punyBase - t)
			}
			out = append(out, punyDigit(q))
			bias = punyAdapt(delta, handled+1, handled == basic)
			delta = 0
			handled++
			remaining--
		}
		delta++
		n++
	}
	return string(out), true
}

func punyDecode(s string) (string, bool) {
	if s == "" || len(s) > 63 {
		return "", false
	}
	out := make([]rune, 0, len(s))
	pos := 0
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		if i == 0 {
			return "", false
		}
		for _, r := range s[:i] {
			if r >= 0x80 {
				return "", false
			}
			out = append(out, r)
		}
		pos = i + 1
	}
	n, bias, i := punyInitialN, punyInitialBias, int32(0)
	for pos < len(s) {
		old, w := i, int32(1)
		for k := punyBase; ; k += punyBase {
			if pos == len(s) {
				return "", false
			}
			c := s[pos]
			pos++
			var digit int32
			switch {
			case c >= 'a' && c <= 'z':
				digit = int32(c - 'a')
			case c >= '0' && c <= '9':
				digit = int32(c-'0') + 26
			default:
				return "", false
			}
			if int64(i)+int64(digit)*int64(w) > math.MaxInt32 {
				return "", false
			}
			i += digit * w
			t := punyThreshold(k, bias)
			if digit < t {
				break
			}
			if int64(w)*int64(punyBase-t) > math.MaxInt32 {
				return "", false
			}
			w *= punyBase - t
		}
		x := int32(len(out) + 1)
		bias = punyAdapt(i-old, x, old == 0)
		if int64(n)+int64(i/x) > utf8.MaxRune {
			return "", false
		}
		n += i / x
		i %= x
		if n < 0x80 || !utf8.ValidRune(n) {
			return "", false
		}
		out = append(out, 0)
		copy(out[i+1:], out[i:])
		out[i] = n
		i++
	}
	return string(out), true
}

// normalizeLinkURL accepts a plain https URL on a public DNS name, port 443,
// without credentials or a fragment. It is only ever displayed, never fetched,
// in this phase; the same rules keep it fetchable safely later.
func normalizeLinkURL(raw string) (string, bool) {
	if raw == "" || len(raw) > linkValueMaxBytes || !utf8.ValidString(raw) {
		return "", false
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f || r >= 0x80 {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Host == "" {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	host, ok := normalizeLinkDomain(u.Hostname())
	if !ok {
		return "", false
	}
	u.Host = host
	u.RawPath = ""
	out := u.String()
	if len(out) > linkValueMaxBytes {
		return "", false
	}
	return out, true
}

// Nostr public keys are 32-byte x-only secp256k1 keys, shown as NIP-19 npub
// bech32 strings. Hex input is accepted and converted; the checksum is always
// verified, so a typo cannot become someone else's key.
func normalizeNostrKey(raw string) (string, bool) {
	if len(raw) == 64 {
		key := make([]byte, 32)
		for i := 0; i < 32; i++ {
			hi, ok1 := hexNibble(raw[2*i])
			lo, ok2 := hexNibble(raw[2*i+1])
			if !ok1 || !ok2 {
				return "", false
			}
			key[i] = hi<<4 | lo
		}
		return bech32Encode("npub", key), true
	}
	hrp, data, ok := bech32Decode(raw)
	if !ok || hrp != "npub" || len(data) != 32 {
		return "", false
	}
	return bech32Encode("npub", data), true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, bool) {
	var acc, bits uint
	out := []byte{}
	maxv := uint(1)<<to - 1
	for _, v := range data {
		if uint(v)>>from != 0 {
			return nil, false
		}
		acc = acc<<from | uint(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, false
	}
	return out, true
}

func bech32Encode(hrp string, payload []byte) string {
	data, _ := convertBits(payload, 8, 5, true)
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(values) ^ 1
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, d := range data {
		b.WriteByte(bech32Charset[d])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(mod>>uint(5*(5-i)))&31])
	}
	return b.String()
}

// bech32Decode is BIP-173 bech32 (not bech32m, which NIP-19 does not use),
// lowercase only, bounded to the 90-character limit.
func bech32Decode(s string) (string, []byte, bool) {
	if len(s) < 8 || len(s) > 90 || strings.ToLower(s) != s {
		return "", nil, false
	}
	sep := strings.LastIndexByte(s, '1')
	if sep < 1 || sep+7 > len(s) {
		return "", nil, false
	}
	hrp := s[:sep]
	for i := 0; i < len(hrp); i++ {
		if hrp[i] < 33 || hrp[i] > 126 {
			return "", nil, false
		}
	}
	data := make([]byte, 0, len(s)-sep-1)
	for i := sep + 1; i < len(s); i++ {
		d := strings.IndexByte(bech32Charset, s[i])
		if d < 0 {
			return "", nil, false
		}
		data = append(data, byte(d))
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != 1 {
		return "", nil, false
	}
	payload, ok := convertBits(data[:len(data)-6], 5, 8, false)
	return hrp, payload, ok
}
