package tlog

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Signed notes (C2SP signed-note, the format of the Go checksum database and
// of every C2SP transparency log): a text ending in a newline, a blank line,
// then one line per signature, "— NAME BASE64(KEYHASH4 || SIG)". The key hash
// is the first four bytes of SHA-256(NAME || "\n" || 0x01 || PUBKEY) for an
// Ed25519 key, and the signature covers the text, its final newline included.

const algEd25519 = 1

// NoteSigner signs notes with one Ed25519 key under one name.
type NoteSigner struct {
	name string
	hash uint32
	key  ed25519.PrivateKey
}

// NewNoteSigner returns a signer; the name must be non-empty with no spaces
// or plus signs (the C2SP rule).
func NewNoteSigner(name string, key ed25519.PrivateKey) (*NoteSigner, error) {
	if !validNoteName(name) || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("tlog: invalid signer name or key")
	}
	pub := key.Public().(ed25519.PublicKey)
	return &NoteSigner{name: name, hash: noteKeyHash(name, pub), key: key}, nil
}

// Name is the signer's name.
func (s *NoteSigner) Name() string { return s.name }

// PublicKey is the signer's Ed25519 public key.
func (s *NoteSigner) PublicKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// VerifierKey is the C2SP verifier key: NAME+KEYHASH(8 hex)+BASE64(0x01||PUBKEY).
func (s *NoteSigner) VerifierKey() string {
	return VerifierKey(s.name, s.PublicKey())
}

// Sign returns the signed note for text, which must end in a newline.
func (s *NoteSigner) Sign(text string) (string, error) {
	if !strings.HasSuffix(text, "\n") || !validNoteText(text) {
		return "", errors.New("tlog: a note's text must be valid UTF-8 ending in a newline")
	}
	var hb [4]byte
	binary.BigEndian.PutUint32(hb[:], s.hash)
	sig := ed25519.Sign(s.key, []byte(text))
	return text + "\n— " + s.name + " " + base64.StdEncoding.EncodeToString(append(hb[:], sig...)) + "\n", nil
}

// VerifierKey formats a C2SP Ed25519 verifier key.
func VerifierKey(name string, pub ed25519.PublicKey) string {
	return fmt.Sprintf("%s+%08x+%s", name, noteKeyHash(name, pub), base64.StdEncoding.EncodeToString(append([]byte{algEd25519}, pub...)))
}

// ParseVerifierKey parses a C2SP Ed25519 verifier key.
func ParseVerifierKey(vkey string) (name string, pub ed25519.PublicKey, err error) {
	name, rest, ok1 := strings.Cut(vkey, "+")
	hash16, key64, ok2 := strings.Cut(rest, "+")
	hash, err1 := strconv.ParseUint(hash16, 16, 32)
	key, err2 := base64.StdEncoding.DecodeString(key64)
	if !ok1 || !ok2 || len(hash16) != 8 || err1 != nil || err2 != nil || !validNoteName(name) || len(key) != 1+ed25519.PublicKeySize || key[0] != algEd25519 {
		return "", nil, errors.New("tlog: malformed verifier key")
	}
	pub = ed25519.PublicKey(key[1:])
	if uint32(hash) != noteKeyHash(name, pub) {
		return "", nil, errors.New("tlog: verifier key hash mismatch")
	}
	return name, pub, nil
}

// OpenNote verifies a signed note against one verifier key and returns its
// text. Signatures by other keys are ignored; at least one signature must be
// by this key, and every one by it must verify.
func OpenNote(note []byte, vkey string) (string, error) {
	name, pub, err := ParseVerifierKey(vkey)
	if err != nil {
		return "", err
	}
	if len(note) > 1<<20 || !utf8.Valid(note) || !validNoteText(string(note)) {
		return "", errors.New("tlog: malformed note")
	}
	split := bytes.LastIndex(note, []byte("\n\n"))
	if split < 0 {
		return "", errors.New("tlog: malformed note")
	}
	text, sigs := note[:split+1], note[split+2:]
	if len(sigs) == 0 || sigs[len(sigs)-1] != '\n' {
		return "", errors.New("tlog: malformed note")
	}
	want := noteKeyHash(name, pub)
	verified := false
	for _, line := range strings.Split(strings.TrimSuffix(string(sigs), "\n"), "\n") {
		rest, ok := strings.CutPrefix(line, "— ")
		if !ok {
			return "", errors.New("tlog: malformed signature line")
		}
		signer, b64, _ := strings.Cut(rest, " ")
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) < 5 || !validNoteName(signer) {
			return "", errors.New("tlog: malformed signature line")
		}
		if signer != name || binary.BigEndian.Uint32(raw) != want {
			continue
		}
		if !ed25519.Verify(pub, text, raw[4:]) {
			return "", errors.New("tlog: invalid signature")
		}
		verified = true
	}
	if !verified {
		return "", errors.New("tlog: note is not signed by this key")
	}
	return string(text), nil
}

func noteKeyHash(name string, pub ed25519.PublicKey) uint32 {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte("\n"))
	h.Write([]byte{algEd25519})
	h.Write(pub)
	return binary.BigEndian.Uint32(h.Sum(nil))
}

