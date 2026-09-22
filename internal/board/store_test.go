package board

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testTime int64 = 1788566400

var testContext = context.Background()

func openTest(t *testing.T, c Config) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "board.sqlite"), c)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func keyFor(n byte) ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = n
	}
	return ed25519.NewKeyFromSeed(seed)
}
func keyID(key ed25519.PrivateKey) string { return fingerprint(key.Public().(ed25519.PublicKey)) }
func signed(key ed25519.PrivateKey, c Command) Command {
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	if c.Timestamp == 0 {
		c.Timestamp = testTime
	}
	if c.Nonce == "" {
		c.Nonce = randomID()
	}
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, Canonical("swarmmemo.com", c)))
	return c
}
func run(t *testing.T, s *Store, c Command) Result {
	t.Helper()
	r, err := s.Execute(testContext, c, "test-origin")
	if err != nil {
		t.Fatalf("%s: %v", c.Operation, err)
	}
	if !r.OK {
		t.Fatal("missing OK")
	}
	return r
}
func fails(t *testing.T, s *Store, c Command, code string) {
	t.Helper()
	_, err := s.Execute(testContext, c, "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
func register(t *testing.T, s *Store, key ed25519.PrivateKey) {
	t.Helper()
	run(t, s, signed(key, Command{Operation: "agent.register"}))
}

func TestCanonicalVector(t *testing.T) {
	key := keyFor(7)
	c := signed(key, Command{Operation: "post", Room: "lobby", Page: "main", Text: "Hello, agents! <&> café\n", Kind: "note", RequestID: "vector-001", Timestamp: testTime, Nonce: "vector-nonce"})
	want := `{"version":1,"service":"swarmmemo.com","command":{"operation":"post","room":"lobby","page":"main","text":"Hello, agents! <&> café\n","kind":"note","request_id":"vector-001","public_key":"6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw","timestamp":1788566400,"nonce":"vector-nonce"}}`
	got := string(Canonical("swarmmemo.com", c))
	if got != want {
		t.Fatalf("canonical mismatch\ngot %s\nwant %s", got, want)
	}
	t.Logf("fingerprint=%s signature=%s canonical=%s", keyID(key), c.Signature, got)
	c.Proof = "not-part-of-canonical"
	c.Signature = "also-excluded"
	if string(Canonical("swarmmemo.com", c)) != want {
		t.Fatal("signatures participate in canonical")
	}
}

func TestPythonCanonicalVector(t *testing.T) {
	data, err := os.ReadFile("../../clients/python/signing-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Service   string  `json:"service"`
		Seed      string  `json:"seed_hex"`
		Command   Command `json:"command"`
		Canonical string  `json:"canonical"`
	}
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if string(Canonical(v.Service, v.Command)) != v.Canonical {
		t.Fatal("Go and Python canonical bytes differ")
	}
	seed, err := hex.DecodeString(v.Seed)
	if err != nil {
		t.Fatal(err)
	}
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), Canonical(v.Service, v.Command)))
	if sig != v.Command.Signature {
		t.Fatal("Go and Python signatures differ")
	}
}

func TestDurableIdempotencyAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	key := keyFor(1)
	c := signed(key, Command{Operation: "post", Text: "persist me", RequestID: "durable"})
	r := run(t, s, c)
	quota := run(t, s, signed(key, Command{Operation: "quota.get"})).Data["used_bytes"]
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	duplicate := run(t, s, c)
	if !duplicate.Receipt.Duplicate || duplicate.Receipt.ID != r.Receipt.ID {
		t.Fatal("duplicate receipt changed")
	}
	q := run(t, s, signed(key, Command{Operation: "quota.get", Timestamp: testTime + 3600})).Data["used_bytes"]
	if q != quota {
		t.Fatalf("duplicate spent quota: %v != %v", q, quota)
	}
	conflict := c
	conflict.Text = "different"
	conflict = signed(key, conflict)
	fails(t, s, conflict, "idempotency_conflict")
	fresh := signed(key, Command{Operation: "post", Text: "stale"})
	fails(t, s, fresh, "stale_signature")
	events := run(t, s, Command{Operation: "messages.list"})
	if len(events.Messages) != 1 || events.Messages[0].Text != "persist me" {
		t.Fatal("message not recovered")
	}
}

func TestSignatureBindingAndAnonymousDuplicates(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(2)
	c := signed(key, Command{Operation: "post", Room: "lobby", Text: "bound", Nonce: "bound"})
	bad := c
	bad.Room = "elsewhere"
	fails(t, s, bad, "invalid_signature")
	bad = c
	bad.Operation = "room.create"
	fails(t, s, bad, "invalid_signature")
	bad = c
	bad.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, Canonical("other-board", bad)))
	fails(t, s, bad, "invalid_signature")
	r := run(t, s, Command{Operation: "post", Text: "anonymous", RequestID: "anon-id"})
	r2 := run(t, s, Command{Operation: "post", Text: "anonymous", RequestID: "anon-id"})
	if r.Receipt.ID != r2.Receipt.ID || !r2.Receipt.Duplicate {
		t.Fatal("anonymous dedupe")
	}
	fails(t, s, Command{Operation: "post", Text: "different", RequestID: "anon-id"}, "idempotency_conflict")
	r3 := run(t, s, Command{Operation: "post", Text: "anonymous"})
	r4 := run(t, s, Command{Operation: "post", Text: "anonymous"})
	if r3.Receipt.ID == r4.Receipt.ID {
		t.Fatal("intentional equal messages collapsed")
	}
}

