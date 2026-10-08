package board

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func feedOrder(r Result) []string {
	out := []string{}
	for _, m := range r.Messages {
		out = append(out, m.ID)
	}
	return out
}

func feedRead(t *testing.T, s *Store, data string, limit int, cursor string) Result {
	t.Helper()
	return run(t, s, Command{Operation: "feed.get", Data: data, Limit: limit, Cursor: cursor})
}

func feedOverride(override string) string { return `{"override":` + override + `}` }

// seedFeed makes a board with votes, quality scores and replies spread over
// rooms and ages; it returns the posts by name.
func seedFeed(t *testing.T, s *Store) map[string]string {
	t.Helper()
	base := time.Unix(testTime, 0)
	at := func(hoursAgo int) { s.now = func() time.Time { return base.Add(-time.Duration(hoursAgo) * time.Hour) } }
	author, other := keyFor(71), keyFor(72)
	voters := []ed25519.PrivateKey{keyFor(73), keyFor(74), keyFor(75)}
	seasoned(t, s, append(append([]ed25519.PrivateKey{}, voters...), other)...)
	posts := map[string]string{}
	for i, p := range []struct {
		name, room string
		hours      int
		signed     bool
	}{
		{"old-loved", "lobby", 60, true}, {"fresh", "lobby", 1, true}, {"mid", "research", 10, true},
		{"anon", "lobby", 3, false}, {"research-new", "research", 2, true}, {"bounty", "bounties", 2, true},
		{"filler", "lobby", 5, true}, {"useful", "research", 20, true}, {"older", "lobby", 40, false},
	} {
		at(p.hours)
		if p.signed {
			posts[p.name] = postAs(t, s, author, Command{Room: p.room, Text: p.name, RequestID: fmt.Sprintf("seed-%d", i)})
		} else {
			posts[p.name] = run(t, s, Command{Operation: "post", Room: p.room, Text: p.name}).Receipt.ID
		}
	}
	at(0)
	for i, k := range voters {
		if _, err := voteAs(s, k, posts["old-loved"], "1", fmt.Sprintf("v-old-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := voteAs(s, voters[0], posts["mid"], "1", "v-mid"); err != nil {
		t.Fatal(err)
	}
	if _, err := voteAs(s, voters[1], posts["filler"], "-1", "v-filler"); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]float64{"useful": 0.95, "filler": 0.05, "fresh": 0.6} {
		if err := s.RecordQuality(testContext, posts[name], q, "jev-test"); err != nil {
			t.Fatal(err)
		}
	}
	postAs(t, s, other, Command{Room: "research", Text: "agree", ReplyTo: posts["useful"], RequestID: "reply-1"})
	s.dropRankings()
	return posts
}

// Parity: with no override feed.get is the hot view, post for post, cold or
// cached, at every page.
func TestFeedDefaultMatchesHot(t *testing.T) {
	s := openTest(t, Config{})
	seedFeed(t, s)
	hot := func() []string {
		return feedOrder(run(t, s, Command{Operation: "messages.list", Data: `{"sort":"hot"}`, Limit: 50}))
	}
	want := hot()
	if len(want) < 6 {
		t.Fatalf("seeded hot view too short: %v", want)
	}
	for _, data := range []string{"", `{}`, `{"profile":"default"}`, feedOverride(`{}`), feedOverride(`{"weights":{"votes":1,"quality":3}}`), feedOverride(`{"freshness":{"bias":1.5,"age_offset_hours":2}}`)} {
		s.dropRankings()
		res := feedRead(t, s, data, 50, "")
		if got := feedOrder(res); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("feed.get %s: %v, hot %v", data, got, want)
		}
		if res.Data["sort"] != "feed" || res.Data["profile_hash"] != FeedProfileHash(DefaultFeedProfile()) {
			t.Fatalf("data %v", res.Data)
		}
		// And the other way round: the hot view read after the feed (from
		// the inputs the feed cached) is the same.
		if got := hot(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("hot after feed: %v", got)
		}
	}
	// Page by page.
	var paged []string
	cursor := ""
	for range 10 {
		res := feedRead(t, s, "", 2, cursor)
		paged = append(paged, feedOrder(res)...)
		if res.Data["has_more"] != true {
			break
		}
		cursor = res.NextCursor
	}
	if strings.Join(paged, ",") != strings.Join(want, ",") {
		t.Fatalf("paged feed %v, hot %v", paged, want)
	}
	// The scores are the ranking function's.
	res := feedRead(t, s, `{"explain":true}`, 50, "")
	explain := res.Data["explain"].([]FeedExplain)
	for _, e := range explain {
		if e.Parts.RoomWeight != 1 || e.Score <= 0 && e.Parts.Votes >= 0 {
			t.Fatalf("explain %+v", e)
		}
		merit := e.Parts.Quality + e.Parts.Votes + e.Parts.Replies
		if math.Abs(merit*e.Parts.Decay-e.Score) > 1e-12 {
			t.Fatalf("parts do not make the score: %+v", e)
		}
	}
}

