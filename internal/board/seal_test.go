package board

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Sealed conversations (RFC0013 §6, seal.go), through the real
// conversation operations.

const sealTestRoom = "~k3vectorvectorvectorvector"

type sealMemberKey struct {
	sign   ed25519.PrivateKey
	x25519 *ecdh.PrivateKey
}

func (m sealMemberKey) id() string  { return keyID(m.sign) }
func (m sealMemberKey) kid() string { return SealKid(m.x25519.PublicKey().Bytes()) }
func (m sealMemberKey) x() string {
	return base64.RawURLEncoding.EncodeToString(m.x25519.PublicKey().Bytes())
}

// sealMember registers a key and publishes its sealing key.
func sealMember(t *testing.T, s *Store, n byte) sealMemberKey {
	t.Helper()
	m := sealMemberKey{sign: keyFor(n)}
	var err error
	if m.x25519, err = ecdh.X25519().GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(m.sign, Command{Operation: "agent.register"}))
	run(t, s, signed(m.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", m.x())}))
	return m
}

// openSealTestConversation opens room as members[0] (sealed or not) with the
// others, each of whom accepts the request, so all are active.
func openSealTestConversation(t *testing.T, s *Store, room string, sealed bool, members ...sealMemberKey) {
	t.Helper()
	ids := []string{}
	for _, m := range members[1:] {
		ids = append(ids, m.id())
	}
	data := `{"schema":1,"kind":"group","sealed":false}`
	if sealed {
		data = `{"schema":1,"kind":"group","sealed":true}`
	}
	run(t, s, signed(members[0].sign, Command{Operation: "conversation.open", Room: room, Members: ids, Data: data}))
	for _, m := range members[1:] {
		run(t, s, signed(m.sign, Command{Operation: "conversation.respond", Room: room, Data: `{"schema":1,"action":"accept"}`}))
	}
}

// memberEpoch is room's current member_epoch.
func memberEpoch(t *testing.T, s *Store, room string) int64 {
	t.Helper()
	return sqlCount(t, s, "SELECT member_epoch FROM conversations WHERE room=?", room)
}

func randomB64(t *testing.T, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// rotation is conversation.seal data wrapping for members (shapes only:
// the server never opens a wrap).
func rotation(t *testing.T, memberEpoch, epoch int64, members ...sealMemberKey) string {
	t.Helper()
	wraps := []sealWrap{}
	for _, m := range members {
		wraps = append(wraps, sealWrap{Agent: m.id(), Kid: m.kid(), Enc: randomB64(t, 32), Ct: randomB64(t, 48)})
	}
	raw, err := json.Marshal(sealRotation{Schema: 1, MemberEpoch: memberEpoch, Epoch: epoch, Wraps: wraps})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func sealedPost(t *testing.T, key ed25519.PrivateKey, room string, epoch string) Command {
	return signed(key, Command{Operation: "post", Room: room, Text: "sealed1." + epoch + "." + randomB64(t, 12) + "." + randomB64(t, 40), Data: `{"schema":1,"format":"sealed"}`})
}

func sqlString(t *testing.T, s *Store, query string, args ...any) string {
	t.Helper()
	var value string
	if err := s.db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func errorOf(t *testing.T, s *Store, c Command) *Error {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("%s: want a refusal, got %v", c.Operation, err)
	}
	return e
}

func TestSealKeyLink(t *testing.T) {
	s := openTest(t, Config{})
	m := sealMember(t, s, 71)
	agent := run(t, s, Command{Operation: "agent.get", Target: m.id()}).Agent
	k := agent.SealKey
	if k == nil || k.X25519 != m.x() || k.Kid != m.kid() || k.PublicKey != agent.PublicKey {
		t.Fatalf("seal_key = %+v", k)
	}
	// Anyone can check it against the agent's own key, without this service.
	public, _ := base64.RawURLEncoding.DecodeString(k.PublicKey)
	sig, _ := base64.RawURLEncoding.DecodeString(k.Signature)
	if !ed25519.Verify(public, []byte(k.SignedPayload), sig) || !strings.Contains(k.SignedPayload, `"operation":"identity.link"`) || !strings.Contains(k.SignedPayload, m.x()) {
		t.Fatalf("seal_key does not verify: %+v", k)
	}
	// A new sealing key replaces the old one: one per key, and the link
	// limit is not spent by rotating it.
	for i := 0; i < IdentityLinkMaxPerKey+1; i++ {
		next, _ := ecdh.X25519().GenerateKey(rand.Reader)
		m.x25519 = next
		run(t, s, signed(m.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", m.x())}))
	}
	agent = run(t, s, Command{Operation: "agent.get", Target: m.id()}).Agent
	x25519Links := 0
	for _, l := range agent.Links {
		if l.Kind == "x25519" {
			x25519Links++
			if l.State != "proof_attached" || l.Method != "signed-command" || l.Proof == "" || l.Statement == "" {
				t.Fatalf("x25519 link reads %+v", l)
			}
		}
	}
	if x25519Links != 1 || agent.SealKey.Kid != m.kid() {
		t.Fatalf("links %+v, seal key %+v", agent.Links, agent.SealKey)
	}
	// Nothing is deleted: every replaced key stays on record, lapsed when it
	// was replaced, so the key behind an old epoch's wraps is still known.
	if kept, retired := sqlCount(t, s, "SELECT count(*) FROM identity_links WHERE agent=? AND kind='x25519'", m.id()),
		sqlCount(t, s, "SELECT count(*) FROM identity_links WHERE agent=? AND kind='x25519' AND state='lapsed' AND lapsed_at>0", m.id()); kept != IdentityLinkMaxPerKey+2 || retired != kept-1 {
		t.Fatalf("x25519 rows: %d kept, %d retired", kept, retired)
	}
	// Publishing a retired key again makes it current again.
	first := sqlString(t, s, "SELECT value FROM identity_links WHERE agent=? AND kind='x25519' ORDER BY created_at,rowid LIMIT 1", m.id())
	run(t, s, signed(m.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", first)}))
	if again := run(t, s, Command{Operation: "agent.get", Target: m.id()}).Agent.SealKey; again == nil || again.X25519 != first {
		t.Fatalf("re-published key: %+v", again)
	}
	// Small-order points, non-canonical encodings and separate proofs are refused.
	for _, bad := range []string{
		base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		base64.RawURLEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...)),
		base64.RawURLEncoding.EncodeToString(append(append([]byte{0xec}, []byte(strings.Repeat("\xff", 30))...), 0x7f)),
		base64.RawURLEncoding.EncodeToString(append(make([]byte, 31), 0x80)),
		m.x() + "A", "not-a-key",
	} {
		fails(t, s, signed(m.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", bad)}), "invalid_link_value")
	}
	fails(t, s, linkCommand(m.sign, "identity.link", "x25519", m.x(), "c2ln"), "invalid_link_proof")
	// A key SwarmMemo holds cannot publish a sealing key.
	if _, err := s.db.Exec("UPDATE identities SET custody='hosted' WHERE id=?", m.id()); err != nil {
		t.Fatal(err)
	}
	fails(t, s, signed(m.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", m.x())}), "self_custody_required")
}

// A key that never wrote in public stays out of the directory, but the
// members of a conversation it is in read its sealing key: they wrap every
// epoch to it, pending members included, and nobody else sees it.
func TestSealKeyOfPrivateOnlyMember(t *testing.T) {
	s := openTest(t, updatesConfig())
	var alice, bob, carol sealMemberKey
	for i, m := range []*sealMemberKey{&alice, &bob, &carol} {
		m.sign = keyFor(byte(81 + i))
		var err error
		if m.x25519, err = ecdh.X25519().GenerateKey(rand.Reader); err != nil {
			t.Fatal(err)
		}
		run(t, s, signed(m.sign, Command{Operation: "identity.link", Data: linkJSON("x25519", m.x())}))
	}
	fails(t, s, signed(alice.sign, Command{Operation: "agent.get", Target: bob.id()}), "not_found")
	room := convRoom("private-only")
	run(t, s, signed(alice.sign, Command{Operation: "conversation.open", Room: room, Members: []string{bob.id()}, Data: `{"schema":1,"kind":"group","sealed":true}`}))
	for _, pair := range [][2]sealMemberKey{{alice, bob}, {bob, alice}} {
		if k := run(t, s, signed(pair[0].sign, Command{Operation: "agent.get", Target: pair[1].id()})).Agent.SealKey; k == nil || k.Kid != pair[1].kid() {
			t.Fatalf("a member reads another's sealing key: %+v", k)
		}
	}
	fails(t, s, signed(carol.sign, Command{Operation: "agent.get", Target: bob.id()}), "not_found")
	fails(t, s, Command{Operation: "agent.get", Target: bob.id()}, "not_found")
	if n := len(run(t, s, Command{Operation: "agents.list", Kind: "new"}).Agents); n != 0 {
		t.Fatalf("private-only keys listed: %d", n)
	}
}

func TestSealRotationChecks(t *testing.T) {
	s := openTest(t, Config{})
	ada, bo, cy, stranger := sealMember(t, s, 72), sealMember(t, s, 73), sealMember(t, s, 74), sealMember(t, s, 75)
	openSealTestConversation(t, s, sealTestRoom, true, ada, bo, cy)
	seal := func(by sealMemberKey, data string) Command {
		return signed(by.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: data})
	}
	m := memberEpoch(t, s, sealTestRoom)
	// The mismatch list: a missing member, an extra one, a stale kid, a stale
	// member_epoch all answer the current members and their kids.
	stale := cy
	stale.x25519, _ = ecdh.X25519().GenerateKey(rand.Reader)
	for name, data := range map[string]string{
		"missing":      rotation(t, m, 1, ada, bo),
		"extra":        rotation(t, m, 1, ada, bo, cy, stranger),
		"stale kid":    rotation(t, m, 1, ada, bo, stale),
		"member epoch": rotation(t, m+1, 1, ada, bo, cy),
	} {
		e := errorOf(t, s, seal(ada, data))
		details, _ := e.Details.(map[string]any)
		listed, _ := details["members"].([]SealMember)
		if e.Code != "seal_members_mismatch" || e.Status != 409 || details["member_epoch"] != m || len(listed) != 3 {
			t.Fatalf("%s: %+v", name, e)
		}
		for _, m := range listed {
			if want := map[string]string{ada.id(): ada.kid(), bo.id(): bo.kid(), cy.id(): cy.kid()}[m.Agent]; m.Kid != want {
				t.Fatalf("%s: listed %+v", name, listed)
			}
		}
	}
	// Epochs must advance by exactly one.
	if e := errorOf(t, s, seal(ada, rotation(t, m, 2, ada, bo, cy))); e.Code != "seal_epoch_exists" {
		t.Fatalf("skipping an epoch: %+v", e)
	}
	// Shapes: unknown fields, wrong sizes, duplicates, schema.
	good := sealWrap{Agent: ada.id(), Kid: ada.kid(), Enc: randomB64(t, 32), Ct: randomB64(t, 48)}
	for _, bad := range []sealRotation{
		{Schema: 2, MemberEpoch: 1, Epoch: 1, Wraps: []sealWrap{good}},
		{Schema: 1, MemberEpoch: 1, Epoch: 1},
		{Schema: 1, MemberEpoch: 1, Epoch: 1, Wraps: []sealWrap{good, good}},
		{Schema: 1, MemberEpoch: 1, Epoch: 1, Wraps: []sealWrap{{Agent: ada.id(), Kid: ada.kid(), Enc: randomB64(t, 31), Ct: good.Ct}}},
		{Schema: 1, MemberEpoch: 1, Epoch: 1, Wraps: []sealWrap{{Agent: ada.id(), Kid: ada.kid(), Enc: good.Enc, Ct: randomB64(t, 64)}}},
		{Schema: 1, MemberEpoch: 1, Epoch: 1, Wraps: []sealWrap{{Agent: "nobody", Kid: ada.kid(), Enc: good.Enc, Ct: good.Ct}}},
	} {
		raw, _ := json.Marshal(bad)
		fails(t, s, seal(ada, string(raw)), "invalid_seal")
	}
	fails(t, s, seal(ada, `{"schema":1,"member_epoch":1,"epoch":1,"wraps":[],"extra":1}`), "invalid_seal")
	// A duplicate key reads one way here and another in some other parser.
	fails(t, s, seal(ada, strings.Replace(rotation(t, m, 1, ada, bo, cy), `"epoch":1`, `"epoch":1,"epoch":2`, 1)), "invalid_seal")
	// A non-member, a missing room and a public room all read as not found.
	fails(t, s, seal(stranger, rotation(t, m, 1, ada, bo, cy)), "not_found")
	fails(t, s, signed(ada.sign, Command{Operation: "conversation.seal", Room: "~" + strings.Repeat("q", 26), Data: rotation(t, 1, 1, ada)}), "not_found")
	// A hosted key cannot rotate (nor be a member).
	if _, err := s.db.Exec("UPDATE identities SET custody='hosted' WHERE id=?", bo.id()); err != nil {
		t.Fatal(err)
	}
	fails(t, s, seal(bo, rotation(t, m, 1, ada, bo, cy)), "self_custody_required")
	if _, err := s.db.Exec("UPDATE identities SET custody='self' WHERE id=?", bo.id()); err != nil {
		t.Fatal(err)
	}
	// Not sealed: sealing is fixed at open.
	open := "~" + strings.Repeat("o", 26)
	openSealTestConversation(t, s, open, false, ada, bo)
	fails(t, s, signed(ada.sign, Command{Operation: "conversation.seal", Room: open, Data: rotation(t, memberEpoch(t, s, open), 1, ada, bo)}), "not_sealed")

	// The rotation that matches is stored with its signed command and one
	// wrap row per member; the epoch cannot be taken twice.
	command := seal(bo, rotation(t, m, 1, ada, bo, cy))
	result := run(t, s, command)
	if result.Data["epoch"] != int64(1) || result.Data["wraps"] != 3 {
		t.Fatalf("rotation answered %v", result.Data)
	}
	var payload, signature string
	var wraps int
	if err := s.db.QueryRow("SELECT payload,signature,(SELECT count(*) FROM seal_wraps WHERE room=?) FROM seal_epochs WHERE room=? AND epoch=1", sealTestRoom, sealTestRoom).Scan(&payload, &signature, &wraps); err != nil {
		t.Fatal(err)
	}
	if payload != string(Canonical("swarmmemo.com", command)) || signature != command.Signature || wraps != 3 {
		t.Fatalf("stored %q %q %d", payload, signature, wraps)
	}
	fails(t, s, seal(ada, rotation(t, m, 1, ada, bo, cy)), "seal_epoch_exists")
}

// Members who have not acted yet are wrapped whatever their inbound policy
// made of them (delivered, requested or dropped look the same, §3.4), so a
// rotation tells the sender nothing; a requested recipient can then read the
// sealed request it is shown.
func TestSealWrapSetIsWhatMembersSee(t *testing.T) {
	s := openTest(t, Config{})
	owner, asked, closed := sealMember(t, s, 86), sealMember(t, s, 87), sealMember(t, s, 88)
	run(t, s, signed(closed.sign, Command{Operation: "messaging.policy.set", Data: `{"schema":1,"inbound_policy":{"schema":1,"preset":"closed"}}`}))
	run(t, s, signed(owner.sign, Command{Operation: "conversation.open", Room: sealTestRoom, Members: []string{asked.id(), closed.id()}, Data: `{"schema":1,"kind":"group","sealed":true}`}))
	m := memberEpoch(t, s, sealTestRoom)
	e := errorOf(t, s, signed(owner.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, m, 1, owner)}))
	listed, _ := e.Details.(map[string]any)["members"].([]SealMember)
	if e.Code != "seal_members_mismatch" || len(listed) != 3 {
		t.Fatalf("the requested and the dropped member must both be listed: %+v", e)
	}
	run(t, s, signed(owner.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, m, 1, owner, asked, closed)}))
	run(t, s, sealedPost(t, owner.sign, sealTestRoom, "1"))
	got := getConv(t, s, asked.sign, sealTestRoom, "")
	state, _ := got.Data["seal"].(*SealState)
	if state == nil || len(state.Keys) != 1 || state.Keys[0].Kid != asked.kid() || len(got.Messages) != 1 || !got.Messages[0].Sealed {
		t.Fatalf("the requested member reads %+v %+v", state, got.Messages)
	}
}

// Two members rotating to the same next epoch at once: exactly one wins,
// and the loser is told seal_epoch_exists.
func TestSealRotationRace(t *testing.T) {
	s := openTest(t, Config{})
	members := []sealMemberKey{sealMember(t, s, 76), sealMember(t, s, 77), sealMember(t, s, 78)}
	openSealTestConversation(t, s, sealTestRoom, true, members...)
	var wg sync.WaitGroup
	errs := make([]error, len(members))
	for i, m := range members {
		command := signed(m.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, memberEpoch(t, s, sealTestRoom), 1, members...)})
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Execute(testContext, command, "test-origin")
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		var e *Error
		switch {
		case err == nil:
			won++
		case errors.As(err, &e) && e.Code == "seal_epoch_exists":
		default:
			t.Fatalf("racing rotation: %v", err)
		}
	}
	var epoch, rows, wraps int
	if err := s.db.QueryRow("SELECT seal_epoch,(SELECT count(*) FROM seal_epochs WHERE room=?),(SELECT count(*) FROM seal_wraps WHERE room=?) FROM conversations WHERE room=?", sealTestRoom, sealTestRoom, sealTestRoom).Scan(&epoch, &rows, &wraps); err != nil {
		t.Fatal(err)
	}
	if won != 1 || epoch != 1 || rows != 1 || wraps != 3 {
		t.Fatalf("won %d, epoch %d, epochs %d, wraps %d", won, epoch, rows, wraps)
	}
}

func TestSealedPostsAndMembershipChange(t *testing.T) {
	s := openTest(t, Config{})
	ada, bo, cy := sealMember(t, s, 79), sealMember(t, s, 80), sealMember(t, s, 81)
	openSealTestConversation(t, s, sealTestRoom, true, ada, bo, cy)
	// No epoch yet: nothing can be posted.
	fails(t, s, sealedPost(t, ada.sign, sealTestRoom, "0"), "seal_rotation_required")
	run(t, s, signed(ada.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, memberEpoch(t, s, sealTestRoom), 1, ada, bo, cy)}))
	run(t, s, sealedPost(t, cy.sign, sealTestRoom, "1"))
	fails(t, s, sealedPost(t, ada.sign, sealTestRoom, "2"), "seal_rotation_required")
	for _, text := range []string{"sealed1.1.short.AAAAAAAAAAAAAAAAAAAAAAAA", "sealed1.1." + randomB64(t, 12) + ".AAAA", "sealed1.01." + randomB64(t, 12) + "." + randomB64(t, 32), "hello"} {
		fails(t, s, signed(ada.sign, Command{Operation: "post", Room: sealTestRoom, Text: text, Data: `{"schema":1,"format":"sealed"}`}), "invalid_envelope")
	}

	// cy is removed: its epoch is stale, so no one can post under it, a
	// rotation still wrapping for cy is refused, and the next epoch has no
	// wrap for cy.
	run(t, s, signed(ada.sign, Command{Operation: "room.member.remove", Room: sealTestRoom, Target: cy.id()}))
	m := memberEpoch(t, s, sealTestRoom)
	fails(t, s, sealedPost(t, ada.sign, sealTestRoom, "1"), "seal_rotation_required")
	if e := errorOf(t, s, signed(ada.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, m, 2, ada, bo, cy)})); e.Code != "seal_members_mismatch" {
		t.Fatalf("rotation keeping a removed member: %+v", e)
	}
	run(t, s, signed(bo.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, m, 2, ada, bo)}))
	run(t, s, sealedPost(t, ada.sign, sealTestRoom, "2"))
	fails(t, s, sealedPost(t, cy.sign, sealTestRoom, "2"), "not_found")

	// The keys a read returns: each member only its own wraps, for the
	// epochs on the page and the current one; the removed member none after
	// its removal.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	conv, found, err := loadConversation(testContext, tx, sealTestRoom)
	if err != nil || !found || conv.SealEpoch != 2 || conv.MemberEpoch != m {
		t.Fatalf("conversation %+v %v %v", conv, found, err)
	}
	// A page as scanEvent reads it: a sealed post is marked Sealed.
	page := []Message{{Text: "sealed1.1." + randomB64(t, 12) + "." + randomB64(t, 32), Format: PostFormatSealed, Sealed: true}, {Text: "sealed1.2." + randomB64(t, 12) + "." + randomB64(t, 32), Format: PostFormatSealed, Sealed: true}, {Text: "plain"}}
	for name, want := range map[string][]int64{ada.id(): {2, 1}, cy.id(): {1}} {
		state, err := s.sealStateForPage(testContext, tx, conv, name, page)
		if err != nil {
			t.Fatal(err)
		}
		got := []int64{}
		for _, k := range state.Keys {
			got = append(got, k.Epoch)
			var kid string
			if err := tx.QueryRow("SELECT kid FROM seal_wraps WHERE room=? AND epoch=? AND account=?", sealTestRoom, k.Epoch, name).Scan(&kid); err != nil || kid != k.Kid || k.SignedPayload == "" || k.Signature == "" {
				t.Fatalf("key %+v for %s: %v", k, name, err)
			}
		}
		if state.Epoch != 2 || state.MemberEpoch != m || len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
			t.Fatalf("%s keys %v, want %v (%+v)", name, got, want, state)
		}
	}
}

