package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func auditPrivateCreate(t *testing.T, s *Store, owner, child ed25519.PrivateKey, room, requestID, nonce string) Command {
	t.Helper()
	var epoch string
	if err := s.db.QueryRow("SELECT private_access_epoch FROM rooms WHERE name=?", room).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "access_epoch": epoch, "disclosure": "private"})
	if err != nil {
		t.Fatal(err)
	}
	c := signed(owner, Command{Operation: "private_read.create", Room: room, Target: base64.RawURLEncoding.EncodeToString(child.Public().(ed25519.PublicKey)),
		RequestID: requestID, Nonce: nonce, Data: string(data), Timestamp: s.now().Unix()})
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, c)))
	return c
}

func auditPrivateRevoke(s *Store, owner ed25519.PrivateKey, childID string) Command {
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation})
	return signed(owner, Command{Operation: "private_read.revoke", Room: "audit-room", Target: childID,
		Data: string(data), RequestID: "audit-revoke", Nonce: "audit-revoke-nonce", Timestamp: s.now().Unix()})
}

func auditPrivateFixture(t *testing.T, config Config) (*Store, ed25519.PrivateKey, ed25519.PrivateKey) {
	t.Helper()
	s := openTest(t, config)
	owner, child := keyFor(230), keyFor(231)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "audit-room", Visibility: "private"}))
	// A later control must not accidentally update last_seen to this later time.
	s.now = func() time.Time { return time.Unix(testTime+30, 0) }
	return s, owner, child
}

