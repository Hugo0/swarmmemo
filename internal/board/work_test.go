package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func workCommand(s *Store, key ed25519.PrivateKey, c Command) Command {
	d := map[string]any{"schema": 1, "generation": s.generation}
	if c.Operation == "work.create" {
		d["title"] = "Review café <&>"
		d["capabilities"] = []string{"review", "go"}
	}
	b, _ := json.Marshal(d)
	c.Data = string(b)
	c.Timestamp = s.now().Unix()
	return signed(key, c)
}

func createTestWork(t *testing.T, s *Store, key ed25519.PrivateKey, room, kind string, ttl int64) string {
	t.Helper()
	id := run(t, s, signed(key, Command{Operation: "post", Room: room, Kind: kind, Text: "Unpaid coordination brief", Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, workCommand(s, key, Command{Operation: "work.create", MessageID: id, TTL: ttl}))
	return id
}

func getTestWork(t *testing.T, s *Store, id string) Work {
	t.Helper()
	return run(t, s, Command{Operation: "work.get", MessageID: id}).Data["work"].(Work)
}

func workResult(t *testing.T, s *Store, key ed25519.PrivateKey, id, room string) string {
	t.Helper()
	return run(t, s, signed(key, Command{Operation: "post", Room: room, ReplyTo: id, Text: "Human-reviewed result reference", Timestamp: s.now().Unix()})).Receipt.ID
}

func TestWorkLifecycleHistoryAndExactReplay(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(90), keyFor(91)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 120})
	ack := run(t, s, claim)
	fails(t, s, workCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 120}), "work_renew_not_extended")
	run(t, s, workCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 180}))
	result := workResult(t, s, worker, id, "lobby")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 2, Target: result}), "work_fence_mismatch")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}))
	s.now = func() time.Time { return time.Unix(testTime+181, 0) }
	w := getTestWork(t, s, id)
	if w.State != "submitted" || w.ResultID != result || !w.ResultAvailable {
		t.Fatalf("submitted %+v", w)
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}))
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "too late"}), "work_state_conflict")
	first, _ := json.Marshal(ack)
	replay, _ := json.Marshal(run(t, s, claim))
	var firstJSON, replayJSON any
	_ = json.Unmarshal(first, &firstJSON)
	_ = json.Unmarshal(replay, &replayJSON)
	if !reflect.DeepEqual(firstJSON, replayJSON) {
		t.Fatalf("replay changed %s %s", first, replay)
	}
	history := run(t, s, Command{Operation: "work.history", MessageID: id, Limit: 2})
	if history.NextCursor == "" || history.Data["has_more"] != true {
		t.Fatal("missing history pagination")
	}
	all := history.Data["transitions"].([]WorkTransition)
	for history.NextCursor != "" {
		history = run(t, s, Command{Operation: "work.history", MessageID: id, Limit: 2, Cursor: history.NextCursor})
		all = append(all, history.Data["transitions"].([]WorkTransition)...)
	}
	if len(all) != 5 {
		t.Fatalf("transitions %d", len(all))
	}
	for i, tr := range all {
		pub, _ := base64.RawURLEncoding.DecodeString(tr.PublicKey)
		sig, _ := base64.RawURLEncoding.DecodeString(tr.Signature)
		if tr.Sequence != int64(i+1) || !ed25519.Verify(pub, []byte(tr.SignedPayload), sig) {
			t.Fatal("invalid exact history signature/sequence")
		}
	}
	if all[1].SignedPayload != string(Canonical("swarmmemo.com", claim)) {
		t.Fatal("canonical payload changed")
	}
	if strings.Contains(string(first), "Review") || strings.Contains(string(first), "public_key") || strings.Contains(string(first), "result_id") {
		t.Fatal("ack contains content or current projection")
	}
}

