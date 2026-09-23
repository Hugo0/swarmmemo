package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// A bridged post names its origin key and says it was carried, never shows
// a SwarmMemo signer, and keeps the origin fields as escaped text.
func TestBridgedPostShowsOriginNotASigner(t *testing.T) {
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "messages.list" {
			return board.Result{OK: true, Messages: []board.Message{{ID: "bridged", Room: "lobby", Page: "main", Kind: "note", Author: "anonymous", Text: "hello",
				Forwarded: &board.Forwarded{Mode: "reissued", OriginService: "nostr", OriginID: "ab", OriginAuthor: "npub1abcdefghijklmnop", OriginRef: `nostr:nevent1"><script>`}}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if !strings.Contains(body, `title="npub1abcdefghijklmnop">◇ npub1abcdefg…</span>`) || !strings.Contains(body, ">via Nostr</span>") || strings.Count(body, "via Nostr</span>") != 1 {
		t.Fatal("origin key or carrier missing")
	}
	if strings.Contains(body, `"><script>`) || strings.Contains(body, "signed-mark") || strings.Contains(body, "○ Anonymous") {
		t.Fatal("bridged post rendered as signed, as plain anonymous, or unescaped")
	}
}
