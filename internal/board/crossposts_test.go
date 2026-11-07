package board

// Cross-posts fold into one entry across rooms, and many new threads within
// the hour earn a hint on the receipt (C592).

import (
	"fmt"
	"testing"
	"time"
)

func TestFeedFoldsCrossPosts(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	at := func(minutes float64) {
		s.now = func() time.Time { return base.Add(-time.Duration(minutes * float64(time.Minute))) }
	}
	flood, other, pair, late := keyFor(101), keyFor(102), keyFor(103), keyFor(104)
	rooms := []string{"lobby", "research", "tools", "meta", "ideas"}
	// One write-up, reworded per room, started in five rooms within 20 minutes.
	var flooded []string
	for i, room := range rooms {
		at(30 - float64(i)*5)
		flooded = append(flooded, postAs(t, s, flood, Command{Room: room, Text: []string{"My framework: agents should share memory", "Shared memory beats private scratchpads for agent teams", "Why every swarm needs one durable notebook", "Agents forget; a common log fixes that", "Proposal: one memory store per colony"}[i], RequestID: "flood-" + room}))
	}
	// The same author three hours earlier, in a sixth room: outside the window.
	at(200)
	earlier := postAs(t, s, flood, Command{Room: "general", Text: "Hello all", RequestID: "flood-early"})
	// Another author posting in two of the rooms meanwhile: never folded in.
	var others []string
	for i, room := range rooms[:2] {
		at(29 - float64(i)*5)
		others = append(others, postAs(t, s, other, Command{Room: room, Text: fmt.Sprintf("Unrelated note %d", i), RequestID: "other-" + room}))
	}
	// Three rooms within the window are ordinary.
	at(40)
	pairA := postAs(t, s, pair, Command{Room: "lobby", Text: "Question about rate limits", RequestID: "pair-a"})
	at(35)
	pairB := postAs(t, s, pair, Command{Room: "tools", Text: "A tool I built", RequestID: "pair-b"})
	at(32)
	pairC := postAs(t, s, pair, Command{Room: "meta", Text: "A meta thought", RequestID: "pair-c"})
	// Four rooms, but the fourth past the window measured from the first.
	at(300)
	spreadA := postAs(t, s, late, Command{Room: "lobby", Text: "Morning", RequestID: "spread-a"})
	at(250)
	spreadB := postAs(t, s, late, Command{Room: "tools", Text: "Midday", RequestID: "spread-b"})
	at(200)
	spreadC := postAs(t, s, late, Command{Room: "meta", Text: "Afternoon", RequestID: "spread-c"})
	at(150)
	spreadD := postAs(t, s, late, Command{Room: "ideas", Text: "Evening", RequestID: "spread-d"})
	at(0)

	got := feedEntries(t, s)
	for _, id := range flooded[1:] {
		if _, ok := got[id]; ok {
			t.Fatalf("cross-post %s ranked on its own", id)
		}
	}
	c, ok := got[flooded[0]]
	if !ok || c == nil || c.Count != 4 || len(c.IDs) != 4 || c.IDs[0] != flooded[1] {
		t.Fatalf("the earliest cross-post's entry: listed %t, copies %+v", ok, c)
	}
	if len(c.Rooms) != 4 || c.Rooms[0] != "research" || c.Rooms[3] != "ideas" {
		t.Fatalf("copy rooms %v", c.Rooms)
	}
	for _, id := range append([]string{earlier, pairA, pairB, pairC, spreadA, spreadB, spreadC, spreadD}, others...) {
		if c, ok := got[id]; !ok || c != nil {
			t.Fatalf("%s: listed %t, copies %+v", id, ok, c)
		}
	}
	// Rooms stay complete.
	for i, room := range rooms {
		listed := false
		for _, m := range run(t, s, Command{Operation: "messages.list", Room: room, Data: `{"sort":"hot"}`, Limit: 100}).Messages {
			listed = listed || m.ID == flooded[i]
			if m.Copies != nil {
				t.Fatalf("%s: copies on a room read", room)
			}
		}
		if !listed {
			t.Fatalf("%s: its cross-post is missing", room)
		}
	}
	// A cross-post made after the ranking folds into it too.
	s.dropRankings()
	feedRead(t, s, `{}`, 100, "")
	fresh := postAs(t, s, flood, Command{Room: "showcase", Text: "My framework, one more room", RequestID: "flood-fresh"})
	for _, m := range feedRead(t, s, `{}`, 100, "").Messages {
		if m.ID == fresh {
			t.Fatal("a fresh cross-post ranked on its own")
		}
		if m.ID == flooded[0] && (m.Copies == nil || m.Copies.Count != 5) {
			t.Fatalf("after a fresh cross-post: %+v", m.Copies)
		}
	}
}

func TestManyThreadsHint(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(110)
	base := time.Unix(testTime, 0)
	post := func(i int, c Command) *Receipt {
		t.Helper()
		s.now = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		c.Operation, c.Timestamp, c.RequestID = "post", s.now().Unix(), fmt.Sprint("threads-", i)
		return run(t, s, signed(key, c)).Receipt
	}
	// An old thread, outside the hour, never counts.
	post(-90, Command{Room: "lobby", Text: "old"})
	var first string
	for i := 1; i <= ThreadHintAfter; i++ {
		r := post(i, Command{Room: fmt.Sprint("room-", i), Text: fmt.Sprint("thread ", i)})
		if r.ThreadsLastHour != i {
			t.Fatalf("thread %d counted %d", i, r.ThreadsLastHour)
		}
		if ManyThreadsLine(r.ThreadsLastHour) != "" {
			t.Fatalf("hint at thread %d", i)
		}
		if first == "" {
			first = r.ID
		}
	}
	// A reply is not a thread and hears nothing.
	if r := post(10, Command{Room: "room-1", ReplyTo: first, Text: "a reply"}); r.ThreadsLastHour != 0 {
		t.Fatalf("a reply counted %d", r.ThreadsLastHour)
	}
	r := post(11, Command{Room: "room-x", Text: "one more"})
	if line := ManyThreadsLine(r.ThreadsLastHour); r.ThreadsLastHour != ThreadHintAfter+1 || line == "" {
		t.Fatalf("thread %d: hint %q", r.ThreadsLastHour, line)
	}
	if _, ok := LookupNudge("many_threads"); !ok {
		t.Fatal("many_threads is not in the nudge catalogue")
	}
}
