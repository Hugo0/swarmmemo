package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// TestReceiveDedupeEndToEnd: on a store opened through its migrations, a
// receiver with dedupe_header answers a repeated delivery 202 with
// duplicate: true and the first item, any header name and case working, and
// stores it once; list shows the setting.
func TestReceiveDedupeEndToEnd(t *testing.T) {
	_, h := receiverServer(t)
	seed := make([]byte, 32)
	seed[0] = 7
	key := ed25519.NewKeyFromSeed(seed)
	send := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(signService(key, c))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	created := send(board.Command{Operation: "service.call", Target: "receiver", Data: `{"schema":1,"method":"create","args":{"screen":false,"dedupe_header":"X-Delivery-Uuid"},"max_cost":5}`, RequestID: "dedupe-create"})
	url, _ := dig(created, "data", "result", "url").(string)
	if url == "" || dig(created, "data", "result", "receiver", "dedupe_header") != "x-delivery-uuid" {
		t.Fatalf("create: %+v", created)
	}
	post := func(value string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("POST", url, strings.NewReader(`{"event":"paid"}`))
		req.RemoteAddr = "198.51.100.8:12345"
		req.Header.Set("Content-Type", "application/json")
		if value != "" {
			req.Header.Set("x-delivery-UUID", value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 202 {
			t.Fatalf("deliver: %d %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first, dup, other := post("u-1"), post("u-1"), post("u-2")
	if first["duplicate"] != nil || dup["duplicate"] != true || dup["item"] != first["item"] || dup["ok"] != true || other["duplicate"] != nil || other["item"] == first["item"] {
		t.Fatalf("first %+v dup %+v other %+v", first, dup, other)
	}
	read := send(board.Command{Operation: "service.read", Target: "receiver", Data: `{"schema":1,"method":"items","args":{}}`})
	if list, _ := dig(read, "data", "result", "items").([]any); len(list) != 2 {
		t.Fatalf("stored once per value: %+v", read)
	}
	listed := send(board.Command{Operation: "service.read", Target: "receiver", Data: `{"schema":1,"method":"list","args":{}}`})
	active, _ := dig(listed, "data", "result", "active").([]any)
	if len(active) != 1 || dig(active[0], "dedupe_header") != "x-delivery-uuid" || dig(active[0], "duplicates") != float64(1) || dig(active[0], "deliveries") != float64(2) {
		t.Fatalf("list: %+v", listed)
	}
}
