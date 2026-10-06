package tlog

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

// KeyFileName is the log key file's name in DATA_DIR when LOG_KEY_FILE does
// not name another path.
const KeyFileName = "log.key"

const keyFileType = "swarmmemo-log-ed25519"

type keyFile struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	PublicKey string `json:"public_key"`
	Seed      string `json:"seed"`
}

// LoadOrCreateKey reads the log's signing key from path, creating it (mode
// 0600, exclusively, so two processes agree on one key) when it does not
// exist. The key lives only in this file, never in the database, so database
// copies carry no signing key. A file others can read is refused.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("log key file: no path (set LOG_KEY_FILE)")
	}
	key, err := loadKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err = writeKey(path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return loadKey(path)
}

func writeKey(path string) error {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	f, err := os.CreateTemp(filepath.Dir(path), ".log-key-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0o600)
	if err == nil {
		err = json.NewEncoder(f).Encode(keyFile{Version: 1, Type: keyFileType, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Seed: base64.RawURLEncoding.EncodeToString(seed)})
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

func loadKey(path string) (ed25519.PrivateKey, error) {
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
		return nil, fmt.Errorf("log key file %s must be a regular file with mode 0600", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, err
	}
	var kf keyFile
	if err = json.Unmarshal(raw, &kf); err != nil || kf.Version != 1 || kf.Type != keyFileType {
		return nil, fmt.Errorf("log key file %s is not a version 1 %s key", path, keyFileType)
	}
	seed, err := base64.RawURLEncoding.DecodeString(kf.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("log key file %s has a malformed seed", path)
	}
	key := ed25519.NewKeyFromSeed(seed)
	if kf.PublicKey != base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("log key file %s: public_key does not match its seed", path)
	}
	return key, nil
}
