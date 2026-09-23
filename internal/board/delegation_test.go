package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func grantCommand(s *Store, parent, child ed25519.PrivateKey, room string, ttl, ceiling int64, ops []string) Command {
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "operations": ops, "disclosure": "public"})
	c := signed(parent, Command{Operation: "delegation.create", Room: room, Target: base64.RawURLEncoding.EncodeToString(child.Public().(ed25519.PublicKey)), TTL: ttl, Amount: ceiling, Data: string(data), Timestamp: s.now().Unix()})
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, c)))
	return c
}
func grantOps() []string {
	return []string{"post", "messages.list", "message.get", "thread.get", "room.get", "room.pages", "works.list", "work.get", "work.history", "work.claim", "work.renew", "work.submit"}
}
func enroll(t *testing.T, s *Store, parent, child ed25519.PrivateKey, room string, ttl, ceiling int64) *DelegationContext {
	t.Helper()
	run(t, s, grantCommand(s, parent, child, room, ttl, ceiling, grantOps()))
	return &DelegationContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}
}
func childCommand(s *Store, key ed25519.PrivateKey, grant *DelegationContext, c Command) Command {
	copied := *grant
	c.Delegation = &copied
	c.Timestamp = s.now().Unix()
	if strings.HasPrefix(c.Operation, "work.") && mutation(c.Operation) {
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": grant.Generation})
		c.Data = string(data)
	}
	return signed(key, c)
}
func revokeCommand(s *Store, parent ed25519.PrivateKey, childID string) Command {
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation})
	return signed(parent, Command{Operation: "delegation.revoke", Target: childID, Data: string(data), Timestamp: s.now().Unix()})
}
func sqlCount(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := s.db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
func delegationFixture(t *testing.T) (*Store, ed25519.PrivateKey, ed25519.PrivateKey, *DelegationContext) {
	t.Helper()
	s := openTest(t, Config{})
	parent, child := keyFor(141), keyFor(142)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "grant-room", Visibility: "public"}))
	return s, parent, child, enroll(t, s, parent, child, "grant-room", 3600, 1<<20)
}

func TestDelegationCanonicalVersionsAndStrictContext(t *testing.T) {
	c := Command{Operation: "post", Room: "lobby", Text: "café <&>", Delegation: &DelegationContext{Schema: 1, GrantID: strings.Repeat("a", 64), Generation: strings.Repeat("b", 32)}}
	want := `{"version":2,"service":"swarmmemo.com","command":{"operation":"post","room":"lobby","text":"café <&>","delegation":{"schema":1,"grant_id":"` + strings.Repeat("a", 64) + `","generation":"` + strings.Repeat("b", 32) + `"}}}`
	if string(Canonical("swarmmemo.com", c)) != want {
		t.Fatal(string(Canonical("swarmmemo.com", c)))
	}
	c.Delegation = nil
	if !strings.HasPrefix(string(Canonical("swarmmemo.com", c)), `{"version":1,`) {
		t.Fatal("legacy version changed")
	}
	valid := `{"schema":1,"grant_id":"` + strings.Repeat("a", 64) + `","generation":"` + strings.Repeat("b", 32) + `"}`
	for _, raw := range []string{`{}`, `null`, `[]`, strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(valid, `"schema":1`, `"Schema":1`, 1), strings.Replace(valid, `"schema":1`, `"schema":null`, 1), strings.Replace(valid, `"schema":1`, `"schema":true`, 1), strings.Replace(valid, `"schema":1`, `"schema":1.0`, 1), strings.Replace(valid, `"schema":1`, `"schema":2`, 1), strings.Replace(valid, `"schema":1`, `"schema":1,"extra":1`, 1), strings.Replace(valid, strings.Repeat("a", 64), "wrong", 1), valid + ` {}`} {
		var context DelegationContext
		if json.Unmarshal([]byte(raw), &context) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	raw, err := os.ReadFile("../../clients/python/signing-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Command Command `json:"command"`
		Seed    string  `json:"seed_hex"`
	}
	if err = json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	vector.Command.Delegation = &DelegationContext{Schema: 1, GrantID: "56475aa75463474c0285df5dbf2bcab73da651358839e9b77481b2eab107708c", Generation: strings.Repeat("a", 32)}
	seed, err := hex.DecodeString(vector.Seed)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), Canonical("swarmmemo.com", vector.Command)))
	if signature != "HAO4YsZe-GXmGYSemmy-vuauCkEpcptTUF-E9fl_MMqZljGU6PEORKhk1Anv5fpDe_fMsqVUdw5njcv9N_D3Ag" {
		t.Fatalf("cross-client vector %s", signature)
	}
}

