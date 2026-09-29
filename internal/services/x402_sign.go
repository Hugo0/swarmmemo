package services

// EIP-712 typed-data signing of EIP-3009 transferWithAuthorization, the
// payment the x402 "exact" scheme uses on EVM chains. Keccak-256 is
// implemented here (the standard library has only FIPS SHA-3, which pads
// differently); secp256k1 signing is btcec's, the module the Nostr bridge
// already uses. No new dependency.

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"math/big"
	"math/bits"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	btcecdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// keccak256 is the legacy Keccak-256 Ethereum uses (domain byte 0x01).
func keccak256(parts ...[]byte) [32]byte {
	return keccakSponge(0x01, parts...)
}

var keccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

var keccakRotc = [24]int{1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44}
var keccakPiln = [24]int{10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1}

// keccakF is the Keccak-f[1600] permutation.
func keccakF(st *[25]uint64) {
	var bc [5]uint64
	for r := 0; r < 24; r++ {
		for i := 0; i < 5; i++ {
			bc[i] = st[i] ^ st[i+5] ^ st[i+10] ^ st[i+15] ^ st[i+20]
		}
		for i := 0; i < 5; i++ {
			t := bc[(i+4)%5] ^ bits.RotateLeft64(bc[(i+1)%5], 1)
			for j := 0; j < 25; j += 5 {
				st[j+i] ^= t
			}
		}
		t := st[1]
		for i := 0; i < 24; i++ {
			j := keccakPiln[i]
			bc[0] = st[j]
			st[j] = bits.RotateLeft64(t, keccakRotc[i])
			t = bc[0]
		}
		for j := 0; j < 25; j += 5 {
			for i := 0; i < 5; i++ {
				bc[i] = st[j+i]
			}
			for i := 0; i < 5; i++ {
				st[j+i] ^= ^bc[(i+1)%5] & bc[(i+2)%5]
			}
		}
		st[0] ^= keccakRC[r]
	}
}

// keccakSponge is the 256-bit sponge (rate 136 bytes) with domain byte ds:
// 0x01 is Keccak-256, 0x06 is FIPS SHA3-256 (which the tests compare
// against the standard library to check the permutation).
func keccakSponge(ds byte, parts ...[]byte) [32]byte {
	const rate = 136
	var st [25]uint64
	var block [rate]byte
	n := 0
	absorb := func() {
		for i := 0; i < rate/8; i++ {
			st[i] ^= binary.LittleEndian.Uint64(block[i*8:])
		}
		keccakF(&st)
		block = [rate]byte{}
		n = 0
	}
	for _, p := range parts {
		for len(p) > 0 {
			c := copy(block[n:], p)
			n += c
			p = p[c:]
			if n == rate {
				absorb()
			}
		}
	}
	block[n] ^= ds
	block[rate-1] ^= 0x80
	absorb()
	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], st[i])
	}
	return out
}

// EVMAddress is a 20-byte account or contract address.
type EVMAddress [20]byte

// ParseEVMAddress reads "0x" followed by 40 hex digits. Mixed case must be a
// valid EIP-55 checksum; all-lower or all-upper is accepted as is.
func ParseEVMAddress(s string) (EVMAddress, bool) {
	var a EVMAddress
	if len(s) != 42 || (s[:2] != "0x" && s[:2] != "0X") {
		return a, false
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return a, false
	}
	copy(a[:], b)
	body := s[2:]
	if body != strings.ToLower(body) && body != strings.ToUpper(body) && a.String()[2:] != body {
		return a, false
	}
	return a, true
}

// String is the EIP-55 checksummed form.
func (a EVMAddress) String() string {
	lower := hex.EncodeToString(a[:])
	h := keccak256([]byte(lower))
	out := []byte("0x" + lower)
	for i := 0; i < 40; i++ {
		c := lower[i]
		nibble := h[i/2]
		if i%2 == 0 {
			nibble >>= 4
		}
		if c >= 'a' && c <= 'f' && nibble&0x0f >= 8 {
			out[i+2] = c - 32
		}
	}
	return string(out)
}

// EIP712Domain is the domain of an EIP-3009 token: its EIP-712 name and
// version (x402 carries them in the requirement's "extra"), chain and
// contract.
type EIP712Domain struct {
	Name, Version     string
	ChainID           int64
	VerifyingContract EVMAddress
}

var (
	eip712DomainType = keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	// TransferWithAuthorizationTypeHash is EIP-3009's
	// TRANSFER_WITH_AUTHORIZATION_TYPEHASH.
	TransferWithAuthorizationTypeHash = keccak256([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))
)

func word(n *big.Int) []byte {
	var w [32]byte
	n.FillBytes(w[:])
	return w[:]
}

func wordInt(n int64) []byte { return word(big.NewInt(n)) }

func wordAddress(a EVMAddress) []byte {
	var w [32]byte
	copy(w[12:], a[:])
	return w[:]
}

// Separator is the domain's hashStruct.
func (d EIP712Domain) Separator() [32]byte {
	name := keccak256([]byte(d.Name))
	version := keccak256([]byte(d.Version))
	return keccak256(eip712DomainType[:], name[:], version[:], wordInt(d.ChainID), wordAddress(d.VerifyingContract))
}

// TransferAuthorization is the EIP-3009 message. Value is in the token's
// atomic units; ValidAfter and ValidBefore are Unix seconds.
type TransferAuthorization struct {
	From, To                EVMAddress
	Value                   int64
	ValidAfter, ValidBefore int64
	Nonce                   [32]byte
}