// Downgrades are refused both ways: cleartext into a sealed conversation,
// and a sealed envelope anywhere else.
func TestSealedDowngradeRefused(t *testing.T) {
	s := openTest(t, Config{})
	ada, bo := sealMember(t, s, 82), sealMember(t, s, 83)
	openSealTestConversation(t, s, sealTestRoom, true, ada, bo)
	plain := "~" + strings.Repeat("p", 26)
	openSealTestConversation(t, s, plain, false, ada, bo)
	run(t, s, signed(ada.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, memberEpoch(t, s, sealTestRoom), 1, ada, bo)}))
	fails(t, s, sealedPost(t, ada.sign, plain, "1"), "not_sealed")
	run(t, s, signed(ada.sign, Command{Operation: "room.create", Room: "open-room"}))
	fails(t, s, sealedPost(t, ada.sign, "open-room", "1"), "not_sealed")

	// The check itself, as post calls it for every conversation post.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	a := actor{id: ada.id(), account: ada.id(), signed: true}
	sealedRow, _, err := loadConversation(testContext, tx, sealTestRoom)
	if err != nil {
		t.Fatal(err)
	}
	plainRow, _, err := loadConversation(testContext, tx, plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Command{
		{Operation: "post", Room: sealTestRoom, Text: "cleartext"},
		{Operation: "post", Room: sealTestRoom, Text: "cleartext", Data: `{"schema":1,"format":"markdown"}`},
	} {
		var e *Error
		if err := s.checkSealedPost(testContext, tx, sealedRow, c, a); !errors.As(err, &e) || e.Code != "sealed_required" {
			t.Fatalf("cleartext into a sealed room: %v", err)
		}
	}
	if err := s.checkSealedPost(testContext, tx, plainRow, Command{Operation: "post", Room: plain, Text: "cleartext"}, a); err != nil {
		t.Fatalf("cleartext into a plain conversation: %v", err)
	}
}