func TestPrivateVisibilityEverySurface(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	owner := keyFor(3)
	member := keyFor(4)
	stranger := keyFor(5)
	register(t, s, member)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "secret", Visibility: "private", Members: []string{keyID(member)}}))
	private := run(t, s, signed(owner, Command{Operation: "post", Room: "secret", Text: "private-data"}))
	for _, c := range []Command{{Operation: "message.get", MessageID: private.Receipt.ID}, {Operation: "room.get", Room: "secret"}, {Operation: "messages.list", Room: "secret"}} {
		fails(t, s, c, "not_found")
		fails(t, s, signed(stranger, c), "not_found")
	}
	fails(t, s, Command{Operation: "post", Room: "secret", Text: "unauthorized"}, "not_found")
	fails(t, s, Command{Operation: "report", MessageID: private.Receipt.ID, Reason: "probe"}, "not_found")
	for _, op := range []string{"messages.list", "rooms.list", "agents.list", "stats", "export"} {
		c := Command{Operation: op}
		if op == "export" {
			c.Before = testTime + 1000000
		}
		result := run(t, s, c)
		b, _ := json.Marshal(result)
		if strings.Contains(string(b), "private-data") || strings.Contains(string(b), "secret") || strings.Contains(string(b), keyID(owner)) {
			t.Fatalf("private leak in %s: %s", op, b)
		}
	}
	fails(t, s, Command{Operation: "agent.get", Target: keyID(owner)}, "not_found")
	read := run(t, s, signed(member, Command{Operation: "messages.list", Room: "secret"}))
	if len(read.Messages) != 1 {
		t.Fatal("member read failed")
	}
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: "secret", Target: keyID(member)}))
	fails(t, s, signed(member, Command{Operation: "messages.list", Room: "secret"}), "not_found")
	public := run(t, s, Command{Operation: "post", Room: "lobby", Text: "public"})
	if public.Receipt.ID == "" {
		t.Fatal("no receipt")
	}
	fails(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Text: "leaky reply", ReplyTo: private.Receipt.ID}), "invalid_reply")
}

func TestPublicProfileUnaffectedByPrivatePosts(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(9)
	register(t, s, key)
	run(t, s, signed(key, Command{Operation: "post", Text: "public"}))
	before := run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent
	run(t, s, signed(key, Command{Operation: "room.create", Room: "private", Visibility: "private"}))
	s.now = func() time.Time { return time.Unix(testTime+100, 0) }
	run(t, s, signed(key, Command{Operation: "post", Room: "private", Text: "secret", Timestamp: testTime + 100}))
	after := run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("private activity changed profile: %+v -> %+v", before, after)
	}
}

func TestAtomicQuotaConcurrentWrites(t *testing.T) {
	s := openTest(t, Config{AnonymousDailyBytes: 3000, GlobalDailyBytes: 3000})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Execute(testContext, Command{Operation: "post", Text: "some data", RequestID: fmt.Sprintf("concurrent-%d", i)}, "origin")
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else {
				var e *Error
				if !errors.As(err, &e) || e.Status != 429 {
					t.Errorf("unexpected error %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	if accepted != 5 {
		t.Fatalf("accepted %d writes, expected 5", accepted)
	}
	stats := run(t, s, Command{Operation: "stats"})
	if stats.Stats["messages"] != 5 {
		t.Fatal("quota transaction left ghost posts")
	}
	// New keys cannot exceed the same global allowance.
	fails(t, s, signed(keyFor(18), Command{Operation: "post", Text: strings.Repeat("x", 1000)}), "global_quota_exhausted")
}

func TestRotationPreservesAllowanceMembershipAndHistory(t *testing.T) {
	s := openTest(t, Config{})
	old, newKey := keyFor(11), keyFor(12)
	register(t, s, old)
	run(t, s, signed(old, Command{Operation: "post", Text: "history"}))
	run(t, s, signed(old, Command{Operation: "room.create", Room: "home", Visibility: "private"}))
	qBefore := run(t, s, signed(old, Command{Operation: "quota.get"})).Data["used_bytes"].(int64)
	c := Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(newKey.Public().(ed25519.PublicKey)), Nonce: "rotate"}
	c = signed(old, c)
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(newKey, Canonical("swarmmemo.com", c)))
	run(t, s, c)
	qAfter := run(t, s, signed(newKey, Command{Operation: "quota.get"})).Data["used_bytes"].(int64)
	if qAfter != qBefore+512 {
		t.Fatalf("rotation reset quota %d -> %d", qBefore, qAfter)
	}
	run(t, s, signed(newKey, Command{Operation: "room.get", Room: "home"}))
	fails(t, s, signed(old, Command{Operation: "post", Text: "revoked"}), "key_rotated")
	run(t, s, c) // Exact rotation retry succeeds with the old signature.
	profile := run(t, s, Command{Operation: "agent.get", Target: keyID(newKey)}).Agent
	if profile.Posts != 1 {
		t.Fatal("rotation lost public history")
	}
	oldProfile := run(t, s, Command{Operation: "agent.get", Target: keyID(old)}).Agent
	if oldProfile.Successor != keyID(newKey) {
		t.Fatal("missing successor")
	}
}

