package httpapi

import (
	"net/http/httptest"
	"strconv"
	"testing"

	"swarmmemo/internal/board"
)

// Independent offline reproduction: read_room is documented to return the
// latest visible public posts, even if moderated tombstones fill a page.
func TestA2AReadRoomFindsPublicPostBeyondModerationPage(t *testing.T) {
	rig := newA2ARig(t, 1000)
	sign := rig.signed()
	wanted := sign(board.Command{Operation: "post", Room: "lobby", Text: "retained public post"})
	for i := 0; i < board.PageDefault+1; i++ {
		id := sign(board.Command{Operation: "post", Room: "lobby", Text: "moderated " + strconv.Itoa(i)})
		if err := rig.store.Moderate(t.Context(), id, "local offline moderation test", true); err != nil {
			t.Fatalf("moderate #%d: %v", i, err)
		}
	}
	first, err := rig.store.Execute(t.Context(), board.Command{Operation: "messages.list", Room: "lobby", Limit: board.PageDefault, Data: `{"sort":"new"}`}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.OlderCursor == "" {
		t.Fatal("no older continuation in real board")
	}
	older, err := rig.store.Execute(t.Context(), board.Command{Operation: "messages.list", Room: "lobby", Limit: board.PageDefault, Data: `{"sort":"new"}`, Older: first.OlderCursor}, "test")
	if err != nil {
		t.Fatal(err)
	}
	foundOlder := false
	for _, m := range older.Messages {
		if m.ID == wanted {
			foundOlder = true
		}
	}
	if !foundOlder {
		t.Fatalf("control: expected retained post %s on next real page; page count=%d", wanted, len(older.Messages))
	}
	got, e := rig.s.a2aReadRoom(httptest.NewRequest("GET", "https://swarmmemo.com/a2a", nil), "lobby", 5)
	if e != nil {
		t.Fatalf("A2A returned error: %v", e)
	}
	if len(got) != 1 || got[0].ID != wanted {
		t.Fatalf("A2A silent data omission: first page has %d tombstones; second page has visible %s, but read_room returned %d public posts", board.PageDefault, wanted, len(got))
	}
	// Exercise the actual public A2A protocol, not only an internal method.
	res, rpcErr := rig.rpc("198.51.100.55", "SendMessage", textMessage("read-visible-history", "read lobby 5", nil))
	if rpcErr != nil {
		t.Fatalf("A2A SendMessage read refused: %v", rpcErr)
	}
	task := res["task"].(map[string]any)
	posts := artifactPart(artifactsOf(task)[1])["data"].(map[string]any)["posts"].([]any)
	if len(posts) != 1 || posts[0].(map[string]any)["id"] != wanted {
		t.Fatalf("A2A protocol lost public post after moderation page: %v", posts)
	}
}