// Digest is keccak256(0x1901 ‖ domain separator ‖ hashStruct(message)).
func (a TransferAuthorization) Digest(d EIP712Domain) [32]byte {
	sep := d.Separator()
	msg := keccak256(TransferWithAuthorizationTypeHash[:], wordAddress(a.From), wordAddress(a.To), wordInt(a.Value), wordInt(a.ValidAfter), wordInt(a.ValidBefore), a.Nonce[:])
	return keccak256([]byte{0x19, 0x01}, sep[:], msg[:])
}

// X402Signer signs EIP-3009 transfer authorizations for the x402 wallet. It
// signs nothing else: no raw digests, no transactions. A signer never
// returns, logs or formats its key.
type X402Signer interface {
	Address() EVMAddress
	SignAuthorization(d EIP712Domain, a TransferAuthorization) ([65]byte, error)
}

// keySigner holds a secp256k1 key in memory.
type keySigner struct {
	key  *btcec.PrivateKey
	addr EVMAddress
}

func (s *keySigner) Address() EVMAddress { return s.addr }

// String and GoString keep the key out of any %v, %+v or %#v.
func (s *keySigner) String() string   { return "x402 signer " + s.addr.String() }
func (s *keySigner) GoString() string { return s.String() }

var errSign = errors.New("x402: signing failed")

// SignAuthorization returns r ‖ s ‖ v with v 27 or 28, as EIP-3009 and the
// x402 "exact" payload expect. The authorization must be from this wallet.
func (s *keySigner) SignAuthorization(d EIP712Domain, a TransferAuthorization) ([65]byte, error) {
	var out [65]byte
	if a.From != s.addr || a.Value <= 0 || a.ValidBefore <= a.ValidAfter {
		return out, errSign
	}
	digest := a.Digest(d)
	return signDigest(s.key, digest)
}

func signDigest(key *btcec.PrivateKey, digest [32]byte) ([65]byte, error) {
	var out [65]byte
	compact := btcecdsa.SignCompact(key, digest[:], false) // [27+recid] ‖ r ‖ s, RFC 6979, low s
	if len(compact) != 65 || compact[0] < 27 || compact[0] > 28 {
		return out, errSign
	}
	copy(out[:64], compact[1:])
	out[64] = compact[0]
	return out, nil
}

// recoverAddress returns the address that signed digest (tests and the
// self-check after signing use it).
func recoverAddress(digest [32]byte, sig [65]byte) (EVMAddress, bool) {
	var compact [65]byte
	compact[0] = sig[64]
	copy(compact[1:], sig[:64])
	pub, _, err := btcecdsa.RecoverCompact(compact[:], digest[:])
	if err != nil {
		return EVMAddress{}, false
	}
	return pubAddress(pub), true
}

func pubAddress(pub *btcec.PublicKey) EVMAddress {
	h := keccak256(pub.SerializeUncompressed()[1:])
	var a EVMAddress
	copy(a[:], h[12:])
	return a
}

func signerFromKeyBytes(b []byte) (*keySigner, error) {
	if len(b) != 32 {
		return nil, errKeyFile
	}
	var n big.Int
	n.SetBytes(b)
	if n.Sign() == 0 || n.Cmp(btcec.S256().Params().N) >= 0 {
		return nil, errKeyFile
	}
	key, _ := btcec.PrivKeyFromBytes(b)
	return &keySigner{key: key, addr: pubAddress(key.PubKey())}, nil
}

// errKeyFile is the one error a key file produces: it never quotes the
// file's content or the reason a byte was refused.
var errKeyFile = errors.New("x402: wallet key file must be 64 hex digits (optionally 0x-prefixed), a valid secp256k1 key, a regular file readable by its owner only")

// LoadX402Signer reads the wallet key from path: 64 hex digits, optionally
// 0x-prefixed, surrounding whitespace ignored. The file must be a regular
// file that neither group nor others can read.
func LoadX402Signer(path string) (X402Signer, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("x402: wallet key file does not exist")
		}
		return nil, errKeyFile
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 256 {
		return nil, errKeyFile
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errKeyFile
	}
	defer clear(raw)
	text := strings.TrimSpace(string(raw))
	text = strings.TrimPrefix(strings.TrimPrefix(text, "0x"), "0X")
	if len(text) != 64 {
		return nil, errKeyFile
	}
	var key [32]byte
	defer clear(key[:])
	if _, err = hex.Decode(key[:], []byte(text)); err != nil {
		return nil, errKeyFile
	}
	return signerFromKeyBytes(key[:])
}

// WriteX402Key writes a fresh random wallet key to path (mode 0600, never
// overwriting) and returns its address. It is the operator's keygen: the
// wallet is a hot wallet that holds only the day's float.
func WriteX402Key(path string) (EVMAddress, error) {
	var key [32]byte
	defer clear(key[:])
	var s *keySigner
	for s == nil {
		if _, err := rand.Read(key[:]); err != nil {
			return EVMAddress{}, err
		}
		s, _ = signerFromKeyBytes(key[:])
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return EVMAddress{}, err
	}
	text := []byte(hex.EncodeToString(key[:]) + "\n")
	defer clear(text)
	_, err = f.Write(text)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return EVMAddress{}, err
	}
	return s.addr, nil
}
