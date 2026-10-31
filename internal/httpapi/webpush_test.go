package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

// C667: a hosted identity lists and removes its push browsers with
// manage_push; /capabilities says web push is off without a VAPID key, and
// why.
func TestManagePushAndCapabilities(t *testing.T) {
	_, s := hostedServer(t)
	token := newIdentity(t, s, "")["token"].(string)
	list := mustTool(t, s, "/mcp/t/"+token, "", "manage_push", map[string]any{})
	if dig(list, "data", "enabled") != false || !strings.Contains(dig(list, "data", "reason").(string), "no web push key") {
		t.Fatalf("manage_push list without a key: %v", list)
	}
	if _, failure := callTool(t, s, "/mcp/t/"+token, "", "manage_push", map[string]any{"action": "remove", "target": strings.Repeat("a", 32)}); !strings.Contains(failure, "push_subscription_not_found") {
		t.Fatalf("manage_push remove of nothing: %q", failure)
	}
	if _, failure := callTool(t, s, web.AssistantMCPPath+"/t/"+token, "", "manage_push", map[string]any{"action": "subscribe"}); !strings.Contains(failure, "action is list or remove") {
		t.Fatalf("manage_push unknown action: %q", failure)
	}
	var caps map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	wp, _ := caps["web_push"].(map[string]any)
	if wp == nil || wp["enabled"] != false || wp["reason"] == nil || wp["carries_message_text"] != false || wp["mcp_tool"] != "manage_push" {
		t.Fatalf("capabilities web_push: %v", wp)
	}
}

// The service worker is served at the root, as JavaScript, revalidated, and
// handles push and clicks only (no fetch handler).
func TestServiceWorkerServedAtRoot(t *testing.T) {
	s := pinServerWith(t, true)
	r := makeRequest(s, "GET", "/sw.js", "", "")
	body := r.Body.String()
	if r.Code != 200 || !strings.HasPrefix(r.Header().Get("Content-Type"), "text/javascript") || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("/sw.js: %d %v", r.Code, r.Header())
	}
	if !strings.Contains(body, "addEventListener('push'") || strings.Contains(body, "addEventListener('fetch'") {
		t.Fatal("/sw.js must handle push and never fetch")
	}
}