func auditPrivateTables(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	result := map[string][][]any{}
	for _, table := range []string{"private_read_grants", "quota", "requests", "identities", "rooms", "members", "audit", "events", "changes", "export_changes", "counters", "meta"} {
		rows, err := s.db.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		result[table] = [][]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(values))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err = rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			result[table] = append(result[table], values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func auditSameTables(t *testing.T, before, after map[string][][]any, tables ...string) {
	t.Helper()
	if len(tables) == 0 {
		for table := range before {
			tables = append(tables, table)
		}
	}
	for _, table := range tables {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Errorf("private control changed forbidden or rolled-back table %s", table)
		}
	}
}

func TestPrivateReadAuditFixedReservationPrepaidRevokeAndNoPublicActivity(t *testing.T) {
	capacity := int64(1024 + 16384) // Exactly one room and one reader reservation.
	s, owner, child := auditPrivateFixture(t, Config{DailyBytes: capacity, GlobalDailyBytes: capacity})
	before := auditPrivateTables(t, s)
	c := auditPrivateCreate(t, s, owner, child, "audit-room", "audit-create", "audit-create-nonce")
	run(t, s, c)
	for _, actor := range []string{keyID(owner), "global"} {
		if got := sqlCount(t, s, "SELECT used FROM quota WHERE actor=? AND day=?", actor, testTime/86400); got != capacity {
			t.Fatalf("reservation actor %s: expected fixed room+16KiB cost, got %d", actor, got)
		}
	}
	after := auditPrivateTables(t, s)
	auditSameTables(t, before, after, "identities", "audit", "events", "changes", "export_changes", "counters", "meta", "rooms", "members")
	if sqlCount(t, s, "SELECT count(*) FROM identities WHERE id=?", keyID(child)) != 0 || sqlCount(t, s, "SELECT count(*) FROM quota WHERE actor=?", keyID(child)) != 0 {
		t.Fatal("private child gained ordinary identity or free quota")
	}
	run(t, s, c)
	auditSameTables(t, after, auditPrivateTables(t, s))
	for _, operation := range []string{"room.get", "messages.list"} {
		read := signed(child, Command{Operation: operation, Room: "audit-room", Timestamp: s.now().Unix(),
			PrivateRead: &PrivateReadContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}})
		run(t, s, read)
	}
	auditSameTables(t, after, auditPrivateTables(t, s)) // Reads perform no durable debit/cache/audit.
	for _, operation := range []string{"private_read.get", "private_read.list"} {
		control := Command{Operation: operation, Room: "audit-room", Timestamp: s.now().Unix()}
		if operation == "private_read.get" {
			control.Target = keyID(child)
		}
		run(t, s, signed(owner, control))
	}
	auditSameTables(t, after, auditPrivateTables(t, s))
	failedEnrollment := auditPrivateCreate(t, s, owner, keyFor(232), "audit-room", "no-quota", "no-quota-nonce")
	if _, err := s.Execute(testContext, failedEnrollment, "audit-source"); err == nil {
		t.Fatal("new enrollment ignored exhausted owner/service capacity")
	}
	auditSameTables(t, after, auditPrivateTables(t, s))
	revoke := auditPrivateRevoke(s, owner, keyID(child))
	run(t, s, revoke)
	revoked := auditPrivateTables(t, s)
	auditSameTables(t, after, revoked, "quota", "identities", "audit", "events", "changes", "export_changes", "counters", "meta", "rooms", "members")
	run(t, s, revoke)
	auditSameTables(t, revoked, auditPrivateTables(t, s))
	fresh := revoke
	fresh.RequestID, fresh.Nonce = "another-revoke", "another-revoke-nonce"
	fresh = signed(owner, fresh)
	fails(t, s, fresh, "private_read_already_revoked")
	auditSameTables(t, revoked, auditPrivateTables(t, s))
	if got := sqlCount(t, s, "SELECT count(*) FROM requests WHERE actor=?", "private-control:"+keyID(owner)); got != 4 {
		t.Fatalf("expected exactly four control receipt keys, got %d", got)
	}
	if sqlCount(t, s, "SELECT max(length(CAST(result AS BLOB))) FROM requests WHERE actor=?", "private-control:"+keyID(owner)) > 1024 {
		t.Fatal("private acknowledgment exceeded reservation bound")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateReadAuditFailureAfterChargeRollsBackGrantQuotaAndCache(t *testing.T) {
	for _, stage := range []string{"grant_insert", "second_receipt_insert"} {
		t.Run(stage, func(t *testing.T) {
			s, owner, child := auditPrivateFixture(t, Config{})
			c := auditPrivateCreate(t, s, owner, child, "audit-room", "audit-atomic", "audit-atomic-nonce")
			ownerUsed := sqlCount(t, s, "SELECT used FROM quota WHERE actor=? AND day=?", keyID(owner), testTime/86400)
			globalUsed := sqlCount(t, s, "SELECT used FROM quota WHERE actor='global' AND day=?", testTime/86400)
			before := auditPrivateTables(t, s)
			condition := fmt.Sprintf(`(SELECT used FROM quota WHERE actor='%s' AND day=%d)=%d AND (SELECT used FROM quota WHERE actor='global' AND day=%d)=%d`, keyID(owner), testTime/86400, ownerUsed+16384, testTime/86400, globalUsed+16384)
			trigger := `CREATE TRIGGER audit_private_abort BEFORE INSERT ON private_read_grants WHEN ` + condition + ` BEGIN SELECT RAISE(ABORT,'audit_private_after_charge'); END`
			if stage == "second_receipt_insert" {
				trigger = `CREATE TRIGGER audit_private_abort BEFORE INSERT ON requests WHEN NEW.request_key='nonce:audit-atomic-nonce' AND ` + condition + fmt.Sprintf(` AND EXISTS(SELECT 1 FROM private_read_grants WHERE child_id='%s') AND EXISTS(SELECT 1 FROM requests WHERE actor='private-control:%s' AND request_key='id:audit-atomic') BEGIN SELECT RAISE(ABORT,'audit_private_after_charge'); END`, keyID(child), keyID(owner))
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Execute(testContext, c, "audit-source"); err == nil || !strings.Contains(err.Error(), "audit_private_after_charge") {
				t.Fatalf("failure injection did not reach the charged transaction: %v", err)
			}
			auditSameTables(t, before, auditPrivateTables(t, s))
			if _, err := s.db.Exec("DROP TRIGGER audit_private_abort"); err != nil {
				t.Fatal(err)
			}
			run(t, s, c)
			accepted := auditPrivateTables(t, s)
			run(t, s, c)
			auditSameTables(t, accepted, auditPrivateTables(t, s))
			if err := s.Integrity(testContext); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrivateReadAuditRevocationCacheFailurePreservesPrepaidTransition(t *testing.T) {
	s, owner, child := auditPrivateFixture(t, Config{DailyBytes: 1024 + 16384, GlobalDailyBytes: 1024 + 16384})
	run(t, s, auditPrivateCreate(t, s, owner, child, "audit-room", "revoke-atomic-create", "revoke-atomic-create-nonce"))
	revoke := auditPrivateRevoke(s, owner, keyID(child))
	before := auditPrivateTables(t, s)
	trigger := fmt.Sprintf(`CREATE TRIGGER audit_private_revoke_abort BEFORE INSERT ON requests
WHEN NEW.request_key='nonce:audit-revoke-nonce'
AND (SELECT revoked_at FROM private_read_grants WHERE child_id='%s')=%d
AND EXISTS(SELECT 1 FROM requests WHERE actor='private-control:%s' AND request_key='id:audit-revoke')
BEGIN SELECT RAISE(ABORT,'audit_private_after_revoke'); END`, keyID(child), s.now().Unix(), keyID(owner))
	if _, err := s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(testContext, revoke, "audit-source"); err == nil || !strings.Contains(err.Error(), "audit_private_after_revoke") {
		t.Fatalf("revoke injection did not follow grant update and first receipt: %v", err)
	}
	auditSameTables(t, before, auditPrivateTables(t, s))
	if _, err := s.db.Exec("DROP TRIGGER audit_private_revoke_abort"); err != nil {
		t.Fatal(err)
	}
	run(t, s, revoke)
	if sqlCount(t, s, "SELECT revoked_at FROM private_read_grants WHERE child_id=?", keyID(child)) != s.now().Unix() {
		t.Fatal("failed cache commit consumed the prepaid revoke")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateReadAuditOwnerRateIsSharedAcrossRooms(t *testing.T) {
	s, owner, first := auditPrivateFixture(t, Config{})
	second, third := keyFor(232), keyFor(233)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "other-audit-room", Visibility: "private", Timestamp: s.now().Unix()}))
	for index, entry := range []struct {
		key  ed25519.PrivateKey
		room string
	}{{first, "audit-room"}, {second, "audit-room"}, {third, "other-audit-room"}} {
		run(t, s, auditPrivateCreate(t, s, owner, entry.key, entry.room, fmt.Sprintf("owner-rate-%d", index), fmt.Sprintf("owner-rate-nonce-%d", index)))
	}
	for _, child := range []ed25519.PrivateKey{first, second} {
		for index := 0; index < 10; index++ {
			run(t, s, signed(child, Command{Operation: "room.get", Room: "audit-room", Timestamp: s.now().Unix(),
				PrivateRead: &PrivateReadContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}}))
		}
	}
	fails(t, s, signed(third, Command{Operation: "room.get", Room: "other-audit-room", Timestamp: s.now().Unix(),
		PrivateRead: &PrivateReadContext{Schema: 1, GrantID: keyID(third), Generation: s.generation}}), "private_read_rate_limited")
}

func TestPrivateReadAuditEmptyPageHasStableResumeCursor(t *testing.T) {
	s, owner, child := auditPrivateFixture(t, Config{})
	run(t, s, auditPrivateCreate(t, s, owner, child, "audit-room", "empty-page", "empty-page-nonce"))
	read := func(cursor string) Result {
		return run(t, s, signed(child, Command{Operation: "messages.list", Room: "audit-room", Cursor: cursor, Timestamp: s.now().Unix(),
			PrivateRead: &PrivateReadContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}}))
	}
	for _, initial := range []string{"", "start"} {
		first := read(initial)
		if len(first.Messages) != 0 || first.NextCursor == "" || first.NextCursor == "start" || first.Generation != s.generation {
			t.Fatal("initial empty private page did not supply a generation-bound resume cursor")
		}
		idle := read(first.NextCursor)
		if len(idle.Messages) != 0 || idle.NextCursor != first.NextCursor {
			t.Fatal("idle private page changed or discarded its resume cursor")
		}
	}
}

// These explicitly synthetic historical rows exercise admission-count pressure,
// not enrollment-signature validation or a claim that thousands of keys enrolled.
// Expired rows remain key-classified and count toward permanent storage limits.
func auditSeedPrivateHistory(t *testing.T, s *Store, owner ed25519.PrivateKey, room string, start, count int) {
	t.Helper()
	query := `WITH RECURSIVE n(v) AS (VALUES(0) UNION ALL SELECT v+1 FROM n WHERE v+1<?)
INSERT INTO private_read_grants(child_id,public_key,owner_account,issuer_id,issuer_key,room,generation,access_epoch,created_at,expires_at,payload,signature,proof)
SELECT printf('%064x',v+?), 'synthetic-history-key-'||(v+?), ?,?,?,?, ?,
(SELECT private_access_epoch FROM rooms WHERE name=?), ?,?, '{}','','' FROM n`
	if _, err := s.db.Exec(query, count, start, start, keyID(owner), keyID(owner), base64.RawURLEncoding.EncodeToString(owner.Public().(ed25519.PublicKey)), room,
		s.generation, room, testTime-100, testTime-1); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateReadAuditHistoricalAdmissionDoesNotBlockPrepaidRevoke(t *testing.T) {
	for _, mode := range []string{"owner_history_across_rooms", "room_history", "global_history"} {
		t.Run(mode, func(t *testing.T) {
			s, owner, child := auditPrivateFixture(t, Config{})
			c := auditPrivateCreate(t, s, owner, child, "audit-room", "history-original", "history-original-nonce")
			run(t, s, c)
			switch mode {
			case "owner_history_across_rooms":
				run(t, s, signed(owner, Command{Operation: "room.create", Room: "history-room", Visibility: "private", Timestamp: s.now().Unix()}))
				auditSeedPrivateHistory(t, s, owner, "history-room", 1, 4095)
			case "room_history":
				auditSeedPrivateHistory(t, s, owner, "audit-room", 1, 4095)
			case "global_history":
				for index := 0; index < 8; index++ {
					historicalOwner, room := keyFor(byte(200+index)), fmt.Sprintf("history-%d", index)
					run(t, s, signed(historicalOwner, Command{Operation: "room.create", Room: room, Visibility: "private", Timestamp: s.now().Unix()}))
					count := 4096
					if index == 7 {
						count--
					}
					auditSeedPrivateHistory(t, s, historicalOwner, room, index*4096+1, count)
				}
				if sqlCount(t, s, "SELECT count(*) FROM private_read_grants WHERE owner_account=?", keyID(owner)) != 1 {
					t.Fatal("global fixture accidentally hit owner limit")
				}
			}
			before := auditPrivateTables(t, s)
			fails(t, s, auditPrivateCreate(t, s, owner, keyFor(232), "audit-room", "over-history", "over-history-nonce"), "private_read_limit")
			auditSameTables(t, before, auditPrivateTables(t, s))
			run(t, s, auditPrivateRevoke(s, owner, keyID(child)))
			if got := sqlCount(t, s, "SELECT count(*) FROM private_read_grants"); got != int64(len(before["private_read_grants"])) {
				t.Fatal("revocation discarded history to make capacity")
			}
			if sqlCount(t, s, "SELECT revoked_at FROM private_read_grants WHERE child_id=?", keyID(child)) == 0 {
				t.Fatal("historical cap prevented prepaid revocation")
			}
			if err := s.Integrity(testContext); err != nil {
				t.Fatal(err)
			}
		})
	}
}