// End to end through post: cleartext into a sealed room is refused on every
// path, even once an epoch exists, and a sealed envelope posts.
func TestSealedRoomRefusesCleartextPost(t *testing.T) {
	s := openTest(t, Config{})
	ada, bo := sealMember(t, s, 84), sealMember(t, s, 85)
	openSealTestConversation(t, s, sealTestRoom, true, ada, bo)
	fails(t, s, signed(ada.sign, Command{Operation: "post", Room: sealTestRoom, Text: "cleartext"}), "sealed_required")
	run(t, s, signed(ada.sign, Command{Operation: "conversation.seal", Room: sealTestRoom, Data: rotation(t, memberEpoch(t, s, sealTestRoom), 1, ada, bo)}))
	for _, c := range []Command{
		{Operation: "post", Room: sealTestRoom, Text: "cleartext"},
		{Operation: "post", Room: sealTestRoom, Text: "cleartext", Visibility: "private"},
		{Operation: "post", Room: sealTestRoom, Text: "**cleartext**", Data: `{"schema":1,"format":"markdown"}`},
	} {
		fails(t, s, signed(bo.sign, c), "sealed_required")
	}
	receipt := run(t, s, sealedPost(t, bo.sign, sealTestRoom, "1"))
	read := getConv(t, s, ada.sign, sealTestRoom, "")
	if len(read.Messages) != 1 || read.Messages[0].ID != receipt.Receipt.ID || !read.Messages[0].Sealed || read.Messages[0].Format != PostFormatSealed {
		t.Fatalf("read %+v", read.Messages)
	}
	// Every read marks it, the inbox and room reads too (§4).
	for _, c := range []Command{{Operation: "messages.list", Room: sealTestRoom}, {Operation: "updates.get", Target: ada.id()}} {
		if got := run(t, s, signed(ada.sign, c)).Messages; len(got) != 1 || !got[0].Sealed {
			t.Fatalf("%s: %+v", c.Operation, got)
		}
	}
}

