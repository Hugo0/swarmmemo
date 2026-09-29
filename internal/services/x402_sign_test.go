package services

import (
	"crypto/sha3"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hex32(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		t.Fatalf("bad hex32 %q", s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func mustAddress(t *testing.T, s string) EVMAddress {
	t.Helper()
	a, ok := ParseEVMAddress(s)
	if !ok {
		t.Fatalf("bad address %q", s)
	}
	return a
}

// The permutation and absorb logic are checked against the standard library's
// FIPS SHA3-256 (same sponge, domain byte 0x06) at every length across three
// blocks, split into parts at random points.
func TestKeccakSpongeMatchesSHA3(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for n := 0; n <= 3*136+5; n++ {
		msg := make([]byte, n)
		for i := range msg {
			msg[i] = byte(r.IntN(256))
		}
		cut := 0
		if n > 0 {
			cut = r.IntN(n + 1)
		}
		if got, want := keccakSponge(0x06, msg[:cut], msg[cut:]), sha3.Sum256(msg); got != want {
			t.Fatalf("length %d: sponge %x, sha3 %x", n, got, want)
		}
	}
}

func TestKeccak256Vectors(t *testing.T) {
	for in, want := range map[string]string{
		"":    "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
		"abc": "4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45",
		// EIP-3009's TRANSFER_WITH_AUTHORIZATION_TYPEHASH, as the USDC contract declares it.
		"TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)": "7c7c6cdb67a18743f49ec6fa9b35f50d52ed05cbed4cc592e13b44501c1a2267",
	} {
		if got := keccak256([]byte(in)); hex.EncodeToString(got[:]) != want {
			t.Errorf("keccak256(%q) = %x, want %s", in, got, want)
		}
	}
}

// The EIP-712 reference example (ethereum/EIPs assets/eip-712/Example.js):
// the domain separator, the signing digest, the key keccak256("cow"), its
// address and the exact v, r, s.
func TestEIP712ReferenceVector(t *testing.T) {
	d := EIP712Domain{Name: "Ether Mail", Version: "1", ChainID: 1, VerifyingContract: mustAddress(t, "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC")}
	if sep := d.Separator(); sep != hex32(t, "f2cee375fa42b42143804025fc449deafd50cc031ca257e0b194a650a912090f") {
		t.Fatalf("domain separator %x", sep)
	}
	personType := keccak256([]byte("Person(string name,address wallet)"))
	mailType := keccak256([]byte("Mail(Person from,Person to,string contents)Person(string name,address wallet)"))
	person := func(name, wallet string) []byte {
		n := keccak256([]byte(name))
		h := keccak256(personType[:], n[:], wordAddress(mustAddress(t, wallet)))
		return h[:]
	}
	contents := keccak256([]byte("Hello, Bob!"))
	msg := keccak256(mailType[:], person("Cow", "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826"), person("Bob", "0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB"), contents[:])
	if msg != hex32(t, "c52c0ee5d84264471806290a3f2c4cecfc5490626bf912d01f240d7a274b371e") {
		t.Fatalf("message hash %x", msg)
	}
	sep := d.Separator()
	digest := keccak256([]byte{0x19, 0x01}, sep[:], msg[:])
	if digest != hex32(t, "be609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2") {
		t.Fatalf("digest %x", digest)
	}
	cow := keccak256([]byte("cow"))
	s, err := signerFromKeyBytes(cow[:])
	if err != nil {
		t.Fatal(err)
	}
	if s.Address().String() != "0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826" {
		t.Fatalf("address %s", s.Address())
	}
	sig, err := signDigest(s.key, digest)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(sig[:32]) != "4355c47d63924e8a72e509b65029052eb6c299d53a04e167c5775fd466751c9d" ||
		hex.EncodeToString(sig[32:64]) != "07299936d304c153f6443dfa05f40ff007d72911b6f72307f996231605b91562" || sig[64] != 28 {
		t.Fatalf("signature %x", sig)
	}
	if got, ok := recoverAddress(digest, sig); !ok || got != s.Address() {
		t.Fatalf("recovered %s", got)
	}
}

// The x402 specification's own exact-EVM example (specs/schemes/exact/
// scheme_exact_evm.md): its signature over its authorization, on Base
// Sepolia USDC (name "USDC", version "2"), recovers to its "from" address.
func TestX402SpecExampleSignatureRecovers(t *testing.T) {
	d := EIP712Domain{Name: "USDC", Version: "2", ChainID: 84532, VerifyingContract: mustAddress(t, "0x036CbD53842c5426634e7929541eC2318f3dCF7e")}
	a := TransferAuthorization{
		From: mustAddress(t, "0x857b06519E91e3A54538791bDbb0E22373e36b66"), To: mustAddress(t, "0x209693Bc6afc0C5328bA36FaF03C514EF312287C"),
		Value: 10000, ValidAfter: 1740672089, ValidBefore: 1740672154, Nonce: hex32(t, "f3746613c2d920b5fdabc0856f2aeb2d4f88ee6037b8cc5d04a71a4462f13480"),
	}
	raw, _ := hex.DecodeString("2d6a7588d6acca505cbf0d9a4a227e0c52c6c34008c8e8986a1283259764173608a2ce6496642e377d6da8dbbf5836e9bd15092f9ecab05ded3d6293af148b571c")
	var sig [65]byte
	copy(sig[:], raw)
	got, ok := recoverAddress(a.Digest(d), sig)
	if !ok || got != a.From {
		t.Fatalf("the specification's example signature recovers to %s, not its from address", got)
	}
}

func TestSignAuthorizationRoundTrip(t *testing.T) {
	// Hardhat's first development account: a public throwaway key.
	key, _ := hex.DecodeString("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	s, err := signerFromKeyBytes(key)
	if err != nil {
		t.Fatal(err)
	}
	if s.Address().String() != "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" {
		t.Fatalf("address %s", s.Address())
	}
	d := EIP712Domain{Name: "USD Coin", Version: "2", ChainID: 8453, VerifyingContract: mustAddress(t, "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")}
	a := TransferAuthorization{From: s.Address(), To: mustAddress(t, "0x209693Bc6afc0C5328bA36FaF03C514EF312287C"), Value: 1000, ValidAfter: 1, ValidBefore: 2, Nonce: [32]byte{7}}
	sig, err := s.SignAuthorization(d, a)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := recoverAddress(a.Digest(d), sig); !ok || got != s.Address() {
		t.Fatalf("recovered %s", got)
	}
	for _, bad := range []TransferAuthorization{
		{From: EVMAddress{1}, To: a.To, Value: 1, ValidAfter: 1, ValidBefore: 2},
		{From: a.From, To: a.To, Value: 0, ValidAfter: 1, ValidBefore: 2},
		{From: a.From, To: a.To, Value: 1, ValidAfter: 2, ValidBefore: 2},
	} {
		if _, err := s.SignAuthorization(d, bad); err == nil {
			t.Fatalf("signed %+v", bad)
		}
	}
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := strings.ToLower(sprintf(f, s)); strings.Contains(out, "ac0974bec39a17e3") {
			t.Fatalf("%s leaks the key: %s", f, out)
		}
	}
}

func TestEIP55(t *testing.T) {
	for _, s := range []string{"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359", "0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB", "0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb"} {
		a, ok := ParseEVMAddress(s)
		if !ok || a.String() != s {
			t.Errorf("%s -> %s %v", s, a, ok)
		}
	}
	for _, s := range []string{"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD", "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed00", "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beae", "0xzaaeb6053f3e94c9b9a09f33669435e7ef1beaed"} {
		if _, ok := ParseEVMAddress(s); ok {
			t.Errorf("%s must be refused", s)
		}
	}
}

func TestLoadX402Signer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k")
	addr, err := WriteX402Key(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = WriteX402Key(path); err == nil {
		t.Fatal("keygen must never overwrite")
	}
	s, err := LoadX402Signer(path)
	if err != nil || s.Address() != addr {
		t.Fatalf("load: %v %v", s, err)
	}
	secret := "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	for name, c := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"group readable": {secret, 0o640},
		"short":          {secret[:60], 0o600},
		"not hex":        {"zz" + secret[2:], 0o600},
		"zero":           {strings.Repeat("0", 64), 0o600},
		"order":          {"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 0o600},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(p, []byte(c.body), c.mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, c.mode)
		_, err := LoadX402Signer(p)
		if err == nil {
			t.Fatalf("%s: loaded", name)
		}
		if strings.Contains(err.Error(), c.body[:8]) || strings.Contains(err.Error(), "zz") {
			t.Fatalf("%s: error quotes the file: %v", name, err)
		}
	}
	p := filepath.Join(dir, "ok0x")
	_ = os.WriteFile(p, []byte("  0x"+secret+"\n"), 0o600)
	if s, err := LoadX402Signer(p); err != nil || s.Address().String() != "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266" {
		t.Fatalf("0x-prefixed key: %v %v", s, err)
	}
}

func sprintf(format string, v any) string { return fmt.Sprintf(format, v) }
