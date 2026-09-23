// Package nostr is the part of Nostr the SwarmMemo bridge needs (RFC0007):
// strict NIP-01 event parsing, event ids, BIP-340 signatures, NIP-19 display
// names and a bounded relay client. Signing and verification are btcec's
// reviewed schnorr package; nothing here implements curve arithmetic.
package nostr

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Bounds on one event. A relay frame larger than MaxEventBytes plus its
// envelope is refused before it is decoded.
const (
	MaxEventBytes = 64 << 10
	MaxTags       = 128
	MaxTagValues  = 16
)

// Event is a NIP-01 event.
type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// ErrInvalid wraps every reason an event is refused.
var ErrInvalid = errors.New("invalid nostr event")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ParseEvent decodes exactly one event object and checks its shape. It does
// not check the id or signature; Verify does.
func ParseEvent(raw []byte) (Event, error) {
	var e Event
	if len(raw) > MaxEventBytes {
		return e, invalid("event exceeds %d bytes", MaxEventBytes)
	}
	if !utf8.Valid(raw) {
		return e, invalid("event is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Event{}, invalid("malformed event")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Event{}, invalid("trailing data after event")
	}
	if !isHex(e.ID, 64) || !isHex(e.PubKey, 64) || !isHex(e.Sig, 128) {
		return Event{}, invalid("id, pubkey and sig must be lowercase hex")
	}
	if e.CreatedAt < 0 || e.Kind < 0 || e.Kind > 65535 {
		return Event{}, invalid("created_at or kind out of range")
	}
	if e.Tags == nil || len(e.Tags) > MaxTags {
		return Event{}, invalid("tags missing or more than %d", MaxTags)
	}
	for _, tag := range e.Tags {
		if len(tag) == 0 || len(tag) > MaxTagValues {
			return Event{}, invalid("a tag has no values or more than %d", MaxTagValues)
		}
	}
	return e, nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Serialize is the NIP-01 id preimage [0,pubkey,created_at,kind,tags,content].
func Serialize(e Event) []byte {
	b := make([]byte, 0, 128+len(e.Content))
	b = append(b, `[0,"`...)
	b = append(b, e.PubKey...)
	b = append(b, `",`...)
	b = strconv.AppendInt(b, e.CreatedAt, 10)
	b = append(b, ',')
	b = strconv.AppendInt(b, int64(e.Kind), 10)
	b = append(b, ",["...)
	for i, tag := range e.Tags {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '[')
		for j, v := range tag {
			if j > 0 {
				b = append(b, ',')
			}
			b = appendQuoted(b, v)
		}
		b = append(b, ']')
	}
	b = append(b, "],"...)
	b = appendQuoted(b, e.Content)
	return append(b, ']')
}

// appendQuoted escapes as NIP-01 requires: the seven short escapes, \u00XX
// for any other control character, and every other character verbatim.
func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		default:
			if c < 0x20 {
				b = append(b, fmt.Sprintf(`\u%04x`, c)...)
			} else {
				b = append(b, c)
			}
		}
	}
	return append(b, '"')
}

// ComputeID is the hex sha256 of the serialization.
func ComputeID(e Event) string {
	sum := sha256.Sum256(Serialize(e))
	return hex.EncodeToString(sum[:])
}

// Verify checks that the id matches the content and that sig is a valid
// BIP-340 signature by pubkey over the id.
func Verify(e Event) error {
	if ComputeID(e) != e.ID {
		return invalid("id does not match the event")
	}
	id, _ := hex.DecodeString(e.ID)
	key, err := hex.DecodeString(e.PubKey)
	if err != nil {
		return invalid("pubkey is not hex")
	}
	sigBytes, err := hex.DecodeString(e.Sig)
	if err != nil {
		return invalid("sig is not hex")
	}
	if !VerifySignature(key, id, sigBytes) {
		return invalid("signature does not verify")
	}
	return nil
}

// VerifySignature is BIP-340 verification of sig by the x-only key over msg.
func VerifySignature(key, msg, sig []byte) bool {
	pub, err := schnorr.ParsePubKey(key)
	if err != nil {
		return false
	}
	s, err := schnorr.ParseSignature(sig)
	if err != nil {
		return false
	}
	return s.Verify(msg, pub)
}

// Key is a bridge signing key.
type Key struct{ priv *btcec.PrivateKey }

// GenerateKey makes a new random key.
func GenerateKey() (Key, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return Key{}, err
	}
	return Key{priv: priv}, nil
}

// KeyFromHex parses a 32-byte secret key; it refuses zero and values not
// below the curve order.
func KeyFromHex(s string) (Key, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return Key{}, errors.New("secret key must be 64 hex characters")
	}
	var scalar btcec.ModNScalar
	if overflow := scalar.SetByteSlice(raw); overflow || scalar.IsZero() {
		return Key{}, errors.New("secret key is out of range")
	}
	return Key{priv: btcec.PrivKeyFromScalar(&scalar)}, nil
}

// Hex is the secret key, for writing the key file only.
func (k Key) Hex() string {
	b := k.priv.Key.Bytes()
	return hex.EncodeToString(b[:])
}

// PublicHex is the x-only public key in hex, as events carry it.
func (k Key) PublicHex() string {
	return hex.EncodeToString(schnorr.SerializePubKey(k.priv.PubKey()))
}

// Sign fills in pubkey, id and sig. The nonce uses fresh auxiliary
// randomness, as BIP-340 recommends.
func (k Key) Sign(e *Event) error {
	var aux [32]byte
	if _, err := rand.Read(aux[:]); err != nil {
		return err
	}
	return k.signWithAux(e, aux)
}

func (k Key) signWithAux(e *Event, aux [32]byte) error {
	if e.Tags == nil {
		e.Tags = [][]string{}
	}
	e.PubKey = k.PublicHex()
	e.ID = ComputeID(*e)
	id, _ := hex.DecodeString(e.ID)
	sig, err := schnorr.Sign(k.priv, id, schnorr.CustomNonce(aux))
	if err != nil {
		return err
	}
	e.Sig = hex.EncodeToString(sig.Serialize())
	return nil
}
