package services

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// NotaryKeyFileName is the notary key file's name in DATA_DIR when
// NOTARY_KEY_FILE does not name another path.
const NotaryKeyFileName = "notary.key"

const notaryKeyFileType = "swarmmemo-notary-ed25519"

// notaryKeyFile is the key file: the Ed25519 seed and its public key, both
// base64url without padding (the agents' key form), and the key ID.
type notaryKeyFile struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	PublicKey string `json:"public_key"`
	KeyID     string `json:"key_id"`
	Seed      string `json:"seed"`
}

// LoadOrCreateNotaryKey reads the notary's signing key from path, the one key
// that signs notary receipts and run receipts. The key lives in its own file,
// never in the database, so database copies (backups, replicas) carry no
// signing key. When the file does not exist it is created with a new key,
// mode 0600, exclusively: two processes starting at once agree on one key.
// A file any group or other user can read or write is refused, as is one
// whose public key does not match its seed. The key is never printed.
func LoadOrCreateNotaryKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("notary key file: no path (set NOTARY_KEY_FILE)")
	}
	key, err := loadNotaryKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err = writeNotaryKey(path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return loadNotaryKey(path)
}

func writeNotaryKey(path string) error {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	// Written whole to a private temporary file, then linked into place: the
	// link fails if the key exists, so a reader never sees a partial file.
	f, err := os.CreateTemp(filepath.Dir(path), ".notary-key-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0o600)
	if err == nil {
		err = json.NewEncoder(f).Encode(notaryKeyFile{Version: 1, Type: notaryKeyFileType, PublicKey: base64.RawURLEncoding.EncodeToString(pub),
			KeyID: keyID(pub), Seed: base64.RawURLEncoding.EncodeToString(seed)})
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Link(tmp, path)
}

func loadNotaryKey(path string) (ed25519.PrivateKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("notary key file %s must be a regular file with mode 0600", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, err
	}
	var kf notaryKeyFile
	if err = StrictObject(raw, &kf); err != nil || kf.Version != 1 || kf.Type != notaryKeyFileType {
		return nil, fmt.Errorf("notary key file %s is not a version 1 %s key", path, notaryKeyFileType)
	}
	seed, err := base64.RawURLEncoding.DecodeString(kf.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("notary key file %s has a malformed seed", path)
	}
	key := ed25519.NewKeyFromSeed(seed)
	pub := key.Public().(ed25519.PublicKey)
	if kf.PublicKey != base64.RawURLEncoding.EncodeToString(pub) || kf.KeyID != keyID(pub) {
		return nil, fmt.Errorf("notary key file %s: public_key or key_id does not match its seed", path)
	}
	return key, nil
}
