// Package ots reads and writes OpenTimestamps proofs and talks to public
// OpenTimestamps calendars: submit a SHA-256 digest (POST /digest), build the
// .ots file from the calendars' answers, and later upgrade its pending
// attestations into Bitcoin ones (GET /timestamp/COMMITMENT). Every input is
// bounded, so a hostile calendar answer cannot exhaust memory or recursion.
package ots

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Magic is the .ots file header, followed by the major version (1).
var Magic = []byte("\x00OpenTimestamps\x00\x00Proof\x00\xbf\x89\xe2\xe8\x84\xe8\x92\x94")

const (
	// MaxFileBytes bounds a stored or parsed .ots file.
	MaxFileBytes = 64 << 10
	// MaxDepth bounds a timestamp's nesting.
	MaxDepth = 256
	// maxOpBytes is the reference implementation's bound on an operation's
	// argument and result.
	maxOpBytes = 4096
	// maxURIBytes bounds a pending attestation's calendar URI.
	maxURIBytes = 1000
)

// Operation tags.
const (
	opSHA1    = 0x02
	opSHA256  = 0x08
	opAppend  = 0xf0
	opPrepend = 0xf1
	opReverse = 0xf2
	opHexlify = 0xf3
)

// Attestation tags.
var (
	TagPending = []byte{0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}
	TagBitcoin = []byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}
)

// Op is one operation: a tag and, for append and prepend, an argument.
type Op struct {
	Tag byte
	Arg []byte
}

// Apply runs the operation on msg.
func (o Op) Apply(msg []byte) ([]byte, error) {
	var out []byte
	switch o.Tag {
	case opSHA256:
		h := sha256.Sum256(msg)
		out = h[:]
	case opSHA1:
		h := sha1.Sum(msg)
		out = h[:]
	case opAppend:
		out = append(slices.Clip(msg), o.Arg...)
	case opPrepend:
		out = append(slices.Clone(o.Arg), msg...)
	case opReverse:
		out = slices.Clone(msg)
		slices.Reverse(out)
	case opHexlify:
		out = []byte(hex.EncodeToString(msg))
	default:
		return nil, fmt.Errorf("ots: unsupported operation 0x%02x", o.Tag)
	}
	if len(out) > maxOpBytes {
		return nil, errors.New("ots: operation result too long")
	}
	return out, nil
}

func (o Op) less(p Op) bool {
	if o.Tag != p.Tag {
		return o.Tag < p.Tag
	}
	return bytes.Compare(o.Arg, p.Arg) < 0
}

// Attestation is a tag and its payload (still varbytes-decoded once).
type Attestation struct {
	Tag     []byte
	Payload []byte
}

// PendingURI is a pending attestation's calendar URI, or "".
func (a Attestation) PendingURI() string {
	if !bytes.Equal(a.Tag, TagPending) {
		return ""
	}
	r := &reader{b: a.Payload}
	uri, err := r.varbytes(maxURIBytes)
	if err != nil || len(r.b) != 0 {
		return ""
	}
	return string(uri)
}

// BitcoinHeight is a Bitcoin attestation's block height, or 0.
func (a Attestation) BitcoinHeight() uint64 {
	if !bytes.Equal(a.Tag, TagBitcoin) {
		return 0
	}
	r := &reader{b: a.Payload}
	h, err := r.varuint()
	if err != nil {
		return 0
	}
	return h
}

// Branch is an operation and the timestamp of its result.
type Branch struct {
	Op    Op
	Stamp *Timestamp
}

// Timestamp proves a message: attestations of the message itself, and
// operations leading to further timestamps.
type Timestamp struct {
	Msg          []byte
	Attestations []Attestation
	Branches     []Branch
}