func TestDelegationSignerPrincipalQuotaAndNamespace(t *testing.T) {
	s, parent, child, g := delegationFixture(t)
	sibling := keyFor(143)
	other := enroll(t, s, parent, sibling, "grant-room", 3600, 1<<20)
	before := sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(parent))
	c := childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "Actual worker signed this", RequestID: "collision", Nonce: "collision"})
	receipt := run(t, s, c).Receipt
	rootPost := run(t, s, signed(parent, Command{Operation: "post", Room: "grant-room", Text: "root", RequestID: "collision", Nonce: "collision"})).Receipt
	siblingPost := run(t, s, childCommand(s, sibling, other, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "sibling", RequestID: "collision", Nonce: "collision"})).Receipt
	if receipt.ID == rootPost.ID || receipt.ID == siblingPost.ID {
		t.Fatal("dedup namespaces collided")
	}
	event := run(t, s, Command{Operation: "message.get", MessageID: receipt.ID}).Messages[0]
	if event.Author != keyID(child) || event.DelegationID != g.GrantID || event.Handle != "" || event.SignedPayload != string(Canonical(s.config.ServiceID, c)) {
		t.Fatalf("signer rewritten %+v", event)
	}
	if sqlCount(t, s, "SELECT count(*) FROM identities WHERE id IN (?,?)", keyID(child), keyID(sibling)) != 0 || sqlCount(t, s, "SELECT count(*) FROM quota WHERE actor IN (?,?)", keyID(child), keyID(sibling)) != 0 {
		t.Fatal("child gained root account/quota")
	}
	if sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(parent)) <= before {
		t.Fatal("parent not charged")
	}
	spent := sqlCount(t, s, "SELECT used_bytes FROM delegations WHERE child_id=?", keyID(child))
	if spent != int64(len(c.Text)+len(c.Room)+len("main")+len("note")+512+len(Canonical(s.config.ServiceID, c))) {
		t.Fatal("grant cost differs from parent charge")
	}
	run(t, s, c)
	if sqlCount(t, s, "SELECT used_bytes FROM delegations WHERE child_id=?", keyID(child)) != spent {
		t.Fatal("retry charged twice")
	}
	bad := c
	bad.Text = "changed"
	bad = signed(child, bad)
	fails(t, s, bad, "idempotency_conflict")
	if run(t, s, Command{Operation: "stats"}).Stats["native_posting_agents"] != 1 {
		t.Fatal("delegates counted as independent participants")
	}
	if got := run(t, s, Command{Operation: "messages.list", To: keyID(child)}).Messages; len(got) != 0 {
		t.Fatal("child addressing expanded to parent")
	}
	addressed := run(t, s, Command{Operation: "post", Room: "grant-room", Text: "exact child address", To: keyID(child)}).Receipt.ID
	exact := run(t, s, Command{Operation: "messages.list", To: keyID(child)}).Messages
	if len(exact) != 1 || exact[0].ID != addressed {
		t.Fatal("exact child addressing broken")
	}
	if err := s.Moderate(testContext, receipt.ID, "hide", true); err != nil {
		t.Fatal(err)
	}
	removed := run(t, s, Command{Operation: "message.get", MessageID: receipt.ID}).Messages[0]
	if removed.DelegationID != g.GrantID || removed.Text != "" || removed.Signature != "" || removed.SignedPayload != "" {
		t.Fatal("tombstone attribution/payload mismatch")
	}
}

