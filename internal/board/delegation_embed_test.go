package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// embedGrant is a site sign-in grant (the embed's Sign in with SwarmMemo,
// C157): the parent signs, the fresh worker key proves the same bytes, and
// data names the embedding site's origin.
func embedGrant(s *Store, parent, child ed25519.PrivateKey, room string, ttl int64, ops []string, origin string) Command {
	data := map[string]any{"schema": 1, "generation": s.generation, "operations": ops, "disclosure": "public"}
	if origin != "" {
		data["origin"] = origin
	}
	raw, _ := json.Marshal(data)
	c := signed(parent, Command{Operation: "delegation.create", Room: room, Target: base64.RawURLEncoding.EncodeToString(child.Public().(ed25519.PublicKey)), TTL: ttl, Amount: 1 << 20, Data: string(raw), Timestamp: s.now().Unix()})
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, c)))
	return c
}

func embedContext(s *Store, child ed25519.PrivateKey) *DelegationContext {
	return &DelegationContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}
}

// A worker key granted vote likes as its parent account, only in its room;
// room.hide and room.restore act with the parent's role there and are logged.
func TestDelegatedVoteAndRoomModeration(t *testing.T) {
	s := openTest(t, Config{})
	owner, reader, other := keyFor(161), keyFor(162), keyFor(163)
	for _, k := range []ed25519.PrivateKey{owner, reader, other} {
		register(t, s, k)
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "site", Visibility: "public"}))
	run(t, s, signed(other, Command{Operation: "room.create", Room: "elsewhere", Visibility: "public"}))
	byOwner := run(t, s, signed(owner, Command{Operation: "post", Room: "site", Text: "welcome"})).Receipt.ID
	byReader := run(t, s, signed(reader, Command{Operation: "post", Room: "site", Text: "first comment"})).Receipt.ID
	byOther := run(t, s, signed(other, Command{Operation: "post", Room: "site", Text: "second comment"})).Receipt.ID
	outside := run(t, s, signed(other, Command{Operation: "post", Room: "elsewhere", Text: "not in the grant"})).Receipt.ID

	worker := keyFor(164)
	run(t, s, embedGrant(s, reader, worker, "site", DelegationSiteMaxTTL, []string{"post", "vote", "messages.list"}, "https://blog.example"))
	g := embedContext(s, worker)
	like := func(id string) Command {
		return childCommand(s, worker, g, Command{Operation: "vote", MessageID: id, Data: `{"value":1}`})
	}
	got := run(t, s, like(byOwner))
	if got.Data["value"] != 1 || sqlCount(t, s, "SELECT count(*) FROM votes WHERE event_id=? AND account=?", byOwner, keyID(reader)) != 1 {
		t.Fatalf("delegated like not counted for the parent account: %+v", got.Data)
	}
	// The parent's own like is the same vote, not a second one.
	run(t, s, signed(reader, Command{Operation: "vote", MessageID: byOwner, Data: `{"value":1}`}))
	if sqlCount(t, s, "SELECT count(*) FROM votes WHERE event_id=?", byOwner) != 1 {
		t.Fatal("a worker key voted separately from its parent")
	}
	fails(t, s, like(byReader), "self_vote")
	fails(t, s, like(outside), "delegation_scope_mismatch")
	fails(t, s, childCommand(s, worker, g, Command{Operation: "room.hide", MessageID: byOther, Reason: "spam"}), "delegation_forbidden")

	// A reader's grant with moderation ops still has only the reader's role.
	readerMod := keyFor(165)
	run(t, s, embedGrant(s, reader, readerMod, "site", 3600, []string{"room.hide", "room.restore"}, "https://blog.example"))
	fails(t, s, childCommand(s, readerMod, embedContext(s, readerMod), Command{Operation: "room.hide", MessageID: byOther, Reason: "spam"}), "moderator_required")

	// The owner's grant hides and restores in its room, on the public log.
	ownerMod := keyFor(166)
	run(t, s, embedGrant(s, owner, ownerMod, "site", 3600, []string{"vote", "room.hide", "room.restore"}, "https://blog.example"))
	om := embedContext(s, ownerMod)
	run(t, s, childCommand(s, ownerMod, om, Command{Operation: "room.hide", MessageID: byOther, Reason: "Spam"}))
	if sqlCount(t, s, "SELECT hidden FROM events WHERE id=?", byOther) != 1 {
		t.Fatal("delegated hide did not hide")
	}
	run(t, s, childCommand(s, ownerMod, om, Command{Operation: "room.restore", MessageID: byOther, Reason: "Not spam"}))
	if sqlCount(t, s, "SELECT hidden FROM events WHERE id=?", byOther) != 0 {
		t.Fatal("delegated restore did not restore")
	}
	log, _ := json.Marshal(run(t, s, Command{Operation: "room.modlog", Room: "site"}))
	if !strings.Contains(string(log), `"hide"`) || !strings.Contains(string(log), `"restore"`) || !strings.Contains(string(log), keyID(ownerMod)) {
		t.Fatalf("delegated moderation not on the public log: %s", log)
	}
	fails(t, s, childCommand(s, ownerMod, om, Command{Operation: "room.hide", MessageID: outside, Reason: "Spam"}), "delegation_scope_mismatch")

	// Revoked: nothing more, likes included.
	run(t, s, revokeCommand(s, reader, keyID(worker)))
	fails(t, s, like(byOther), "delegation_inactive")
}

