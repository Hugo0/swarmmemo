package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// appRoomsSetup is an app (Hex Arena), one of its house agents, and three
// agents with handles.
type appRoomsSetup struct {
	appTestSetup
	house             ed25519.PrivateKey
	alice, bob, carol ed25519.PrivateKey
}

func newAppRooms(t *testing.T) appRoomsSetup {
	t.Helper()
	a := newAppTest(t, appClientJSON)
	created := run(t, a.s, signed(a.app, Command{Operation: "app.house.create", Handle: "dealer", RequestID: "house"}))
	r := appRoomsSetup{appTestSetup: a, house: hostedKey(t, a.s, created.Data["token"].(string)), alice: keyFor(191), bob: keyFor(192), carol: keyFor(193)}
	for i, k := range []ed25519.PrivateKey{r.alice, r.bob, r.carol} {
		run(t, a.s, signed(k, Command{Operation: "agent.register", Handle: []string{"alice", "bobby", "carol"}[i]}))
	}
	return r
}

func (r appRoomsSetup) room(name string) string { return "app/" + keyID(r.app) + "/" + name }

func pubOf(k ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAppRoomNames(t *testing.T) {
	app := strings.Repeat("a", 64)
	for name, want := range map[string]bool{
		"app/" + app + "/table":            true,
		"app/" + app + "/game-1/seat/7/dm": true,
		"app/" + app:                       false,
		"app/" + app + "/":                 false,
		"app/" + app + "/A":                false,
		"app/" + app + "/a/b/c/d/e":        false,
		"app/" + app[:63] + "/x":           false,
		"app/" + app + "//x":               false,
	} {
		if IsAppRoom(name) != want || ValidRoomName(name) != want {
			t.Errorf("IsAppRoom(%q) = %v, want %v", name, IsAppRoom(name), want)
		}
	}
	if AppRoomApp("app/"+app+"/x") != app || AppRoomApp("lobby") != "" {
		t.Fatal("AppRoomApp")
	}
	for members, want := range map[int64]int64{0: 5, 1: 5, 10: 5, 11: 10, 20: 10, 21: 15, 101: 55} {
		if got := SlowModeWait(5, members); got != want {
			t.Errorf("SlowModeWait(5, %d) = %d, want %d", members, got, want)
		}
	}
}

// Only the app creates and closes its rooms: its key, its house agents or a
// token with app-rooms. They are private, unlisted, and closing archives.
func TestAppRoomCreateAndClose(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	table := r.room("table-1")
	fails(t, s, signed(r.alice, Command{Operation: "room.create", Room: table, RequestID: "x1"}), "owner_required")
	fails(t, s, signed(r.alice, Command{Operation: "room.create", Room: "app/" + keyID(r.alice) + "/mine", RequestID: "x2"}), "owner_required")
	fails(t, s, signed(r.app, Command{Operation: "room.create", Room: table, Visibility: "public", RequestID: "x3"}), "invalid_visibility")
	fails(t, s, signed(r.app, Command{Operation: "room.create", Room: "lobby2", Data: `{"slow_mode_seconds":2}`, RequestID: "x4"}), "invalid_policy")
	res := run(t, s, signed(r.app, Command{Operation: "room.create", Room: table, Members: []string{keyID(r.alice)}, RequestID: "c1"}))
	if res.Data["visibility"] != "private" || res.Data["app"] != keyID(r.app) {
		t.Fatalf("room.create = %v", res.Data)
	}
	got := run(t, s, signed(r.alice, Command{Operation: "room.get", Room: table})).Room
	if got.Visibility != "private" || got.Owner != keyID(r.app) {
		t.Fatalf("room.get = %+v", got)
	}
	fails(t, s, signed(r.app, Command{Operation: "room.create", Room: table, RequestID: "c1b"}), "room_exists")
	// A house agent creates one too (and is its member); the app owns it.
	seat := r.room("seat/1")
	run(t, s, signedNow(s, r.house, Command{Operation: "room.create", Room: seat, Members: []string{keyID(r.bob)}, RequestID: "c2"}))
	if got := run(t, s, signed(r.bob, Command{Operation: "room.get", Room: seat})).Room; got.Owner != keyID(r.app) {
		t.Fatalf("house room owner %s", got.Owner)
	}
	// A house agent acts as the owner: membership and policy.
	run(t, s, signedNow(s, r.house, Command{Operation: "room.member.add", Room: table, Target: keyID(r.carol), RequestID: "m1"}))
	run(t, s, signedNow(s, r.house, Command{Operation: "room.policy.set", Room: table, Data: `{"slow_mode_seconds":3}`, RequestID: "p1"}))
	fails(t, s, signed(r.app, Command{Operation: "room.owner.transfer", Room: table, Target: keyID(r.alice), RequestID: "t1"}), "owner_required")
	// Unlisted: no member's or the app's rooms.list shows them.
	for _, k := range []ed25519.PrivateKey{r.app, r.alice, r.bob} {
		for _, room := range run(t, s, signed(k, Command{Operation: "rooms.list"})).Rooms {
			if IsAppRoom(room.Name) {
				t.Fatalf("rooms.list shows %s", room.Name)
			}
		}
	}
	// A member lists the members; a stranger does not learn the room exists.
	members := run(t, s, signed(r.alice, Command{Operation: "room.members", Room: table})).Data
	if members["count"] != 3 || members["masked"] != nil {
		t.Fatalf("room.members = %v", members)
	}
	fails(t, s, signed(r.bob, Command{Operation: "room.members", Room: table}), "not_found")
	// Closing: a member is not the owner; the house agent closes it.
	run(t, s, signed(r.alice, Command{Operation: "post", Room: table, Text: "gg", RequestID: "a1"}))
	fails(t, s, signed(r.alice, Command{Operation: "room.close", Room: table, RequestID: "k0"}), "owner_required")
	fails(t, s, signed(r.bob, Command{Operation: "room.close", Room: table, RequestID: "k0b"}), "not_found")
	closed := run(t, s, signedNow(s, r.house, Command{Operation: "room.close", Room: table, Reason: "game over", RequestID: "k1"})).Data
	if closed["closed"] != true || closed["archived_at"] == int64(0) {
		t.Fatalf("room.close = %v", closed)
	}
	fails(t, s, signed(r.alice, Command{Operation: "post", Room: table, Text: "again", RequestID: "a2"}), "room_closed")
	fails(t, s, signed(r.app, Command{Operation: "room.policy.set", Room: table, Data: `{"closed":false}`, RequestID: "p2"}), "room_closed")
	if msgs := run(t, s, signed(r.alice, Command{Operation: "messages.list", Room: table})).Messages; len(msgs) != 1 || msgs[0].Text != "gg" {
		t.Fatalf("an archived room keeps its messages: %+v", msgs)
	}
	if p := run(t, s, signed(r.alice, Command{Operation: "room.get", Room: table})).Room.Policy; p.ArchivedAt == 0 || !p.Closed {
		t.Fatalf("archived policy = %+v", p)
	}
	// A shared room's owner archives it the same way; a personal room never.
	run(t, s, signed(r.alice, Command{Operation: "room.create", Room: "alice-room", RequestID: "ar"}))
	run(t, s, signed(r.alice, Command{Operation: "room.close", Room: "alice-room", RequestID: "ak"}))
	fails(t, s, signed(r.alice, Command{Operation: "room.close", Room: PersonalRoom(keyID(r.alice)), RequestID: "pk"}), "personal_room")
}

// An app's token reaches only its own app's rooms, with the scope's rule;
// app-rooms creates and closes them, dm only reads and posts in pairs.
func TestAppRoomTokenScopeIsolation(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	other := keyFor(194)
	run(t, s, signed(other, Command{Operation: "app.register", Data: `{"schema":1,"name":"Other","homepage":"https://other.example"}`, RequestID: "o"}))
	person, tokens := r.signIn(t, r.client.ID, s.OAuthAppResource(keyID(r.app)), "app-rooms")
	_, dmTokens := r.signIn(t, r.client.ID, s.OAuthAppResource(keyID(r.app)), "dm")
	mine := r.room("t1")
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "room.create", Room: mine, Members: []string{keyID(r.alice)}, RequestID: "b1"}); err != nil {
		t.Fatalf("token room.create: %v", err)
	}
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "post", Room: mine, Text: "hi", RequestID: "b2"}); err != nil {
		t.Fatalf("token post: %v", err)
	}
	theirs := "app/" + keyID(other) + "/t"
	run(t, s, signed(other, Command{Operation: "room.create", Room: theirs, Members: []string{person}, RequestID: "o1"}))
	run(t, s, signed(other, Command{Operation: "post", Room: theirs, Text: "other app", RequestID: "o2"}))
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: "hex-public", RequestID: "pub"}))
	for name, c := range map[string]Command{
		"another app's room":      {Operation: "messages.list", Room: theirs},
		"post there":              {Operation: "post", Room: theirs, Text: "x", RequestID: "y1"},
		"create there":            {Operation: "room.create", Room: "app/" + keyID(other) + "/new", RequestID: "y2"},
		"close there":             {Operation: "room.close", Room: theirs, RequestID: "y3"},
		"its members":             {Operation: "room.members", Room: theirs},
		"a public room it owns":   {Operation: "post", Room: "hex-public", Text: "x", RequestID: "y4"},
		"a slug room":             {Operation: "room.create", Room: "hexx", RequestID: "y5"},
		"membership":              {Operation: "room.member.add", Room: mine, Target: keyID(r.bob), RequestID: "y6"},
		"policy":                  {Operation: "room.policy.set", Room: mine, Data: `{"slow_mode_seconds":1}`, RequestID: "y7"},
		"every room at once":      {Operation: "messages.list"},
		"the person's inbox":      {Operation: "updates.get"},
		"another app's room info": {Operation: "room.get", Room: theirs},
	} {
		if _, err := bearer(s, tokens.AccessToken, c); appErrCode(err) != "insufficient_scope" {
			t.Errorf("%s (%s): %s, want insufficient_scope", name, c.Operation, appErrCode(err))
		}
	}
	for name, c := range map[string]Command{
		"create": {Operation: "room.create", Room: r.room("dm-new"), RequestID: "z1"},
		"close":  {Operation: "room.close", Room: mine, RequestID: "z2"},
	} {
		if _, err := bearer(s, dmTokens.AccessToken, c); appErrCode(err) != "insufficient_scope" {
			t.Errorf("dm token %s: %s", name, appErrCode(err))
		}
	}
	// The stream of an app-rooms token: this app's rooms the person is in,
	// never the other app's, nor a reply to the person elsewhere.
	start, err := bearer(s, tokens.AccessToken, Command{Operation: "stream.read"})
	if err != nil {
		t.Fatalf("stream.read: %v", err)
	}
	pm := hostedKeyOf(t, s, person)
	lobby := run(t, s, signed(pm, Command{Operation: "post", Room: "lobby", Text: "the person, in public", RequestID: "l1"}))
	run(t, s, signed(r.alice, Command{Operation: "post", Room: "lobby", ReplyTo: lobby.Receipt.ID, Text: "a reply in public", RequestID: "l2"}))
	run(t, s, signed(r.alice, Command{Operation: "post", Room: "lobby", To: person, Text: "to the person in public", RequestID: "l3"}))
	run(t, s, signed(other, Command{Operation: "post", Room: theirs, Text: "other app again", RequestID: "o3"}))
	run(t, s, signed(r.alice, Command{Operation: "post", Room: mine, Text: "in this app's room", RequestID: "a1"}))
	got, err := bearer(s, tokens.AccessToken, Command{Operation: "stream.read", Cursor: start.NextCursor})
	if err != nil {
		t.Fatalf("stream.read: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "in this app's room" || len(got.Data["positions"].([]string)) != 1 {
		t.Fatalf("app-rooms stream = %s", mustJSON(t, got.Messages))
	}
	// The same person's own key streams everything of theirs: app rooms of
	// both apps and the inbox.
	all := run(t, s, signed(pm, Command{Operation: "stream.read", Cursor: start.NextCursor}))
	texts := map[string]bool{}
	for _, m := range all.Messages {
		texts[m.Text] = true
	}
	for _, want := range []string{"a reply in public", "to the person in public", "other app again", "in this app's room"} {
		if !texts[want] {
			t.Errorf("signed stream lacks %q: %v", want, texts)
		}
	}
	// A dm token streams only two-member rooms: this one has three.
	got, err = bearer(s, dmTokens.AccessToken, Command{Operation: "stream.read", Cursor: start.NextCursor})
	if err != nil || len(got.Messages) != 0 {
		t.Fatalf("dm stream = %v %v", got.Messages, err)
	}
	if _, err := bearer(s, tokens.AccessToken, Command{Operation: "room.close", Room: mine, RequestID: "k"}); err != nil {
		t.Fatalf("token room.close: %v", err)
	}
}

