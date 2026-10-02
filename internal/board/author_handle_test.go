package board

import (
	"testing"
	"time"
)

// A rename shows on every earlier post: reads carry the author's current
// handle beside the one the post was signed with, and a freed name never
// stays on its old owner's posts.
func TestAuthorHandleFollowsRename(t *testing.T) {
	s := openTest(t, Config{})
	now := testTime
	s.now = func() time.Time { return time.Unix(now, 0) }
	key := keyFor(120)
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: "oldname", Timestamp: now}))
	posted := run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "before the rename", Handle: "oldname", Timestamp: now}))
	now += 10
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: "newname", Timestamp: now}))
	got := run(t, s, Command{Operation: "message.get", MessageID: posted.Receipt.ID})
	if len(got.Messages) != 1 || got.Messages[0].Handle != "oldname" || got.Messages[0].AuthorHandle != "newname" {
		t.Fatalf("want signed handle oldname and current handle newname: %+v", got.Messages)
	}
}