func TestWorkStrictSchemaAndRootAuthority(t *testing.T) {
	s := openTest(t, Config{})
	owner, other := keyFor(90), keyFor(91)
	id := run(t, s, signed(owner, Command{Operation: "post", Kind: "request", Text: "root"})).Receipt.ID
	fails(t, s, Command{Operation: "work.create", MessageID: id, Data: `{}`}, "signature_required")
	base := workCommand(s, owner, Command{Operation: "work.create", MessageID: id})
	invalid := []string{`{}`, `null`, base.Data + ` {}`, strings.Replace(base.Data, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(base.Data, `"schema":1`, `"schema":null`, 1), strings.Replace(base.Data, `"schema":1`, `"schema":1,"extra":true`, 1), strings.Replace(base.Data, `["review","go"]`, `["review","review"]`, 1), strings.Replace(base.Data, `["review","go"]`, `null`, 1), strings.Replace(base.Data, `["review","go"]`, `["Upper"]`, 1), strings.Replace(base.Data, `Review café \u003c\u0026\u003e`, ``, 1)}
	for _, raw := range invalid {
		c := base
		c.Data = raw
		c.Nonce = ""
		c = signed(owner, c)
		fails(t, s, c, "invalid_work_data")
	}
	fails(t, s, workCommand(s, other, Command{Operation: "work.create", MessageID: id}), "work_forbidden")
	note := run(t, s, signed(owner, Command{Operation: "post", Kind: "note", Text: "not native request"})).Receipt.ID
	fails(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: note}), "invalid_work_root")
	// The imported kind is reserved to the registered curator account, so the
	// curator both posts that root and tries to make work of its own message.
	curator := keyFor(77)
	run(t, s, signed(curator, Command{Operation: "agent.register", Handle: curatorHandle}))
	summary := run(t, s, signed(curator, Command{Operation: "post", Kind: "imported", Text: "not native request"})).Receipt.ID
	fails(t, s, workCommand(s, curator, Command{Operation: "work.create", MessageID: summary}), "invalid_work_root")
	reply := run(t, s, signed(owner, Command{Operation: "post", Kind: "request", ReplyTo: id, Text: "reply"})).Receipt.ID
	fails(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: reply}), "invalid_work_root")
	anonymous := run(t, s, Command{Operation: "post", Kind: "request", Text: "anonymous"}).Receipt.ID
	fails(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: anonymous}), "work_forbidden")
	run(t, s, base)
	fails(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: id}), "work_exists")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.claim", MessageID: id, TTL: 60}), "work_forbidden")
	for _, ttl := range []int64{0, 59, 3601} {
		fails(t, s, workCommand(s, other, Command{Operation: "work.claim", MessageID: id, TTL: ttl}), "invalid_ttl")
	}
	c := workCommand(s, other, Command{Operation: "work.claim", MessageID: id, TTL: 60})
	c.Data = base.Data
	c = signed(other, c)
	fails(t, s, c, "invalid_work_data")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id}), "invalid_reason")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "bad\x00reason"}), "invalid_reason")
}

func TestWorkRejectExpiryRenewAndModeration(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(90), keyFor(91)
	id := createTestWork(t, s, owner, "lobby", "request", 300)
	fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 301}), "invalid_ttl")
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60}))
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "release stalled claim"}))
	if w := getTestWork(t, s, id); w.State != "open" || w.Worker != nil || w.ClaimExpiresAt != 0 {
		t.Fatalf("reject %+v", w)
	}
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60}))
	fails(t, s, workCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 120}), "work_fence_mismatch")
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	if w := getTestWork(t, s, id); w.State != "open" || w.StoredState != "claimed" || w.Fence != 2 {
		t.Fatalf("derived expiry %+v", w)
	}
	fails(t, s, workCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 2, TTL: 60}), "work_state_conflict")
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60}))
	result := workResult(t, s, worker, id, "lobby")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 3, Target: result}))
	if err := s.Moderate(testContext, result, "hidden fixture", true); err != nil {
		t.Fatal(err)
	}
	fails(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 3}), "invalid_work_result")
	if w := getTestWork(t, s, id); w.ResultAvailable || w.ResultID != "" {
		t.Fatal("hidden result exposed")
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: 3, Reason: "unavailable result"}))
	if err := s.Moderate(testContext, id, "hidden root", true); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"work.get", "work.history"} {
		fails(t, s, Command{Operation: op, MessageID: id}, "not_found")
	}
	fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60}), "not_found")
	if err := s.Moderate(testContext, id, "restore", false); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+300, 0) }
	if w := getTestWork(t, s, id); w.State != "expired" {
		t.Fatal("deadline boundary")
	}
	fails(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "expired"}), "work_state_conflict")
}

