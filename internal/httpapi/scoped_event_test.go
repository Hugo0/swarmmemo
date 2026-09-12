package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestScopedEventGetHTTPPrivateTransportAndCapability(t *testing.T) {
	f := newWorkPrivacyFixture(t)
	f.run(f.sign(f.owner, board.Command{Operation: "room.create", Room: "second-private", Visibility: "private", Members: []string{f.workerID}}))
	other := f.run(f.sign(f.owner, board.Command{Operation: "post", Room: "second-private", Text: "OTHER_ROOM_SENTINEL"})).Receipt.ID
	for _, transport := range []string{"json", "c64"} {
		t.Run(transport, func(t *testing.T) {
			command := f.sign(f.worker, board.Command{Operation: "message.get", Room: "private-work", MessageID: f.privateID})
			status, body := f.command(transport, command)
			var result board.Result
			if status != 200 || json.Unmarshal([]byte(body), &result) != nil || len(result.Messages) != 1 || result.Messages[0].ID != f.privateID || result.Generation != f.generation || result.Messages[0].ArchiveEligible {
				t.Fatalf("matching scoped read failed: %d %s", status, body)
			}
			var missing string
			for _, id := range []string{other, f.publicID, strings.Repeat("f", 32)} {
				status, body = f.command(transport, f.sign(f.worker, board.Command{Operation: "message.get", Room: "private-work", MessageID: id}))
				if status != 404 || strings.Contains(body, "OTHER_ROOM_SENTINEL") || strings.Contains(body, "Public fixture") || strings.Contains(body, id) {
					t.Fatalf("scope leaked event: %d %s", status, body)
				}
				if missing == "" {
					missing = body
				} else if !sameWorkPrivacyJSON(missing, body) {
					t.Fatal("cross-room/public/unknown errors differ")
				}
			}
			status, body = f.command(transport, f.sign(f.worker, board.Command{Operation: "message.get", MessageID: other}))
			if status != 200 || !strings.Contains(body, "OTHER_ROOM_SENTINEL") {
				t.Fatal("legacy unscoped behavior changed")
			}
			status, body = f.command(transport, board.Command{Operation: "message.get", Room: "private-work", MessageID: f.publicID})
			if status != 404 || strings.Contains(body, "Public fixture") {
				t.Fatal("anonymous private-room selection fell back to public read")
			}
		})
	}
	status, body := f.request("GET", "/capabilities", "")
	var capability struct {
		PrivateReads map[string]bool `json:"private_reads"`
	}
	if status != 200 || json.Unmarshal([]byte(body), &capability) != nil || len(capability.PrivateReads) != 1 || !capability.PrivateReads["message_get_room_filter"] {
		t.Fatal("missing precise optional-room capability")
	}
	f.run(f.sign(f.owner, board.Command{Operation: "room.member.remove", Room: "private-work", Target: f.workerID}))
	for _, transport := range []string{"json", "c64"} {
		status, body = f.command(transport, f.sign(f.worker, board.Command{Operation: "message.get", Room: "private-work", MessageID: f.privateID}))
		if status != 404 || strings.Contains(body, "PRIVATE_TITLE_SENTINEL") {
			t.Fatal("removed member can still read filtered private event")
		}
	}
}
