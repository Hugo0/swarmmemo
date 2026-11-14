package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// C977: a call's answer leads with its result, on /call/ and over MCP, and
// in that result the payload (title, text) leads; only the order changes.
func TestCallAnswerLeadsWithResult(t *testing.T) {
	in := `{"ok":true,"data":{"call":{"cost":87,"charge":{"total":87}},"method":"page","result":{"bytes":3,"cost_note":"n <b>","text":"hi&","title":"T","url":"u"},"service":"fetch"},"next":{"about":{}}}`
	out, ok := leadCallAnswer([]byte(in))
	want := `{"ok":true,"data":{"result":{"title":"T","text":"hi&","bytes":3,"cost_note":"n <b>","url":"u"},"call":{"cost":87,"charge":{"total":87}},"method":"page","service":"fetch"},"next":{"about":{}}}`
	if !ok || string(out) != want {
		t.Fatalf("lead:\n%s\nwant\n%s", out, want)
	}
	var a, b any
	if json.Unmarshal([]byte(in), &a) != nil || json.Unmarshal(out, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("not the same value")
	}
	for _, other := range []string{`{"ok":true,"data":{"result":1}}`, `{"ok":true,"messages":[]}`, `[1]`, `{"ok":true}{}`} {
		if _, ok := leadCallAnswer([]byte(other)); ok {
			t.Errorf("%s is not a call answer", other)
		}
	}

	_, s := anonCallServer(t, 2000)
	w := secReq(s, "GET", "/call/notary/stamp?text=lead", "", "198.51.100.44:1", nil)
	if body := w.Body.String(); w.Code != 200 || !strings.HasPrefix(body, `{"ok":true,"data":{"result":{`) || !strings.HasSuffix(body, "}\n") {
		t.Fatalf("/call/ answer: %d %s", w.Code, body)
	}
	server := httptest.NewServer(s)
	defer server.Close()
	res := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"notary_stamp","arguments":{"text":"lead over mcp"}}}`)
	content, _ := dig(res, "result", "content").([]any)
	if len(content) == 0 || !strings.Contains(dig(content[0], "text").(string), `"data":{"result":{`) || dig(res, "result", "structuredContent", "data", "call", "state") != "done" {
		t.Fatalf("MCP answer: %v", res)
	}
}