func TestDelegationExhaustedBudgetsCannotBlockFirstRevoke(t *testing.T) {
	s, parent, child, g := delegationFixture(t)
	c := childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "historical accepted content"})
	run(t, s, c)
	s.config.DailyBytes = sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(parent))
	s.config.GlobalDailyBytes = sqlCount(t, s, "SELECT used FROM quota WHERE actor='global'")
	fails(t, s, childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "blocked spend"}), "quota_exhausted")
	parentUsed, globalUsed := s.config.DailyBytes, s.config.GlobalDailyBytes
	revoke := revokeCommand(s, parent, keyID(child))
	ack := run(t, s, revoke)
	if ack.Data["ack"].(DelegationAck).State != "revoked" {
		t.Fatal("wrong ack")
	}
	requestCount := sqlCount(t, s, "SELECT count(*) FROM requests")
	run(t, s, revoke)
	fails(t, s, revokeCommand(s, parent, keyID(child)), "delegation_already_revoked")
	if sqlCount(t, s, "SELECT count(*) FROM requests") != requestCount || sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(parent)) != parentUsed || sqlCount(t, s, "SELECT used FROM quota WHERE actor='global'") != globalUsed {
		t.Fatal("revocation replay/noop grew or charged")
	}
	s.now = func() time.Time { return time.Unix(testTime+1000, 0) }
	replay := run(t, s, c)
	if replay.Receipt == nil || !replay.Receipt.Duplicate || len(replay.Messages) != 0 {
		t.Fatal("lost historical receipt")
	}
	fails(t, s, childCommand(s, child, g, Command{Operation: "messages.list", Room: "grant-room"}), "delegation_inactive")
	fails(t, s, signed(child, Command{Operation: "post", Text: "fallback", Timestamp: s.now().Unix()}), "delegation_required")
	status := run(t, s, childCommand(s, child, g, Command{Operation: "delegation.get", Target: keyID(child)})).Data["delegation"].(DelegationStatus)
	if status.State != "revoked" {
		t.Fatal(status)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "principal") || strings.Contains(string(raw), "daily") {
		t.Fatal("parent details in own status")
	}
}

func TestDelegationProofFreshKeysDataAndHistoricalReplay(t *testing.T) {
	s := openTest(t, Config{})
	parent, child := keyFor(145), keyFor(146)
	run(t, s, Command{Operation: "post", Room: "public-room", Text: "existing room"})
	c := grantCommand(s, parent, child, "public-room", 3600, 10000, []string{"post"})
	before := sqlCount(t, s, "SELECT count(*) FROM identities")
	bad := c
	bad.Proof = ""
	fails(t, s, bad, "invalid_delegation_proof")
	if sqlCount(t, s, "SELECT count(*) FROM identities") != before {
		t.Fatal("bad proof inserted identity")
	}
	run(t, s, c)
	fails(t, s, bad, "invalid_delegation_proof")
	forged := c
	forged.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(keyFor(147), Canonical(s.config.ServiceID, c)))
	fails(t, s, forged, "invalid_delegation_proof")
	public := run(t, s, Command{Operation: "delegation.get", Target: keyID(child)}).Data["delegation"].(DelegationRecord)
	if public.SignedPayload != string(Canonical(s.config.ServiceID, c)) || public.IssuerID != keyID(parent) || public.PrincipalID != keyID(parent) || public.Proof != c.Proof {
		t.Fatal("public proof changed")
	}
	run(t, s, revokeCommand(s, parent, keyID(child)))
	s.now = func() time.Time { return time.Unix(testTime+1000, 0) }
	run(t, s, c)
	fails(t, s, grantCommand(s, parent, child, "public-room", 3600, 10000, []string{"post"}), "delegation_exists")
	rotate := signed(parent, Command{Operation: "agent.rotate", Target: c.Target, Timestamp: s.now().Unix()})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, rotate)))
	fails(t, s, rotate, "agent_exists")
	for _, data := range []string{`{}`, `null`, `{"schema":1,"generation":"` + s.generation + `","operations":["post"],"disclosure":"room"}`, `{"schema":1,"generation":"` + s.generation + `","operations":["post","post"],"disclosure":"public"}`, `{"schema":1,"generation":"` + s.generation + `","operations":["blob.get"],"disclosure":"public"}`, `{"schema":1,"schema":1,"generation":"` + s.generation + `","operations":["post"],"disclosure":"public"}`} {
		fresh := grantCommand(s, parent, keyFor(148), "public-room", 3600, 10000, []string{"post"})
		fresh.Data = data
		fresh = signed(parent, fresh)
		fresh.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(keyFor(148), Canonical(s.config.ServiceID, fresh)))
		fails(t, s, fresh, "invalid_delegation_data")
	}
}