// Merge adds other's attestations and branches (for the same message).
func (t *Timestamp) Merge(other *Timestamp) error {
	if !bytes.Equal(t.Msg, other.Msg) {
		return errors.New("ots: merging timestamps of different messages")
	}
	for _, a := range other.Attestations {
		if !slices.ContainsFunc(t.Attestations, func(b Attestation) bool {
			return bytes.Equal(a.Tag, b.Tag) && bytes.Equal(a.Payload, b.Payload)
		}) {
			t.Attestations = append(t.Attestations, a)
		}
	}
	for _, b := range other.Branches {
		i := slices.IndexFunc(t.Branches, func(c Branch) bool { return c.Op.Tag == b.Op.Tag && bytes.Equal(c.Op.Arg, b.Op.Arg) })
		if i < 0 {
			t.Branches = append(t.Branches, b)
			continue
		}
		if err := t.Branches[i].Stamp.Merge(b.Stamp); err != nil {
			return err
		}
	}
	return nil
}

// Walk visits every timestamp in the tree, depth first.
func (t *Timestamp) Walk(visit func(*Timestamp)) {
	visit(t)
	for _, b := range t.Branches {
		b.Stamp.Walk(visit)
	}
}

// Status summarises a timestamp: its pending calendar URIs and the lowest
// Bitcoin block height attesting it (0 while none does).
func (t *Timestamp) Status() (pending []string, height uint64) {
	t.Walk(func(s *Timestamp) {
		for _, a := range s.Attestations {
			if uri := a.PendingURI(); uri != "" {
				pending = append(pending, uri)
			}
			if h := a.BitcoinHeight(); h > 0 && (height == 0 || h < height) {
				height = h
			}
		}
	})
	return pending, height
}

// BitcoinRoot is the message a Bitcoin attestation of block height attests,
// the block's Merkle root in header byte order; nil when none does.
func (t *Timestamp) BitcoinRoot(height uint64) []byte {
	var root []byte
	t.Walk(func(s *Timestamp) {
		for _, a := range s.Attestations {
			if root == nil && height > 0 && a.BitcoinHeight() == height {
				root = s.Msg
			}
		}
	})
	return root
}

// Serialize writes the timestamp in the reference implementation's order:
// attestations then operations, each sorted, all but the last prefixed 0xff.
func (t *Timestamp) Serialize(w *bytes.Buffer) {
	atts := slices.Clone(t.Attestations)
	slices.SortFunc(atts, func(a, b Attestation) int {
		if c := bytes.Compare(a.Tag, b.Tag); c != 0 {
			return c
		}
		return bytes.Compare(a.Payload, b.Payload)
	})
	branches := slices.Clone(t.Branches)
	slices.SortFunc(branches, func(a, b Branch) int {
		switch {
		case a.Op.less(b.Op):
			return -1
		case b.Op.less(a.Op):
			return 1
		}
		return 0
	})
	items := len(atts) + len(branches)
	n := 0
	for _, a := range atts {
		if n++; n < items {
			w.WriteByte(0xff)
		}
		w.WriteByte(0x00)
		w.Write(a.Tag)
		writeVarbytes(w, a.Payload)
	}
	for _, b := range branches {
		if n++; n < items {
			w.WriteByte(0xff)
		}
		w.WriteByte(b.Op.Tag)
		if b.Op.Tag == opAppend || b.Op.Tag == opPrepend {
			writeVarbytes(w, b.Op.Arg)
		}
		b.Stamp.Serialize(w)
	}
}

// ParseTimestamp parses a serialized timestamp of msg (a calendar answer)
// that must use all of data.
func ParseTimestamp(data, msg []byte) (*Timestamp, error) {
	if len(data) > MaxFileBytes {
		return nil, errors.New("ots: timestamp too large")
	}
	r := &reader{b: data}
	t, err := r.timestamp(msg, 0)
	if err != nil {
		return nil, err
	}
	if len(r.b) != 0 {
		return nil, errors.New("ots: trailing bytes after timestamp")
	}
	return t, nil
}

// File is a detached .ots proof of a SHA-256 digest.
type File struct {
	Digest []byte
	Stamp  *Timestamp
}

// Bytes is the .ots file.
func (f *File) Bytes() []byte {
	var w bytes.Buffer
	w.Write(Magic)
	w.WriteByte(0x01)
	w.WriteByte(opSHA256)
	w.Write(f.Digest)
	f.Stamp.Serialize(&w)
	return w.Bytes()
}