// Masked membership: members read aliases and no keys on every read path
// until the room closes; the app and its moderators always read the full
// record, and so does everyone after close.
func TestMaskedAppRoom(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("mafia")
	run(t, s, signed(r.alice, Command{Operation: "room.create", Room: "plain", Visibility: "private", RequestID: "pl"}))
	fails(t, s, signed(r.alice, Command{Operation: "room.policy.set", Room: "plain", Data: `{"reveal_on_close":true}`, RequestID: "pl2"}), "invalid_policy")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob), keyID(r.carol)}, Data: `{"reveal_on_close":true}`, RequestID: "c"}))
	first := run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "I am the villager", RequestID: "a1"}))
	reply := run(t, s, signed(r.bob, Command{Operation: "post", Room: room, ReplyTo: first.Receipt.ID, Text: "sure you are", RequestID: "b1"}))
	// A member addresses another by alias (maskedRecipient).
	aliceAlias := run(t, s, signed(r.bob, Command{Operation: "message.get", MessageID: first.Receipt.ID})).Messages[0].Author
	run(t, s, signed(r.bob, Command{Operation: "post", Room: room, To: aliceAlias, Text: "psst", RequestID: "b2"}))
	// The rule is fixed after the first message.
	fails(t, s, signed(r.app, Command{Operation: "room.policy.set", Room: room, Data: `{"reveal_on_close":false}`, RequestID: "p"}), "invalid_policy")

	// Nothing a member reads names bob: not his fingerprint, key or handle.
	secrets := []string{keyID(r.bob), pubOf(r.bob), "bobby"}
	noLeak := func(what string, v any) {
		t.Helper()
		raw := mustJSON(t, v)
		for _, secret := range secrets {
			if strings.Contains(raw, secret) {
				t.Errorf("%s leaks %q: %s", what, secret, raw)
			}
		}
	}
	reads := map[string]Command{
		"messages.list": {Operation: "messages.list", Room: room},
		"message.get":   {Operation: "message.get", MessageID: reply.Receipt.ID},
		"thread.get":    {Operation: "thread.get", MessageID: first.Receipt.ID},
		"room.get":      {Operation: "room.get", Room: room},
		"room.members":  {Operation: "room.members", Room: room},
		"updates.get":   {Operation: "updates.get", Target: keyID(r.alice)},
		"journal.get":   {Operation: "journal.get"},
		"home":          {Operation: "home"},
		"all rooms":     {Operation: "messages.list"},
	}
	for name, c := range reads {
		res, err := s.Execute(testContext, signed(r.alice, c), "test-origin")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		noLeak(name, res)
	}
	start := run(t, s, signed(r.carol, Command{Operation: "stream.read"})).NextCursor
	run(t, s, signed(r.bob, Command{Operation: "post", Room: room, Text: "later", RequestID: "b3"}))
	stream := run(t, s, signed(r.carol, Command{Operation: "stream.read", Cursor: start}))
	if len(stream.Messages) != 1 || !stream.Messages[0].Masked {
		t.Fatalf("stream = %s", mustJSON(t, stream.Messages))
	}
	noLeak("stream.read", stream)
	// Aliases are stable per author and never a fingerprint.
	msgs := run(t, s, signed(r.alice, Command{Operation: "messages.list", Room: room})).Messages
	aliases := map[string]string{}
	for _, m := range msgs {
		if !m.Masked || !strings.HasPrefix(m.Author, MaskAliasPrefix) || m.PublicKey != "" || m.Signature != "" || m.SignedPayload != "" || m.Handle != "" || m.AuthorHandle != "" {
			t.Fatalf("masked message = %+v", m)
		}
		aliases[m.Text] = m.Author
		if m.Text == "psst" && !strings.HasPrefix(m.To, MaskAliasPrefix) {
			t.Fatalf("recipient not masked: %q", m.To)
		}
	}
	if aliases["sure you are"] != aliases["psst"] || aliases["sure you are"] == aliases["I am the villager"] {
		t.Fatalf("aliases = %v", aliases)
	}
	// carol sees the same alias for bob as alice does.
	if m := run(t, s, signed(r.carol, Command{Operation: "message.get", MessageID: reply.Receipt.ID})).Messages[0]; m.Author != aliases["sure you are"] {
		t.Fatalf("carol reads %s, alice %s", m.Author, aliases["sure you are"])
	}
	// No author or recipient filter reaches the room.
	for name, c := range map[string]Command{
		"target": {Operation: "messages.list", Target: keyID(r.bob)},
		"handle": {Operation: "messages.list", Target: "bobby"},
		"to":     {Operation: "messages.list", To: keyID(r.alice)},
		"room":   {Operation: "messages.list", Room: room, Target: keyID(r.bob)},
		"others": {Operation: "updates.get", Target: keyID(r.bob)},
	} {
		for _, m := range run(t, s, signed(r.alice, c)).Messages {
			if m.Room == room {
				t.Errorf("%s filter reached the masked room: %+v", name, m)
			}
		}
	}
	// No export carries a private room.
	for _, m := range run(t, s, Command{Operation: "export", Before: testTime + 10*86400}).Messages {
		if m.Room == room {
			t.Fatal("the public export carried a masked room")
		}
	}
	// The app (owner), its house agent (once it joins: it reads as a
	// member) and a moderator read the full record.
	run(t, s, signed(r.app, Command{Operation: "room.moderator.add", Room: room, Target: keyID(r.carol), RequestID: "mod"}))
	run(t, s, signedNow(s, r.house, Command{Operation: "room.member.add", Room: room, Target: keyID(r.house), RequestID: "join"}))
	for _, k := range []ed25519.PrivateKey{r.app, r.house, r.carol} {
		m := run(t, s, signedNow(s, k, Command{Operation: "message.get", MessageID: reply.Receipt.ID})).Messages[0]
		if m.Masked || m.Author != keyID(r.bob) || m.PublicKey != pubOf(r.bob) || m.SignedPayload == "" {
			t.Fatalf("owner or moderator read = %+v", m)
		}
	}
	if members := run(t, s, signed(r.app, Command{Operation: "room.members", Room: room})).Data; members["masked"] != nil {
		t.Fatalf("the app reads aliases: %v", members)
	}
	// After close, everyone reads the full record, and it never reopens.
	run(t, s, signed(r.app, Command{Operation: "room.close", Room: room, RequestID: "close"}))
	if m := run(t, s, signed(r.alice, Command{Operation: "message.get", MessageID: reply.Receipt.ID})).Messages[0]; m.Masked || m.Author != keyID(r.bob) || m.PublicKey != pubOf(r.bob) {
		t.Fatalf("after close = %+v", m)
	}
	if got := run(t, s, signed(r.alice, Command{Operation: "room.get", Room: room})).Room; !strings.Contains(strings.Join(got.Members, " "), keyID(r.bob)) {
		t.Fatalf("members after close = %v", got.Members)
	}
}

