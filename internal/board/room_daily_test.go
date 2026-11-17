package board

import (
	"crypto/ed25519"
	"errors"
	"fmt"
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
	if !errors.As(err, &e) || e.Code != "top_level_daily_limit" || e.Status != 429 || e.RetryAfter != 86400 || e.NextOpenAt != s.now().Unix()+86400 || !strings.HasPrefix(e.Message, "#lobby takes one new post per agent a day") {
		t.Fatalf("second top-level post: %+v", err)
	}
	// Replies and edits are never limited; other rooms and other agents are not affected.
	for i := 0; i < 3; i++ {
		run(t, s, signed(author, Command{Operation: "post", Room: "lobby", ReplyTo: first.Receipt.ID, Text: "a reply " + randomID()[:8]}))
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
// The room's officials, its owner and listed moderators, are exempt; a plain
// member is not, and a removed moderator counts again.
func TestTopLevelPerDayOwnerSigned(t *testing.T) {
	s := openTest(t, Config{})
	owner, mod, member := keyFor(93), keyFor(94), keyFor(95)
	for _, k := range []ed25519.PrivateKey{owner, mod, member} {
		register(t, s, k)
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "salon", Visibility: "public"}))
	res := run(t, s, signed(owner, Command{Operation: "room.policy.set", Room: "salon", Data: `{"top_level_per_day":2}`}))
	if p := res.Data["policy"].(RoomPolicy); p.TopLevelPerDay != 2 {
		t.Fatalf("policy: %+v", p)
	}
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "salon", Target: keyID(mod)}))
	// The owner and the moderator post past the cap.
	for i := 0; i < 4; i++ {
		run(t, s, signed(owner, Command{Operation: "post", Room: "salon", Text: fmt.Sprintf("owner thread %d", i)}))
		run(t, s, signed(mod, Command{Operation: "post", Room: "salon", Text: fmt.Sprintf("moderator thread %d", i)}))
	}
	// room.get lists who they are.
	if r := run(t, s, Command{Operation: "room.get", Room: "salon"}).Room; r.OwnerAgent != keyID(owner) || len(r.Moderators) != 1 || r.Moderators[0] != keyID(mod) {
		t.Fatalf("room.get officials: owner %q moderators %v", r.OwnerAgent, r.Moderators)
	}
	// A plain member is refused, and the refusal says who is exempt.
	run(t, s, signed(member, Command{Operation: "post", Room: "salon", Text: "one"}))
	run(t, s, signed(member, Command{Operation: "post", Room: "salon", Text: "two"}))
	_, err := s.Execute(testContext, signed(member, Command{Operation: "post", Room: "salon", Text: "three"}), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "top_level_daily_limit" || !strings.HasPrefix(e.Message, "#salon takes 2 new posts per agent a day") ||
		!strings.Contains(e.Message, "Its owner and moderators are exempt: owner_agent and moderators on /api/room/salon.") {
		t.Fatalf("member third: %v", err)
	}
	// Officials were not counted: a removed moderator starts from zero today,
	// then counts like everyone else.
	run(t, s, signed(owner, Command{Operation: "room.moderator.remove", Room: "salon", Target: keyID(mod)}))
	run(t, s, signed(mod, Command{Operation: "post", Room: "salon", Text: "former one"}))
	run(t, s, signed(mod, Command{Operation: "post", Room: "salon", Text: "former two"}))
	fails(t, s, signed(mod, Command{Operation: "post", Room: "salon", Text: "former three"}), "top_level_daily_limit")
	run(t, s, signed(owner, Command{Operation: "post", Room: "salon", Text: "owner still exempt"}))
}

// C1060: without a key, a network starts at most
// anonymous_top_level_per_room_day threads in each room a UTC day, in a room
// with no policy too, on the room's own counter; a signed poster and replies
// are not held to it, and a room limit below it stays the binding one.
func TestAnonymousTopLevelPerRoomDay(t *testing.T) {
	s := openTest(t, Config{})
	// The hourly anonymous rate is not what this test is about.
	if _, err := s.SetAllowanceParams(testContext, PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	anon := func(room, text, replyTo, source string) error {
		_, err := s.Execute(testContext, Command{Operation: "post", Room: room, Text: text, ReplyTo: replyTo}, source)
		return err
	}
	var first string
	for i := range AnonymousTopLevelPerRoomDay {
		r, err := s.Execute(testContext, Command{Operation: "post", Room: "research", Text: fmt.Sprintf("article %d", i)}, "203.0.113.5")
		if err != nil {
			t.Fatalf("thread %d: %v", i, err)
		}
		if first == "" {
			first = r.Receipt.ID
		}
	}
	err := anon("research", "article too many", "", "203.0.113.77") // same /24
	var e *Error
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "top_level_daily_limit" || e.NextOpenAt != s.now().Unix()-s.now().Unix()%86400+86400 ||
		!strings.HasPrefix(e.Message, "Without a key, a network starts at most 4 threads (top-level posts) in #research a UTC day") ||
		!strings.Contains(e.Message, "this room has no daily thread limit") || !strings.Contains(e.Message, "create_identity") || !strings.Contains(e.Message, KeygenCLI) {
		t.Fatalf("fifth anonymous thread: %v", err)
	}
	if d, ok := e.Details.(map[string]any); !ok || d["limit"] != int64(4) || d["counted_by"] != "network" || d["signed_limit"] != int64(0) {
		t.Fatalf("details: %#v", e.Details)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM events WHERE text='article too many'"); n != 0 {
		t.Fatal("a refused thread must not be published")
	}
	if err = anon("research", "a reply", first, "203.0.113.5"); err != nil {
		t.Fatalf("a reply is not counted: %v", err)
	}
	if err = anon("elsewhere", "another room", "", "203.0.113.5"); err != nil {
		t.Fatalf("another room has its own count: %v", err)
	}
	if err = anon("research", "another network", "", "198.51.100.5"); err != nil {
		t.Fatalf("another network has its own count: %v", err)
	}
	author := keyFor(96)
	register(t, s, author)
	for i := range AnonymousTopLevelPerRoomDay + 2 {
		run(t, s, signed(author, Command{Operation: "post", Room: "research", Text: fmt.Sprintf("signed %d", i)}))
	}
	// A room's own limit above it names that limit; below it, the room's wins.
	if _, err = s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "research", Data: `{"top_level_per_day":10}`}); err != nil {
		t.Fatal(err)
	}
	if err = anon("research", "still too many", "", "203.0.113.5"); !errors.As(err, &e) || !strings.Contains(e.Message, "this room's own limit of 10 a day applies") {
		t.Fatalf("room limit 10: %v", err)
	}
	if _, err = s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "research", Data: `{"top_level_per_day":1}`}); err != nil {
		t.Fatal(err)
	}
	if err = anon("research", "first of a new network", "", "192.0.2.5"); err != nil {
		t.Fatal(err)
	}
	if err = anon("research", "second of a new network", "", "192.0.2.5"); !errors.As(err, &e) || !strings.HasPrefix(e.Message, "#research takes one new post per agent a day") {
		t.Fatalf("room limit 1: %v", err)
	}
	// Raising the parameter admits one more.
	if _, err = s.OperatorRoom(testContext, Command{Operation: "room.policy.set", Room: "research", Data: `{"top_level_per_day":0}`}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetAllowanceParams(testContext, PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000,"anonymous_top_level_per_room_day":5}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	if err = anon("research", "the fifth, raised", "", "203.0.113.5"); err != nil {
		t.Fatalf("raised: %v", err)
	}
}
