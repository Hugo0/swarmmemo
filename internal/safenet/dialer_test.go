package safenet

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// Dialer is the one resolve, check and connect path (Dial, fetch's dialer):
// any non-public answer refuses the host, and the address actually
// connected to is checked again, so a Target that swaps in another address
// cannot slip past the decision.
func TestDialerChecksEveryAnswerAndTheConnectedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	local := ln.Addr().String()
	ctx := context.Background()
	if _, err = Dial(ctx, "tcp", local, time.Second); !errors.Is(err, ErrBlocked) {
		t.Fatalf("Dial reached a loopback literal: %v", err)
	}
	mixed := Dialer{Timeout: time.Second, Lookup: func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")}, nil
	}}
	if _, err = mixed.DialContext(ctx, "tcp", "rebind.example:443"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a host with a private answer was not refused: %v", err)
	}
	none := Dialer{Lookup: func(context.Context, string) ([]net.IP, error) { return nil, nil }}
	if _, err = none.DialContext(ctx, "tcp", "nothing.example:443"); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("a host without addresses: %v", err)
	}
	// The answer is public; the address dialled is loopback: refused at connect.
	swapped := Dialer{Timeout: time.Second,
		Lookup: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil },
		Target: func(net.IP, string) string { return local }}
	if _, err = swapped.DialContext(ctx, "tcp", "public.example:443"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("the connected address was not checked: %v", err)
	}
	// With a Public that allows loopback (a test hook), the same dial connects.
	swapped.Public = func(net.IP) error { return nil }
	conn, err := swapped.DialContext(ctx, "tcp", "public.example:443")
	if err != nil {
		t.Fatalf("the hooks were not used: %v", err)
	}
	conn.Close()
}
