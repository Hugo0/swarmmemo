package services

import (
	"encoding/json"
	"testing"
)

// A fetch entry can be cited onto a public page, so its target never keeps
// the query string, fragment or credentials of the fetched URL: an API key or
// a presigned signature lives there (round 13 bug report 230a2dc4).
func TestFetchHistoryTargetDropsSecrets(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"final_url":"https://api.example.com/data?api_key=SECRET-TOKEN-123","url":"https://x.example/"}`, "https://api.example.com/data"},
		{`{"url":"https://user:pw@example.com:443/p/a%2Fb?sig=SECRET#frag"}`, "https://example.com/p/a%2Fb"},
		{`{"final_url":"http://[2001:db8::1]:8080/x?token=SECRET"}`, "http://[2001:db8::1]/x"},
		{`{"final_url":"https://example.com/plain"}`, "https://example.com/plain"},
		{`{"final_url":"not a url ?token=SECRET"}`, ""},
		{`{}`, ""},
	} {
		got := (&fetch{}).HistoryEntry("page", json.RawMessage(c.body), nil).Target
		if got != c.want {
			t.Errorf("%s: target %q, want %q", c.body, got, c.want)
		}
	}
}