// Slow mode scales with the member count; the owner is exempt.
func TestAppRoomSlowMode(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("slow")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice)}, Data: `{"slow_mode_seconds":5}`, RequestID: "c"}))
	run(t, s, signed(r.alice, Command{Operation: "post", Room: room, Text: "one", RequestID: "a1"}))
	slow := func(c Command, retry int) {
		t.Helper()
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "room_slow_mode" || e.RetryAfter != retry {
			t.Fatalf("want room_slow_mode retry %d, got %v", retry, err)
		}
	}
	slow(signed(r.alice, Command{Operation: "post", Room: room, Text: "two", RequestID: "a2"}), 5)
	run(t, s, signed(r.app, Command{Operation: "post", Room: room, Text: "owner", RequestID: "o1"}))
	run(t, s, signed(r.app, Command{Operation: "post", Room: room, Text: "owner again", RequestID: "o2"}))
	s.now = func() time.Time { return time.Unix(testTime+5, 0) }
	run(t, s, signedNow(s, r.alice, Command{Operation: "post", Room: room, Text: "two", RequestID: "a3"}))
	// Eleven members: twice the wait.
	for i := 0; i < 9; i++ {
		k := keyFor(byte(220 + i))
		register(t, s, k)
		run(t, s, signedNow(s, r.app, Command{Operation: "room.member.add", Room: room, Target: keyID(k), RequestID: "m" + string(rune('a'+i))}))
	}
	s.now = func() time.Time { return time.Unix(testTime+10, 0) }
	slow(signedNow(s, r.alice, Command{Operation: "post", Room: room, Text: "three", RequestID: "a4"}), 5)
	fails(t, s, signedNow(s, r.app, Command{Operation: "room.policy.set", Room: room, Data: `{"slow_mode_seconds":3601}`, RequestID: "p"}), "invalid_policy")
}

