package board

import (
	"encoding/json"
	"path/filepath"
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
	fails(t, s, svcRead(nil, "x402", "frames_search", map[string]any{"query": "weather"}), "service_unavailable")
	fails(t, s, svcCall(keyFor(1), "x402", "call", map[string]any{"resource": "frames:mpp.weather"}, 1<<20, "x2"), "service_unavailable")
	if len(svcSpends(t, s, meter)) != 0 {
		t.Fatal("nothing may be charged")
	}
}

// A 1.23 database gains x402_vetted.reason on open, its rows kept and read
// as the operator's (an empty reason).
func TestX402VettedReasonMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("ALTER TABLE x402_vetted DROP COLUMN reason; INSERT INTO x402_vetted(id,url,method,pay_to,state,changed_at) VALUES('r','https://example.com/x','GET','0x1','vetted',1)"); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path, Config{}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if sqlCount(t, s, "SELECT count(*) FROM x402_vetted WHERE id='r' AND state='vetted' AND reason=''") != 1 {
		t.Fatal("the vetting row must survive the migration with an empty reason")
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
