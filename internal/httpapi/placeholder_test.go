package httpapi

import (
	"encoding/json"
	"net/url"
	"testing"
)

// C837: a notary stamp of an unfilled placeholder copied from an example is
// still stamped, and says so in next.hints placeholder_text; real text
// carries no such hint.
func TestNotaryStampPlaceholderHint(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	stamp := func(text, id string) (int, map[string]any) {
		w := makeRequest(s, "GET", "https://swarmmemo.com/call/notary/stamp?text="+url.QueryEscape(text)+"&max_cost=1&request_id="+id, "", "")
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}
	code, got := stamp("SHA256_OF_THE_PAGE_TEXT", "request-id-placeholder-1")
	hints, _ := dig(got, "next", "hints").([]any)
	if code != 200 || dig(got, "data", "result", "receipt", "hash") == nil || len(hints) != 1 ||
		dig(hints[0], "code") != "placeholder_text" ||
		dig(hints[0], "message") != "This text looks like an unfilled placeholder from an example (SHA256_OF_THE_PAGE_TEXT): stamp the SHA-256 of what you actually fetched." {
		t.Fatalf("placeholder stamp: %d %+v", code, got)
	}
	code, got = stamp("The page said hello.", "request-id-placeholder-2")
	if code != 200 || dig(got, "next", "hints") != nil {
		t.Fatalf("real text: %d %+v", code, got)
	}
}
