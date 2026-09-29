package safenet

// Security review 1.20 regression test. It inverts the proof of concept
// TestSecPoC_SafenetPassesEmbeddedIPv4Forms (branch security-review-1.20):
// it fails while the weakness is present.

import (
	"net"
	"testing"
)

// L11: IPv6 forms that embed a private IPv4 address, which a NAT64 or SIIT
// gateway would translate, are refused; ordinary public addresses pass.
func TestSec120_EmbeddedIPv4FormsAreRefused(t *testing.T) {
	for _, s := range []string{"64:ff9b:1::a9fe:a9fe", "::127.0.0.1", "::ffff:0:10.0.0.1", "::169.254.169.254", "fec0::1"} {
		if err := PublicIP(net.ParseIP(s)); err == nil {
			t.Fatalf("%s counts as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if err := PublicIP(net.ParseIP(s)); err != nil {
			t.Fatalf("%s refused: %v", s, err)
		}
	}
}
