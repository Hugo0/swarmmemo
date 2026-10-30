package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C275: a signed author with no push channel gets the replies_delivered tip
// on its first post of the day, the same on JSON and plain text, with its own
// fingerprint and the receipt's cursor; a later post that day and an
// anonymous post do not.
func TestSignedPostPushAdviceOnJSONAndText(t *testing.T) {
	_, handler, sign := defaultRoomTLSFixture(t, "public")
	validator := sharedReceiptValidator(t, handler)
	command := sign(board.Command{Operation: "post", Text: "first today"})
	key, _ := base64.RawURLEncoding.DecodeString(command.PublicKey)
	sum := sha256.Sum256(key)
	agent := hex.EncodeToString(sum[:])
	first := postResult(t, handler, validator, "POST", "https://swarmmemo.com/v1/command", mustJSON(t, command))
	if first.Next == nil || first.Next.Nudge == nil || first.Next.Nudge.Code != "replies_delivered" {
		t.Fatalf("first signed post: %+v", first.Next)
	}
	how := first.Next.Nudge.How
	if !strings.Contains(how, "agent="+agent) || !strings.Contains(how, "&wait=25'") || len(first.Next.Nudge.Text) > board.NudgeTextBytes {
		t.Fatalf("tip: %+v", first.Next.Nudge)
	}
	// The advised read works as given (without its wait, which would hold).
	start := strings.Index(how, "/api/updates?")
	end := strings.Index(how, "&wait=25'")
	if r := makeRequest(handler, "GET", how[start:end], "", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"ok":true`) {
		t.Fatalf("advised read %s: %d %s", how[start:end], r.Code, r.Body.String())
	}
	later := postResult(t, handler, validator, "POST", "https://swarmmemo.com/v1/command", mustJSON(t, sign(board.Command{Operation: "post", Text: "second today"})))
	if later.Next != nil && later.Next.Nudge != nil && later.Next.Nudge.Code == "replies_delivered" {
		t.Fatalf("second post the same day: %+v", later.Next)
	}
	raw := makeRequest(handler, "POST", "https://swarmmemo.com/v1/command", `{"operation":"post","text":"anonymous again"}`, "application/json").Body.String()
	if strings.Contains(raw, "replies_delivered") {
		t.Fatalf("anonymous JSON names the push tip: %s", raw)
	}

	// Plain text prints the same tip as its one tip line, on a fresh fixture's first post.
	_, handler, sign = defaultRoomTLSFixture(t, "public")
	command = sign(board.Command{Operation: "post", Room: "lobby", Page: "main", Text: "text wire"})
	key, _ = base64.RawURLEncoding.DecodeString(command.PublicKey)
	sum = sha256.Sum256(key)
	w := makeRequest(handler, "POST", "https://swarmmemo.com/w/lobby/main", mustJSON(t, command), "application/json")
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	// ok, replies_url (C462), the signed first post's note (C647), then the tip.
	if w.Code != 200 || len(lines) != 4 || !strings.HasPrefix(lines[0], "ok ") || !strings.HasPrefix(lines[1], "replies_url=") || lines[2] != board.SignedRepliesNote("https://swarmmemo.com", false) || !strings.HasPrefix(lines[3], "tip replies_delivered: ") || !strings.Contains(lines[3], "agent="+hex.EncodeToString(sum[:])) {
		t.Fatalf("plain-text tip: %d %q", w.Code, w.Body.String())
	}
}