// The score is a pure function of the inputs and the profile.
func TestFeedScoreIsPure(t *testing.T) {
	now := testTime
	hour := int64(3600)
	scored := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	posts := []rankedPost{
		{id: "a", seq: 1, at: now - 2*hour, score: 2, quality: scored(0.9), replies: 3, room: "lobby", account: "x", signed: true},
		{id: "b", seq: 2, at: now - 10*hour, score: 0, replies: 9, room: "research", account: "y", signed: false},
		{id: "c", seq: 3, at: now, score: -1, quality: scored(0.2), edited: scored(0.1), room: "lobby", account: "z", signed: true},
	}
	def := defaultFeedScorer(BiasDefault)
	for _, r := range def.score(posts, now) {
		if want := Ranking.Rank(r.score, r.effectiveQuality(), r.replies, now-r.at, BiasDefault); r.rank != want {
			t.Fatalf("%s: default score %v, Ranking.Rank %v", r.id, r.rank, want)
		}
	}
	half := 12.0
	p := DefaultFeedProfile()
	p.Weights = FeedWeights{Quality: 1, Votes: 2, ReplyAgents: 1, ReplyAgentsMax: 5}
	p.Freshness = FeedFreshness{HalfLifeHours: &half}
	p.Sources.Rooms = []FeedRoom{{Room: "research", Weight: 2}}
	got := map[string]float64{}
	for _, r := range newFeedScorer(p, nil).score(posts, now) {
		got[r.id] = r.rank
	}
	want := map[string]float64{
		"a": (1*0.9 + 2*2 + 1*3) * math.Exp2(-2.0/12),
		"b": 2 * (1*0.5 + 0 + 1*5) * math.Exp2(-10.0/12),
		"c": (1*0.1 + 2*-1 + 0) * 1,
	}
	for id, w := range want {
		if math.Abs(got[id]-w) > 1e-12 {
			t.Errorf("%s: score %v, want %v", id, got[id], w)
		}
	}
	// Filters drop before scoring.
	p.Filters = FeedFilters{SignedOnly: true, MinQuality: 0.15, MutedRooms: []string{"research"}}
	if out := newFeedScorer(p, map[string]bool{"z": true}).score(posts, now); len(out) != 1 || out[0].id != "a" {
		t.Fatalf("filters kept %v", out)
	}
}

