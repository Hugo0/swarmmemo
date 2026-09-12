package board

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPrivateReadActiveAdmissionAndInvalidation(t *testing.T) {
	s, parent, _, _ := privateReadFixture(t)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "second-reader-room", Visibility: "private"}))
	for i := 0; i < 7; i++ {
		room := "reader-room"
		if i > 2 {
			room = "second-reader-room"
		}
		privateReadEnroll(t, s, parent, keyFor(byte(201+i)), room)
	}
	fails(t, s, privateReadEnrollCommand(s, parent, keyFor(208), "second-reader-room"), "private_read_limit")
	// Expired records remain retained but no longer consume active admission.
	s.now = func() time.Time { return time.Unix(testTime+86401, 0) }
	privateReadEnroll(t, s, parent, keyFor(208), "second-reader-room")
	if sqlCount(t, s, "SELECT count(*) FROM private_read_grants") != 9 {
		t.Fatal("inactive history pruned")
	}
}

func TestPrivateReadGlobalActiveAdmission(t *testing.T) {
	s, _, _, _ := privateReadFixture(t)
	// Synthetic active rows isolate the global boundary without hundreds of
	// unrelated owner/control writes; every joined issuer/room epoch is current.
	_, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<255)
 INSERT INTO private_read_grants(child_id,public_key,owner_account,issuer_id,issuer_key,room,generation,access_epoch,created_at,expires_at,payload,signature,proof)
 SELECT printf('%064x',x),'synthetic-'||x,g.owner_account,g.issuer_id,g.issuer_key,g.room,g.generation,g.access_epoch,g.created_at,g.expires_at,g.payload,g.signature,g.proof FROM n CROSS JOIN (SELECT * FROM private_read_grants LIMIT 1) g`)
	if err != nil {
		t.Fatal(err)
	}
	next := keyFor(210)
	run(t, s, signed(next, Command{Operation: "room.create", Room: "new-owner-room", Visibility: "private"}))
	fails(t, s, privateReadEnrollCommand(s, next, keyFor(211), "new-owner-room"), "private_read_limit")
	if sqlCount(t, s, "SELECT count(*) FROM private_read_grants WHERE owner_account=?", keyID(next)) != 0 {
		t.Fatal("partial global admission")
	}
	// Inactivation releases active capacity without deleting any classification.
	if _, err = s.db.Exec("UPDATE private_read_grants SET revoked_at=? WHERE child_id=(SELECT child_id FROM private_read_grants LIMIT 1)", testTime); err != nil {
		t.Fatal(err)
	}
	privateReadEnroll(t, s, next, keyFor(211), "new-owner-room")
}

func TestPrivateReadBoundedRateMapAndIdleEviction(t *testing.T) {
	s, _, child, grant := privateReadFixture(t)
	for i := 0; i < 1024; i++ {
		s.privateRates[fmt.Sprintf("synthetic:%d", i)] = privateReadBucket{tokens: 0, updated: s.now(), seen: s.now()}
	}
	command := func() Command {
		return privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"})
	}
	fails(t, s, command(), "private_read_rate_limited")
	if len(s.privateRates) != 1024 || !s.privateServiceRate.updated.IsZero() {
		t.Fatal("rejected map admission mutated rate authority")
	}
	s.now = func() time.Time { return time.Unix(testTime+59, 0) }
	fails(t, s, command(), "private_read_rate_limited")
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	run(t, s, command())
	if len(s.privateRates) != 3 {
		t.Fatal("idle map eviction did not leave exactly scoped buckets")
	}
}

func TestPrivateReadDeniedActivityIsNotIdle(t *testing.T) {
	s, _, child, grant := privateReadFixture(t)
	command := func() Command {
		return privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"})
	}
	run(t, s, command())
	s.now = func() time.Time { return time.Unix(testTime+61, 0) }
	s.privateServiceRate = privateReadBucket{tokens: 0, updated: s.now(), seen: s.now()}
	previous := s.privateRates["child:"+keyID(child)]
	fails(t, s, command(), "private_read_rate_limited")
	current := s.privateRates["child:"+keyID(child)]
	if current.seen != s.now() || current.tokens != previous.tokens || current.updated != previous.updated {
		t.Fatal("denied activity changed debit or retained stale idle clock")
	}
}

func TestPrivateReadRealConcurrentAdmission(t *testing.T) {
	s, _, child, grant := privateReadFixture(t)
	ctx, cancel := context.WithTimeout(testContext, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	errorsFound := make(chan error, 2)
	for i := 0; i < 2; i++ {
		command := privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"})
		go func() { _, err := s.Execute(ctx, command, "test"); errorsFound <- err }()
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(s.privateSlots) < 2 {
		select {
		case <-ctx.Done():
			t.Fatal("read slots not acquired")
		case <-ticker.C:
		}
	}
	fails(t, s, privateReadCommand(s, child, grant, Command{Operation: "room.get", Room: "reader-room"}), "private_read_rate_limited")
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = <-errorsFound; err != nil {
			t.Fatal(err)
		}
	}
	if len(s.privateSlots) != 0 {
		t.Fatal("read slot leaked")
	}
}

func TestPrivateReadOversizeEventHasNoPartialResult(t *testing.T) {
	s, parent, child, grant := privateReadFixture(t)
	id := run(t, s, signed(parent, Command{Operation: "post", Room: "reader-room", Text: "ordinary valid fixture"})).Receipt.ID
	// Defensive database fixture exercises a single-row response cap independently
	// of the ordinary writer's stricter envelope limit.
	if _, err := s.db.Exec("UPDATE events SET reason=printf('%300000s','x') WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	result, err := s.Execute(testContext, privateReadCommand(s, child, grant, Command{Operation: "message.get", Room: "reader-room", MessageID: id}), "test")
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "private_read_response_limit" || result.OK || len(result.Messages) != 0 {
		t.Fatal("partial oversized event returned", err)
	}
}
