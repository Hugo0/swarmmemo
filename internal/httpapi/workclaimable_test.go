package httpapi

import (
	"strings"
	"testing"
)

// C625: /api/works?claimable=1 travels as works.list data claimable true,
// 0 as false; any other value, or the parameter twice or with data, is 400.
// Over a real store the filter answers 200, and MCP find_work takes it too.
func TestWorksClaimableFilter(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	for path, want := range map[string]string{
		"/api/works?kind=open&claimable=1": `{"claimable":true,"schema":1}`,
		"/api/works?claimable=true":        `{"claimable":true,"schema":1}`,
		"/api/works?claimable=0":           `{"claimable":false,"schema":1}`,
		"/api/works?claimable=1&standing=" + "0123456789abcdef0123456789abcdef": `{"claimable":true,"schema":1,"standing":"0123456789abcdef0123456789abcdef"}`,
		// C627: summary=1 asks for each row's eligibility_summary.
		"/api/works?kind=open&summary=1":             `{"schema":1,"summary":true}`,
		"/api/works?summary=1&eligible_for=" + hex64: `{"eligible_for":"` + hex64 + `","schema":1,"summary":true}`,
	} {
		f := &fakeService{}
		if w := makeRequest(New(f, nil, Config{}), "GET", path, "", ""); w.Code != 200 || len(f.commands) != 1 || f.commands[0].Data != want {
			t.Fatalf("%s: %d %+v", path, w.Code, f.commands)
		}
	}
	for _, path := range []string{"/api/works?claimable=yes", "/api/works?summary=yes", "/api/works?claimable=1&claimable=1", `/api/works?claimable=1&data={"schema":1}`} {
		f := &fakeService{}
		if w := makeRequest(New(f, nil, Config{}), "GET", path, "", ""); w.Code != 400 || len(f.commands) != 0 {
			t.Fatalf("%s: %d %+v", path, w.Code, f.commands)
		}
	}
	_, s := hostedServer(t)
	if w := get(s, "/api/works?kind=open&claimable=1", "application/json"); w.Code != 200 {
		t.Fatalf("real claimable=1: %d %s", w.Code, w.Body.String())
	}
	if w := get(s, "/api/works?summary=1", "application/json"); w.Code != 200 {
		t.Fatalf("real summary=1: %d %s", w.Code, w.Body.String())
	}
	got := mustTool(t, s, "/mcp", "", "find_work", map[string]any{"claimable": true})
	if _, ok := dig(got, "data", "works").([]any); !ok {
		t.Fatalf("find_work claimable %v", got)
	}
}