func validNoteName(name string) bool {
	return name != "" && utf8.ValidString(name) && strings.IndexFunc(name, unicode.IsSpace) < 0 && !strings.Contains(name, "+")
}

// validNoteText refuses invalid UTF-8 and ASCII control characters other
// than newline (a literal U+FFFD is fine).
func validNoteText(text string) bool {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r < 0x20 && r != '\n' || r == utf8.RuneError && size == 1 {
			return false
		}
		i += size
	}
	return true
}

// Checkpoint is a C2SP tlog-checkpoint body: the log's origin line, the tree
// size in decimal and the root hash in standard base64, one per line.
type Checkpoint struct {
	Origin string
	Size   int64
	Root   Hash
}

// String is the checkpoint body, the text a note signs.
func (c Checkpoint) String() string {
	return fmt.Sprintf("%s\n%d\n%s\n", c.Origin, c.Size, base64.StdEncoding.EncodeToString(c.Root[:]))
}

// ParseCheckpoint parses a checkpoint body. Extension lines after the root
// are allowed and ignored.
func ParseCheckpoint(text string) (Checkpoint, error) {
	lines := strings.SplitN(text, "\n", 4)
	if len(lines) < 4 || lines[0] == "" {
		return Checkpoint{}, errors.New("tlog: malformed checkpoint")
	}
	size, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil || size < 0 || strconv.FormatInt(size, 10) != lines[1] {
		return Checkpoint{}, errors.New("tlog: malformed checkpoint size")
	}
	root, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil || len(root) != 32 {
		return Checkpoint{}, errors.New("tlog: malformed checkpoint root")
	}
	c := Checkpoint{Origin: lines[0], Size: size}
	copy(c.Root[:], root)
	return c, nil
}

// PromiseType is a promise body's second line. It is not a decimal number,
// so a promise never parses as a checkpoint (whose second line is the tree
// size): the domain separation between the two notes one key signs.
const PromiseType = "promise/v1"

// Promise is a signed inclusion promise (the log's SCT, carrying the leaf
// index): the log commits that the leaf whose hash is Leaf is at Index, and
// that a checkpoint covering it is signed by MergeBy. Its body is the origin
// line, "promise/v1", then one "NAME VALUE" line each for index, leaf
// (standard base64), kind, id, received and merge-by (Unix seconds), in that
// order.
type Promise struct {
	Origin   string
	Index    int64
	Leaf     Hash
	Kind     string
	ID       string
	Received int64
	MergeBy  int64
}

// String is the promise body, the text a note signs.
func (p Promise) String() string {
	return fmt.Sprintf("%s\n%s\nindex %d\nleaf %s\nkind %s\nid %s\nreceived %d\nmerge-by %d\n",
		p.Origin, PromiseType, p.Index, base64.StdEncoding.EncodeToString(p.Leaf[:]), p.Kind, p.ID, p.Received, p.MergeBy)
}

// ParsePromise parses a promise body: exactly the lines String writes, no
// extension lines.
func ParsePromise(text string) (Promise, error) {
	bad := errors.New("tlog: malformed promise")
	lines := strings.Split(text, "\n")
	if len(lines) != 9 || lines[8] != "" || !validNoteName(lines[0]) || lines[1] != PromiseType {
		return Promise{}, bad
	}
	field := func(i int, name string) (string, bool) {
		v, ok := strings.CutPrefix(lines[i], name+" ")
		return v, ok && v != "" && strings.IndexFunc(v, unicode.IsSpace) < 0
	}
	number := func(i int, name string) (int64, bool) {
		v, ok := field(i, name)
		n, err := strconv.ParseInt(v, 10, 64)
		return n, ok && err == nil && n >= 0 && strconv.FormatInt(n, 10) == v
	}
	p := Promise{Origin: lines[0]}
	var ok [6]bool
	p.Index, ok[0] = number(2, "index")
	leaf, okLeaf := field(3, "leaf")
	raw, err := base64.StdEncoding.DecodeString(leaf)
	ok[1] = okLeaf && err == nil && len(raw) == len(p.Leaf)
	p.Kind, ok[2] = field(4, "kind")
	p.ID, ok[3] = field(5, "id")
	p.Received, ok[4] = number(6, "received")
	p.MergeBy, ok[5] = number(7, "merge-by")
	for _, v := range ok {
		if !v {
			return Promise{}, bad
		}
	}
	if p.MergeBy < p.Received {
		return Promise{}, bad
	}
	copy(p.Leaf[:], raw)
	return p, nil
}

// ParseHash decodes a hash in standard base64 or lowercase hex.
func ParseHash(s string) (Hash, error) {
	var h Hash
	var raw []byte
	var err error
	if len(s) == 64 {
		raw, err = hex.DecodeString(s)
	} else {
		raw, err = base64.StdEncoding.DecodeString(s)
	}
	if err != nil || len(raw) != 32 {
		return h, errors.New("tlog: malformed hash")
	}
	copy(h[:], raw)
	return h, nil
}