// An override reorders the feed as its weights say.
func TestFeedOverrideReorders(t *testing.T) {
	s := openTest(t, Config{})
	posts := seedFeed(t, s)
	first := func(data string) string {
		ids := feedOrder(feedRead(t, s, data, 50, ""))
		if len(ids) == 0 {
			t.Fatalf("%s: empty", data)
		}
		return ids[0]
	}
	has := func(data, name string) bool {
		for _, id := range feedOrder(feedRead(t, s, data, 50, "")) {
			if id == posts[name] {
				return true
			}
		}
		return false
	}
	if got := first(""); got != posts["fresh"] {
		t.Fatalf("default first %v", got)
	}
	// All-time votes first.
	if got := first(feedOverride(`{"freshness":{"bias":0}}`)); got != posts["old-loved"] {
		t.Fatalf("bias 0 first %v", got)
	}
	// Quality only, slow decay: the useful post.
	if got := first(feedOverride(`{"weights":{"votes":0,"reply_agents":0,"quality":10},"freshness":{"half_life_hours":720}}`)); got != posts["useful"] {
		t.Fatalf("quality first %v", got)
	}
	// A heavy room weight lifts research, and a room off the front page
	// joins when followed.
	if got := first(feedOverride(`{"sources":{"front":false,"rooms":[{"room":"research","weight":3},{"room":"lobby","weight":0.25}]}}`)); got != posts["research-new"] {
		t.Fatalf("room weight first %v", got)
	}
	if has("", "bounty") || !has(feedOverride(`{"sources":{"rooms":[{"room":"bounties"}]}}`), "bounty") {
		t.Fatal("a followed room off the front page")
	}
	// Filters.
	if has(feedOverride(`{"filters":{"signed_only":true}}`), "anon") || !has("", "anon") {
		t.Fatal("signed_only")
	}
	if has(feedOverride(`{"filters":{"min_quality":0.5}}`), "filler") || !has(feedOverride(`{"filters":{"min_quality":0.5}}`), "anon") {
		t.Fatal("min_quality keeps unscored as neutral and drops filler")
	}
	if has(feedOverride(`{"filters":{"muted_rooms":["research"]}}`), "useful") {
		t.Fatal("muted room")
	}
	if has(feedOverride(`{"filters":{"muted_authors":["`+keyID(keyFor(71))+`"]}}`), "fresh") || !has(feedOverride(`{"filters":{"muted_authors":["`+keyID(keyFor(71))+`"]}}`), "anon") {
		t.Fatal("muted author")
	}
	// Simulations rank only when included.
	sim := run(t, s, Command{Operation: "post", Room: "lobby", Text: "simulated", Kind: "simulation"}).Receipt.ID
	posts["sim"] = sim
	if has("", "sim") || !has(feedOverride(`{"filters":{"include_kinds":["simulation"]}}`), "sim") {
		t.Fatal("include_kinds")
	}
	// The merged profile is returned with its hash; a different override is
	// a different hash.
	res := feedRead(t, s, feedOverride(`{"weights":{"votes":2.1}}`), 5, "")
	p := res.Data["profile"].(FeedProfile)
	if p.Weights.Votes != 2 || res.Data["overridden"] != true || res.Data["profile_hash"] == FeedProfileHash(DefaultFeedProfile()) {
		t.Fatalf("snapped profile %+v data %v", p.Weights, res.Data)
	}
}