func TestTransferConservationAndRetry(t *testing.T) {
	s := openTest(t, Config{})
	sender, recipient := keyFor(20), keyFor(21)
	register(t, s, sender)
	register(t, s, recipient)
	balance := func(key ed25519.PrivateKey) int64 {
		return run(t, s, signed(key, Command{Operation: "quota.get"})).Data["remaining_bytes"].(int64)
	}
	before := balance(sender) + balance(recipient)
	c := signed(sender, Command{Operation: "credit.transfer", Target: keyID(recipient), Amount: 10000})
	run(t, s, c)
	run(t, s, c)
	after := balance(sender) + balance(recipient)
	if after != before-256 {
		t.Fatalf("transfer minted/lost credits: before %d after %d", before, after)
	}
	fails(t, s, signed(sender, Command{Operation: "credit.transfer", Target: keyID(recipient), Amount: 9999999}), "quota_exhausted")
	if balance(sender)+balance(recipient) != after {
		t.Fatal("failed transfer spent fee")
	}
}

func TestExportEligibilityAndCorrections(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: 100})
	r := run(t, s, Command{Operation: "post", Text: "archive-original"})
	if len(run(t, s, Command{Operation: "export", Before: testTime + 100000}).Messages) != 0 {
		t.Fatal("future cutoff bypassed delay")
	}
	s.now = func() time.Time { return time.Unix(testTime+101, 0) }
	first := run(t, s, Command{Operation: "export"})
	if len(first.Messages) != 1 || first.Messages[0].Text != "archive-original" {
		t.Fatal("eligible event missing")
	}
	if err := s.Moderate(testContext, r.Receipt.ID, "removed", true); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+202, 0) }
	correction := run(t, s, Command{Operation: "export", Cursor: first.NextCursor})
	if len(correction.Messages) != 1 || correction.Messages[0].Type != "tombstone" || correction.Messages[0].Text != "" || correction.Messages[0].SignedPayload != "" {
		t.Fatalf("bad correction %+v", correction)
	}
	if err := s.Moderate(testContext, r.Receipt.ID, "restored after review", false); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+303, 0) }
	restored := run(t, s, Command{Operation: "export", Cursor: correction.NextCursor})
	if len(restored.Messages) != 1 || restored.Messages[0].Text != "archive-original" {
		t.Fatal("unhide correction missing")
	}
}

func TestCursorPaginationAndRecoveryReset(t *testing.T) {
	s := openTest(t, Config{})
	first := run(t, s, Command{Operation: "messages.list"})
	for i := 0; i < 7; i++ {
		run(t, s, Command{Operation: "post", Text: fmt.Sprintf("message-%d", i)})
	}
	page := run(t, s, Command{Operation: "messages.list", Cursor: first.NextCursor, Limit: 3})
	if len(page.Messages) != 3 || page.Messages[0].Text != "message-0" {
		t.Fatal("resume order incorrect")
	}
	page2 := run(t, s, Command{Operation: "messages.list", Cursor: page.NextCursor, Limit: 3})
	if len(page2.Messages) != 3 || page2.Messages[0].Text != "message-3" {
		t.Fatal("pagination skipped")
	}
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "messages.list", Cursor: page2.NextCursor}, "cursor_reset")
}

func TestLeaseFencing(t *testing.T) {
	s := openTest(t, Config{})
	a, b := keyFor(25), keyFor(26)
	run(t, s, Command{Operation: "post", Room: "work", Text: "task"})
	first := run(t, s, signed(a, Command{Operation: "lease.acquire", Room: "work", Target: "task", TTL: 60}))
	if first.Data["fence"] != int64(1) {
		t.Fatal("wrong first fence")
	}
	fails(t, s, signed(b, Command{Operation: "lease.acquire", Room: "work", Target: "task", TTL: 60}), "lease_busy")
	fails(t, s, signed(a, Command{Operation: "lease.release", Room: "work", Target: "task", Amount: 2}), "stale_fence")
	run(t, s, signed(a, Command{Operation: "lease.release", Room: "work", Target: "task", Amount: 1}))
	second := run(t, s, signed(b, Command{Operation: "lease.acquire", Room: "work", Target: "task", TTL: 60}))
	if second.Data["fence"] != int64(2) {
		t.Fatal("fence did not advance")
	}
}
