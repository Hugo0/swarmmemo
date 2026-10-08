package tlog

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memTree stores hashes in memory the way the board stores them in SQLite.
type memTree struct {
	hashes map[[2]int64]Hash
	size   int64
}

func (t *memTree) ReadHash(level int, index int64) (Hash, error) {
	h, ok := t.hashes[[2]int64{int64(level), index}]
	if !ok {
		return Hash{}, fmt.Errorf("missing hash %d/%d", level, index)
	}
	return h, nil
}

func (t *memTree) append(data []byte) error {
	if t.hashes == nil {
		t.hashes = map[[2]int64]Hash{}
	}
	stored, err := AppendHashes(t.size, LeafHash(data), t)
	if err != nil {
		return err
	}
	for _, s := range stored {
		t.hashes[[2]int64{int64(s.Level), s.Index}] = s.Hash
	}
	t.size++
	return nil
}

// naiveRoot is RFC 6962's MTH by its definition, independent of storage.
func naiveRoot(leaves [][]byte) Hash {
	if len(leaves) == 0 {
		return EmptyRoot
	}
	if len(leaves) == 1 {
		return LeafHash(leaves[0])
	}
	k := int(largestPow2Below(int64(len(leaves))))
	return NodeHash(naiveRoot(leaves[:k]), naiveRoot(leaves[k:]))
}

// The RFC 6962 test vectors (certificate-transparency-go and Trillian).
var rfcLeaves = []string{"", "00", "10", "2021", "3031", "40414243", "5051525354555657", "606162636465666768696a6b6c6d6e6f"}

var rfcRoots = []string{
	"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
	"fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
	"aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
	"d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7",
	"4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
	"76e67dadbcdf1e10e1b74ddc608abd2f98dfb16fbce75277b5232a127f2087ef",
	"ddb89be403809e325750d3d263cd78929c2942b7942a34b77e122c9594a74c8c",
	"5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hexes(hs []Hash) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = hex.EncodeToString(h[:])
	}
	return out
}

func TestRFC6962Vectors(t *testing.T) {
	if got := hex.EncodeToString(EmptyRoot[:]); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty root %s", got)
	}
	var tree memTree
	for i, leaf := range rfcLeaves {
		if err := tree.append(mustHex(t, leaf)); err != nil {
			t.Fatal(err)
		}
		root, err := TreeHash(tree.size, &tree)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(root[:]); got != rfcRoots[i] {
			t.Fatalf("root of %d leaves: got %s want %s", i+1, got, rfcRoots[i])
		}
	}
	// Inclusion paths (Trillian's merkle/testonly vectors).
	inclusion := []struct {
		index, size int64
		path        []string
	}{
		{0, 1, nil},
		{0, 8, []string{"96a296d224f285c67bee93c30f8a309157f0daa35dc5b87e410b78630a09cfc7", "5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e", "6b47aaf29ee3c2af9af889bc1fb9254dabd31177f16232dd6aab035ca39bf6e4"}},
		{5, 8, []string{"bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b", "ca854ea128ed050b41b35ffc1b87b8eb2bde461e9e3b5596ece6b9d5975a0ae0", "d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7"}},
		{2, 3, []string{"fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125"}},
		{1, 5, []string{"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d", "5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e", "bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b"}},
	}
	for _, c := range inclusion {
		proof, err := InclusionProof(c.index, c.size, &tree)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(hexes(proof), ",") != strings.Join(c.path, ",") {
			t.Fatalf("inclusion %d/%d: got %v want %v", c.index, c.size, hexes(proof), c.path)
		}
	}
	// Consistency proofs (Trillian's vectors).
	consistency := []struct {
		m, n  int64
		proof []string
	}{
		{1, 1, nil},
		{1, 8, []string{"96a296d224f285c67bee93c30f8a309157f0daa35dc5b87e410b78630a09cfc7", "5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e", "6b47aaf29ee3c2af9af889bc1fb9254dabd31177f16232dd6aab035ca39bf6e4"}},
		{6, 8, []string{"0ebc5d3437fbe2db158b9f126a1d118e308181031d0a949f8dededebc558ef6a", "ca854ea128ed050b41b35ffc1b87b8eb2bde461e9e3b5596ece6b9d5975a0ae0", "d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7"}},
		{2, 5, []string{"5f083f0a1a33ca076a95279832580db3e0ef4584bdff1f54c8a360f50de3031e", "bc1a0643b12e4d2d7c77918f44e0f4f79a838b6cf9ec5b5c283e1f4d88599e6b"}},
	}
	for _, c := range consistency {
		proof, err := ConsistencyProof(c.m, c.n, &tree)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(hexes(proof), ",") != strings.Join(c.proof, ",") {
			t.Fatalf("consistency %d→%d: got %v want %v", c.m, c.n, hexes(proof), c.proof)
		}
	}
}

