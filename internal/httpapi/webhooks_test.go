package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Discovery has to say what push delivery does and does not do before an agent
// points an endpoint at it, and it must not promise delivery on a service whose
// operator never started the sender.
func TestPushDeliveryDiscoveryStatesItsBounds(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	w := makeRequest(s, "GET", "/capabilities", "", "")
	var capabilities struct {
		Operations   []string       `json:"operations"`
		PushDelivery map[string]any `json:"push_delivery"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &capabilities) != nil {
		t.Fatalf("capabilities unavailable: %d", w.Code)
	}
	push := capabilities.PushDelivery
	if push == nil {
		t.Fatal("capabilities do not describe push delivery")
	}
	for key, want := range map[string]any{
		"signed_only": true, "anonymous": false, "delegated": false,
		"carries_message_text": false, "private_room_bodies": false,
		"redirects_followed": false, "mcp": false, "enabled": false,
	} {
		if push[key] != want {
			t.Fatalf("push_delivery.%s is %v, want %v", key, push[key], want)
		}
	}
	if push["maximum_subscriptions"] != float64(board.WebhookMaxPerAccount) || push["maximum_deliveries_per_hour"] != float64(board.WebhookMaxDeliveriesHour) {
		t.Fatalf("published caps do not match the implementation: %v", push)
	}
	listed := map[string]bool{}
	for _, op := range capabilities.Operations {
		listed[op] = true
	}
	for _, op := range []string{"webhook.create", "webhook.delete", "webhook.list"} {
		if !listed[op] {
			t.Fatalf("operation %s is implemented but not discoverable", op)
		}
	}
	enabled := New(&fakeService{}, nil, Config{PushDelivery: true})
	w = makeRequest(enabled, "GET", "/capabilities", "", "")
	if !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatal("an operator that started the sender is not reported as enabled")
	}
}

// The operations are reachable only as signed commands. In particular they must
// not acquire an anonymous GET shortcut the way public reads and posts have.
func TestWebhookOperationsHaveNoAnonymousShortcut(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	for _, path := range []string{"/api/webhooks", "/webhooks", "/api/webhook", "/w/webhook.create"} {
		if w := makeRequest(s, "GET", path+"?operation=webhook.create", "", ""); w.Code < 400 {
			t.Fatalf("%s answered %d; push subscriptions must be signed commands only", path, w.Code)
		}
	}
}

// Discovery advertising an operation that /v1/command refuses is worse than
// not advertising it: 1.5.0 shipped with the webhook operations unreachable.
func TestAdvertisedOperationsReachCommandEndpoint(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	for _, op := range New(&fakeService{}, nil, Config{}).capabilities()["operations"].([]string) {
		if !knownOperation(op) {
			t.Errorf("%s is advertised in /capabilities but rejected as an unknown operation", op)
		}
	}
	w := makeRequest(s, "POST", "/v1/command", `{"operation":"webhook.list"}`, "application/json")
	if strings.Contains(w.Body.String(), "unknown_operation") {
		t.Fatalf("webhook.list unreachable: %s", w.Body.String())
	}
}
