package board

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func styleSet(room, css string) Command {
	data, _ := json.Marshal(map[string]string{"css": css})
	return Command{Operation: "room.style.set", Room: room, Data: string(data)}
}

// Only a room's owner sets or clears its style; moderators, members, other
// keys, anonymous callers and delegated keys cannot. The owner sees what the
// sanitizer dropped; the room carries the source; the log names it by hash.
func TestRoomStyleOwnershipWarningsAndLog(t *testing.T) {
	s := openTest(t, Config{})
	owner, moderator, other := keyFor(171), keyFor(172), keyFor(173)
	for _, key := range [][]byte{owner, moderator, other} {
		register(t, s, key)
	}
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "garden"}))
	run(t, s, signed(owner, Command{Operation: "room.moderator.add", Room: "garden", Target: keyID(moderator)}))
	css := "@import url(//evil.example/x.css);\n:scope { --trust-plate: #000; --trust-ink: #fff; --trust-muted: #ccc; background: #000 }\n"

	for _, key := range [][]byte{moderator, other} {
		fails(t, s, signed(key, styleSet("garden", css)), "owner_required")
	}
	fails(t, s, styleSet("garden", css), "signature_required")
	fails(t, s, signed(owner, Command{Operation: "room.style.set", Room: "garden", Data: `{"css":"a{}","extra":1}`}), "invalid_style")
	fails(t, s, signed(owner, styleSet("garden", "   ")), "invalid_style")
	fails(t, s, signed(owner, styleSet("garden", strings.Repeat("a", RoomStyleBytes+1))), "invalid_style")
	if n := sqlCount(t, s, "SELECT count(*) FROM room_styles"); n != 0 {
		t.Fatalf("a refused style was stored: %d", n)
	}

	res := run(t, s, signed(owner, styleSet("garden", css)))
	warnings := res.Data["warnings"].([]string)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "@import") || !strings.HasPrefix(res.Data["stylesheet"].(string), "/room-style/garden/") {
		t.Fatalf("set result: %+v", res.Data)
	}
	sum := sha256.Sum256([]byte(css))
	got := roomGet(t, s, "garden").Style
	if got == nil || got.CSS != css || got.SHA256 != hex.EncodeToString(sum[:]) || got.UpdatedAt != testTime {
		t.Fatalf("room.get style: %+v", got)
	}
	entry := modlog(t, s, "garden")[0]
	if entry.Action != "style" || entry.Actor != keyID(owner) || entry.SignedPayload != "" || !strings.Contains(entry.Detail, hex.EncodeToString(sum[:])) || strings.Contains(entry.Detail, "trust-plate") {
		t.Fatalf("style log entry: %+v", entry)
	}

	for _, key := range [][]byte{moderator, other} {
		fails(t, s, signed(key, Command{Operation: "room.style.clear", Room: "garden"}), "owner_required")
	}
	run(t, s, signed(owner, Command{Operation: "room.style.clear", Room: "garden"}))
	if roomGet(t, s, "garden").Style != nil || modlog(t, s, "garden")[0].Action != "style.clear" {
		t.Fatal("clear left the style or did not log it")
	}
	fails(t, s, signed(owner, Command{Operation: "room.style.clear", Room: "garden"}), "no_style")

	// A delegated worker key never changes its parent's rooms.
	child := keyFor(174)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "grant-room", Visibility: "public"}))
	g := enroll(t, s, owner, child, "grant-room", 3600, 1<<20)
	fails(t, s, childCommand(s, child, g, styleSet("grant-room", ":scope{color:#000}")), "delegation_forbidden")
}

// A personal room's style opens the room, like its policy; nobody else can.
func TestPersonalRoomStyleAndServing(t *testing.T) {
	s := openTest(t, Config{})
	owner, other := keyFor(175), keyFor(176)
	register(t, s, owner)
	register(t, s, other)
	fails(t, s, signed(other, styleSet(personal(owner), ":scope{color:#000}")), "room_write_restricted")
	run(t, s, signed(owner, styleSet(personal(owner), ":scope{color:#000}")))
	style, err := s.RoomStyle(testContext, personal(owner))
	if err != nil || style == nil || style.Source != ":scope{color:#000}" || style.Owner != keyID(owner) {
		t.Fatalf("personal room style: %+v %v", style, err)
	}
	if style, _ := s.RoomStyle(testContext, "nowhere"); style != nil {
		t.Fatal("style for a room that has none")
	}
	// A private room's style is never served.
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "vault", Visibility: "private"}))
	run(t, s, signed(owner, styleSet("vault", ":scope{color:#000}")))
	if style, _ := s.RoomStyle(testContext, "vault"); style != nil {
		t.Fatal("a private room's style would be served")
	}
}