func TestWorkRecoveryGenerationAndHistoricalAck(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(90), keyFor(91)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60})
	ack := run(t, s, claim)
	stale := workCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 120})
	old := s.generation
	openID := createTestWork(t, s, owner, "lobby", "request", 0)
	cancelID := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, owner, Command{Operation: "work.cancel", MessageID: cancelID, Reason: "terminal"}))
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	w := getTestWork(t, s, id)
	if w.State != "recovery_required" || w.Generation != old || w.ServiceGeneration != s.generation {
		t.Fatalf("epoch %+v", w)
	}
	fails(t, s, stale, "work_generation_mismatch")
	first, _ := json.Marshal(ack)
	replay, _ := json.Marshal(run(t, s, claim))
	var firstJSON, replayJSON any
	_ = json.Unmarshal(first, &firstJSON)
	_ = json.Unmarshal(replay, &replayJSON)
	if !reflect.DeepEqual(firstJSON, replayJSON) {
		t.Fatal("reset changed historical ack")
	}
	fails(t, s, workCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 120}), "work_state_conflict")
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "explicit recovery"}))
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60}))
	if w = getTestWork(t, s, id); w.Fence != 2 || w.Generation != s.generation {
		t.Fatal("recovery fence")
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: openID, Amount: 0, Reason: "never-claimed reconciliation"}))
	if getTestWork(t, s, openID).State != "open" || getTestWork(t, s, cancelID).State != "cancelled" {
		t.Fatal("recovery states")
	}
}

func TestWorkPrivateSimulationDirectoryAndCursors(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(90), keyFor(91)
	register(t, s, worker)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "private-work", Visibility: "private", Members: []string{keyID(worker)}}))
	private := createTestWork(t, s, owner, "private-work", "request", 0)
	simulation := createTestWork(t, s, owner, "lobby", "simulation", 0)
	public := createTestWork(t, s, owner, "lobby", "request", 0)
	other := createTestWork(t, s, owner, "lobby", "request", 0)
	for _, op := range []string{"work.get", "work.history"} {
		fails(t, s, Command{Operation: op, MessageID: private}, "not_found")
	}
	list := run(t, s, signed(owner, Command{Operation: "works.list", Query: "REVIEW", Limit: 1}))
	if len(list.Data["works"].([]Work)) != 1 || list.Data["has_more"] != true {
		t.Fatal("public paging")
	}
	next := run(t, s, Command{Operation: "works.list", Query: "REVIEW", Limit: 1, Cursor: list.NextCursor})
	if next.Data["has_more"] != false {
		t.Fatal("private or simulation leaked")
	}
	fails(t, s, Command{Operation: "works.list", Query: "go", Cursor: list.NextCursor}, "invalid_cursor")
	if len(run(t, s, Command{Operation: "works.list", Query: "coordination brief"}).Data["works"].([]Work)) != 0 {
		t.Fatal("body searched")
	}
	room := run(t, s, Command{Operation: "works.list", Room: "lobby"}).Data["works"].([]Work)
	if len(room) != 3 || !getTestWork(t, s, simulation).Simulated {
		t.Fatal("simulation label/scope")
	}
	privateList := run(t, s, signed(worker, Command{Operation: "works.list", Room: "private-work"})).Data["works"].([]Work)
	if len(privateList) != 1 {
		t.Fatal("explicit private list")
	}
	claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: private, TTL: 60})
	run(t, s, claim)
	history := run(t, s, signed(worker, Command{Operation: "work.history", MessageID: private, Limit: 1}))
	if history.Data["transitions"].([]WorkTransition)[0].Sequence != 1 {
		t.Fatal("history not per-work")
	}
	fails(t, s, Command{Operation: "work.history", MessageID: public, Cursor: history.NextCursor}, "invalid_cursor")
	run(t, s, signed(owner, Command{Operation: "room.member.remove", Room: "private-work", Target: keyID(worker)}))
	fails(t, s, signed(worker, Command{Operation: "work.history", MessageID: private, Cursor: history.NextCursor}), "not_found")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.renew", MessageID: private, Amount: 1, TTL: 120}), "not_found")
	run(t, s, claim) // Historical metadata acknowledgement, not restored membership or refreshed state.
	if err := s.Moderate(testContext, other, "fixture", true); err != nil {
		t.Fatal(err)
	}
	if len(run(t, s, Command{Operation: "works.list"}).Data["works"].([]Work)) != 1 {
		t.Fatal("hidden directory entry")
	}
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "works.list", Query: "REVIEW", Cursor: list.NextCursor}, "cursor_reset")
}