// TestProofsRoundTrip checks every inclusion and consistency proof in trees
// up to 70 leaves against the naive root, and that a corrupted proof fails.
func TestProofsRoundTrip(t *testing.T) {
	var tree memTree
	var leaves [][]byte
	for n := int64(1); n <= 70; n++ {
		data := []byte(fmt.Sprintf("leaf %d", n-1))
		leaves = append(leaves, data)
		if err := tree.append(data); err != nil {
			t.Fatal(err)
		}
		root, err := TreeHash(n, &tree)
		if err != nil {
			t.Fatal(err)
		}
		if root != naiveRoot(leaves) {
			t.Fatalf("incremental root of %d differs from the naive root", n)
		}
		for i := int64(0); i < n; i++ {
			proof, err := InclusionProof(i, n, &tree)
			if err != nil {
				t.Fatal(err)
			}
			if err = VerifyInclusion(i, n, LeafHash(leaves[i]), proof, root); err != nil {
				t.Fatalf("inclusion %d/%d: %v", i, n, err)
			}
			if len(proof) > 0 {
				proof[0][0] ^= 1
				if VerifyInclusion(i, n, LeafHash(leaves[i]), proof, root) == nil {
					t.Fatalf("corrupted inclusion %d/%d verified", i, n)
				}
			}
			if VerifyInclusion(i, n, LeafHash([]byte("other")), proof, root) == nil {
				t.Fatal("wrong leaf verified")
			}
		}
		for m := int64(0); m <= n; m++ {
			proof, err := ConsistencyProof(m, n, &tree)
			if err != nil {
				t.Fatal(err)
			}
			rootM := naiveRoot(leaves[:m])
			if err = VerifyConsistency(m, n, proof, rootM, root); err != nil {
				t.Fatalf("consistency %d→%d: %v", m, n, err)
			}
			if m > 0 && m < n {
				bad := rootM
				bad[0] ^= 1
				if VerifyConsistency(m, n, proof, bad, root) == nil {
					t.Fatalf("consistency %d→%d verified a wrong old root", m, n)
				}
			}
		}
	}
}

