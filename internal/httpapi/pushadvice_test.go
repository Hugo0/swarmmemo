package httpapi

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C275: a signed author with no push channel learns how replies reach it on
// its first post of the day: the replies note (C647), on JSON and plain text.
// The replies_delivered tip that restated it is retired (C1118): no wire
// names it.
func TestSignedPostPushAdviceOnJSONAndText(t *testing.T) {
	_, handler, sign := defaultRoomTLSFixture(t, "public")
	validator := sharedReceiptValidator(t, handler)
	first := postResult(t, handler, validator, "POST", "https://swarmmemo.com/v1/command", mustJSON(t, sign(board.Command{Operation: "post", Text: "first today"})))
	if first.Receipt == nil || first.Receipt.RepliesNote != board.SignedRepliesNote("https://swarmmemo.com", false) || first.Next != nil && first.Next.Nudge != nil {
		t.Fatalf("first signed post: %+v %+v", first.Receipt, first.Next)
	}
	raw := makeRequest(handler, "POST", "https://swarmmemo.com/v1/command", mustJSON(t, sign(board.Command{Operation: "post", Text: "second today"})), "application/json").Body.String()
	if strings.Contains(raw, "replies_delivered") {
		t.Fatalf("a signed post names the retired tip: %s", raw)
	}

	// Plain text: ok, replies_url (C462), the signed first post's note (C647), and no tip line.
	_, handler, sign = defaultRoomTLSFixture(t, "public")
	w := makeRequest(handler, "POST", "https://swarmmemo.com/w/lobby/main", mustJSON(t, sign(board.Command{Operation: "post", Room: "lobby", Page: "main", Text: "text wire"})), "application/json")
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if w.Code != 200 || len(lines) != 3 || !strings.HasPrefix(lines[0], "ok ") || !strings.HasPrefix(lines[1], "replies_url=") || lines[2] != board.SignedRepliesNote("https://swarmmemo.com", false) {
		t.Fatalf("plain-text post: %d %q", w.Code, w.Body.String())
	}
}
