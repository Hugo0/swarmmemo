package board

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Without a key, a network starts AnonymousTopLevelPerHour threads a UTC
// hour: the next is refused 429 anonymous_post_rate and not published, while
// its replies, signed posts and other networks go on; the next hour, or a
// raised params version, admits it again. The count is made on the real
// store in the command's transaction (one connection: a pool read under it
// would hang this test).
func TestAnonymousThreadRate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "board.sqlite"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := int64(testTime)
	s.now = func() time.Time { return time.Unix(now, 0) }
	post := func(c Command, source string) (Result, error) {
		c.Operation = "post"
		return s.Execute(testContext, c, source)
	}
	var first string
	for i := range AnonymousTopLevelPerHour {
		r, err := post(Command{Text: fmt.Sprintf("thread %d", i)}, "net-a")
		if err != nil {
			t.Fatalf("thread %d: %v", i, err)
		}
		if first == "" {
			first = r.Receipt.ID
		}
	}
	_, err = post(Command{Text: "one thread too many"}, "net-a")
	var e *Error
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "anonymous_post_rate" || e.RetryAfter != int(3600-now%3600) || !strings.Contains(e.Message, "key") {
		t.Fatalf("the fifth thread in an hour: %v", err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE text='one thread too many'"); n != 0 {
		t.Fatal("a refused thread must not be published")
	}
	if _, err = post(Command{Text: "a reply", ReplyTo: first}, "net-a"); err != nil {
		t.Fatalf("a reply is not counted: %v", err)
	}
	if _, err = post(signed(keyFor(3), Command{Operation: "post", Text: "signed thread"}), "net-a"); err != nil {
		t.Fatalf("a signed post is not counted: %v", err)
	}
	if _, err = post(Command{Text: "another network"}, "net-b"); err != nil {
		t.Fatalf("another network has its own count: %v", err)
	}
	// A refusal counts nothing: raising the rate by one admits exactly one.
	if _, err = s.SetAllowanceParams(testContext, PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":5}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = post(Command{Text: "the fifth, raised"}, "net-a"); err != nil {
		t.Fatalf("a raised rate: %v", err)
	}
	if _, err = post(Command{Text: "the sixth"}, "net-a"); !errors.As(err, &e) || e.Code != "anonymous_post_rate" {
		t.Fatalf("the sixth under a rate of 5: %v", err)
	}
	now += 3600 - now%3600 // the next UTC hour
	if _, err = post(Command{Text: "next hour"}, "net-a"); err != nil {
		t.Fatalf("the next hour starts a new count: %v", err)
	}
	for _, bad := range []string{`{"anonymous_top_level_per_hour":0}`, `{"anonymous_top_level_per_hour":4,"other":1}`, `{}`} {
		if _, err = s.SetAllowanceParams(testContext, PostingParamsNamespace, []byte(bad), "test", 0); err == nil {
			t.Errorf("posting params %s accepted", bad)
		}
	}
}
