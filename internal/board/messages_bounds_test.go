package board

import (
	"fmt"
	"strings"
	"testing"
)

func TestNewestMessagePages(t *testing.T) {
	for _, size := range []int{10, 20 << 10, 40 << 10} {
		for _, view := range []string{"room", "front", "all", "signed"} {
			t.Run(fmt.Sprintf("%s/%d", view, size), func(t *testing.T) {
				s := openTest(t, Config{MaxTextBytes: 64 << 10, DailyBytes: 1 << 30, AnonymousDailyBytes: 1 << 30, GlobalDailyBytes: 1 << 30})
				var ids []string
				for i := 0; i < 7; i++ {
					ids = append(ids, run(t, s, Command{Operation: "post", Room: "lobby", Text: strings.Repeat("m", size)}).Receipt.ID)
				}
				c := Command{Operation: "messages.list", Limit: 5, Data: `{"sort":"new"}`}
				plainData := ""
				if view == "room" || view == "signed" {
					c.Room = "lobby"
				} else if view == "all" {
					c.Data = `{"sort":"new","scope":"all"}`
					plainData = `{"scope":"all"}`
				}
				read := func(cmd Command) Result {
					if view == "signed" {
						cmd = signed(keyFor(33), cmd)
					}
					return run(t, s, cmd)
				}
				first := read(c)
				wantCount := 5
				if size == 20<<10 {
					wantCount = 3
				} else if size == 40<<10 {
					wantCount = 1
				}
				if len(first.Messages) != wantCount || first.Data["has_more"] != true {
					t.Fatalf("initial bounded page: %d messages, %v", len(first.Messages), first.Data)
				}
				previous := int64(1 << 62)
				for i, m := range first.Messages {
					if m.ID != ids[len(ids)-1-i] || m.Sequence >= previous {
						t.Fatalf("initial page must contain newest messages in strictly descending sequence: %+v", m)
					}
					previous = m.Sequence
				}
				seq, err := s.parseCursor(first.NextCursor)
				if err != nil || seq != first.Messages[0].internalSequence {
					t.Fatalf("cursor must mark newest delivered message: %d, %v", seq, err)
				}
				c.Cursor = first.NextCursor
				if empty := read(c); len(empty.Messages) != 0 || empty.NextCursor != c.Cursor || empty.Data["has_more"] != false {
					t.Fatal("poll without arrivals must stay empty and keep its cursor")
				}
				var arrivals []string
				for i := 0; i < 4; i++ {
					arrivals = append(arrivals, run(t, s, Command{Operation: "post", Room: "lobby", Text: "new arrival"}).Receipt.ID)
				}
				for _, data := range []string{c.Data, plainData} {
					poll := c
					poll.Data, poll.Limit = data, 3
					previous = first.Messages[0].Sequence
					var got []string
					for page := 0; ; page++ {
						if page > len(arrivals) {
							t.Fatal("forward polling did not terminate")
						}
						res := read(poll)
						for _, m := range res.Messages {
							if m.Sequence <= previous {
								t.Fatalf("cursor must read strictly newer messages in ascending sequence: %+v", m)
							}
							previous = m.Sequence
							got = append(got, m.ID)
						}
						poll.Cursor = res.NextCursor
						if res.Data["has_more"] == false {
							break
						}
					}
					if strings.Join(got, ",") != strings.Join(arrivals, ",") {
						t.Fatalf("cursor read (%s): got %v, want %v", data, got, arrivals)
					}
				}
			})
		}
	}
}

// The byte budget can cut a page to a handful of messages. Without has_more a
// caller cannot tell that short page apart from the end of the feed.
func TestMessageListReportsTruncationAndFullPages(t *testing.T) {
	s := openTest(t, Config{ServiceID: "swarmmemo.com", MaxTextBytes: 64 << 10, DailyBytes: 1 << 30, AnonymousDailyBytes: 1 << 30, GlobalDailyBytes: 1 << 30})
	big := strings.Repeat("m", 40<<10)
	for i := 0; i < 6; i++ {
		run(t, s, Command{Operation: "post", Room: "bounds", Text: big})
	}
	page := run(t, s, Command{Operation: "messages.list", Room: "bounds", Limit: 50})
	if len(page.Messages) >= 6 {
		t.Fatalf("fixture did not exercise the byte budget: %d messages", len(page.Messages))
	}
	if page.Data["has_more"] != true {
		t.Fatal("a byte-budget truncation must report has_more")
	}
	run(t, s, Command{Operation: "post", Room: "small", Text: "one"})
	run(t, s, Command{Operation: "post", Room: "small", Text: "two"})
	if full := run(t, s, Command{Operation: "messages.list", Room: "small", Limit: 2}); full.Data["has_more"] != true {
		t.Fatal("a page filled to the limit must report has_more")
	}
	done := run(t, s, Command{Operation: "messages.list", Room: "small", Limit: 50})
	if len(done.Messages) != 2 || done.Data["has_more"] != false {
		t.Fatal("an exhausted query must report has_more false")
	}
	// Walking forward from the last message ends the walk rather than looping.
	end := run(t, s, Command{Operation: "messages.list", Room: "small", Cursor: done.NextCursor, Limit: 50})
	if len(end.Messages) != 0 || end.Data["has_more"] != false || end.NextCursor == "" {
		t.Fatal("an empty forward page must report has_more false and keep a cursor")
	}
}
