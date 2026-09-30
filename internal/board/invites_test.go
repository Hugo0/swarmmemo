package board

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

// signedNow signs c at the store's current time, for tests that move the clock.
func signedNow(s *Store, key ed25519.PrivateKey, c Command) Command {
	c.Timestamp = s.now().Unix()
	return signed(key, c)
}

func globalUsed(t *testing.T, s *Store) int64 {
	t.Helper()
	var used int64
	if err := s.db.QueryRow("SELECT coalesce(sum(used),0) FROM quota WHERE actor='global'").Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}

func TestRoomInviteAdmitsOnceAndStoresOnlyAHash(t *testing.T) {
	s := openTest(t, Config{})
	owner, guest, late := keyFor(61), keyFor(62), keyFor(63)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "case-1", Visibility: "private"}))
	before := globalUsed(t, s)
	create := signed(owner, Command{Operation: "room.invite.create", Room: "case-1", RequestID: "inv-1"})
	created := run(t, s, create)
	secret, _ := created.Data["secret"].(string)
	if len(secret) != 43 || created.Data["code"] != "case-1."+secret || created.Data["expires_at"] != testTime+InviteTTLDefault {
		t.Fatalf("create answered %v", created.Data)
	}
	if spent := globalUsed(t, s) - before; spent != 256 {
		t.Fatalf("create spent %d, want 256", spent)
	}
	// Neither the invite row, the audit log nor the stored retry receipt holds
	// the secret; an exact retry answers without it.
	for _, table := range []string{"room_invites", "audit", "requests"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE instr(CAST(quote("+map[string]string{"room_invites": "secret_sha256||room", "audit": "detail||target", "requests": "result"}[table]+") AS TEXT),?)>0", secret).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s stores the invite secret", table)
		}
	}
	retry := run(t, s, create)
	if _, shown := retry.Data["secret"]; shown || retry.Data["invite_id"] != created.Data["invite_id"] {
		t.Fatalf("retry answered %v", retry.Data)
	}

	// A key that has never signed anything joins, and can then post and read.
	before = globalUsed(t, s)
	joined := run(t, s, signed(guest, Command{Operation: "room.invite.accept", Room: "case-1", Data: secret, RequestID: "acc-1"}))
	if joined.Data["member"] != keyID(guest) || joined.Data["invite_id"] != created.Data["invite_id"] {
		t.Fatalf("accept answered %v", joined.Data)
	}
	if spent := globalUsed(t, s) - before; spent != 256 {
		t.Fatalf("accept spent %d, want 256", spent)
	}
	run(t, s, signed(guest, Command{Operation: "post", Room: "case-1", Text: "hello from the guest"}))
	read := run(t, s, signed(owner, Command{Operation: "messages.list", Room: "case-1"}))
	if len(read.Messages) != 1 || read.Messages[0].Author != keyID(guest) {
		t.Fatalf("owner read %+v", read.Messages)
	}
	// Single use: a second key is refused, and the used invite is kept, marked.
	if e := errorOf(t, s, signed(late, Command{Operation: "room.invite.accept", Room: "case-1", Data: secret})); e.Code != "invite_invalid" || e.Status != 403 {
		t.Fatalf("reuse: %v", e)
	}
	var usedBy string
	if err := s.db.QueryRow("SELECT used_by FROM room_invites WHERE room='case-1'").Scan(&usedBy); err != nil || usedBy != keyID(guest) {
		t.Fatalf("used invite: %q %v", usedBy, err)
	}
}

// Wrong, used, expired, other-room and orphaned secrets, a missing room and
// garbage are one answer, byte for byte.
func TestRoomInviteRefusalsAreOneAnswer(t *testing.T) {
	s := openTest(t, Config{})
	owner, guest, other := keyFor(64), keyFor(65), keyFor(66)
	register(t, s, other)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "alpha", Visibility: "private"}))
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "beta", Visibility: "private"}))
	mint := func(room string, ttl int64) string {
		return run(t, s, signedNow(s, owner, Command{Operation: "room.invite.create", Room: room, TTL: ttl})).Data["secret"].(string)
	}
	alpha, short, used := mint("alpha", 0), mint("alpha", 60), mint("alpha", 0)
	run(t, s, signed(other, Command{Operation: "room.invite.accept", Room: "alpha", Data: used}))
	s.now = func() time.Time { return time.Unix(testTime+61, 0) }
	transferred := mint("beta", 0)
	run(t, s, signedNow(s, owner, Command{Operation: "room.owner.transfer", Room: "beta", Target: keyID(other)}))

	want := errorOf(t, s, signedNow(s, guest, Command{Operation: "room.invite.accept", Room: "alpha", Data: strings.Repeat("A", 43)}))
	if want.Code != "invite_invalid" || want.Status != 403 {
		t.Fatalf("wrong secret: %v", want)
	}
	for name, c := range map[string]Command{
		"expired":    {Room: "alpha", Data: short},
		"used":       {Room: "alpha", Data: used},
		"other room": {Room: "beta", Data: alpha},
		"no room":    {Room: "gamma", Data: alpha},
		"orphaned":   {Room: "beta", Data: transferred},
		"garbage":    {Room: "alpha", Data: "not-a-secret"},
		"empty":      {Room: "alpha"},
	} {
		c.Operation = "room.invite.accept"
		if got := errorOf(t, s, signedNow(s, guest, c)); *got != *want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	// None of the refusals used the valid invite, and nothing was deleted.
	run(t, s, signedNow(s, guest, Command{Operation: "room.invite.accept", Room: "alpha", Data: alpha}))
	var kept int
	if err := s.db.QueryRow("SELECT count(*) FROM room_invites").Scan(&kept); err != nil || kept != 4 {
		t.Fatalf("kept %d invites: %v", kept, err)
	}
}