// The signed-note vector from golang.org/x/mod/sumdb/note's documentation.
func TestNoteVector(t *testing.T) {
	skey := "AYEKFALVFGyNhPJEMzD1QIDr+Y7hfZx09iUvxdXHKDFz" // PRIVATE+KEY+PeterNeumann+c74f20a3+...
	raw, err := base64.StdEncoding.DecodeString(skey)
	if err != nil || raw[0] != 1 {
		t.Fatal(err)
	}
	signer, err := NewNoteSigner("PeterNeumann", ed25519.NewKeyFromSeed(raw[1:]))
	if err != nil {
		t.Fatal(err)
	}
	if got := signer.VerifierKey(); got != "PeterNeumann+c74f20a3+ARpc2QcUPDhMQegwxbzhKqiBfsVkmqq/LDE4izWy10TW" {
		t.Fatalf("verifier key %s", got)
	}
	text := "If you think cryptography is the answer to your problem,\nthen you don't know what your problem is.\n"
	note, err := signer.Sign(text)
	if err != nil {
		t.Fatal(err)
	}
	want := text + "\n— PeterNeumann x08go/ZJkuBS9UG/SffcvIAQxVBtiFupLLr8pAcElZInNIuGUgYN1FFYC2pZSNXgKvqfqdngotpRZb6KE6RyyBwJnAM=\n"
	if note != want {
		t.Fatalf("note:\n%s\nwant:\n%s", note, want)
	}
	got, err := OpenNote([]byte(note), signer.VerifierKey())
	if err != nil || got != text {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err = OpenNote([]byte(strings.Replace(note, "cryptography", "cryptographY", 1)), signer.VerifierKey()); err == nil {
		t.Fatal("tampered note opened")
	}
	other, _ := NewNoteSigner("Other", ed25519.NewKeyFromSeed(make([]byte, 32)))
	if _, err = OpenNote([]byte(note), other.VerifierKey()); err == nil {
		t.Fatal("note opened with another key")
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	c := Checkpoint{Origin: "swarmmemo.com/log", Size: 42}
	c.Root[3] = 7
	got, err := ParseCheckpoint(c.String())
	if err != nil || got != c {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{"", "o\n", "o\n01\nAAAA\n", "o\n-1\n" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n", "o\n1\nAAAA\n"} {
		if _, err = ParseCheckpoint(bad); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
}

// A promise round-trips, is strict about its lines, and never parses as a
// checkpoint: the same key signs both, so the bodies must not overlap.
func TestPromiseRoundTrip(t *testing.T) {
	p := Promise{Origin: "swarmmemo.com/log", Index: 482113, Kind: "message", ID: strings.Repeat("9f", 16), Received: 1791472440, MergeBy: 1791474240}
	p.Leaf[0], p.Leaf[31] = 0xde, 0xad
	body := p.String()
	got, err := ParsePromise(body)
	if err != nil || got != p {
		t.Fatalf("%+v %v", got, err)
	}
	if want := "swarmmemo.com/log\npromise/v1\nindex 482113\nleaf " + base64.StdEncoding.EncodeToString(p.Leaf[:]) + "\nkind message\nid " + p.ID + "\nreceived 1791472440\nmerge-by 1791474240\n"; body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", body, want)
	}
	if _, err = ParseCheckpoint(body); err == nil {
		t.Fatal("a promise parsed as a checkpoint")
	}
	if _, err = ParsePromise(Checkpoint{Origin: p.Origin, Size: 3}.String()); err == nil {
		t.Fatal("a checkpoint parsed as a promise")
	}
	for _, bad := range []string{
		"", body + "extra\n", strings.TrimSuffix(body, "\n"), strings.Replace(body, "promise/v1", "promise/v2", 1),
		strings.Replace(body, "index 482113", "index 0482113", 1), strings.Replace(body, "index 482113", "index -1", 1),
		strings.Replace(body, "kind message", "kind  message", 1), strings.Replace(body, "id ", "ID ", 1),
		strings.Replace(body, "leaf ", "leaf AAAA", 1), strings.Replace(body, "merge-by 1791474240", "merge-by 1", 1),
		strings.Replace(body, "received 1791472440\nmerge-by", "merge-by 1791474240\nreceived", 1),
	} {
		if _, err = ParsePromise(bad); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
	signer, _ := NewNoteSigner(p.Origin, ed25519.NewKeyFromSeed(make([]byte, 32)))
	note, err := signer.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := OpenNote([]byte(note), signer.VerifierKey()); err != nil || text != body {
		t.Fatalf("open: %q %v", text, err)
	}
}

func TestKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil || !k1.Equal(k2) {
		t.Fatal("key changed on reload", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadOrCreateKey(path); err == nil {
		t.Fatal("world-readable key accepted")
	}
}

func FuzzVerify(f *testing.F) {
	f.Add(int64(3), int64(8), int64(5), []byte("proof bytes that are 32 long....and more"))
	f.Fuzz(func(t *testing.T, index, n, m int64, raw []byte) {
		var proof []Hash
		for len(raw) >= 32 && len(proof) < 70 {
			var h Hash
			copy(h[:], raw)
			proof, raw = append(proof, h), raw[32:]
		}
		_ = VerifyInclusion(index, n, Hash{}, proof, Hash{})
		_ = VerifyConsistency(m, n, proof, Hash{}, Hash{})
	})
}

func FuzzOpenNote(f *testing.F) {
	signer, _ := NewNoteSigner("swarmmemo.com/log", ed25519.NewKeyFromSeed(make([]byte, 32)))
	note, _ := signer.Sign(Checkpoint{Origin: "swarmmemo.com/log", Size: 1}.String())
	f.Add([]byte(note))
	vkey := signer.VerifierKey()
	f.Fuzz(func(t *testing.T, note []byte) {
		text, err := OpenNote(note, vkey)
		if err == nil {
			_, _ = ParseCheckpoint(text)
			_, _ = ParsePromise(text)
		}
	})
}
