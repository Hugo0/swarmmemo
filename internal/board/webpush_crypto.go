package board

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"swarmmemo/internal/b64"
)

// Web push message encryption and server identification, standard library
// only (C667, S5):
//
//   - RFC 8291 (Message Encryption for Web Push) over RFC 8188's aes128gcm
//     content coding: an ephemeral P-256 key per message, ECDH with the
//     browser's p256dh key, HKDF-SHA-256 keyed by its auth secret, one
//     record of AES-128-GCM.
//   - RFC 8292 (VAPID): an ES256 JWT naming the push service's origin, sent
//     as Authorization: vapid t=JWT, k=PUBLIC_KEY.
//
// The VAPID key is the operator's, read from a file (WEBPUSH_KEY_FILE); it
// never enters the database or a log.

const (
	// webPushRecordSize is the aes128gcm record size header: one record holds
	// the whole (small) payload.
	webPushRecordSize = 4096
	// webPushJWTSeconds is how long a VAPID token is valid; RFC 8292 caps it
	// at 24 hours.
	webPushJWTSeconds = 12 * 3600
	// webPushKeyFileBytes bounds the key file.
	webPushKeyFileBytes = 512
)

// vapidKey is the server's VAPID key pair.
type vapidKey struct {
	private *ecdsa.PrivateKey
	// public is the uncompressed P-256 point (65 bytes) in unpadded
	// base64url: what a browser passes as applicationServerKey.
	public string
}

// loadVAPIDKey reads WEBPUSH_KEY_FILE: one line, the P-256 private scalar
// (32 bytes) in unpadded base64url, the form web-push tools print. Blank
// lines and # comments are ignored. The file must be a regular file no group
// or other user can read or write; the key is never printed.
func loadVAPIDKey(path string) (*vapidKey, error) {
	how := " (create one with: umask 077; openssl ecparam -name prime256v1 -genkey -noout | openssl ec -outform DER 2>/dev/null | tail -c +8 | head -c 32 | basenc --base64url -w0 | tr -d = > " + path + ")"
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("WEBPUSH_KEY_FILE: %w%s", err, how)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("WEBPUSH_KEY_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("WEBPUSH_KEY_FILE %s must be a regular file with mode 0600", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, webPushKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("WEBPUSH_KEY_FILE: %w", err)
	}
	if len(raw) > webPushKeyFileBytes {
		return nil, fmt.Errorf("WEBPUSH_KEY_FILE %s is larger than %d bytes", path, webPushKeyFileBytes)
	}
	var line string
	for _, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if line != "" {
			return nil, fmt.Errorf("WEBPUSH_KEY_FILE %s must hold one key line", path)
		}
		line = l
	}
	scalar, err := b64.URLSize(line, 32)
	if err != nil {
		return nil, fmt.Errorf("WEBPUSH_KEY_FILE %s is not a 32-byte P-256 private key in unpadded base64url%s", path, how)
	}
	return newVAPIDKey(scalar)
}

// newVAPIDKey is the key pair of a P-256 private scalar.
func newVAPIDKey(scalar []byte) (*vapidKey, error) {
	private, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar)
	if err != nil {
		return nil, errors.New("WEBPUSH_KEY_FILE: not a valid P-256 private key")
	}
	public, err := private.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &vapidKey{private: private, public: base64.RawURLEncoding.EncodeToString(public)}, nil
}

// generateVAPIDKey makes a fresh key pair. Only tests call it: a deployment
// loads its key from WEBPUSH_KEY_FILE, so every browser subscription stays
// bound to one key across restarts.
func generateVAPIDKey() (*vapidKey, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	scalar, err := private.Bytes()
	if err != nil {
		return nil, err
	}
	return newVAPIDKey(scalar)
}

// authorization is the RFC 8292 Authorization header for a push to
// endpoint: an ES256 JWT for the endpoint's origin, valid until now +
// webPushJWTSeconds, naming subject (a mailto: or https: contact).
func (k *vapidKey) authorization(endpoint, subject string, now int64) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now + webPushJWTSeconds, "sub": subject})
	if err != nil {
		return "", err
	}
	input := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 is the raw R || S, each 32 bytes, not DER.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + input + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + k.public, nil
}

// encryptWebPush encrypts plaintext for a browser subscription (RFC 8291):
// uaPublic is its p256dh key (65 bytes), authSecret its auth secret (16
// bytes). The result is the aes128gcm body: salt, record size, the
// ephemeral key as keyid, then one record.
func encryptWebPush(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	if len(plaintext)+1+16 > webPushRecordSize-86 {
		return nil, errors.New("web push: payload too large for one record")
	}
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, err
	}
	ephemeral, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return nil, err
	}
	return sealWebPush(plaintext, ua, ephemeral, authSecret, salt)
}

// sealWebPush is encryptWebPush with its ephemeral key and salt given.
func sealWebPush(plaintext []byte, ua *ecdh.PublicKey, ephemeral *ecdh.PrivateKey, authSecret, salt []byte) ([]byte, error) {
	shared, err := ephemeral.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := ephemeral.PublicKey().Bytes()
	cek, nonce, err := webPushKeys(shared, authSecret, salt, ua.Bytes(), asPublic)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The last (only) record ends with the delimiter 0x02 and no padding.
	record := aead.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil)
	var out bytes.Buffer
	out.Write(salt)
	_ = binary.Write(&out, binary.BigEndian, uint32(webPushRecordSize))
	out.WriteByte(byte(len(asPublic)))
	out.Write(asPublic)
	out.Write(record)
	return out.Bytes(), nil
}

// webPushKeys derives the content-encryption key and nonce (RFC 8291 §3.4,
// RFC 8188 §2.2) from the ECDH secret, the auth secret, the salt and both
// public keys (the browser's, then the sender's ephemeral one).
func webPushKeys(shared, authSecret, salt, uaPublic, asPublic []byte) (cek, nonce []byte, err error) {
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	// IKM = HKDF(salt=auth_secret, ikm=ecdh_secret, info=key_info, 32).
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, keyInfo, 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}