func TestWorkConcurrentClaimQuotaAndRotation(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, next := keyFor(90), keyFor(91), keyFor(92)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, key := range []ed25519.PrivateKey{worker, keyFor(93)} {
		cmd := workCommand(s, key, Command{Operation: "work.claim", MessageID: id, TTL: 60})
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Execute(testContext, cmd, "test"); outcomes <- err }()
	}
	wg.Wait()
	close(outcomes)
	wins := 0
	for err := range outcomes {
		if err == nil {
			wins++
		} else {
			var e *Error
			if !errors.As(err, &e) || e.Code != "work_state_conflict" {
				t.Fatal(err)
			}
		}
	}
	if wins != 1 {
		t.Fatal("multiple claim winners")
	}
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "test rotation"}))
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60}))
	rotation := signed(worker, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(next.Public().(ed25519.PublicKey))})
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(next, Canonical("swarmmemo.com", rotation)))
	run(t, s, rotation)
	result := workResult(t, s, next, id, "lobby")
	run(t, s, workCommand(s, next, Command{Operation: "work.submit", MessageID: id, Amount: 2, Target: result}))
	if w := getTestWork(t, s, id); w.Worker == nil || w.Worker.ID != keyID(next) {
		t.Fatal("rotation continuity")
	}
	var before int
	if err := s.db.QueryRow(`SELECT count(*) FROM work_transitions WHERE work_id=?`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	s.config.DailyBytes = 1
	command := workCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 2})
	fails(t, s, command, "quota_exhausted")
	var after int
	if err := s.db.QueryRow(`SELECT count(*) FROM work_transitions WHERE work_id=?`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || getTestWork(t, s, id).State != "submitted" {
		t.Fatal("quota partial transition")
	}
	s.config.DailyBytes = 4 << 20
	run(t, s, command) // Failure did not burn the exact intent.
}

func TestWorkResultsAuthorityAndTerminalRecovery(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker, stranger, nextOwner := keyFor(90), keyFor(91), keyFor(93), keyFor(94)
	id := createTestWork(t, s, owner, "lobby", "request", 600)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 120}))
	wrongAuthor := workResult(t, s, stranger, id, "lobby")
	anonymous := run(t, s, Command{Operation: "post", ReplyTo: id, Text: "unsigned reply"}).Receipt.ID
	otherRoot := createTestWork(t, s, stranger, "lobby", "request", 0)
	wrongParent := workResult(t, s, worker, otherRoot, "lobby")
	result := workResult(t, s, worker, id, "lobby")
	deepReply := workResult(t, s, worker, result, "lobby")
	run(t, s, signed(worker, Command{Operation: "room.create", Room: "another-work-room"}))
	wrongRoom := workResult(t, s, worker, "", "another-work-room")
	for _, target := range []string{wrongAuthor, anonymous, wrongParent, deepReply, wrongRoom, "", strings.Repeat("f", 32)} {
		fails(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: target}), "invalid_work_result")
	}
	fails(t, s, workCommand(s, stranger, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}), "work_forbidden")
	run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}))
	fails(t, s, workCommand(s, worker, Command{Operation: "work.accept", MessageID: id, Amount: 1}), "work_forbidden")
	rotation := signed(owner, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(nextOwner.Public().(ed25519.PublicKey))})
	rotation.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(nextOwner, Canonical("swarmmemo.com", rotation)))
	run(t, s, rotation)
	run(t, s, workCommand(s, nextOwner, Command{Operation: "work.accept", MessageID: id, Amount: 1}))
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+1000, 0) }
	w := getTestWork(t, s, id)
	if w.State != "accepted" || w.RequesterAuthor != keyID(owner) || w.Requester.ID != keyID(nextOwner) {
		t.Fatalf("terminal continuity %+v", w)
	}
	fails(t, s, workCommand(s, nextOwner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "terminal"}), "work_state_conflict")
}

