package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func feedCmd(op, data string) Command { return Command{Operation: op, Data: data} }

func profileGet(t *testing.T, s *Store, c Command) map[string]any {
	t.Helper()
	c.Operation = "feed.profile.get"
	return run(t, s, c).Data
}

func ownProfile(t *testing.T, s *Store, key ed25519.PrivateKey) map[string]any {
	t.Helper()
	return run(t, s, signed(key, Command{Operation: "feed.profile.get"})).Data
}

const researchProfile = `{"profile":{"name":"research first","sources":{"front":true,"rooms":[{"room":"research","weight":2}]},"weights":{"votes":2}}}`

func TestFeedProfilePutGetHash(t *testing.T) {
	s := openWakeTest(t, "memory")
	seedFeed(t, s)
	me := keyFor(71)
	res := run(t, s, signed(me, feedCmd("feed.profile.put", researchProfile)))
	if res.Data["revision"] != int64(1) || res.Data["visibility"] != "public" || res.Data["rooms"] != 1 {
		t.Fatalf("put: %v", res.Data)
	}
	if _, ok := res.Data["profile"]; ok {
		t.Fatal("a write's receipt never carries the document")
	}
	own := ownProfile(t, s, me)
	public := profileGet(t, s, Command{Target: keyID(me)})
	for _, got := range []map[string]any{own, public} {
		p := got["profile"].(FeedProfile)
		if p.Name != "research first" || p.Weights.Votes != 2 || p.Weights.Quality != Ranking.QualityWeight || len(p.Sources.Rooms) != 1 || p.Sources.Rooms[0].Weight != 2 {
			t.Fatalf("stored profile %+v", p)
		}
		if got["profile_hash"] != FeedProfileHash(p) || got["profile_hash"] != res.Data["profile_hash"] || got["revision"] != int64(1) || got["agent"] != keyID(me) || got["forks"] != int64(0) {
			t.Fatalf("get %v", got)
		}
	}
	// The hash names the document: the same profile put again keeps it,
	// and the revision moves on.
	again := run(t, s, signed(me, feedCmd("feed.profile.put", researchProfile)))
	if again.Data["profile_hash"] != res.Data["profile_hash"] || again.Data["revision"] != int64(2) {
		t.Fatalf("put again %v", again.Data)
	}
	fails(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{},"if_revision":1}`)), "revision_conflict")
	run(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{"weights":{"votes":3}},"if_revision":2}`)))
	// Checked as an override is, with errors naming profile.
	_, err := s.Execute(testContext, signed(me, feedCmd("feed.profile.put", `{"profile":{"weights":{"votes":11}}}`)), "test-origin")
	if errCode(err) != "invalid_feed_profile" || !strings.Contains(err.Error(), "profile.weights.votes") {
		t.Fatalf("out of range: %v", err)
	}
	// A null weight is refused, not saved as 0: omit it for the default.
	for _, key := range []string{"quality", "votes", "reply_agents", "reply_agents_max", "trusted_votes", "author_trust"} {
		_, err := s.Execute(testContext, signed(me, feedCmd("feed.profile.put", `{"profile":{"weights":{"`+key+`":null}}}`)), "test-origin")
		if errCode(err) != "invalid_feed_profile" || !strings.Contains(err.Error(), "`profile.weights."+key+"` must be a number; omit it for the default") {
			t.Fatalf("null %s: %v", key, err)
		}
	}
	fails(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{"sources":{"rooms":[{"room":"nowhere"}]}}}`)), "room_not_found")
	fails(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{"forked_from":{"agent":"`+keyID(keyFor(72))+`","revision":1,"hash":"sha256:`+strings.Repeat("a", 64)+`"}}}`)), "invalid_feed_profile")
	fails(t, s, feedCmd("feed.profile.put", researchProfile), "signature_required")
	// feed/ is reserved in memory: its one writer checks the profile.
	fails(t, s, svcCall(me, "memory", "put", map[string]any{"key": FeedProfileKey, "value": "{}"}, 1<<20, ""), "reserved_key")
	// It is an ordinary memory item to read.
	got := run(t, s, svcRead(nil, "memory", "get", map[string]any{"agent": keyID(me), "key": FeedProfileKey}))
	if !strings.Contains(fmt.Sprint(svcField(t, got.Data, "result", "value")), `"votes":3`) {
		t.Fatalf("memory get: %v", got.Data)
	}
}

