package tlog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The log key's history (C983). A key's key_id is the lowercase hex SHA-256
// of its raw 32-byte Ed25519 public key, the notary key_id's rule. A rotation
// from one key to the next is a signed note whose body is RotationType's
// lines, signed by both keys (C2SP allows several signature lines): a reader
// holding any earlier key walks forward to the current one.

// RotationType is a rotation body's second line: not a decimal number nor
// PromiseType, so a rotation never parses as a checkpoint or a promise.
const RotationType = "key-rotation/v1"

// KeyID is the key_id of an Ed25519 public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// VerifierKeyID is the key_id of a C2SP verifier key.
func VerifierKeyID(vkey string) (string, error) {
	_, pub, err := ParseVerifierKey(vkey)
	if err != nil {
		return "", err
	}
	return KeyID(pub), nil
}

// Rotation is a key-rotation body: the log's origin, the key_id of the key it
// retires, the verifier key that signs from tree size At on.
type Rotation struct {
	Origin string
	From   string
	To     string
	At     int64
}

// String is the rotation body, the text both keys sign.
func (r Rotation) String() string {
	return fmt.Sprintf("%s\n%s\nfrom %s\nto %s\nat %d\n", r.Origin, RotationType, r.From, r.To, r.At)
}

// ParseRotation parses a rotation body: exactly the lines String writes.
func ParseRotation(text string) (Rotation, error) {
	bad := errors.New("tlog: malformed key rotation")
	lines := strings.Split(text, "\n")
	if len(lines) != 6 || lines[5] != "" || !validNoteName(lines[0]) || lines[1] != RotationType {
		return Rotation{}, bad
	}
	from, ok1 := strings.CutPrefix(lines[2], "from ")
	to, ok2 := strings.CutPrefix(lines[3], "to ")
	at, ok3 := strings.CutPrefix(lines[4], "at ")
	n, err := strconv.ParseInt(at, 10, 64)
	if !ok1 || !ok2 || !ok3 || err != nil || n < 0 || strconv.FormatInt(n, 10) != at || len(from) != 64 || strings.ToLower(from) != from {
		return Rotation{}, bad
	}
	if _, err = hex.DecodeString(from); err != nil {
		return Rotation{}, bad
	}
	if _, _, err = ParseVerifierKey(to); err != nil {
		return Rotation{}, bad
	}
	return Rotation{Origin: lines[0], From: from, To: to, At: n}, nil
}

// SignRotation is the rotation note from old to next at tree size at, signed
// by both keys, old first.
func SignRotation(old, next *NoteSigner, at int64) (string, error) {
	if old.Name() != next.Name() {
		return "", errors.New("tlog: a rotation keeps the log's name")
	}
	body := Rotation{Origin: old.Name(), From: KeyID(old.PublicKey()), To: next.VerifierKey(), At: at}.String()
	note, err := old.Sign(body)
	if err != nil {
		return "", err
	}
	second, err := next.Sign(body)
	if err != nil {
		return "", err
	}
	_, line, _ := strings.Cut(second, body+"\n")
	return note + line, nil
}

// OpenRotation verifies a rotation note against the key it retires (oldKey,
// a verifier key) and the key it names, which must sign it too.
func OpenRotation(note, oldKey string) (Rotation, error) {
	text, err := OpenNote([]byte(note), oldKey)
	if err != nil {
		return Rotation{}, err
	}
	r, err := ParseRotation(text)
	if err != nil {
		return Rotation{}, err
	}
	name, pub, _ := ParseVerifierKey(oldKey)
	if r.From != KeyID(pub) || r.Origin != name {
		return Rotation{}, errors.New("tlog: the rotation does not retire this key")
	}
	if _, err = OpenNote([]byte(note), r.To); err != nil {
		return Rotation{}, fmt.Errorf("tlog: the new key does not sign its rotation: %w", err)
	}
	return r, nil
}