func TestFeedValidation(t *testing.T) {
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "hello"})
	for _, c := range []struct {
		data, code, says string
	}{
		{`{"sort":"hot"}`, "invalid_feed_profile", "sort is not an argument this method takes"},
		{feedOverride(`{"weights":{"vots":1}}`), "invalid_feed_profile", "override.weights.vots is not an argument this method takes"},
		{feedOverride(`{"name":"x"}`), "invalid_feed_profile", "override.name is not an argument"},
		{feedOverride(`{"weights":{"votes":11}}`), "invalid_feed_profile", "`override.weights.votes` must be 0 to 10 in steps of 0.25"},
		{feedOverride(`{"weights":{"votes":-1}}`), "invalid_feed_profile", "override.weights.votes"},
		{feedOverride(`{"weights":{"votes":"2"}}`), "invalid_feed_profile", "must be a number"},
		{feedOverride(`{"weights":{"votes":null}}`), "invalid_feed_profile", "must be a number"},
		{feedOverride(`{"weights":{"trusted_votes":1}}`), "invalid_feed_profile", "must be 0 for now"},
		{feedOverride(`{"weights":{"reply_agents_max":17}}`), "invalid_feed_profile", "reply_agents_max"},
		{feedOverride(`{"freshness":{"bias":5}}`), "invalid_feed_profile", "override.freshness.bias"},
		{feedOverride(`{"freshness":{"age_offset_hours":0}}`), "invalid_feed_profile", "age_offset_hours"},
		{feedOverride(`{"freshness":{"bias":1,"half_life_hours":12}}`), "invalid_feed_profile", "not both"},
		{feedOverride(`{"freshness":{"half_life_hours":0.5}}`), "invalid_feed_profile", "half_life_hours"},
		{feedOverride(`{"sources":{"front":false}}`), "invalid_feed_profile", "front page, rooms or both"},
		{feedOverride(`{"sources":{"rooms":[{"room":"lobby","weight":4}]}}`), "invalid_feed_profile", "override.sources.rooms.0.weight"},
		{feedOverride(`{"sources":{"rooms":[{"room":"lobby"},{"room":"lobby"}]}}`), "invalid_feed_profile", "twice"},
		{feedOverride(`{"sources":{"rooms":[{"room":"Bad Room"}]}}`), "invalid_feed_profile", "room name"},
		{feedOverride(`{"sources":{"rooms":[{"room":"nowhere"}]}}`), "room_not_found", "nowhere"},
		{feedOverride(`{"sources":{"rooms":[{"room":"lobby"}]},"filters":{"muted_rooms":["lobby"]}}`), "invalid_feed_profile", "mutes lobby"},
		{feedOverride(`{"filters":{"include_kinds":["note"]}}`), "invalid_feed_profile", "include_kinds"},
		{feedOverride(`{"filters":{"muted_authors":["abc"]}}`), "invalid_feed_profile", "fingerprints"},
		{feedOverride(`{"filters":{"min_quality":2}}`), "invalid_feed_profile", "min_quality"},
		{feedOverride(`{"schema":2}`), "invalid_feed_profile", "schema"},
		{feedOverride(`[]`), "invalid_feed_profile", "`override` must be an object"},
		{feedOverride(`{"weights":{"votes":1,"votes":2}}`), "invalid_feed_profile", "repeats a field"},
		{feedOverride(`{"weights":{"quality":1}` + strings.Repeat(" ", FeedOverrideBytes) + `}`), "invalid_feed_profile", "at most"},
		{`{"profile":"self"}`, "profile_not_found", "profile=default"},
		{`{"offset":-1}`, "invalid_offset", "offset"},
		{`{"offset":2001}`, "invalid_offset", "offset"},
		{`{"explain":"yes"}`, "invalid_feed_profile", "explain"},
		{`nope`, "invalid_feed_profile", "JSON object"},
	} {
		_, err := s.Execute(testContext, Command{Operation: "feed.get", Data: c.data}, "test-origin")
		var e *Error
		if !asError(err, &e) || e.Code != c.code || !strings.Contains(e.Message, c.says) {
			t.Errorf("%s: want %s (%q), got %v", c.data, c.code, c.says, err)
		}
	}
	// A private room reads as not found.
	owner := keyFor(76)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "secret", Visibility: "private", RequestID: "secret"}))
	if _, err := s.Execute(testContext, Command{Operation: "feed.get", Data: feedOverride(`{"sources":{"rooms":[{"room":"secret"}]}}`)}, "test-origin"); errCode(err) != "room_not_found" {
		t.Fatalf("private room: %v", err)
	}
	// Unknown command fields are refused as on any operation.
	if _, err := s.Execute(testContext, Command{Operation: "feed.get", Room: "lobby"}, "test-origin"); errCode(err) != "unexpected_field" {
		t.Fatalf("room on feed.get: %v", err)
	}
	// A cursor belongs to its feed.
	for range 3 {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: "more"})
	}
	res := feedRead(t, s, "", 1, "")
	if _, err := s.Execute(testContext, Command{Operation: "feed.get", Data: feedOverride(`{"weights":{"votes":2}}`), Cursor: res.NextCursor}, "test-origin"); errCode(err) != "invalid_cursor" {
		t.Fatalf("cursor of another feed: %v", err)
	}
	if _, err := s.Execute(testContext, Command{Operation: "feed.get", Data: `{"offset":1}`, Cursor: res.NextCursor}, "test-origin"); errCode(err) != "invalid_cursor" {
		t.Fatalf("offset and cursor: %v", err)
	}
	if _, err := s.Execute(testContext, Command{Operation: "feed.get", Cursor: "x:y"}, "test-origin"); err == nil {
		t.Fatal("accepted a forged cursor")
	}
}

