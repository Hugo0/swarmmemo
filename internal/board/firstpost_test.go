package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// C675: "is this someone's first post?" is checkable. messages.list data
// first_post "mark" sets first_post: true on each message that is its
// author's first public post; "only" lists only those, and "unanswered"
// only those no one but their author has replied to. Opt-in: without it a
// message read is unchanged, what installed inbox clients check key by key.
func TestFirstPostReads(t *testing.T) {
	s := openTest(t, Config{})
	newbie, veteran, helper, quiet := keyFor(191), keyFor(192), keyFor(193), keyFor(194)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(testTime+sec, 0) } }
	post := func(key ed25519.PrivateKey, room, replyTo, text string) string {
		t.Helper()
		return run(t, s, signed(key, Command{Operation: "post", Room: room, ReplyTo: replyTo, Text: text, Timestamp: s.now().Unix()})).Receipt.ID
	}
	run(t, s, signed(newbie, Command{Operation: "room.create", Room: "hush", Visibility: "private", Timestamp: s.now().Unix()}))
	old := post(veteran, "lobby", "", "An old hand's first post, long ago")
	at(60)
	vetLater := post(veteran, "lobby", "", "An old hand's later post")
	// newbie's private-room post is no public post: its lobby post is its first.
	post(newbie, "hush", "", "A private note first")
	first := post(newbie, "lobby", "", "Hello, I am new here: how do paid tasks work?")
	post(newbie, "lobby", first, "Bumping my own question")
	second := post(newbie, "lobby", "", "My second post")
	quietFirst := post(quiet, "lobby", "", "Another newcomer, also unanswered")
	helperFirst := post(helper, "lobby", old, "A reply as a first post")

	list := func(data string) []Message {
		t.Helper()
		return run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: data}).Messages
	}
	marked := func(ms []Message) []string {
		var ids []string
		for _, m := range ms {
			if m.FirstPost {
				ids = append(ids, m.ID)
			}
		}
		return ids
	}
	ids := func(ms []Message) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}

	// Without the option nothing changes on the wire.
	for _, m := range list(`{"sort":"new"}`) {
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), "first_post") {
			t.Fatalf("unasked read carries first_post: %s", raw)
		}
	}
	// mark: each author's first public post, in any order.
	got := marked(list(`{"sort":"new","first_post":"mark"}`))
	want := map[string]bool{old: true, first: true, quietFirst: true, helperFirst: true}
	if len(got) != len(want) {
		t.Fatalf("marked %v, want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("marked %s (vetLater %s second %s)", id, vetLater, second)
		}
	}
	// only: just those, newest first.
	if g, w := ids(list(`{"sort":"new","first_post":"only"}`)), strings.Join([]string{old, first, quietFirst, helperFirst}, ","); g != reverseIDs(w) {
		t.Fatalf("only: %s, want %s", g, reverseIDs(w))
	}
	// unanswered: newbie's own bump does not answer it; the veteran's first
	// post has helper's reply.
	if g := ids(list(`{"first_post":"unanswered"}`)); g != strings.Join([]string{first, quietFirst, helperFirst}, ",") {
		t.Fatalf("unanswered: %s", g)
	}
	post(helper, "lobby", first, "Welcome! Paid tasks: find_work, then claim_work.")
	if g := ids(list(`{"first_post":"unanswered"}`)); g != strings.Join([]string{quietFirst, helperFirst}, ",") {
		t.Fatalf("unanswered after a reply: %s", g)
	}
	// A hidden first post is left out of the filter.
	hideEvent(t, s, quietFirst)
	if g := ids(list(`{"first_post":"unanswered"}`)); g != helperFirst {
		t.Fatalf("unanswered with a hidden post: %s", g)
	}
	// Across rooms and on the GET-equivalent board read too.
	if all := run(t, s, Command{Operation: "messages.list", Data: `{"scope":"all","first_post":"unanswered"}`}).Messages; ids(all) != helperFirst || !all[0].FirstPost {
		t.Fatalf("board read: %s", ids(all))
	}
	// A filter is chronological: a ranked read refuses it; mark works anywhere.
	fails(t, s, Command{Operation: "messages.list", Data: `{"sort":"hot","first_post":"only"}`}, "invalid_list_options")
	fails(t, s, Command{Operation: "messages.list", Data: `{"first_post":"yes"}`}, "invalid_list_options")
	run(t, s, Command{Operation: "messages.list", Data: `{"sort":"hot","first_post":"mark"}`})
}

func reverseIDs(csv string) string {
	parts := strings.Split(csv, ",")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ",")
}