func TestRoomInviteCreateIsTheOwnersOnPrivateRooms(t *testing.T) {
	s := openTest(t, Config{})
	owner, member, stranger := keyFor(67), keyFor(68), keyFor(69)
	register(t, s, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "team", Visibility: "private", Members: []string{keyID(member)}}))
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "open-room", Visibility: "public"}))
	for _, tc := range []struct {
		key  ed25519.PrivateKey
		c    Command
		code string
	}{
		{member, Command{Room: "team"}, "owner_required"},
		{stranger, Command{Room: "team"}, "not_found"},
		{owner, Command{Room: "open-room"}, "private_room_required"},
		{owner, Command{Room: "team", TTL: 59}, "invalid_ttl"},
		{owner, Command{Room: "team", TTL: InviteTTLMax + 1}, "invalid_ttl"},
		{owner, Command{Room: "Not A Slug"}, "invalid_slug"},
	} {
		tc.c.Operation = "room.invite.create"
		if e := errorOf(t, s, signed(tc.key, tc.c)); e.Code != tc.code {
			t.Errorf("%+v: %v, want %s", tc.c, e, tc.code)
		}
	}
	fails(t, s, Command{Operation: "room.invite.create", Room: "team"}, "signature_required")
	// Open invites are capped; one that expires frees its place.
	for i := 0; i < InviteOpenMax; i++ {
		run(t, s, signed(owner, Command{Operation: "room.invite.create", Room: "team", TTL: 60 + int64(i)}))
	}
	fails(t, s, signed(owner, Command{Operation: "room.invite.create", Room: "team"}), "invite_limit")
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	run(t, s, signedNow(s, owner, Command{Operation: "room.invite.create", Room: "team"}))
}

func TestRoomInviteAcceptByAMemberOrIntoAFullRoom(t *testing.T) {
	s := openTest(t, Config{})
	owner, guest := keyFor(70), keyFor(71)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "full", Visibility: "private"}))
	secret := run(t, s, signed(owner, Command{Operation: "room.invite.create", Room: "full"})).Data["secret"].(string)
	// The owner accepting its own invite changes nothing and leaves it unused.
	fails(t, s, signed(owner, Command{Operation: "room.invite.accept", Room: "full", Data: secret}), "already_member")
	// Fill the room to its limit directly; the last filler then makes room.
	filler := ""
	for i := 0; i < RoomMembersMax; i++ {
		filler = randomID()
		if _, err := s.db.Exec("INSERT INTO members(room,account) VALUES('full',?)", filler); err != nil {
			t.Fatal(err)
		}
	}
	fails(t, s, signed(guest, Command{Operation: "room.invite.accept", Room: "full", Data: secret}), "member_limit")
	if _, err := s.db.Exec("DELETE FROM members WHERE room='full' AND account=?", filler); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(guest, Command{Operation: "room.invite.accept", Room: "full", Data: secret}))
}

// Both operations run inside the command's one transaction on the store's
// only connection: neither may reach for the pool meanwhile (the 1.22.0
// stall), so each answers at once and a concurrent health ping gets through.
func TestRoomInviteNeverWaitsOnItsOwnConnection(t *testing.T) {
	s := openTest(t, Config{})
	owner, guest := keyFor(72), keyFor(73)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "quick", Visibility: "private"}))
	within := func(c Command) Result {
		t.Helper()
		health := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			health <- s.Health(ctx)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := s.Execute(ctx, c, "test-origin")
		if err != nil || ctx.Err() != nil {
			t.Fatalf("%s: %v (context %v)", c.Operation, err, ctx.Err())
		}
		if err = <-health; err != nil {
			t.Fatalf("concurrent health during %s: %v", c.Operation, err)
		}
		return res
	}
	secret := within(signed(owner, Command{Operation: "room.invite.create", Room: "quick"})).Data["secret"].(string)
	within(signed(guest, Command{Operation: "room.invite.accept", Room: "quick", Data: secret}))
}