// Pages neither repeat nor skip while posts arrive; once the pinned ranking
// has expired the cursor resumes by keyset instead of failing.
func TestFeedPagingStable(t *testing.T) {
	s := openTest(t, Config{})
	base := time.Unix(testTime, 0)
	for i := range 30 {
		s.now = func() time.Time { return base.Add(-time.Duration(30-i) * time.Minute) }
		run(t, s, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("post %d", i)})
	}
	s.now = func() time.Time { return base }
	override := feedOverride(`{"weights":{"votes":2},"freshness":{"half_life_hours":6}}`)
	all := feedOrder(feedRead(t, s, override, 50, ""))
	if len(all) != 30 {
		t.Fatalf("ranked %d", len(all))
	}
	seen := map[string]bool{}
	var got []string
	cursor := ""
	for page := 0; ; page++ {
		res := feedRead(t, s, override, 7, cursor)
		for _, id := range feedOrder(res) {
			if seen[id] {
				t.Fatalf("page %d repeats %s", page, id)
			}
			seen[id] = true
			got = append(got, id)
		}
		if res.Data["has_more"] != true {
			break
		}
		cursor = res.NextCursor
		// New posts arrive between pages.
		run(t, s, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("late %d", page)})
	}
	if strings.Join(got, ",") != strings.Join(all, ",") {
		t.Fatalf("paged %v\nwant %v", got, all)
	}
	// Expire the snapshot: the cursor resumes below its last post.
	first := feedRead(t, s, override, 10, "")
	s.now = func() time.Time { return base.Add(RankSnapshotTTL + time.Minute) }
	res := feedRead(t, s, override, 10, first.NextCursor)
	if res.Data["resumed_from"] != "keyset" {
		t.Fatalf("resumed_from %v", res.Data["resumed_from"])
	}
	for _, id := range feedOrder(res) {
		for _, before := range feedOrder(first) {
			if id == before {
				t.Fatalf("keyset resume repeated %s", id)
			}
		}
	}
	if len(feedOrder(res)) == 0 {
		t.Fatal("keyset resume read nothing")
	}
	// When the cursor's last post has left the ranking, it resumes below that
	// post's (score, seq).
	s.now = func() time.Time { return base.Add(2 * RankSnapshotTTL) }
	page := feedRead(t, s, override, 5, "")
	ids := feedOrder(page)
	if err := s.Moderate(testContext, ids[len(ids)-1], "test", true); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(3*RankSnapshotTTL + time.Minute) }
	next := feedRead(t, s, override, 5, page.NextCursor)
	if next.Data["resumed_from"] != "keyset" || len(feedOrder(next)) == 0 {
		t.Fatalf("keyset by score: %v %v", next.Data["resumed_from"], feedOrder(next))
	}
	for _, id := range feedOrder(next) {
		for _, before := range ids {
			if id == before {
				t.Fatalf("score keyset repeated %s", id)
			}
		}
	}
	// Offsets page the pinned ranking too.
	s.now = func() time.Time { return base.Add(RankSnapshotTTL + time.Minute) }
	p1 := feedRead(t, s, override, 5, "")
	p2 := feedRead(t, s, `{"offset":5,"override":{"weights":{"votes":2},"freshness":{"half_life_hours":6}}}`, 5, "")
	if p1.Data["next_offset"] != 5 || len(feedOrder(p2)) != 5 || feedOrder(p2)[0] == feedOrder(p1)[0] {
		t.Fatalf("offset pages %v %v", p1.Data, feedOrder(p2))
	}
}