// url() may name a live public attachment posted, visibly, in the room or in
// its owner's personal room; nothing else.
func TestRoomStyleAttachments(t *testing.T) {
	s := openTest(t, Config{})
	owner, other := keyFor(177), keyFor(178)
	register(t, s, owner)
	register(t, s, other)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "gallery"}))
	run(t, s, signed(other, Command{Operation: "post", Room: "elsewhere", Text: "open"}))
	post := func(key []byte, room string) Attachment {
		a := upload(t, s, key, room, "bytes", 3600)
		run(t, s, signed(key, Command{Operation: "post", Room: room, Text: "with a file", Attachments: []string{a.ID}}))
		return a
	}
	run(t, s, signed(owner, Command{Operation: "post", Room: personal(owner), Text: "opens the personal room"}))
	inRoom := post(owner, "gallery")
	inPersonal := post(owner, personal(owner))
	elsewhere := post(other, "elsewhere")
	unposted := upload(t, s, owner, "gallery", "never posted", 3600)
	hidden := post(owner, "gallery")
	var hiddenEvent string
	if err := s.db.QueryRow("SELECT event_id FROM event_attachments WHERE blob_id=?", hidden.ID).Scan(&hiddenEvent); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(owner, Command{Operation: "room.hide", MessageID: hiddenEvent, Reason: "test"}))
	expired := post(owner, "gallery")
	if _, err := s.db.Exec("UPDATE blobs SET expires_at=? WHERE id=?", testTime-1, expired.ID); err != nil {
		t.Fatal(err)
	}

	run(t, s, signed(owner, styleSet("gallery", ":scope{color:#000}")))
	style, err := s.RoomStyle(testContext, "gallery")
	if err != nil || style == nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{inRoom.ID: true, inPersonal.ID: true, elsewhere.ID: false, unposted.ID: false, hidden.ID: false, expired.ID: false, "nope": false} {
		if got := style.Attachment(id); got != want {
			t.Errorf("attachment %s: %v, want %v", id, got, want)
		}
	}
	// The same rule applies when the owner saves: a foreign attachment is dropped.
	res := run(t, s, signed(owner, styleSet("gallery", ":scope{background:url(/a/"+elsewhere.ID+")} p{background:url(/a/"+inRoom.ID+")}")))
	if w := res.Data["warnings"].([]string); len(w) != 1 || !strings.Contains(w[0], "attachment") {
		t.Fatalf("warnings: %v", w)
	}
	// room.style.check previews without storing and without a signature.
	check := run(t, s, Command{Operation: "room.style.check", Room: "gallery", Data: `{"css":"p{background:url(/a/` + inRoom.ID + `)}"}`})
	if !strings.Contains(check.Data["css"].(string), "/a/"+inRoom.ID) || check.Data["scope"] == "" {
		t.Fatalf("check: %+v", check.Data)
	}
	if roomGet(t, s, "gallery").Style.CSS == check.Data["css"] {
		t.Fatal("check stored its input")
	}
}

// Operator-owned rooms are styled from the local CLI; key-owned rooms refuse it.
func TestOperatorRoomStyle(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(179)
	register(t, s, owner)
	run(t, s, Command{Operation: "post", Room: "guides", Text: "opens an operator room"})
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "keyed"}))
	data, _ := json.Marshal(map[string]string{"css": ":scope{color:#000} .toast{color:#000}"})
	res, err := s.OperatorRoom(testContext, Command{Operation: "room.style.set", Room: "guides", Data: string(data)})
	if err != nil || len(res.Data["warnings"].([]string)) != 1 || modlog(t, s, "guides")[0].Actor != operatorActor {
		t.Fatalf("operator style: %+v %v", res, err)
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.style.set", Room: "keyed", Data: string(data)}); err == nil {
		t.Fatal("operator styled a key-owned room")
	}
	if _, err := s.OperatorRoom(testContext, Command{Operation: "room.style.clear", Room: "guides"}); err != nil {
		t.Fatal(err)
	}
}