// Ordinary grants run up to seven days; a site sign-in grant, whose data
// names the site's exact web origin, up to ninety. The parent's grant list
// shows the origin with the room.
func TestEmbedGrantTTLAndOrigin(t *testing.T) {
	s := openTest(t, Config{})
	parent := keyFor(171)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "site", Visibility: "public"}))
	if DelegationMaxTTL != 7*86400 || DelegationSiteMaxTTL != 90*86400 {
		t.Fatal("ordinary grants last up to seven days, site sign-in grants up to ninety")
	}
	// Without an origin a grant is an ordinary one: seven days at most.
	fails(t, s, embedGrant(s, parent, keyFor(172), "site", DelegationMaxTTL+1, []string{"post"}, ""), "invalid_ttl")
	fails(t, s, embedGrant(s, parent, keyFor(172), "site", DelegationSiteMaxTTL, []string{"post"}, ""), "invalid_ttl")
	fails(t, s, embedGrant(s, parent, keyFor(172), "site", DelegationSiteMaxTTL+1, []string{"post"}, "https://blog.example"), "invalid_ttl")
	run(t, s, embedGrant(s, parent, keyFor(175), "site", DelegationMaxTTL, []string{"post"}, ""))
	for i, origin := range []string{"http://blog.example", "https://blog.example/", "https://Blog.example", "https://blog.example/path", "https://user@blog.example", "javascript:alert(1)", "https://blog.example:", "https://blog.example:0443", "null", "https://blog example", "https://blog.example?x", "https://[::1]"} {
		fails(t, s, embedGrant(s, parent, keyFor(byte(180+i)), "site", 3600, []string{"post"}, origin), "invalid_delegation_data")
	}
	for _, origin := range []string{"https://blog.example", "https://xn--bcher-kva.example:8443", "http://localhost:8080", "http://127.0.0.1:9"} {
		if !ValidWebOrigin(origin) {
			t.Fatalf("refused %s", origin)
		}
	}
	run(t, s, embedGrant(s, parent, keyFor(173), "site", DelegationSiteMaxTTL, []string{"post", "vote"}, "https://blog.example"))
	run(t, s, embedGrant(s, parent, keyFor(174), "site", 3600, []string{"post"}, ""))
	list := run(t, s, signed(parent, Command{Operation: "delegations.list"})).Data["delegations"].([]DelegationStatus)
	origins := map[string]string{}
	for _, g := range list {
		if g.Room != "site" {
			t.Fatalf("grant list misses the room: %+v", g)
		}
		origins[g.GrantID] = g.Origin
	}
	if origins[keyID(keyFor(173))] != "https://blog.example" || origins[keyID(keyFor(174))] != "" || origins[keyID(keyFor(175))] != "" || len(origins) != 3 {
		t.Fatalf("origins %v", origins)
	}
	for _, g := range list {
		if g.GrantID == keyID(keyFor(173)) && g.ExpiresAt-g.CreatedAt != DelegationSiteMaxTTL {
			t.Fatalf("ninety-day grant expires at %d", g.ExpiresAt)
		}
	}
}