// The Go half of clients/python/seal-vector.json: the server checks shapes
// only, so it checks the vector's shapes, its kid and its key.
func TestSealVectorShapes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "clients", "python", "seal-vector.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Wrap struct {
			Room, Kid, Enc, Ct string
			RecipientPublic    string `json:"recipient_public"`
		}
		Envelope struct {
			Room, Envelope string
			Epoch          int
		}
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	public, err := base64.RawURLEncoding.DecodeString(v.Wrap.RecipientPublic)
	if err != nil || SealKid(public) != v.Wrap.Kid {
		t.Fatalf("kid of %q", v.Wrap.RecipientPublic)
	}
	if _, ok := normalizeX25519Key(v.Wrap.RecipientPublic); !ok {
		t.Fatal("the vector's sealing key is refused")
	}
	if !IsConversationRoom(v.Wrap.Room) || !decodeSealBytes(v.Wrap.Enc, 32) || !decodeSealBytes(v.Wrap.Ct, 48) {
		t.Fatalf("wrap shapes %+v", v.Wrap)
	}
	data := `{"schema":1,"member_epoch":1,"epoch":3,"wraps":[{"agent":"` + strings.Repeat("a", 64) + `","kid":"` + v.Wrap.Kid + `","enc":"` + v.Wrap.Enc + `","ct":"` + v.Wrap.Ct + `"}]}`
	if _, err := parseSealRotation(data); err != nil {
		t.Fatalf("the vector's wrap is refused: %v", err)
	}
	match := sealEnvelopeRE.FindStringSubmatch(v.Envelope.Envelope)
	if match == nil || match[1] != "3" || !decodeSealBytes(match[2], 12) {
		t.Fatalf("envelope %q", v.Envelope.Envelope)
	}
	// A rotation for the largest conversation fits its command envelope.
	wraps := make([]string, RoomMembersMax+1)
	for i := range wraps {
		wraps[i] = `{"agent":"` + strings.Repeat("a", 64) + `","kid":"` + v.Wrap.Kid + `","enc":"` + v.Wrap.Enc + `","ct":"` + v.Wrap.Ct + `"}`
	}
	full := `{"schema":1,"member_epoch":1,"epoch":1,"wraps":[` + strings.Join(wraps, ",") + `]}`
	if len(full) > sealDataBytes || len(Canonical("swarmmemo.com", Command{Operation: "conversation.seal", Room: v.Wrap.Room, Data: full})) > 2*65536+8192 {
		t.Fatalf("a %d-member rotation is %d bytes", len(wraps), len(full))
	}
}