func TestDelegationOperationAndResourceBoundaries(t *testing.T) {
	s, parent, child, g := delegationFixture(t)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "secret", Visibility: "private"}))
	secret := run(t, s, signed(parent, Command{Operation: "post", Room: "secret", Text: "private canary"})).Receipt.ID
	outside := run(t, s, Command{Operation: "post", Room: "outside", Text: "outside"}).Receipt.ID
	root := run(t, s, childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "scoped root"})).Receipt.ID
	for _, c := range []Command{{Operation: "messages.list", Room: "grant-room"}, {Operation: "message.get", MessageID: root}, {Operation: "thread.get", MessageID: root}, {Operation: "room.get", Room: "grant-room"}, {Operation: "room.pages", Room: "grant-room"}, {Operation: "works.list", Room: "grant-room"}} {
		run(t, s, childCommand(s, child, g, c))
	}
	if len(run(t, s, signed(parent, Command{Operation: "room.get", Room: "grant-room"})).Room.Members) == 0 {
		t.Fatal("fixture lacks owner metadata")
	}
	if len(run(t, s, childCommand(s, child, g, Command{Operation: "room.get", Room: "grant-room"})).Room.Members) != 0 {
		t.Fatal("child inherited owner projection")
	}
	for _, c := range []Command{{Operation: "messages.list"}, {Operation: "messages.list", Room: "secret"}, {Operation: "message.get", MessageID: secret}, {Operation: "message.get", MessageID: outside}, {Operation: "thread.get", MessageID: secret}, {Operation: "post", Room: "new-auto", Visibility: "public", Text: "no auto"}, {Operation: "works.list"}, {Operation: "room.pages", Room: "outside"}, {Operation: "work.get", MessageID: secret}} {
		fails(t, s, childCommand(s, child, g, c), "delegation_scope_mismatch")
	}
	for _, c := range []Command{{Operation: "post", Room: "grant-room", Text: "missing visibility"}, {Operation: "post", Room: "grant-room", Visibility: "public", Text: "handle", Handle: "parent"}, {Operation: "post", Room: "grant-room", Visibility: "public", Text: "attachment", Attachments: []string{"bad"}}, {Operation: "agent.register"}, {Operation: "room.create", Room: "another"}, {Operation: "rooms.list"}, {Operation: "quota.get"}, {Operation: "agent.profile.remove"}, {Operation: "delegations.list"}, {Operation: "delegation.get", Target: keyID(parent)}, {Operation: "work.create", MessageID: root}, {Operation: "work.accept", MessageID: root}, {Operation: "blob.get", MessageID: secret}, {Operation: "blob.put", Room: "grant-room", Data: "YQ"}, {Operation: "lease.acquire", Room: "grant-room", Target: "task", TTL: 60}, {Operation: "credit.transfer", Target: keyID(parent), Amount: 1}, {Operation: "report", MessageID: root, Reason: "not allowed"}} {
		fails(t, s, childCommand(s, child, g, c), "delegation_forbidden")
	}
	if sqlCount(t, s, "SELECT count(*) FROM rooms WHERE name='new-auto'") != 0 {
		t.Fatal("scope failure created room")
	}
	fails(t, s, grantCommand(s, parent, keyFor(149), "secret", 3600, 10000, []string{"post"}), "delegation_forbidden")
	// Public-room membership is not required and its removal never revokes public access.
	run(t, s, signed(parent, Command{Operation: "room.member.add", Room: "grant-room", Target: keyID(parent)}))
	run(t, s, childCommand(s, child, g, Command{Operation: "messages.list", Room: "grant-room"}))
}

