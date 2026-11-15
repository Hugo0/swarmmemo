package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// PublicThreads answers each thread exactly as an anonymous thread.get does
// (C554): the same messages with their attachments, votes and edits, the
// same counts and has_more, and the same error for a thread it cannot read,
// which never fails the others.
func TestPublicThreadsMatchThreadGet(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob, carol := keyFor(81), keyFor(82), keyFor(83)
	run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "The room's first post"}))
	file := upload(t, s, alice, "lobby", "attached bytes", 0)
	root := run(t, s, signed(alice, Command{Operation: "post", Room: "lobby", Text: "A root with a file", Attachments: []string{file.ID}})).Receipt.ID
	reply := run(t, s, signed(bob, Command{Operation: "post", ReplyTo: root, Text: "A reply"})).Receipt.ID
	run(t, s, signed(carol, Command{Operation: "post", ReplyTo: reply, Text: "A reply to the reply"}))
	run(t, s, signed(bob, Command{Operation: "post", ReplyTo: root, Text: "A reply, edited", Data: dataJSON(`"supersedes":"` + reply + `"`)}))
	hidden := run(t, s, signed(carol, Command{Operation: "post", ReplyTo: root, Text: "A hidden reply"})).Receipt.ID
	if err := s.Moderate(testContext, hidden, "fixture", true); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(carol, Command{Operation: "vote", MessageID: root, Data: `{"value":1}`}))
	// A long thread: more replies than the limit, and one over the byte budget.
	long := run(t, s, signed(alice, Command{Operation: "post", Room: "research", Text: "A long thread"})).Receipt.ID
	for i := 0; i < 8; i++ {
		run(t, s, signed(bob, Command{Operation: "post", ReplyTo: long, Text: fmt.Sprintf("Reply %c %s", 'g'+i, strings.Repeat("x", 9000))}))
	}
	alone := run(t, s, Command{Operation: "post", Room: "help", Text: "An anonymous post with no replies"}).Receipt.ID
	run(t, s, signed(alice, Command{Operation: "room.create", Room: "private", Visibility: "private"}))
	private := run(t, s, signed(alice, Command{Operation: "post", Room: "private", Text: "Private"})).Receipt.ID

	ids := []string{root, long, alone, private, "0123456789abcdef0123456789abcdef", reply, root}
	for _, limit := range []int{3, 30} {
		got, errs, err := s.PublicThreads(testContext, ids, limit)
		if err != nil || len(got) != len(ids) || len(errs) != len(ids) {
			t.Fatalf("limit %d: %d results, %d errors, %v", limit, len(got), len(errs), err)
		}
		for i, id := range ids {
			want, wantErr := s.Execute(testContext, Command{Operation: "thread.get", MessageID: id, Limit: limit}, "web-public-read")
			var a, b *Error
			if (wantErr == nil) != (errs[i] == nil) || wantErr != nil && (!errors.As(wantErr, &a) || !errors.As(errs[i], &b) || a.Code != b.Code) {
				t.Fatalf("limit %d, %s: error %v, thread.get's %v", limit, id, errs[i], wantErr)
			}
			if wantErr != nil {
				continue
			}
			// The cursor is sealed afresh each time; what it holds is compared.
			for _, r := range []*Result{&want, &got[i]} {
				c, err := s.decodeConversationCursor(r.NextCursor, "thread.get", r.Data["root_id"].(string))
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(c)
				r.NextCursor = string(encoded)
			}
			w, _ := json.Marshal(want)
			g, _ := json.Marshal(got[i])
			if string(w) != string(g) {
				t.Fatalf("limit %d, %s:\n got %s\nwant %s", limit, id, g, w)
			}
		}
		if n := len(got[0].Messages); n == 0 || len(got[0].Messages[0].Attachments) != 1 || got[0].Messages[0].Votes == nil {
			t.Fatalf("limit %d: root thread lacks its file or votes: %+v", limit, got[0].Messages)
		}
		if more, _ := got[1].Data["has_more"].(bool); !more {
			t.Fatalf("limit %d: the long thread has no more", limit)
		}
	}
}
