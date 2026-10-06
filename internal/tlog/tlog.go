// Package tlog is SwarmMemo's transparency log arithmetic: RFC 6962 Merkle
// tree hashing, inclusion and consistency proofs over stored subtree hashes,
// and C2SP signed notes and checkpoints (signed tree heads). It holds no
// state: the board stores the hashes and calls these functions.
//
// Storage model: every complete, aligned subtree has one stored hash, at
// (level, index) where the subtree covers leaves [index<<level,
// (index+1)<<level). Level 0 is the leaf hashes. Appending leaf i stores
// (0,i) and then one hash per level while the new leaf completes a subtree,
// so a tree of n leaves stores fewer than 2n hashes, none ever rewritten.
// Any tree head or proof reads O(log n) of them.
package tlog

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/bits"
)

// Hash is a SHA-256 tree hash.
type Hash [32]byte

// HashReader reads stored subtree hashes (see the package comment).
type HashReader interface {
	ReadHash(level int, index int64) (Hash, error)
}

// HashReaderFunc adapts a function to HashReader.
type HashReaderFunc func(level int, index int64) (Hash, error)

func (f HashReaderFunc) ReadHash(level int, index int64) (Hash, error) { return f(level, index) }

// LeafHash is RFC 6962's leaf hash: SHA-256(0x00 || data).
func LeafHash(data []byte) Hash {
	h := sha256.New()
	h.Write([]byte{0})
	h.Write(data)
	var out Hash
	h.Sum(out[:0])
	return out
}

// NodeHash is RFC 6962's interior node hash: SHA-256(0x01 || left || right).
func NodeHash(left, right Hash) Hash {
	var buf [65]byte
	buf[0] = 1
	copy(buf[1:], left[:])
	copy(buf[33:], right[:])
	return sha256.Sum256(buf[:])
}

// EmptyRoot is the root of the empty tree: SHA-256 of the empty string.
var EmptyRoot = Hash(sha256.Sum256(nil))

// StoredHash is one hash to store at (Level, Index).
type StoredHash struct {
	Level int
	Index int64
	Hash  Hash
}

// AppendHashes returns the hashes to store when leaf index (with leaf hash
// leaf) is appended to a tree of exactly index leaves: (0,index) and every
// subtree the leaf completes. read supplies the left siblings, all stored
// earlier.
func AppendHashes(index int64, leaf Hash, read HashReader) ([]StoredHash, error) {
	if index < 0 {
		return nil, errors.New("tlog: negative index")
	}
	out := []StoredHash{{0, index, leaf}}
	h := leaf
	for level, i := 0, index; i&1 == 1; level, i = level+1, i>>1 {
		left, err := read.ReadHash(level, i-1)
		if err != nil {
			return nil, err
		}
		h = NodeHash(left, h)
		out = append(out, StoredHash{level + 1, i >> 1, h})
	}
	return out, nil
}

// largestPow2Below is the largest power of two strictly less than n (n > 1).
func largestPow2Below(n int64) int64 {
	return int64(1) << (bits.Len64(uint64(n-1)) - 1)
}

// subtree is MTH(D[lo:hi]) for a range whose lo is aligned to the largest
// power of two not above hi-lo, which every range RFC 6962 asks for is.
func subtree(lo, hi int64, read HashReader) (Hash, error) {
	n := hi - lo
	if n <= 0 {
		return Hash{}, errors.New("tlog: empty subtree")
	}
	if n&(n-1) == 0 {
		level := bits.TrailingZeros64(uint64(n))
		if lo&(n-1) != 0 {
			return Hash{}, errors.New("tlog: unaligned subtree")
		}
		return read.ReadHash(level, lo>>level)
	}
	k := largestPow2Below(n)
	left, err := subtree(lo, lo+k, read)
	if err != nil {
		return Hash{}, err
	}
	right, err := subtree(lo+k, hi, read)
	if err != nil {
		return Hash{}, err
	}
	return NodeHash(left, right), nil
}

// TreeHash is the root of the first n leaves.
func TreeHash(n int64, read HashReader) (Hash, error) {
	if n < 0 {
		return Hash{}, errors.New("tlog: negative size")
	}
	if n == 0 {
		return EmptyRoot, nil
	}
	return subtree(0, n, read)
}