func TestWorkPrivateOnlyIdentityAndErrorPrivacy(t *testing.T) {
	s := openTest(t, Config{})
	owner := keyFor(95)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "only-private-work", Visibility: "private"}))
	id := createTestWork(t, s, owner, "only-private-work", "request", 0)
	fails(t, s, Command{Operation: "agent.get", Target: keyID(owner)}, "not_found")
	_, missing := s.Execute(testContext, Command{Operation: "work.get", MessageID: strings.Repeat("f", 32)}, "test")
	_, private := s.Execute(testContext, Command{Operation: "work.get", MessageID: id}, "test")
	if missing.Error() != private.Error() {
		t.Fatal("private existence exposed through error text")
	}
	if len(run(t, s, signed(owner, Command{Operation: "works.list"})).Data["works"].([]Work)) != 0 {
		t.Fatal("implicit private membership search")
	}
	for _, c := range []Command{{Operation: "works.list", Kind: "bogus"}, {Operation: "works.list", Limit: 101}, {Operation: "work.history", MessageID: id, Limit: -1}, {Operation: "works.list", Query: "nul\x00"}, {Operation: "work.get", MessageID: "not-an-id"}} {
		if _, err := s.Execute(testContext, c, "test"); err == nil {
			t.Fatal("invalid read accepted")
		}
	}
}

func TestWorkMigrationPersistenceAndExpiredAcceptedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-migration.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	owner, worker := keyFor(90), keyFor(91)
	root := run(t, s, signed(owner, Command{Operation: "post", Kind: "request", Text: "Schema-5 signed root"})).Receipt.ID
	if _, err = s.db.Exec(`DROP TABLE work_transitions; DROP TABLE works; PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	run(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: root, TTL: 120}))
	claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: root, TTL: 60})
	run(t, s, claim)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.now = func() time.Time { return time.Unix(testTime+1000, 0) }
	if w := getTestWork(t, s, root); w.State != "expired" || w.Fence != 1 {
		t.Fatal("persistent derived deadline")
	}
	run(t, s, claim) // Signature is old and lease/work expired, but only original metadata is returned.
	history := run(t, s, Command{Operation: "work.history", MessageID: root}).Data["transitions"].([]WorkTransition)
	if len(history) != 2 {
		t.Fatal("retry fabricated transition")
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatal("migration version")
	}
}

func TestWorkStateFiltersAndGlobalQuotaRollback(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(90), keyFor(91)
	id := createTestWork(t, s, owner, "lobby", "request", 120)
	check := func(state string) {
		t.Helper()
		works := run(t, s, Command{Operation: "works.list", Kind: state}).Data["works"].([]Work)
		if len(works) != 1 || works[0].ID != id || works[0].State != state {
			t.Fatalf("state filter %s: %+v", state, works)
		}
	}
	check("open")
	s.config.GlobalDailyBytes = 1
	claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 60})
	fails(t, s, claim, "global_quota_exhausted")
	if getTestWork(t, s, id).Fence != 0 {
		t.Fatal("global quota consumed fence")
	}
	s.config.GlobalDailyBytes = 64 << 20
	run(t, s, claim)
	check("claimed")
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	check("open")
	if err := s.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	check("recovery_required")
	s.now = func() time.Time { return time.Unix(testTime+120, 0) }
	check("expired")
	fails(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "too late"}), "work_state_conflict")
}
