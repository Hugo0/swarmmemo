package nostr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// keyFile is the bridge key on disk. The public fields are for the operator;
// only secret_key is trusted, and the public key is re-derived and compared.
type keyFile struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	PublicKey string `json:"public_key"`
	Npub      string `json:"npub"`
	SecretKey string `json:"secret_key"`
}

const keyFileType = "nostr-secp256k1"

// WriteKeyFile creates path (it must not exist) with mode 0600 and a new key,
// and returns the npub. The secret is never printed.
func WriteKeyFile(path string) (string, error) {
	k, err := GenerateKey()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	npub := Npub(k.PublicHex())
	err = json.NewEncoder(f).Encode(keyFile{Version: 1, Type: keyFileType, PublicKey: k.PublicHex(), Npub: npub, SecretKey: k.Hex()})
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return npub, nil
}

// LoadKeyFile reads a key written by WriteKeyFile. It refuses a file any
// group or other user can read or write.
func LoadKeyFile(path string) (Key, error) {
	f, err := os.Open(path)
	if err != nil {
		return Key{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Key{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Key{}, fmt.Errorf("nostr key file %s must be a regular file with mode 0600", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return Key{}, err
	}
	var kf keyFile
	if err = json.Unmarshal(raw, &kf); err != nil || kf.Version != 1 || kf.Type != keyFileType {
		return Key{}, errors.New("nostr key file is not a version 1 nostr-secp256k1 key")
	}
	k, err := KeyFromHex(kf.SecretKey)
	if err != nil {
		return Key{}, err
	}
	if kf.PublicKey != "" && kf.PublicKey != k.PublicHex() {
		return Key{}, errors.New("nostr key file public_key does not match its secret_key")
	}
	return k, nil
}