// Caps: 50 rooms at most; a read builds at most FeedColdSlices room slices
// and names the rest as warming; the candidate inputs stay shared.
func TestFeedCaps(t *testing.T) {
	s := openTest(t, Config{})
	rooms := []string{}
	for i := range FeedRoomsMax + 1 {
		room := fmt.Sprintf("room%02d", i)
		rooms = append(rooms, `{"room":"`+room+`"}`)
		run(t, s, Command{Operation: "post", Room: room, Text: "hello " + room})
	}
	fails(t, s, Command{Operation: "feed.get", Data: feedOverride(`{"sources":{"rooms":[` + strings.Join(rooms, ",") + `]}}`)}, "too_many_rooms")
	override := feedOverride(`{"sources":{"front":false,"rooms":[` + strings.Join(rooms[:FeedRoomsMax], ",") + `]}}`)
	warming := FeedRoomsMax
	for read := 0; warming > 0; read++ {
		res := feedRead(t, s, override, 100, "")
		w := res.Data["warming"].([]string)
		if want := max(warming-FeedColdSlices, 0); len(w) != want {
			t.Fatalf("read %d: %d warming, want %d", read, len(w), want)
		}
		if got := len(res.Messages); got != FeedRoomsMax-len(w) {
			t.Fatalf("read %d: %d posts with %d warming", read, got, len(w))
		}
		warming = len(w)
	}
	s.rankMu.Lock()
	slices := len(s.candCache)
	s.rankMu.Unlock()
	if slices != FeedRoomsMax {
		t.Fatalf("%d cached slices", slices)
	}
	// Every candidate walk is bounded by HotCandidates, and a ranking keeps
	// at most a page past the largest offset.
	if FeedRoomSlice > HotCandidates || FeedColdSlices*FeedRoomSlice > HotCandidates {
		t.Fatal("cold slices exceed the candidate budget")
	}
	s.rankMu.Lock()
	for _, e := range s.rankCache {
		if len(e.base) > HotCandidates+PageMax+1 {
			t.Fatal("ranking past its bound")
		}
	}
	s.rankMu.Unlock()
}

// Everything feed.get reads goes through the read's own transaction: on the
// single connection, a pool use under it would block until the deadline.
func TestFeedSingleConnection(t *testing.T) {
	s := openTest(t, Config{})
	seedFeed(t, s)
	if n := s.db.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("store pool of %d connections", n)
	}
	for _, data := range []string{"", feedOverride(`{"sources":{"rooms":[{"room":"research","weight":2}]},"filters":{"muted_authors":["` + keyID(keyFor(71)) + `"]}}`)} {
		s.dropRankings()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		_, err := s.Execute(ctx, Command{Operation: "feed.get", Data: data}, "test-origin")
		cancel()
		if err != nil || time.Since(start) > 4*time.Second {
			t.Fatalf("feed.get under one connection: %v after %v", err, time.Since(start))
		}
	}
}

// The profile document and its hash are canonical: equal profiles hash
// equal however the override spelled them.
func TestFeedProfileHash(t *testing.T) {
	a, err := applyFeedOverride(DefaultFeedProfile(), json.RawMessage(`{"filters":{"muted_rooms":["b","a","a"]},"sources":{"rooms":[{"room":"y"},{"room":"x","weight":1.1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := applyFeedOverride(DefaultFeedProfile(), json.RawMessage(`{"sources":{"rooms":[{"room":"x","weight":1},{"room":"y","weight":1}]},"filters":{"muted_rooms":["a","b"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if FeedProfileHash(a) != FeedProfileHash(b) || !strings.HasPrefix(FeedProfileHash(a), "sha256:") {
		t.Fatalf("%s vs %s", FeedProfileHash(a), FeedProfileHash(b))
	}
}
