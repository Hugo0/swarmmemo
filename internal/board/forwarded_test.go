package board

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestForwardedProvenanceComesOnlyFromTheBridgeContext(t *testing.T) {
	s := openTest(t, Config{ServiceID: "swarmmemo.com", MaxTextBytes: 1024, DailyBytes: 1 << 20, AnonymousDailyBytes: 1 << 20, GlobalDailyBytes: 1 << 24})
	f := Forwarded{Mode: "reissued", OriginService: "nostr", OriginID: strings.Repeat("ab", 32), OriginAuthor: "npub1xyz", OriginRef: "nostr:nevent1xyz"}
	res, err := s.Execute(WithForwarded(testContext, f), Command{Operation: "post", Text: "from nostr"}, "nostr:"+strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	native, err := s.Execute(testContext, Command{Operation: "post", Text: "native"}, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Execute(testContext, Command{Operation: "messages.list", Room: "lobby"}, "198.51.100.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got.Messages {
		switch m.ID {
		case res.Receipt.ID:
			if m.Forwarded == nil || *m.Forwarded != f || m.Author != "anonymous" || m.PublicKey != "" {
				t.Fatalf("bridged message: %+v", m)
			}
			encoded, _ := json.Marshal(m)
			if !strings.Contains(string(encoded), `"forwarded":{"mode":"reissued","origin_service":"nostr"`) {
				t.Fatalf("JSON: %s", encoded)
			}
		case native.Receipt.ID:
			if m.Forwarded != nil {
				t.Fatal("native post carries provenance")
			}
		}
	}
	// Hiding keeps who carried it, and removes the body.
	if err := s.Moderate(testContext, res.Receipt.ID, "test", true); err != nil {
		t.Fatal(err)
	}
	hidden, _ := s.Execute(testContext, Command{Operation: "message.get", MessageID: res.Receipt.ID}, "198.51.100.1")
	if m := hidden.Messages[0]; m.Text != "" || m.Forwarded == nil {
		t.Fatalf("tombstone: %+v", m)
	}
	// A signed post cannot be marked as bridged, and the record must be valid.
	key := keyFor(7)
	if _, err := s.Execute(WithForwarded(testContext, f), signed(key, Command{Operation: "post", Text: "signed"}), "192.0.2.1"); err == nil {
		t.Fatal("a signed post was stored as bridged")
	}
	for _, bad := range []Forwarded{
		{Mode: "verbatim", OriginService: "nostr", OriginID: "a", OriginAuthor: "b", OriginRef: "c"},
		{Mode: "reissued", OriginService: "Nostr", OriginID: "a", OriginAuthor: "b", OriginRef: "c"},
		{Mode: "reissued", OriginService: "nostr", OriginID: "a b", OriginAuthor: "b", OriginRef: "c"},
		{Mode: "reissued", OriginService: "nostr", OriginID: "a", OriginAuthor: "", OriginRef: "c"},
	} {
		if _, err := s.Execute(WithForwarded(testContext, bad), Command{Operation: "post", Text: "x"}, "192.0.2.1"); err == nil {
			t.Errorf("stored invalid provenance %+v", bad)
		}
	}
}