// Schema 13 adds room_styles. A schema 12 database with rooms, policies and a
// moderation log opens, gains the table, and keeps its rows.
func TestSchema13RoomStyleMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema12.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	owner := keyFor(180)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "kept"}))
	run(t, s, signed(owner, policySet(owner, "kept", `{"write":"owner","rules":"Be kind."}`)))
	if _, err = s.db.Exec("DROP TABLE room_styles; PRAGMA user_version=12"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	if got := sqlCount(t, s, "PRAGMA user_version"); got != SchemaVersion || SchemaVersion != 13 {
		t.Fatalf("user_version %d", got)
	}
	if r := roomGet(t, s, "kept"); r.Policy.Write != "owner" || r.Policy.Rules != "Be kind." || r.Style != nil || len(modlog(t, s, "kept")) != 1 {
		t.Fatalf("schema 12 room after migration: %+v %+v", r, r.Policy)
	}
	run(t, s, signed(owner, styleSet("kept", ":scope{color:#000}")))
	if roomGet(t, s, "kept").Style == nil {
		t.Fatal("style not stored after migration")
	}
}

// room.style.check is unsigned, and sanitizing a hostile 32 KiB sheet takes
// tens of milliseconds: inside the transaction it would hold the store's only
// database connection, so an anonymous caller at its request allowance could
// stall every read and write. The sanitize runs after the transaction ends.
func TestRoomStyleCheckSanitizesOutsideTheTransaction(t *testing.T) {
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "x"})
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.checkRoomStyle(t.Context(), tx, Command{Operation: "room.style.check", Room: "lobby", Data: `{"css":"p{color:red}"}`}, actor{}, time.Now().Unix())
	tx.Rollback()
	if err != nil || res.afterCommit == nil || res.Data != nil {
		t.Fatalf("the sanitizer ran inside the transaction: %v %+v", err, res.Data)
	}
	check := run(t, s, Command{Operation: "room.style.check", Room: "lobby", Data: `{"css":"p{color:red}"}`})
	if css, _ := check.Data["css"].(string); !strings.Contains(css, "color:red") || !check.OK {
		t.Fatalf("check after the transaction: %+v", check)
	}
}

// Sanitizing caller-supplied CSS is bounded service-wide: with every slot
// taken, a check or a set is turned away before any work, and a set the
// sanitizer refuses is refused before its transaction (where a refusal is
// rolled back uncharged) begins.
func TestRoomStyleSanitizingIsBounded(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(174)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "bounded"}))
	for range cap(s.styleSlots) {
		s.styleSlots <- struct{}{}
	}
	fails(t, s, Command{Operation: "room.style.check", Room: "bounded", Data: `{"css":"p{color:red}"}`}, "busy")
	fails(t, s, signed(owner, styleSet("bounded", "p{color:red}")), "busy")
	for range cap(s.styleSlots) {
		<-s.styleSlots
	}
	fails(t, s, signed(owner, styleSet("bounded", strings.Repeat(".post{color:red}", 1100))), "invalid_style")
	run(t, s, signed(owner, styleSet("bounded", "p{color:red}")))
	if len(s.styleSlots) != 0 {
		t.Fatalf("%d style slots leaked", len(s.styleSlots))
	}
}

// A file kept without a ttl (expires_at 0, the default since files stopped
// expiring) is live for room CSS.
func TestStyleAttachmentWithoutTTL(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(143)
	register(t, s, owner)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "styled"}))
	data := base64.RawURLEncoding.EncodeToString([]byte("bytes"))
	blob := run(t, s, signed(owner, Command{Operation: "blob.put", Room: "styled", Data: data})).Data["blob"].(Attachment)
	run(t, s, signed(owner, Command{Operation: "post", Room: "styled", Text: "file", Attachments: []string{blob.ID}}))
	if ok, err := styleAttachment(testContext, s.db, "styled", keyID(owner), blob.ID, testTime); err != nil || !ok {
		t.Fatalf("ttl-less attachment refused: %v %v", ok, err)
	}
}
