package services

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestPersonalSignRoundTrip(t *testing.T) {
	key, _ := hex.DecodeString("0123456789012345678901234567890123456789012345678901234567890123")
	msg := []byte("example.org wants you to sign in with your Ethereum account:\n0x14791697260E4c9A71f18484C9f997B308e59325\n")
	addr, sig, err := SignPersonal(key, msg)
	if err != nil {
		t.Fatal(err)
	}
	// The ethers.js documentation's example key and its address.
	if addr.String() != "0x14791697260E4c9A71f18484C9f997B308e59325" {
		t.Fatalf("address %s", addr)
	}
	if got, ok := RecoverPersonalSign(msg, sig); !ok || got != addr {
		t.Fatalf("recover: %s %v", got, ok)
	}
	// v as 0/1 is accepted too.
	low := sig[:130] + map[string]string{"1b": "00", "1c": "01"}[sig[130:]]
	if got, ok := RecoverPersonalSign(msg, low); !ok || got != addr {
		t.Fatalf("recover with v 0/1: %s %v", got, ok)
	}
	// Another message recovers another address; malformed signatures fail.
	if got, ok := RecoverPersonalSign(append(msg, '!'), sig); ok && got == addr {
		t.Fatal("a changed message recovered the signer")
	}
	for _, bad := range []string{"", sig[2:], sig + "00", strings.ToUpper(sig[:2]) + sig[2:], sig[:130] + "1d", "0x" + strings.Repeat("zz", 65)} {
		if _, ok := RecoverPersonalSign(msg, bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