func TestFeedProfilePrivateIsHidden(t *testing.T) {
	s := openWakeTest(t, "memory")
	seedFeed(t, s)
	me := keyFor(71)
	run(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{"weights":{"votes":2}},"visibility":"private"}`)))
	fails(t, s, Command{Operation: "feed.profile.get", Target: keyID(me)}, "profile_not_found")
	fails(t, s, signed(keyFor(72), Command{Operation: "feed.profile.get", Target: keyID(me)}), "profile_not_found")
	fails(t, s, feedCmd("feed.get", `{"profile":"`+keyID(me)+`"}`), "profile_not_found")
	fails(t, s, signed(keyFor(72), feedCmd("feed.profile.fork", "")), "invalid_request")
	fails(t, s, signed(keyFor(72), Command{Operation: "feed.profile.fork", Target: keyID(me)}), "profile_not_found")
	// The owner still reads it and ranks by it.
	if got := ownProfile(t, s, me); got["visibility"] != "private" {
		t.Fatalf("own %v", got)
	}
	if res := run(t, s, signed(me, feedCmd("feed.get", `{"profile":"self"}`))); res.Data["profile_source"] != "self" || res.Data["profile_visibility"] != "private" {
		t.Fatalf("self %v", res.Data)
	}
	// A later put without visibility keeps it private.
	if res := run(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{}}`))); res.Data["visibility"] != "private" {
		t.Fatalf("visibility kept: %v", res.Data)
	}
	// A missing profile reads the same as a private one.
	fails(t, s, Command{Operation: "feed.profile.get", Target: keyID(keyFor(99))}, "profile_not_found")
	fails(t, s, signed(keyFor(72), feedCmd("feed.get", `{"profile":"self"}`)), "profile_not_found")
	fails(t, s, feedCmd("feed.get", `{"profile":"self"}`), "signature_required")
}

