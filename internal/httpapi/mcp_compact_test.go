package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// An MCP read returns compact messages: no signed fields and no page-only
// fields, in the structured content and in the JSON text a model reads;
// full=true restores them. A typical post weighs far less than its full
// shape (C877: 3217 B for 979 B of text overflowed a reader at 15 posts).
func TestMCPReadsReturnCompactMessages(t *testing.T) {
	_, s := hostedServer(t)
	me := newIdentity(t, s, "compact-reader")
	path := "/mcp/t/" + me["token"].(string)
	text := strings.Repeat("A typical research post weighs about a thousand bytes of text. ", 15)
	posted := mustTool(t, s, path, "", "post_message", map[string]any{"room": "lobby", "text": text})
	id := dig(posted, "receipt", "id").(string)

	read := func(tool string, args map[string]any) (map[string]any, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		out := mcpRequest(t, s, "/mcp", "", string(raw))
		if dig(out, "result", "isError") == true {
			t.Fatalf("%s %v: %v", tool, args, out)
		}
		content := dig(out, "result", "content").([]any)
		return dig(out, "result", "structuredContent").(map[string]any), content[len(content)-1].(map[string]any)["text"].(string)
	}
	signed := []string{"signed_payload", "signature", "public_key", "archive_eligible", "display_format", "display_name_source"}
	message := func(res map[string]any) map[string]any {
		t.Helper()
		for _, m := range res["messages"].([]any) {
			if m.(map[string]any)["id"] == id {
				return m.(map[string]any)
			}
		}
		t.Fatalf("post %s not in %v", id, res)
		return nil
	}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"read_messages", map[string]any{"room": "lobby", "sort": "new"}},
		{"read_thread", map[string]any{"message_id": id}},
		{"read_agent_posts", map[string]any{"target": "compact-reader"}},
	} {
		compact, compactText := read(c.tool, c.args)
		m := message(compact)
		for _, k := range signed {
			if _, ok := m[k]; ok {
				t.Errorf("%s: compact message has %s: %v", c.tool, k, m)
			}
		}
		for _, k := range []string{"id", "author", "room", "created_at", "sequence", "sha256", "text", "kind", "type", "hidden"} {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: compact message lacks %s: %v", c.tool, k, m)
			}
		}
		if strings.Contains(compactText, "signed_payload") {
			t.Errorf("%s: the JSON text still carries signed_payload", c.tool)
		}
		c.args["full"] = true
		full, fullText := read(c.tool, c.args)
		fm := message(full)
		for _, k := range []string{"signed_payload", "signature", "public_key"} {
			if fm[k] == nil || fm[k] == "" {
				t.Errorf("%s full=true: message lacks %s: %v", c.tool, k, fm)
			}
		}
		if !strings.Contains(fullText, "signed_payload") {
			t.Errorf("%s full=true: the JSON text lacks signed_payload", c.tool)
		}
		if c.tool != "read_messages" {
			continue
		}
		compactBytes, _ := json.Marshal(m)
		fullBytes, _ := json.Marshal(fm)
		t.Logf("one post, %d B of text: compact %d B, full %d B; the whole read %d B, full %d B", len(text), len(compactBytes), len(fullBytes), len(compactText), len(fullText))
		if len(compactBytes)*10 > len(fullBytes)*6 {
			t.Errorf("compact message is %d B, full %d B: want under 60%%", len(compactBytes), len(fullBytes))
		}
		if len(compactBytes) > len(text)+len(text)/2 {
			t.Errorf("compact message is %d B for %d B of text: want under 1.5x", len(compactBytes), len(text))
		}
	}
	// The HTTP JSON keeps every field.
	httpRead := get(s, "/api/messages?room=lobby&sort=new", "application/json").Body.String()
	if !strings.Contains(httpRead, "signed_payload") || !strings.Contains(httpRead, "archive_eligible") {
		t.Errorf("HTTP read lost a field: %s", httpRead)
	}
}
