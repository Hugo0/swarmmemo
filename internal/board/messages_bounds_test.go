package board

import (
	"strings"
	"testing"
)

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
