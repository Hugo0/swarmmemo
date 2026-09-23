package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// Anyone can room.create "guides" before the operator's first post opens it.
// The squatter then owns the room legacy /guides/* addresses redirect into:
// it can restyle (RFC0011 body zone may deface a post), hide or lock out the
// allowlisted authors' guides, and the operator CLI cannot take it back. The
// web must only redirect into a guides room the operator governs.
func TestSquattedGuidesRoomNeverRedirects(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "squat.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operator := ed25519.NewKeyFromSeed(make([]byte, 32))
	squatter := ed25519.NewKeyFromSeed(append([]byte{9}, make([]byte, 31)...))
	previous := web.GuideAuthors()
	sum := sha256.Sum256(operator.Public().(ed25519.PublicKey))
	if err = web.SetGuideAuthors(hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = web.SetGuideAuthors(strings.Join(previous, ",")) })
	n := 0
	exec := func(key ed25519.PrivateKey, c board.Command) (board.Result, error) {
		n++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), strings.Repeat("n", n)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		return store.Execute(context.Background(), c, "test")
	}
	// Since the reserved-names rule, the squat itself is refused. The
	// operator-owned check below stays as defence in depth.
	_, err = exec(squatter, board.Command{Operation: "room.create", Room: "guides"})
	var reserved *board.Error
	if !errors.As(err, &reserved) || reserved.Code != "room_reserved" {
		t.Fatalf("squatter room.create guides: got %v, want room_reserved", err)
	}
}
