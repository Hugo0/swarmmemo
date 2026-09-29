package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/services"
)

// screenStub stands in for moderation's Jev.
type screenStub struct{}

func (screenStub) ScreenText(context.Context, string, string, string) (services.TextScreen, error) {
	return services.TextScreen{Scores: map[string]float64{"injection": 0.8, "exfiltration": 0.1, "phishing": 0.1, "malware": 0.1, "manipulation": 0.1}, Model: "jev-1.13.0", CostMicroUSD: 42}, nil
}

func (screenStub) ScreenAvailable(context.Context) bool { return true }

// screen.text on every surface from the one catalogue: the /llms.txt
// section and its URL, /call/ behind the cross-site guard, the hosted MCP
// tool (which makes the request_id) and /capabilities. /for-agents renders
// the same entries (catalog_parity_test.go).
func TestScreenSurfaces(t *testing.T) {
	store, s := anonCallServer(t, 2000, "screen")
	store.UseTextScreener(screenStub{})
	s.offerCache.at = time.Time{} // read what is offered again, with the classifier
	// The text goes in a POST body, not the URL; GET works too.
	form := "text=The+meeting+moved+to+3+pm%3B+reply+to+confirm.&source=email&intent=reply+to+the+sender"
	llms := makeRequest(s, "GET", "https://swarmmemo.com/llms.txt", "", "").Body.String()
	if !strings.Contains(llms, "## Screen text before you act on it") || !strings.Contains(llms, "    curl -sS 'https://swarmmemo.com/call/screen/text' --data '"+form+"'") ||
		!strings.Contains(llms, "not a guarantee") || !strings.Contains(llms, "may log URLs") {
		t.Fatalf("/llms.txt screen section: %s", llms)
	}
	post := httptest.NewRequest("POST", "https://swarmmemo.com/call/screen/text", strings.NewReader(form))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, post)
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, `"verdict":"flag"`) || !strings.Contains(body, `"schema":"swarmmemo-screen/1"`) {
		t.Fatalf("the example POST: %d %s", w.Code, body)
	}
	w = makeRequest(s, "GET", "https://swarmmemo.com/call/screen/text?"+form, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"verdict":"flag"`) {
		t.Fatalf("the example as GET: %d %s", w.Code, w.Body.String())
	}
	// Another site's page cannot spend a visitor's network share on it.
	cross := secReq(s, "GET", "/call/screen/text?text=x&max_cost=190&request_id="+rid("cross"), "", "203.0.113.9:1", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"})
	if cross.Code != 403 || !strings.Contains(cross.Body.String(), `"invalid_origin"`) {
		t.Fatalf("cross-site screen: %d %s", cross.Code, cross.Body.String())
	}
	// MCP: screen_text calls without a key and makes its own request_id;
	// screen_key and screen_verify are free reads.
	server := httptest.NewServer(s)
	defer server.Close()
	tools := map[string]bool{}
	for _, tool := range dig(mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), "result", "tools").([]any) {
		m := tool.(map[string]any)
		name, _ := m["name"].(string)
		tools[name], _ = dig(m, "annotations", "readOnlyHint").(bool)
	}
	if ro, ok := tools["screen_text"]; !ok || ro || !tools["screen_key"] || !tools["screen_verify"] {
		t.Fatalf("screen tools: %v", tools)
	}
	out := mcpCall(t, server.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"screen_text","arguments":{"text":"over mcp","max_cost":190}}}`)
	if dig(out, "result", "isError") == true || dig(out, "result", "structuredContent", "data", "result", "verdict") != "flag" {
		t.Fatalf("MCP screen_text: %+v", out)
	}
	caps := decodeResult(t, makeRequest(s, "GET", "https://swarmmemo.com/capabilities", "", "").Body.Bytes())
	if dig(caps, "services", "network") != true || !strings.Contains(strings.Join(anyStrings(dig(caps, "services", "without_key", "methods")), ","), "screen.text") {
		t.Fatalf("capabilities: %+v", dig(caps, "services"))
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