// ParseFile parses a .ots file for a SHA-256 digest.
func ParseFile(data []byte) (*File, error) {
	if len(data) > MaxFileBytes {
		return nil, errors.New("ots: file too large")
	}
	if !bytes.HasPrefix(data, Magic) {
		return nil, errors.New("ots: not an OpenTimestamps proof")
	}
	r := &reader{b: data[len(Magic):]}
	if v, err := r.varuint(); err != nil || v != 1 {
		return nil, errors.New("ots: unsupported version")
	}
	tag, err := r.byte()
	if err != nil || tag != opSHA256 {
		return nil, errors.New("ots: the file hash must be SHA-256")
	}
	digest, err := r.take(32)
	if err != nil {
		return nil, err
	}
	t, err := r.timestamp(digest, 0)
	if err != nil {
		return nil, err
	}
	if len(r.b) != 0 {
		return nil, errors.New("ots: trailing bytes after timestamp")
	}
	return &File{Digest: slices.Clone(digest), Stamp: t}, nil
}

// reader parses with a budget on the bytes of computed messages, so a small
// but wide timestamp cannot make the parser allocate without bound.
type reader struct {
	b      []byte
	budget int
}

// msgBudget bounds the computed messages' total size while parsing.
const msgBudget = 4 << 20

func (r *reader) byte() (byte, error) {
	if len(r.b) == 0 {
		return 0, errors.New("ots: truncated")
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c, nil
}

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || len(r.b) < n {
		return nil, errors.New("ots: truncated")
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out, nil
}

func (r *reader) varuint() (uint64, error) {
	var v uint64
	for shift := 0; shift < 64; shift += 7 {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("ots: varuint too long")
}

func (r *reader) varbytes(max int) ([]byte, error) {
	n, err := r.varuint()
	if err != nil {
		return nil, err
	}
	if n > uint64(max) {
		return nil, errors.New("ots: varbytes too long")
	}
	return r.take(int(n))
}

func (r *reader) timestamp(msg []byte, depth int) (*Timestamp, error) {
	if depth > MaxDepth {
		return nil, errors.New("ots: timestamp too deep")
	}
	t := &Timestamp{Msg: slices.Clone(msg)}
	item := func(tag byte) error {
		if tag == 0x00 {
			atag, err := r.take(8)
			if err != nil {
				return err
			}
			payload, err := r.varbytes(8192)
			if err != nil {
				return err
			}
			a := Attestation{Tag: slices.Clone(atag), Payload: slices.Clone(payload)}
			if bytes.Equal(a.Tag, TagPending) {
				uri := a.PendingURI()
				if uri == "" || !validURI(uri) {
					return errors.New("ots: malformed pending attestation")
				}
			}
			t.Attestations = append(t.Attestations, a)
			return nil
		}
		op := Op{Tag: tag}
		if tag == opAppend || tag == opPrepend {
			arg, err := r.varbytes(maxOpBytes)
			if err != nil {
				return err
			}
			if len(arg) == 0 {
				return errors.New("ots: empty operation argument")
			}
			op.Arg = slices.Clone(arg)
		}
		next, err := op.Apply(msg)
		if err != nil {
			return err
		}
		if r.budget += len(next); r.budget > msgBudget {
			return errors.New("ots: timestamp too large")
		}
		stamp, err := r.timestamp(next, depth+1)
		if err != nil {
			return err
		}
		t.Branches = append(t.Branches, Branch{Op: op, Stamp: stamp})
		return nil
	}
	tag, err := r.byte()
	if err != nil {
		return nil, err
	}
	for tag == 0xff {
		if tag, err = r.byte(); err != nil {
			return nil, err
		}
		if err = item(tag); err != nil {
			return nil, err
		}
		if tag, err = r.byte(); err != nil {
			return nil, err
		}
	}
	if err = item(tag); err != nil {
		return nil, err
	}
	return t, nil
}

// validURI is the reference implementation's rule for a calendar URI:
// URL-safe characters only.
func validURI(uri string) bool {
	for _, c := range uri {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/:-", c)) {
			return false
		}
	}
	return uri != ""
}

func writeVarbytes(w *bytes.Buffer, b []byte) {
	n := uint64(len(b))
	for n >= 0x80 {
		w.WriteByte(byte(n) | 0x80)
		n >>= 7
	}
	w.WriteByte(byte(n))
	w.Write(b)
}
