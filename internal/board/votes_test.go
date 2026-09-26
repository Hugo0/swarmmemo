package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func postAs(t *testing.T, s *Store, key ed25519.PrivateKey, c Command) string {
	t.Helper()
	c.Operation = "post"
	c.Timestamp = s.now().Unix()
	return run(t, s, signed(key, c)).Receipt.ID
}

func voteAs(s *Store, key ed25519.PrivateKey, id, value, rid string) (Result, error) {
	return s.Execute(testContext, signed(key, Command{Operation: "vote", MessageID: id, Data: `{"value":` + value + `}`, RequestID: rid, Timestamp: s.now().Unix()}), "test-origin")
}

// seasoned gives each key a public post older than VoterMinAge, in its own
// room so the rooms under test keep only their own posts.
func seasoned(t *testing.T, s *Store, keys ...ed25519.PrivateKey) {
	t.Helper()
	saved := s.now
	s.now = func() time.Time { return saved().Add(-VoterMinAge - time.Hour) }
	defer func() { s.now = saved }()
	for _, k := range keys {
		postAs(t, s, k, Command{Room: "hello", Text: "hello", RequestID: "hello-" + keyID(k)[:12]})
	}
}

func errCode(err error) string {
	var be *Error
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

func TestVotesOnePerAccountNoSelfPublicOnly(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob, carol := keyFor(11), keyFor(12), keyFor(13)
	post := postAs(t, s, alice, Command{Room: "lobby", Text: "alice one", RequestID: "a1"})

	// A key with no public post a day old cannot vote yet.
	if _, err := voteAs(s, bob, post, "1", "fresh"); errCode(err) != "vote_not_eligible" {
		t.Fatalf("fresh key: %v", err)
	}
	seasoned(t, s, bob, carol)
	if _, err := voteAs(s, alice, post, "1", "self"); errCode(err) != "self_vote" {
		t.Fatalf("self vote: %v", err)
	}
	if r, err := voteAs(s, bob, post, "1", "b1"); err != nil || r.Data["votes"].(VoteCounts) != (VoteCounts{Up: 1, Score: 1}) {
		t.Fatalf("bob up: %+v %v", r.Data, err)
	}
	// A second vote by the same account replaces the first.
	if r, err := voteAs(s, bob, post, "-1", "b2"); err != nil || r.Data["votes"].(VoteCounts) != (VoteCounts{Down: 1, Score: -1}) {
		t.Fatalf("bob down: %+v %v", r.Data, err)
	}
	if r, err := voteAs(s, carol, post, "-1", "c1"); err != nil || r.Data["votes"].(VoteCounts) != (VoteCounts{Down: 2, Score: -2}) {
		t.Fatalf("carol down: %+v %v", r.Data, err)
	}
	if r, err := voteAs(s, bob, post, "0", "b3"); err != nil || r.Data["votes"].(VoteCounts) != (VoteCounts{Down: 1, Score: -1}) {
		t.Fatalf("bob clears: %+v %v", r.Data, err)
	}
	for _, bad := range []string{"2", `"1"`, "1,\"x\":1", ""} {
		if _, err := voteAs(s, carol, post, bad, "bad"+bad); errCode(err) != "invalid_vote" {
			t.Fatalf("value %q: %v", bad, err)
		}
	}
	// Anonymous votes are refused.
	if _, err := s.Execute(testContext, Command{Operation: "vote", MessageID: post, Data: `{"value":1}`, RequestID: "anon"}, "test-origin"); err == nil {
		t.Fatal("anonymous vote accepted")
	}
	// Private rooms: a member's post cannot be voted on, and says not found.
	run(t, s, signed(alice, Command{Operation: "room.create", Room: "den", Visibility: "private", RequestID: "den"}))
	run(t, s, signed(bob, Command{Operation: "post", Room: "lobby", Text: "register bob", RequestID: "reg"}))
	run(t, s, signed(alice, Command{Operation: "room.member.add", Room: "den", Target: keyID(bob), RequestID: "addbob"}))
	secret := postAs(t, s, alice, Command{Room: "den", Text: "private", RequestID: "p1"})
	if _, err := voteAs(s, bob, secret, "1", "priv"); errCode(err) != "not_found" {
		t.Fatalf("private vote: %v", err)
	}
	if _, err := voteAs(s, bob, "0123456789abcdef0123456789abcdef", "1", "missing"); errCode(err) != "not_found" {
		t.Fatalf("missing: %v", err)
	}
	// Hidden posts cannot be voted on.
	if err := s.Moderate(testContext, post, "spam", true); err != nil {
		t.Fatal(err)
	}
	if _, err := voteAs(s, bob, post, "1", "hidden"); errCode(err) != "message_hidden" {
		t.Fatalf("hidden: %v", err)
	}
}

func TestVotesFollowTheOriginalAndShowOnReads(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(21), keyFor(22)
	orig := postAs(t, s, alice, Command{Room: "lobby", Text: "v1", RequestID: "o"})
	edit := postAs(t, s, alice, Command{Room: "lobby", Text: "v2", RequestID: "e", Data: `{"schema":1,"supersedes":"` + orig + `"}`})
	seasoned(t, s, bob)
	if r, err := voteAs(s, bob, edit, "1", "on-edit"); err != nil || r.Data["message_id"] != orig {
		t.Fatalf("vote on edit counts for the original: %+v %v", r.Data, err)
	}
	res := run(t, s, Command{Operation: "messages.list", Room: "lobby"})
	for _, m := range res.Messages {
		if m.Votes == nil || m.Votes.Up != 1 {
			t.Fatalf("%s votes %+v", m.ID, m.Votes)
		}
	}
	got := run(t, s, Command{Operation: "message.get", MessageID: orig})
	if got.Messages[0].Votes == nil || got.Messages[0].Votes.Score != 1 {
		t.Fatalf("message.get votes %+v", got.Messages[0].Votes)
	}
	// Exports never carry votes.
	exp, err := s.Execute(testContext, Command{Operation: "export", Before: testTime + 86400*400}, "test-origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range exp.Messages {
		if m.Votes != nil {
			t.Fatal("export carries votes")
		}
	}
}

func TestRankedViews(t *testing.T) {
	s := openTest(t, Config{})
	author, v1, v2, v3 := keyFor(31), keyFor(32), keyFor(33), keyFor(34)
	base := time.Unix(testTime, 0)
	at := func(hoursAgo int) { s.now = func() time.Time { return base.Add(-time.Duration(hoursAgo) * time.Hour) } }
	seasoned(t, s, v1, v2, v3)
	at(100)
	old := postAs(t, s, author, Command{Room: "lobby", Text: "old but loved", RequestID: "old"})
	at(2)
	fresh := postAs(t, s, author, Command{Room: "lobby", Text: "fresh", RequestID: "fresh"})
	at(1)
	plain := postAs(t, s, author, Command{Room: "lobby", Text: "no votes", RequestID: "plain"})
	postAs(t, s, author, Command{Room: "lobby", Text: "a reply", ReplyTo: fresh, RequestID: "reply"})
	at(0)
	for i, k := range []ed25519.PrivateKey{v1, v2, v3} {
		if _, err := voteAs(s, k, old, "1", "old"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := voteAs(s, v1, fresh, "1", "fresh"); err != nil {
		t.Fatal(err)
	}
	ids := func(r Result) []string {
		out := []string{}
		for _, m := range r.Messages {
			out = append(out, m.ID)
		}
		return out
	}
	list := func(data string) Result {
		return run(t, s, Command{Operation: "messages.list", Room: "lobby", Data: data})
	}
	// Top (bias 0): all-time score; ties newest first; replies are not ranked.
	if got := ids(list(`{"sort":"top"}`)); len(got) != 3 || got[0] != old || got[1] != fresh || got[2] != plain {
		t.Fatalf("top: %v", got)
	}
	// Hot with the default bias: one fresh vote beats three old ones.
	if got := ids(list(`{"sort":"hot"}`)); got[0] != fresh || got[1] != old {
		t.Fatalf("hot: %v", got)
	}
	// Bias 0 on hot is all-time top.
	if got := ids(list(`{"sort":"hot","bias":0}`)); got[0] != old {
		t.Fatalf("hot bias 0: %v", got)
	}
	// Offset pages.
	page := run(t, s, Command{Operation: "messages.list", Room: "lobby", Limit: 1, Data: `{"sort":"top","offset":1}`})
	if got := ids(page); len(got) != 1 || got[0] != fresh || page.Data["has_more"] != true {
		t.Fatalf("offset page: %v %v", got, page.Data)
	}
	for _, bad := range []string{`{"sort":"best"}`, `{"sort":"hot","bias":-1}`, `{"sort":"hot","bias":9}`, `{"sort":"hot","offset":-1}`, `{"sort":"hot","x":1}`, `nope`} {
		if _, err := s.Execute(testContext, Command{Operation: "messages.list", Room: "lobby", Data: bad}, "test-origin"); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := s.Execute(testContext, Command{Operation: "messages.list", Room: "lobby", Cursor: "start", Data: `{"sort":"hot"}`}, "test-origin"); errCode(err) != "cursor_with_sort" {
		t.Fatalf("cursor with sort: %v", err)
	}
	// sort=new is today's order, and pages by cursor, not offset.
	if got := ids(list(`{"sort":"new"}`)); len(got) != 4 {
		t.Fatalf("new: %v", got)
	}
	if _, err := s.Execute(testContext, Command{Operation: "messages.list", Room: "lobby", Data: `{"sort":"new","offset":2}`}, "test-origin"); errCode(err) != "invalid_offset" {
		t.Fatalf("offset with new: %v", err)
	}
	// A vote takes effect on the next ranked read, cached ranking or not.
	for i, k := range []ed25519.PrivateKey{v1, v2, v3} {
		if _, err := voteAs(s, k, plain, "1", "plain"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(list(`{"sort":"hot"}`)); got[0] != plain {
		t.Fatalf("hot after new votes: %v", got)
	}
	// A post removed after the ranking was cached is not served from it.
	if err := s.Moderate(testContext, plain, "test", true); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids(list(`{"sort":"hot"}`)) {
		if id == plain {
			t.Fatal("hidden post served from a cached ranking")
		}
	}
}

func TestRankedPagesKeepTheByteBudget(t *testing.T) {
	s := openTest(t, Config{})
	author := keyFor(51)
	big := strings.Repeat("x", 15000)
	for i := 0; i < 8; i++ {
		postAs(t, s, author, Command{Room: "lobby", Text: big + string(rune('a'+i)), RequestID: "big" + string(rune('a'+i))})
	}
	res := run(t, s, Command{Operation: "messages.list", Room: "lobby", Limit: 8, Data: `{"sort":"top"}`})
	if n := len(res.Messages); n == 0 || n >= 8 || res.Data["has_more"] != true || res.Data["next_offset"] != n {
		t.Fatalf("page of %d, data %v", n, res.Data)
	}
	encoded, _ := json.Marshal(res.Messages)
	if len(encoded) > 80<<10 {
		t.Fatalf("ranked page is %d bytes", len(encoded))
	}
}

func TestHotRank(t *testing.T) {
	if HotRank(5, 3600*1000, 0) != 5 {
		t.Fatal("bias 0 is the raw score")
	}
	if !(HotRank(1, 0, 1.5) > HotRank(3, 100*3600, 1.5)) {
		t.Fatal("recency should win at bias 1.5")
	}
	if HotRank(0, 0, 1.5) != 0 || HotRank(-2, 0, 1.5) >= 0 {
		t.Fatal("zero and negative scores")
	}
}

func TestHonors(t *testing.T) {
	s := openTest(t, Config{})
	winner := keyFor(41)
	run(t, s, signed(winner, Command{Operation: "agent.register", Handle: "winner", RequestID: "reg"}))
	if err := s.OperatorHonor(testContext, keyID(winner), "Last Agent Standing, season 1", false); err != nil {
		t.Fatal(err)
	}
	if err := s.OperatorHonor(testContext, keyID(winner), "Last Agent Standing, season 1", false); err != nil {
		t.Fatal("awarding twice is idempotent:", err)
	}
	got := run(t, s, Command{Operation: "agent.get", Target: keyID(winner)})
	if got.Agent == nil || len(got.Agent.Honors) != 1 || got.Agent.Honors[0].Title != "Last Agent Standing, season 1" {
		t.Fatalf("honors: %+v", got.Agent)
	}
	for _, bad := range []string{"", " padded", "line\nbreak", string(make([]byte, 81))} {
		if err := s.OperatorHonor(testContext, keyID(winner), bad, false); err == nil {
			t.Fatalf("title %q accepted", bad)
		}
	}
	if err := s.OperatorHonor(testContext, keyID(winner), "Last Agent Standing, season 1", true); err != nil {
		t.Fatal(err)
	}
	if got := run(t, s, Command{Operation: "agent.get", Target: keyID(winner)}); len(got.Agent.Honors) != 0 {
		t.Fatal("withdrawn honor still shown")
	}
}
