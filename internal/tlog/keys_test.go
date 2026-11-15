package tlog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func testSigner(t *testing.T, name string, seed byte) *NoteSigner {
	t.Helper()
	s, err := NewNoteSigner(name, ed25519.NewKeyFromSeed(append(make([]byte, 31), seed)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestKeyID: key_id is the hex SHA-256 of the raw public key (C983), the
// notary key_id's rule, and a verifier key names the same one.
func TestKeyID(t *testing.T) {
	s := testSigner(t, "example.com/log", 1)
	sum := sha256.Sum256(s.PublicKey())
	if got := KeyID(s.PublicKey()); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("KeyID = %s", got)
	}
	if got, err := VerifierKeyID(s.VerifierKey()); err != nil || got != KeyID(s.PublicKey()) {
		t.Fatalf("VerifierKeyID = %s, %v", got, err)
	}
}

// TestRotationNote: a rotation opens under the key it retires and is signed
// by the key it names; one signed only by the new key, a tampered body and a
// rotation from another key are all refused.
func TestRotationNote(t *testing.T) {
	old, next, other := testSigner(t, "example.com/log", 1), testSigner(t, "example.com/log", 2), testSigner(t, "example.com/log", 3)
	note, err := SignRotation(old, next, 42)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenRotation(note, old.VerifierKey())
	if err != nil {
		t.Fatal(err)
	}
	if r.From != KeyID(old.PublicKey()) || r.To != next.VerifierKey() || r.At != 42 || r.Origin != "example.com/log" {
		t.Fatalf("rotation %+v", r)
	}
	if _, err = OpenNote([]byte(note), next.VerifierKey()); err != nil {
		t.Fatalf("the new key's signature: %v", err)
	}
	if got := strings.Count(note, "\n— "); got != 2 {
		t.Fatalf("%d signature lines, want 2:\n%s", got, note)
	}
	// Signed by the new key only: no signature of the retired key.
	forged, _ := next.Sign(Rotation{Origin: "example.com/log", From: KeyID(old.PublicKey()), To: next.VerifierKey(), At: 42}.String())
	if _, err = OpenRotation(forged, old.VerifierKey()); err == nil {
		t.Fatal("a rotation the retired key never signed opened")
	}
	// Signed by the old key only.
	half, _ := old.Sign(Rotation{Origin: "example.com/log", From: KeyID(old.PublicKey()), To: next.VerifierKey(), At: 42}.String())
	if _, err = OpenRotation(half, old.VerifierKey()); err == nil {
		t.Fatal("a rotation its new key never signed opened")
	}
	if _, err = OpenRotation(strings.Replace(note, "at 42", "at 43", 1), old.VerifierKey()); err == nil {
		t.Fatal("a tampered rotation opened")
	}
	if _, err = OpenRotation(note, other.VerifierKey()); err == nil {
		t.Fatal("a rotation opened under a key it does not retire")
	}
	// A rotation body is neither a checkpoint nor a promise.
	if _, err = ParseCheckpoint(r.String()); err == nil {
		t.Fatal("a rotation parsed as a checkpoint")
	}
	if _, err = ParsePromise(r.String()); err == nil {
		t.Fatal("a rotation parsed as a promise")
	}
}