// stream.read resumes from any position it gave, and from a cursor the
// stream sets in place of its command's (WithStreamCursor).
func TestStreamReadResume(t *testing.T) {
	r := newAppRooms(t)
	s := r.s
	room := r.room("live")
	run(t, s, signed(r.app, Command{Operation: "room.create", Room: room, Members: []string{keyID(r.alice), keyID(r.bob)}, RequestID: "c"}))
	start := run(t, s, signed(r.alice, Command{Operation: "stream.read"}))
	if len(start.Messages) != 0 || start.NextCursor == "" {
		t.Fatalf("start = %+v", start)
	}
	for _, text := range []string{"one", "two", "three"} {
		run(t, s, signed(r.bob, Command{Operation: "post", Room: room, Text: text, RequestID: "p" + text}))
	}
	page := run(t, s, signed(r.alice, Command{Operation: "stream.read", Cursor: start.NextCursor, Limit: 2}))
	positions := page.Data["positions"].([]string)
	if len(page.Messages) != 2 || page.Data["has_more"] != true || len(positions) != 2 || page.Messages[1].Text != "two" {
		t.Fatalf("page = %s %v", mustJSON(t, page.Messages), page.Data)
	}
	// Resume from the first message's position: the second and third.
	cmd := signed(r.alice, Command{Operation: "stream.read", Cursor: start.NextCursor})
	res, err := s.Execute(WithStreamCursor(testContext, positions[0]), cmd, "test-origin")
	if err != nil || len(res.Messages) != 2 || res.Messages[0].Text != "two" || res.Messages[1].Text != "three" {
		t.Fatalf("resume = %v %v", res.Messages, err)
	}
	// Caught up: nothing, the same cursor.
	again, err := s.Execute(WithStreamCursor(testContext, res.NextCursor), cmd, "test-origin")
	if err != nil || len(again.Messages) != 0 || again.NextCursor != res.NextCursor {
		t.Fatalf("caught up = %v %v", again, err)
	}
	fails(t, s, Command{Operation: "stream.read"}, "signature_required")
}
