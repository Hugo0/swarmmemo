package board

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/services/servicestest"
)

// The x402 relay reaches the network only through the webhook dialer: loopback,
// private, link-local and metadata addresses are refused after resolution.
func TestX402UsesTheWebhookDialer(t *testing.T) {
	s := openTest(t, Config{Features: Features{Services: []string{"x402"}}})
	d := s.serviceDeps()
	if d.DB == nil || d.Dial == nil {
		t.Fatal("x402 needs the database and the SSRF-safe dialer")
	}
	for _, addr := range []string{"127.0.0.1:443", "10.0.0.1:443", "169.254.169.254:80", "[::1]:443", "192.168.1.1:443"} {
		conn, err := d.Dial(testContext, "tcp", addr)
		if err == nil {
			conn.Close()
			t.Fatalf("%s was dialled", addr)
		}
		if code, _ := svcErr(err); !strings.Contains(code, "webhook_address_blocked") {
			t.Fatalf("%s: %v", addr, err)
		}
	}
}

// SERVICES naming x402 without a config lists the service but pays nothing:
// every call is service_unavailable and nothing is charged.
func TestX402UnconfiguredIsUnavailable(t *testing.T) {
	s := openTest(t, Config{Features: Features{Services: []string{"x402"}}})
	meter := servicestest.NewMeter(1 << 30)
	s.UseServiceMeter(meter, &servicestest.Params{})
	t.Cleanup(s.stopServices)
	list := run(t, s, Command{Operation: "services.list"})
	if !strings.Contains(svcFieldString(t, list.Data), `"id":"x402"`) {
		t.Fatalf("x402 must be listed: %+v", list.Data)
	}
	fails(t, s, svcCall(keyFor(1), "x402", "call", map[string]any{"resource": "anything"}, 1<<20, "x1"), "service_unavailable")
	fails(t, s, svcRead(keyFor(1), "x402", "resources", map[string]any{}), "service_unavailable")
	if len(svcSpends(t, s, meter)) != 0 {
		t.Fatal("nothing may be charged")
	}
}

func svcFieldString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
