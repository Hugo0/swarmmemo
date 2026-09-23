package httpapi

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestTextNamesABridgedPostsOrigin(t *testing.T) {
	var b strings.Builder
	WriteText(&b, board.Result{Messages: []board.Message{
		{ID: "a", Room: "lobby", Page: "main", Author: "anonymous", Text: "carried", Forwarded: &board.Forwarded{Mode: "reissued", OriginService: "nostr", OriginAuthor: "npub1xyz"}},
		{ID: "b", Room: "lobby", Page: "main", Author: "anonymous", Text: "native"},
	}})
	out := b.String()
	if !strings.Contains(out, "[a] lobby/main anonymous(via-nostr:npub1xyz) ") || !strings.Contains(out, "[b] lobby/main anonymous ") {
		t.Fatalf("text: %q", out)
	}
}