// InclusionProof is RFC 6962 PATH(index, D[0:n]): the audit path proving
// leaf index is in the tree of size n.
func InclusionProof(index, n int64, read HashReader) ([]Hash, error) {
	if index < 0 || index >= n {
		return nil, fmt.Errorf("tlog: leaf %d is not in a tree of size %d", index, n)
	}
	var proof []Hash
	var walk func(m, lo, hi int64) error
	walk = func(m, lo, hi int64) error {
		if hi-lo == 1 {
			return nil
		}
		k := largestPow2Below(hi - lo)
		var sibling Hash
		var err error
		if m < k {
			if err = walk(m, lo, lo+k); err == nil {
				sibling, err = subtree(lo+k, hi, read)
			}
		} else {
			if err = walk(m-k, lo+k, hi); err == nil {
				sibling, err = subtree(lo, lo+k, read)
			}
		}
		proof = append(proof, sibling)
		return err
	}
	if err := walk(index, 0, n); err != nil {
		return nil, err
	}
	return proof, nil
}

// ConsistencyProof is RFC 6962 PROOF(m, D[0:n]): it proves the tree of size m
// is a prefix of the tree of size n. It is empty when m is 0 or m equals n.
func ConsistencyProof(m, n int64, read HashReader) ([]Hash, error) {
	if m < 0 || m > n {
		return nil, fmt.Errorf("tlog: no consistency proof from %d to %d", m, n)
	}
	if m == 0 || m == n {
		return []Hash{}, nil
	}
	var proof []Hash
	var walk func(m, lo, hi int64, complete bool) error
	walk = func(m, lo, hi int64, complete bool) error {
		if m == hi-lo {
			if complete {
				return nil
			}
			h, err := subtree(lo, hi, read)
			proof = append(proof, h)
			return err
		}
		k := largestPow2Below(hi - lo)
		var h Hash
		var err error
		if m <= k {
			if err = walk(m, lo, lo+k, complete); err == nil {
				h, err = subtree(lo+k, hi, read)
			}
		} else {
			if err = walk(m-k, lo+k, hi, false); err == nil {
				h, err = subtree(lo, lo+k, read)
			}
		}
		proof = append(proof, h)
		return err
	}
	if err := walk(m, 0, n, true); err != nil {
		return nil, err
	}
	return proof, nil
}

// ErrProof is a proof that does not verify.
var ErrProof = errors.New("tlog: proof does not verify")

// VerifyInclusion checks an inclusion proof (RFC 9162 §2.1.3.2).
func VerifyInclusion(index, n int64, leaf Hash, proof []Hash, root Hash) error {
	if index < 0 || index >= n {
		return ErrProof
	}
	fn, sn := index, n-1
	r := leaf
	for _, p := range proof {
		if sn == 0 {
			return ErrProof
		}
		if fn&1 == 1 || fn == sn {
			r = NodeHash(p, r)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			r = NodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 || r != root {
		return ErrProof
	}
	return nil
}

// VerifyConsistency checks a consistency proof between the tree of size m
// with root rootM and the tree of size n with root rootN (RFC 9162 §2.1.4.2).
func VerifyConsistency(m, n int64, proof []Hash, rootM, rootN Hash) error {
	switch {
	case m < 0 || m > n:
		return ErrProof
	case m == n:
		if len(proof) != 0 || rootM != rootN {
			return ErrProof
		}
		return nil
	case m == 0:
		if len(proof) != 0 || rootM != EmptyRoot {
			return ErrProof
		}
		return nil
	case len(proof) == 0:
		return ErrProof
	}
	if m&(m-1) == 0 {
		proof = append([]Hash{rootM}, proof...)
	}
	fn, sn := m-1, n-1
	for fn&1 == 1 {
		fn >>= 1
		sn >>= 1
	}
	fr, sr := proof[0], proof[0]
	for _, c := range proof[1:] {
		if sn == 0 {
			return ErrProof
		}
		if fn&1 == 1 || fn == sn {
			fr = NodeHash(c, fr)
			sr = NodeHash(c, sr)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			sr = NodeHash(sr, c)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 || fr != rootM || sr != rootN {
		return ErrProof
	}
	return nil
}