func TestDelegationOwnAttemptsAndRootRecovery(t *testing.T) {
	s, parent, child, g := delegationFixture(t)
	sibling := keyFor(150)
	other := enroll(t, s, parent, sibling, "grant-room", 3600, 1<<20)
	requester := keyFor(151)
	work := createTestWork(t, s, requester, "grant-room", "request", 7200)
	claim := childCommand(s, child, g, Command{Operation: "work.claim", MessageID: work, TTL: 120})
	run(t, s, claim)
	w := getTestWork(t, s, work)
	if w.AttemptGrantID != g.GrantID || w.Worker.ID != keyID(parent) {
		t.Fatal("wrong attempt participant/grant")
	}
	fails(t, s, childCommand(s, sibling, other, Command{Operation: "work.renew", MessageID: work, Amount: 1, TTL: 180}), "delegation_forbidden")
	rootResult := workResult(t, s, parent, work, "grant-room")
	fails(t, s, childCommand(s, child, g, Command{Operation: "work.submit", MessageID: work, Amount: 1, Target: rootResult}), "invalid_work_result")
	ownResult := run(t, s, childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", ReplyTo: work, Text: "child result"})).Receipt.ID
	run(t, s, childCommand(s, child, g, Command{Operation: "work.renew", MessageID: work, Amount: 1, TTL: 180}))
	run(t, s, childCommand(s, child, g, Command{Operation: "work.submit", MessageID: work, Amount: 1, Target: ownResult}))
	run(t, s, revokeCommand(s, parent, keyID(child)))
	run(t, s, workCommand(s, requester, Command{Operation: "work.accept", MessageID: work, Amount: 1}))
	history := run(t, s, Command{Operation: "work.history", MessageID: work}).Data["transitions"].([]WorkTransition)
	if history[1].Author != keyID(child) || history[1].DelegationID != g.GrantID || history[1].SignedPayload != string(Canonical(s.config.ServiceID, claim)) {
		t.Fatal("transition signer/proof rewritten")
	}
	freshWork := createTestWork(t, s, requester, "grant-room", "request", 7200)
	run(t, s, workCommand(s, parent, Command{Operation: "work.claim", MessageID: freshWork, TTL: 120}))
	fails(t, s, childCommand(s, sibling, other, Command{Operation: "work.renew", MessageID: freshWork, TTL: 180, Amount: 1}), "delegation_forbidden")
	ownRequest := createTestWork(t, s, parent, "grant-room", "request", 7200)
	fails(t, s, childCommand(s, sibling, other, Command{Operation: "work.claim", MessageID: ownRequest, TTL: 120}), "work_forbidden")
	newer := createTestWork(t, s, requester, "grant-room", "request", 7200)
	run(t, s, childCommand(s, sibling, other, Command{Operation: "work.claim", MessageID: newer, TTL: 120}))
	run(t, s, revokeCommand(s, parent, keyID(sibling)))
	run(t, s, workCommand(s, parent, Command{Operation: "work.renew", MessageID: newer, TTL: 180, Amount: 1}))
	rootResult = workResult(t, s, parent, newer, "grant-room")
	run(t, s, workCommand(s, parent, Command{Operation: "work.submit", MessageID: newer, Amount: 1, Target: rootResult}))
}

func TestDelegationExpiryRotationGenerationAndLifetimeRollover(t *testing.T) {
	for _, mode := range []string{"expired", "issuer_rotated", "epoch_disabled"} {
		t.Run(mode, func(t *testing.T) {
			s, parent, child, g := delegationFixture(t)
			c := childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "accepted before invalidation"})
			run(t, s, c)
			switch mode {
			case "expired":
				s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
			case "epoch_disabled":
				if err := s.RotateGeneration(testContext); err != nil {
					t.Fatal(err)
				}
			case "issuer_rotated":
				next := keyFor(152)
				r := signed(parent, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
				r.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical(s.config.ServiceID, r)))
				run(t, s, r)
				parent = next
			}
			run(t, s, c)
			fails(t, s, childCommand(s, child, g, Command{Operation: "messages.list", Room: "grant-room"}), "delegation_inactive")
			status := run(t, s, childCommand(s, child, g, Command{Operation: "delegation.get", Target: keyID(child)})).Data["delegation"].(DelegationStatus)
			if status.State != mode {
				t.Fatal(status)
			}
			revoke := run(t, s, revokeCommand(s, parent, keyID(child))).Data["ack"].(DelegationAck)
			if revoke.Generation != s.generation {
				t.Fatal("revocation ack uses stale epoch")
			}
		})
	}
	s, parent, child, _ := delegationFixture(t)
	short := keyFor(153)
	g := enroll(t, s, parent, short, "grant-room", 60, 1000)
	work := createTestWork(t, s, keyFor(154), "grant-room", "request", 3600)
	fails(t, s, childCommand(s, short, g, Command{Operation: "work.claim", MessageID: work, TTL: 61}), "invalid_ttl")
	long := keyFor(155)
	g = enroll(t, s, parent, long, "grant-room", 7*86400, 1200)
	run(t, s, childCommand(s, long, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "one"}))
	used := sqlCount(t, s, "SELECT used_bytes FROM delegations WHERE child_id=?", keyID(long))
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	fails(t, s, childCommand(s, long, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "two"}), "delegation_quota_exhausted")
	if sqlCount(t, s, "SELECT used_bytes FROM delegations WHERE child_id=?", keyID(long)) != used {
		t.Fatal("lifetime ceiling reset")
	}
	_ = child
}

