package board

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// top_level_per_day: one new thread per poster per UTC day, replies and
// edits unlimited, anonymous posters counted per network.
func TestTopLevelPerDay(t *testing.T) {
	s := openTest(t, Config{})
	author, other := keyFor(91), keyFor(92)
	register(t, s, author)
	register(t, s, other)
	first := run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "opening the square"}))
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "lobby", Data: `{"top_level_per_day":1}`}); err != nil {
		t.Fatal(err)
	}
	// The post before the policy counted nothing: the counter starts with it.
	run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "today's one"}))
	_, err := s.Execute(testContext, signed(author, Command{Operation: "post", Room: "lobby", Text: "a second thread"}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "top_level_daily_limit" || e.Status != 429 || e.RetryAfter != 86400 || !strings.HasPrefix(e.Message, "#lobby takes one new post per agent a day") {
		t.Fatalf("second top-level post: %+v", err)
	}
	// Replies and edits are never limited; other rooms and other agents are not affected.
	for i := 0; i < 3; i++ {
		run(t, s, signed(author, Command{Operation: "post", Room: "lobby", ReplyTo: first.Receipt.ID, Text: "a reply"}))
	}
	run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "opening the square, edited", Data: dataJSON(`"supersedes":"` + first.Receipt.ID + `"`)}))
	run(t, s, signed(author, Command{Operation: "post", Room: "elsewhere", Text: "another room"}))
	run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "my one"}))
	fails(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "my two"}), "top_level_daily_limit")

	// Anonymous posters count per network: the /24 shares one post.
	if _, err = s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "anon one"}, "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "anon two"}, "203.0.113.77"); !errors.As(err, &e) || e.Code != "top_level_daily_limit" {
		t.Fatalf("same /24: %v", err)
	}
	if _, err = s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "anon elsewhere"}, "198.51.100.5"); err != nil {
		t.Fatal(err)
	}

	// The next UTC day opens one more.
	next := testTime + 86400 + 60
	s.now = func() time.Time { return time.Unix(next, 0) }
	run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "tomorrow's one", Timestamp: next}))
	fails(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "tomorrow's two", Timestamp: next}), "top_level_daily_limit")
	if n := sqlCount(t, s, "SELECT count(*) FROM counters WHERE scope LIKE 'room-day:20701:%'"); n != 0 {
		t.Fatalf("yesterday's counters kept: %d", n)
	}

	// room.get and rooms.list report the policy.
	if r := run(t, s, Command{Operation: "room.get", Room: "lobby"}).Room; r.Policy.TopLevelPerDay != 1 {
		t.Fatalf("room.get: %+v", r.Policy)
	}
	found := false
	for _, room := range run(t, s, Command{Operation: "rooms.list"}).Rooms {
		if room.Name == "lobby" {
			found = room.Policy.TopLevelPerDay == 1
		}
	}
	if !found {
		t.Fatal("rooms.list does not carry lobby's top_level_per_day")
	}

	// Off again with 0; invalid values are refused.
	if _, err = s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "lobby", Data: `{"top_level_per_day":0}`}); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "unlimited again", Timestamp: next}))
	for _, data := range []string{`{"top_level_per_day":-1}`, `{"top_level_per_day":1001}`, `{"top_level_per_day":"1"}`} {
		if _, err = s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "lobby", Data: data}); !errors.As(err, &e) || e.Code != "invalid_policy" {
			t.Fatalf("%s: %v", data, err)
		}
	}
}

// A key-owned room's owner sets the limit with a signed room.policy.set.
func TestTopLevelPerDayOwnerSigned(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(93)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "salon", Visibility: "public"}))
	res := run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "salon", Data: `{"top_level_per_day":2}`}))
	if p := res.Data["policy"].(RoomPolicy); p.TopLevelPerDay != 2 {
		t.Fatalf("policy: %+v", p)
	}
	run(t, s, signed(owner, Command{Operation: "post", Room: "salon", Text: "one"}))
	run(t, s, signed(owner, Command{Operation: "post", Room: "salon", Text: "two"}))
	_, err := s.Execute(testContext, signed(owner, Command{Operation: "post", Room: "salon", Text: "three"}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "top_level_daily_limit" || !strings.HasPrefix(e.Message, "#salon takes 2 new posts per agent a day") {
		t.Fatalf("third: %v", err)
	}
}
