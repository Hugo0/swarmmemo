package nostr

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
)

// NIP-19 names are bech32 (BIP-173) display encodings. They are never parsed
// for authority: events carry hex keys, and only hex is verified.

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

func hrpExpand(hrp string) []byte {
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

// convertBits regroups bits; pad is true when encoding.
func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	acc, bits := uint32(0), uint(0)
	maxv := uint32(1)<<to - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, b := range data {
		if uint32(b)>>from != 0 {
			return nil, errors.New("bech32: value out of range")
		}
		acc = acc<<from | uint32(b)
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
		return nil, errors.New("bech32: invalid padding")
	}
	return out, nil
}

func bech32Encode(hrp string, data []byte) string {
	values, _ := convertBits(data, 8, 5, true)
	poly := bech32Polymod(append(append(hrpExpand(hrp), values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range values {
		b.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(poly>>uint(5*(5-i)))&31])
	}
	return b.String()
}

// bech32Decode accepts lowercase strings up to 1000 characters (NIP-19
// relaxes BIP-173's 90 for TLV entities).
func bech32Decode(s string) (string, []byte, error) {
	if len(s) > 1000 || strings.ToLower(s) != s {
		return "", nil, errors.New("bech32: too long or not lowercase")
	}
	sep := strings.LastIndexByte(s, '1')
	if sep < 1 || sep+7 > len(s) {
		return "", nil, errors.New("bech32: bad separator")
	}
	hrp := s[:sep]
	values := make([]byte, 0, len(s)-sep-1)
	for i := sep + 1; i < len(s); i++ {
		v := strings.IndexByte(bech32Charset, s[i])
		if v < 0 {
			return "", nil, errors.New("bech32: bad character")
		}
		values = append(values, byte(v))
	}
	if bech32Polymod(append(hrpExpand(hrp), values...)) != 1 {
		return "", nil, errors.New("bech32: bad checksum")
	}
	data, err := convertBits(values[:len(values)-6], 5, 8, false)
	return hrp, data, err
}

// Npub is the NIP-19 display form of a hex public key; "" if it is not one.
func Npub(pubHex string) string {
	raw, err := hex.DecodeString(pubHex)
	if err != nil || len(raw) != 32 {
		return ""
	}
	return bech32Encode("npub", raw)
}

// DecodeNpub returns the hex key an npub names.
func DecodeNpub(s string) (string, error) {
	hrp, data, err := bech32Decode(s)
	if err != nil || hrp != "npub" || len(data) != 32 {
		return "", errors.New("not an npub")
	}
	return hex.EncodeToString(data), nil
}

// NEvent is the NIP-19 nevent for an event: its id, author and kind (TLV 0,
// 2 and 3). Relay hints are omitted. "" if the id or author is not hex.
func NEvent(idHex, authorHex string, kind int) string {
	id, err1 := hex.DecodeString(idHex)
	author, err2 := hex.DecodeString(authorHex)
	if err1 != nil || err2 != nil || len(id) != 32 || len(author) != 32 {
		return ""
	}
	tlv := make([]byte, 0, 2+32+2+32+2+4)
	tlv = append(append(tlv, 0, 32), id...)
	tlv = append(append(tlv, 2, 32), author...)
	tlv = binary.BigEndian.AppendUint32(append(tlv, 3, 4), uint32(kind))
	return bech32Encode("nevent", tlv)
}

// DecodeNEvent returns the id, author and kind in an nevent. Unknown TLV
// types are skipped, as NIP-19 requires.
func DecodeNEvent(s string) (id, author string, kind int, err error) {
	hrp, data, err := bech32Decode(s)
	if err != nil || hrp != "nevent" {
		return "", "", 0, errors.New("not an nevent")
	}
	kind = -1
	for len(data) >= 2 {
		t, n := data[0], int(data[1])
		if len(data) < 2+n {
			return "", "", 0, errors.New("nevent: truncated TLV")
		}
		v := data[2 : 2+n]
		switch {
		case t == 0 && n == 32:
			id = hex.EncodeToString(v)
		case t == 2 && n == 32:
			author = hex.EncodeToString(v)
		case t == 3 && n == 4:
			kind = int(binary.BigEndian.Uint32(v))
		}
		data = data[2+n:]
	}
	if id == "" {
		return "", "", 0, errors.New("nevent: no id")
	}
	return id, author, kind, nil
}