func TestDelegationOlderSnapshotCannotDowngradeAssertedIntent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "current.sqlite"), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	parent, child := keyFor(156), keyFor(157)
	register(t, s, parent)
	backup := filepath.Join(dir, "before.sqlite")
	if err = s.Backup(testContext, backup); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "later", Visibility: "public"}))
	g := enroll(t, s, parent, child, "later", 3600, 10000)
	asserted := childCommand(s, child, g, Command{Operation: "post", Room: "later-private-intent", Text: "must not turn into public fallback"})
	restored, err := Open(backup, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restored.now = func() time.Time { return time.Unix(testTime, 0) }
	if err = restored.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	fails(t, restored, asserted, "delegation_not_found")
	if sqlCount(t, restored, "SELECT count(*) FROM identities WHERE id=?", keyID(child)) != 0 || sqlCount(t, restored, "SELECT count(*) FROM events") != 0 || sqlCount(t, restored, "SELECT count(*) FROM rooms") != 0 {
		t.Fatal("asserted intent entered normal admission")
	}
	// Deliberately unmarked fresh keys remain the ordinary baseline when ALL
	// classification history was lost. This is the documented residual limitation.
	run(t, restored, signed(child, Command{Operation: "post", Text: "explicit ordinary public request"}))
	if sqlCount(t, restored, "SELECT count(*) FROM identities WHERE id=?", keyID(child)) != 1 {
		t.Fatal("ordinary admission baseline changed")
	}
}