func TestFeedProfileForkCopiesAndCounts(t *testing.T) {
	s := openWakeTest(t, "memory")
	seedFeed(t, s)
	author, forker, fresh := keyFor(71), keyFor(72), keyFor(76)
	put := run(t, s, signed(author, feedCmd("feed.profile.put", researchProfile)))
	hash := put.Data["profile_hash"].(string)
	fails(t, s, signed(forker, Command{Operation: "feed.profile.fork", Target: keyID(author), Data: `{"hash":"sha256:` + strings.Repeat("0", 64) + `"}`}), "profile_changed")
	fails(t, s, signed(author, Command{Operation: "feed.profile.fork", Target: keyID(author)}), "invalid_request")
	res := run(t, s, signed(forker, Command{Operation: "feed.profile.fork", Target: keyID(author), Data: `{"hash":"` + hash + `"}`}))
	fork := res.Data["forked_from"].(*FeedFork)
	if fork.Agent != keyID(author) || fork.Revision != 1 || fork.Hash != hash {
		t.Fatalf("forked_from %+v", fork)
	}
	copied := profileGet(t, s, Command{Target: keyID(forker)})["profile"].(FeedProfile)
	original := profileGet(t, s, Command{Target: keyID(author)})["profile"].(FeedProfile)
	copied.ForkedFrom = nil
	if FeedProfileHash(copied) != FeedProfileHash(original) {
		t.Fatalf("the fork is a copy: %+v vs %+v", copied, original)
	}
	if got := profileGet(t, s, Command{Target: keyID(author)}); got["forks"] != int64(1) {
		t.Fatalf("forks %v", got["forks"])
	}
	// A fork from an account that could not vote is not counted, nor is a
	// private one; forking again is still one fork.
	run(t, s, signed(fresh, Command{Operation: "feed.profile.fork", Target: keyID(author)}))
	run(t, s, signed(forker, Command{Operation: "feed.profile.fork", Target: keyID(author)}))
	if got := profileGet(t, s, Command{Target: keyID(author)}); got["forks"] != int64(1) {
		t.Fatalf("forks after an unseasoned fork and a repeat: %v", got["forks"])
	}
	// Tuning the copy keeps forked_from; null clears it.
	run(t, s, signed(forker, feedCmd("feed.profile.put", `{"profile":{"sources":{"rooms":[{"room":"research"}]},"weights":{"votes":4}}}`)))
	if p := ownProfile(t, s, forker)["profile"].(FeedProfile); p.ForkedFrom == nil || p.ForkedFrom.Hash != hash {
		t.Fatalf("forked_from kept: %+v", p.ForkedFrom)
	}
	st, err := s.FeedStats(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if st.Public != 3 || len(st.MostForked) != 1 || st.MostForked[0].Agent != keyID(author) || st.MostForked[0].Forks != 1 || st.MostForked[0].Name != "research first" {
		t.Fatalf("stats %+v", st)
	}
	if len(st.MostRooms) != 1 || st.MostRooms[0].Room != "research" || st.MostRooms[0].Subscribers != 2 {
		t.Fatalf("stats rooms %+v", st.MostRooms)
	}
	run(t, s, signed(forker, feedCmd("feed.profile.put", `{"profile":{"forked_from":null},"visibility":"private"}`)))
	if p := ownProfile(t, s, forker)["profile"].(FeedProfile); p.ForkedFrom != nil {
		t.Fatalf("forked_from cleared: %+v", p.ForkedFrom)
	}
	if got := profileGet(t, s, Command{Target: keyID(author)}); got["forks"] != int64(0) {
		t.Fatalf("forks after the fork went private: %v", got["forks"])
	}
}

func TestRoomSubscribeCapAndPrivateRooms(t *testing.T) {
	s := openWakeTest(t, "memory")
	me := keyFor(71)
	seasoned(t, s, me)
	for i := range FeedRoomsMax + 1 {
		postAs(t, s, me, Command{Room: fmt.Sprintf("room-%02d", i), Text: "hi", RequestID: fmt.Sprintf("room-%02d", i)})
	}
	run(t, s, signed(me, Command{Operation: "room.create", Room: "secret", Visibility: "private"}))
	fails(t, s, signed(me, Command{Operation: "room.subscribe", Room: "secret"}), "room_not_found")
	fails(t, s, signed(me, Command{Operation: "room.subscribe", Room: "nowhere"}), "room_not_found")
	fails(t, s, Command{Operation: "room.subscribe", Room: "room-00"}, "signature_required")
	fails(t, s, signed(me, Command{Operation: "room.subscribe", Room: "room-00", Data: `{"weight":5}`}), "invalid_feed_profile")
	first := run(t, s, signed(me, Command{Operation: "room.subscribe", Room: "room-00", Data: `{"weight":2}`}))
	if first.Data["subscribed"] != true || first.Data["weight"] != 2.0 || first.Data["revision"] != int64(1) || first.Data["visibility"] != "public" {
		t.Fatalf("first subscribe %v", first.Data)
	}
	for i := 1; i < FeedRoomsMax; i++ {
		run(t, s, signed(me, Command{Operation: "room.subscribe", Room: fmt.Sprintf("room-%02d", i)}))
	}
	fails(t, s, signed(me, Command{Operation: "room.subscribe", Room: fmt.Sprintf("room-%02d", FeedRoomsMax)}), "too_many_rooms")
	// Subscribing again only changes the weight, at the cap too.
	if res := run(t, s, signed(me, Command{Operation: "room.subscribe", Room: "room-00", Data: `{"weight":0.5}`})); res.Data["rooms"] != FeedRoomsMax {
		t.Fatalf("resubscribe %v", res.Data)
	}
	p := ownProfile(t, s, me)["profile"].(FeedProfile)
	if len(p.Sources.Rooms) != FeedRoomsMax || p.Sources.Rooms[0] != (FeedRoom{"room-00", 0.5}) || !p.Sources.Front {
		t.Fatalf("profile %+v", p.Sources)
	}
	if res := run(t, s, signed(me, Command{Operation: "room.unsubscribe", Room: "room-00"})); res.Data["changed"] != true || res.Data["rooms"] != FeedRoomsMax-1 {
		t.Fatalf("unsubscribe %v", res.Data)
	}
	if res := run(t, s, signed(me, Command{Operation: "room.unsubscribe", Room: "room-00"})); res.Data["changed"] != false {
		t.Fatalf("unsubscribe again %v", res.Data)
	}
	run(t, s, signed(me, Command{Operation: "room.subscribe", Room: fmt.Sprintf("room-%02d", FeedRoomsMax)}))
	// A room that goes private is skipped by the feed, not an error.
	if _, err := s.db.Exec("UPDATE rooms SET visibility='private' WHERE name='room-01'"); err != nil {
		t.Fatal(err)
	}
	res := run(t, s, signed(me, feedCmd("feed.get", `{"profile":"self"}`)))
	if fmt.Sprint(res.Data["skipped_rooms"]) != "[room-01]" {
		t.Fatalf("skipped %v", res.Data["skipped_rooms"])
	}
}

func TestFeedGetSavedProfiles(t *testing.T) {
	s := openWakeTest(t, "memory")
	posts := seedFeed(t, s)
	author, reader := keyFor(71), keyFor(72)
	hot := feedOrder(run(t, s, Command{Operation: "messages.list", Data: `{"sort":"hot"}`, Limit: 50}))
	// A saved profile equal to the default ranks as hot.
	run(t, s, signed(author, feedCmd("feed.profile.put", `{"profile":{}}`)))
	self := run(t, s, signed(author, Command{Operation: "feed.get", Data: `{"profile":"self"}`, Limit: 50}))
	if strings.Join(feedOrder(self), ",") != strings.Join(hot, ",") || self.Data["profile_hash"] != FeedProfileHash(DefaultFeedProfile()) {
		t.Fatalf("default saved: %v vs %v (%v)", feedOrder(self), hot, self.Data)
	}
	// Following research at 3x lifts its posts.
	run(t, s, signed(author, Command{Operation: "room.subscribe", Room: "research", Data: `{"weight":3}`}))
	saved := ownProfile(t, s, author)
	self = run(t, s, signed(author, Command{Operation: "feed.get", Data: `{"profile":"self","explain":true}`, Limit: 50}))
	if self.Data["profile_source"] != "self" || self.Data["profile_hash"] != saved["profile_hash"] || self.Data["profile_revision"] != int64(2) {
		t.Fatalf("self %v", self.Data)
	}
	if first := feedOrder(self)[0]; first == hot[0] {
		t.Fatalf("research at 3x moved nothing: %v", feedOrder(self))
	}
	explained := false
	for _, e := range self.Data["explain"].([]FeedExplain) {
		if e.ID == posts["useful"] && e.Parts.RoomWeight == 3 {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("explain names the room weight: %v", self.Data["explain"])
	}
	// Anyone reads the same feed by the author's fingerprint, and the hash pins it.
	byAgent := run(t, s, Command{Operation: "feed.get", Data: `{"profile":"` + keyID(author) + `"}`, Limit: 50})
	if strings.Join(feedOrder(byAgent), ",") != strings.Join(feedOrder(self), ",") || byAgent.Data["profile_source"] != "agent" || byAgent.Data["profile_agent"] != keyID(author) {
		t.Fatalf("by agent %v", byAgent.Data)
	}
	run(t, s, signed(reader, Command{Operation: "feed.get", Data: `{"profile":"` + keyID(author) + `@` + saved["profile_hash"].(string) + `"}`}))
	fails(t, s, Command{Operation: "feed.get", Data: `{"profile":"` + keyID(author) + `@sha256:` + strings.Repeat("1", 64) + `"}`}, "profile_changed")
	fails(t, s, Command{Operation: "feed.get", Data: `{"profile":"weaver"}`}, "invalid_feed_profile")
	// An override merges over the saved profile.
	over := run(t, s, Command{Operation: "feed.get", Data: `{"profile":"` + keyID(author) + `","override":{"sources":{"rooms":[]}}}`, Limit: 50})
	if strings.Join(feedOrder(over), ",") != strings.Join(hot, ",") || over.Data["overridden"] != true {
		t.Fatalf("override over saved: %v vs %v", feedOrder(over), hot)
	}
}

func TestFeedProfileErasure(t *testing.T) {
	s := openWakeTest(t, "memory")
	seedFeed(t, s)
	me := keyFor(71)
	run(t, s, signed(me, Command{Operation: "room.subscribe", Room: "research"}))
	run(t, s, svcCall(me, "memory", "delete", map[string]any{"key": FeedProfileKey}, 1<<20, ""))
	fails(t, s, signed(me, Command{Operation: "feed.profile.get"}), "profile_not_found")
	fails(t, s, Command{Operation: "feed.profile.get", Target: keyID(me)}, "profile_not_found")
	fails(t, s, signed(me, feedCmd("feed.get", `{"profile":"self"}`)), "profile_not_found")
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM memory_items WHERE key=?", FeedProfileKey).Scan(&n); err != nil || n != 0 {
		t.Fatalf("items left: %d %v", n, err)
	}
	if st, err := s.FeedStats(testContext); err != nil || st.Public != 0 {
		t.Fatalf("stats after erasure %+v %v", st, err)
	}
}

func TestFeedProfilesNeedMemory(t *testing.T) {
	s := openTest(t, Config{})
	me := keyFor(71)
	fails(t, s, signed(me, Command{Operation: "room.subscribe", Room: "lobby"}), "service_unavailable")
	fails(t, s, signed(me, feedCmd("feed.get", `{"profile":"self"}`)), "service_unavailable")
	if st, err := s.FeedStats(testContext); st != nil || err != nil {
		t.Fatalf("stats without memory: %v %v", st, err)
	}
	// Step 1 is unchanged without it.
	run(t, s, feedCmd("feed.get", ""))
}

// A stored item that is not a profile (written before feed/ was reserved)
// reads as no profile, and stats skip it.
func TestFeedProfileIgnoresForeignItems(t *testing.T) {
	s := openWakeTest(t, "memory")
	seedFeed(t, s)
	me := keyFor(71)
	if _, err := s.db.Exec("INSERT INTO memory_items(account,key,value,visibility,bytes,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", keyID(me), FeedProfileKey, "not json", "public", 20, 1, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "feed.profile.get", Target: keyID(me)}, "profile_not_found")
	if _, err := s.FeedStats(testContext); err != nil {
		t.Fatal(err)
	}
	res := run(t, s, signed(me, feedCmd("feed.profile.put", `{"profile":{}}`)))
	if res.Data["revision"] != int64(2) {
		t.Fatalf("replaces it: %v", res.Data)
	}
	var raw string
	if err := s.db.QueryRow("SELECT value FROM memory_items WHERE key=?", FeedProfileKey).Scan(&raw); err != nil || !json.Valid([]byte(raw)) {
		t.Fatalf("value %s %v", raw, err)
	}
}
