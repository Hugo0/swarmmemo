package httpapi

import (
	"fmt"
	"testing"
)

func TestLimiterGroupsIPv6ByPrefix(t *testing.T) {
	l := NewLimiter()
	// One /64 walking through fresh addresses must share one bucket rather
	// than claiming a new table entry per address.
	for i := 0; i < 20000; i++ {
		l.Admit(fmt.Sprintf("2001:db8:1:2::%x", i))
	}
	if n := len(l.buckets); n != 1 {
		t.Fatalf("one /64 took %d table entries", n)
	}
	if l.Admit("2001:db8:1:2::ffff") {
		t.Fatal("the /64 kept its budget after spending it from many addresses")
	}
	if !l.Admit("2001:db8:1:3::1") {
		t.Fatal("a neighbouring /64 was refused")
	}
	for _, peer := range []string{"192.0.2.1", "192.0.2.2", "not-an-address"} {
		if got := limiterKey(peer); got != peer {
			t.Errorf("limiterKey(%q) = %q, want it unchanged", peer, got)
		}
	}
}