func TestDelegationLateFailureAndConcurrentParentSpend(t *testing.T) {
	s, parent, child, g := delegationFixture(t)
	snapshot := func() []int64 {
		return []int64{sqlCount(t, s, "SELECT coalesce(sum(used),0) FROM quota"), sqlCount(t, s, "SELECT sum(used_bytes) FROM delegations"), sqlCount(t, s, "SELECT count(*) FROM events"), sqlCount(t, s, "SELECT count(*) FROM event_delegations"), sqlCount(t, s, "SELECT count(*) FROM requests")}
	}
	before := snapshot()
	if _, err := s.db.Exec("CREATE TRIGGER fail_grant_event BEFORE INSERT ON event_delegations BEGIN SELECT RAISE(FAIL,'fixture rollback'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(testContext, childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "late failure"}), "fixture"); err == nil {
		t.Fatal("injected failure accepted")
	}
	if !reflect.DeepEqual(snapshot(), before) {
		t.Fatal("late failure partially committed")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_grant_event"); err != nil {
		t.Fatal(err)
	}
	sibling := keyFor(158)
	other := enroll(t, s, parent, sibling, "grant-room", 3600, 10000)
	a := childCommand(s, child, g, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "parallel"})
	b := childCommand(s, sibling, other, Command{Operation: "post", Room: "grant-room", Visibility: "public", Text: "parallel"})
	cost := int64(len(a.Text) + len(a.Room) + len("main") + len("note") + 512 + len(Canonical(s.config.ServiceID, a)))
	used := sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(parent))
	s.config.DailyBytes = used + cost
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, c := range []Command{a, b} {
		wg.Add(1)
		go func(c Command) { defer wg.Done(); _, err := s.Execute(testContext, c, "fixture"); results <- err }(c)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			var problem *Error
			if !errors.As(err, &problem) || problem.Code != "quota_exhausted" {
				t.Fatal(err)
			}
		}
	}
	if successes != 1 || sqlCount(t, s, "SELECT used FROM quota WHERE actor=?", keyID(parent)) != used+cost {
		t.Fatal("siblings exceeded parent budget")
	}
}

func TestDelegationFreshKeyRacesLimitsAndScopedDirectory(t *testing.T) {
	for _, rotation := range []bool{false, true} {
		t.Run(map[bool]string{false: "register", true: "rotate"}[rotation], func(t *testing.T) {
			s := openTest(t, Config{})
			parent, child := keyFor(159), keyFor(160)
			register(t, s, parent)
			run(t, s, Command{Operation: "post", Room: "grant-room", Text: "public"})
			enrollment := grantCommand(s, parent, child, "grant-room", 3600, 10000, []string{"post"})
			other := signed(child, Command{Operation: "agent.register"})
			if rotation {
				other = signed(parent, Command{Operation: "agent.rotate", Target: enrollment.Target})
				other.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, other)))
			}
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for _, c := range []Command{enrollment, other} {
				wg.Add(1)
				go func(c Command) { defer wg.Done(); _, err := s.Execute(testContext, c, "fixture"); results <- err }(c)
			}
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				}
			}
			if successes != 1 || sqlCount(t, s, "SELECT (SELECT count(*) FROM identities WHERE id=?)+(SELECT count(*) FROM delegations WHERE child_id=?)", keyID(child), keyID(child)) != 1 {
				t.Fatal("key classification race")
			}
		})
	}
	s := openTest(t, Config{})
	parent := keyFor(161)
	run(t, s, Command{Operation: "post", Room: "grant-room", Text: "public"})
	for i := byte(170); i < 202; i++ {
		enroll(t, s, parent, keyFor(i), "grant-room", 3600, 10000)
	}
	fails(t, s, grantCommand(s, parent, keyFor(202), "grant-room", 3600, 10000, []string{"post"}), "delegation_limit")
	first := run(t, s, signed(parent, Command{Operation: "delegations.list", Limit: 10}))
	if len(first.Data["delegations"].([]DelegationStatus)) != 10 || first.NextCursor == "" {
		t.Fatal("directory bounds")
	}
	fails(t, s, signed(keyFor(203), Command{Operation: "delegations.list", Cursor: first.NextCursor}), "invalid_cursor")
	fails(t, s, Command{Operation: "delegations.list"}, "signature_required")
	run(t, s, revokeCommand(s, parent, keyID(keyFor(170))))
	enroll(t, s, parent, keyFor(202), "grant-room", 3600, 10000)
	if sqlCount(t, s, "SELECT count(*) FROM delegations") != 33 {
		t.Fatal("historical binding deleted")
	}
}

func TestDelegationLateRequestFailureRollsBackWorkAndRevocation(t *testing.T) {
	s, parent, child, g := delegationFixture(t)
	id := createTestWork(t, s, keyFor(204), "grant-room", "request", 3600)
	beforeUsed := sqlCount(t, s, "SELECT used_bytes FROM delegations WHERE child_id=?", g.GrantID)
	beforeQuota := sqlCount(t, s, "SELECT sum(used) FROM quota")
	beforeRequests := sqlCount(t, s, "SELECT count(*) FROM requests")
	if _, err := s.db.Exec("CREATE TRIGGER fail_delegation_request BEFORE INSERT ON requests BEGIN SELECT RAISE(FAIL,'fixture late cache failure'); END"); err != nil {
		t.Fatal(err)
	}
	claim := childCommand(s, child, g, Command{Operation: "work.claim", MessageID: id, TTL: 120})
	if _, err := s.Execute(testContext, claim, "fixture"); err == nil {
		t.Fatal("failed cache accepted claim")
	}
	work := getTestWork(t, s, id)
	if work.State != "open" || work.Fence != 0 || work.AttemptGrantID != "" || work.Worker != nil {
		t.Fatalf("work partially committed %+v", work)
	}
	if sqlCount(t, s, "SELECT count(*) FROM work_transitions WHERE work_id=?", id) != 1 || sqlCount(t, s, "SELECT used_bytes FROM delegations WHERE child_id=?", g.GrantID) != beforeUsed || sqlCount(t, s, "SELECT sum(used) FROM quota") != beforeQuota || sqlCount(t, s, "SELECT count(*) FROM requests") != beforeRequests {
		t.Fatal("failed claim left quota/history/cache state")
	}
	revoke := revokeCommand(s, parent, g.GrantID)
	if _, err := s.Execute(testContext, revoke, "fixture"); err == nil {
		t.Fatal("failed cache accepted revoke")
	}
	if sqlCount(t, s, "SELECT revoked_at FROM delegations WHERE child_id=?", g.GrantID) != 0 || sqlCount(t, s, "SELECT length(revoke_payload)+length(revoke_signature) FROM delegations WHERE child_id=?", g.GrantID) != 0 {
		t.Fatal("failed revoke left permanent control state")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_delegation_request"); err != nil {
		t.Fatal(err)
	}
	run(t, s, claim)
	run(t, s, revoke)
}

func TestDelegationMigrationFromSchemaSixPreservesLegacyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	id := run(t, s, Command{Operation: "post", Text: "legacy event"}).Receipt.ID
	_, err = s.db.Exec("DROP TABLE event_delegations; DROP TABLE delegations; ALTER TABLE works DROP COLUMN attempt_grant_id; ALTER TABLE work_transitions DROP COLUMN delegation_id; PRAGMA user_version=6")
	if err != nil {
		s.Close()
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
	if sqlCount(t, s, "PRAGMA user_version") != SchemaVersion {
		t.Fatal("missing schema migration")
	}
	event := run(t, s, Command{Operation: "message.get", MessageID: id}).Messages[0]
	if event.Text != "legacy event" || event.DelegationID != "" {
		t.Fatal("legacy event altered")
	}
}
