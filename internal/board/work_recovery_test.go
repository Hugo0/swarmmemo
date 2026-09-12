package board

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWorkOlderSnapshotRequiresEpochReconciliation(t *testing.T) {
	original := openTest(t, Config{})
	owner, worker := keyFor(100), keyFor(101)
	id := createTestWork(t, original, owner, "lobby", "request", 3600)
	oldGeneration := original.generation
	backup := filepath.Join(t.TempDir(), "before-claim.sqlite")
	if err := original.Backup(testContext, backup); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("online backup must remain owner-only", err)
	}

	// These commands really commit after the snapshot. A retained response is
	// evidence of their acceptance, not evidence that an older backup contains them.
	lostClaim := workCommand(original, worker, Command{Operation: "work.claim", MessageID: id, TTL: 120, RequestID: "accepted-after-backup"})
	lostAck := run(t, original, lostClaim).Data["ack"].(WorkAck)
	lostResult := workResult(t, original, worker, id, "lobby")
	lostSubmit := workCommand(original, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: lostResult})
	run(t, original, lostSubmit)
	neverAcceptedRenew := workCommand(original, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 180})
	if work := getTestWork(t, original, id); work.State != "submitted" || work.Fence != 1 || work.ResultID != lostResult {
		t.Fatal("post-snapshot history was not actually committed")
	}

	restored, err := Open(backup, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	restored.now = func() time.Time { return time.Unix(testTime, 0) }
	if err = restored.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
	// Inspect the actual restored file without admitting traffic before recovery.
	var state string
	var storedFence, resultCount, claimCacheCount int64
	if err = restored.db.QueryRow("SELECT state,fence FROM works WHERE id=?", id).Scan(&state, &storedFence); err != nil {
		t.Fatal(err)
	}
	if err = restored.db.QueryRow("SELECT count(*) FROM events WHERE id=?", lostResult).Scan(&resultCount); err != nil {
		t.Fatal(err)
	}
	if err = restored.db.QueryRow("SELECT count(*) FROM requests WHERE actor=? AND request_key=?", keyID(worker), "id:"+lostClaim.RequestID).Scan(&claimCacheCount); err != nil {
		t.Fatal(err)
	}
	if state != "open" || storedFence != 0 || resultCount != 0 || claimCacheCount != 0 {
		t.Fatal("fixture is not an older snapshot missing the accepted attempt")
	}
	if err = restored.RotateGeneration(testContext); err != nil {
		t.Fatal(err)
	}
	if restored.generation == oldGeneration {
		t.Fatal("recovery did not create a new epoch")
	}
	work := getTestWork(t, restored, id)
	if work.State != "recovery_required" || work.StoredState != "open" || work.Generation != oldGeneration || work.ServiceGeneration != restored.generation || work.Fence != 0 || work.ResultAvailable {
		t.Fatalf("restored work misleadingly resumed: %+v", work)
	}
	history := run(t, restored, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if len(history) != 1 || history[0].Operation != "work.create" || history[0].Generation != oldGeneration {
		t.Fatal("restore invented or retained post-backup transition history")
	}
	fails(t, restored, Command{Operation: "message.get", MessageID: lostResult}, "not_found")
	// Unlike an exact retained accepted retry, the missing cache entry cannot
	// acknowledge this lost accepted claim. It must not silently execute it again.
	fails(t, restored, lostClaim, "work_generation_mismatch")
	fails(t, restored, lostSubmit, "work_generation_mismatch")
	fails(t, restored, workCommand(restored, worker, Command{Operation: "work.claim", MessageID: id, TTL: 120}), "work_state_conflict")
	fails(t, restored, workCommand(restored, owner, Command{Operation: "work.reject", MessageID: id, Amount: 1, Reason: "wrong restored fence"}), "work_fence_mismatch")
	run(t, restored, workCommand(restored, owner, Command{Operation: "work.reject", MessageID: id, Amount: 0, Reason: "Explicitly reconcile older backup"}))
	if work = getTestWork(t, restored, id); work.State != "open" || work.Generation != restored.generation || work.Worker != nil {
		t.Fatal("requester reconciliation did not explicitly clear the old epoch")
	}
	newAck := run(t, restored, workCommand(restored, worker, Command{Operation: "work.claim", MessageID: id, TTL: 120})).Data["ack"].(WorkAck)
	if newAck.Fence != lostAck.Fence || newAck.WorkID != lostAck.WorkID || newAck.ServiceID != lostAck.ServiceID || newAck.Generation == lostAck.Generation {
		t.Fatal("fixture must reuse the integer but distinguish the full external fencing tuple")
	}
	// Even with the same worker/account, same current integer, fresh timestamp,
	// and a valid renew TTL, an old-epoch command must fail its signed generation.
	fails(t, restored, neverAcceptedRenew, "work_generation_mismatch")
	history = run(t, restored, Command{Operation: "work.history", MessageID: id}).Data["transitions"].([]WorkTransition)
	if len(history) != 3 || history[1].Operation != "work.reject" || history[2].Operation != "work.claim" || history[2].Generation != newAck.Generation {
		t.Fatal("recovered history does not distinguish reconciliation and fresh claim")
	}
	if err = restored.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

// Capture all relevant tables, not only counts: an aborted transaction must
// preserve existing quotas, receipt bytes, signatures and current work fields.
func snapshotWorkAtomicTables(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	result := map[string][][]any{}
	for _, table := range []string{"works", "work_transitions", "quota", "requests", "identities"} {
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
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
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

func TestWorkSQLFailureAfterChargeRollsBackEveryTable(t *testing.T) {
	for _, stage := range []string{"transition_insert", "second_request_insert"} {
		t.Run(stage, func(t *testing.T) {
			s := openTest(t, Config{})
			owner, worker := keyFor(102), keyFor(103)
			id := createTestWork(t, s, owner, "lobby", "request", 3600)
			claim := workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 120, RequestID: "atomic-claim", Nonce: "atomic-claim-nonce"})
			cost := int64(len(Canonical("swarmmemo.com", claim))) + 512
			var globalBefore int64
			if err := s.db.QueryRow("SELECT used FROM quota WHERE actor='global' AND day=?", testTime/86400).Scan(&globalBefore); err != nil {
				t.Fatal(err)
			}
			before := snapshotWorkAtomicTables(t, s)
			// Both triggers assert that quota charging and the work update already
			// happened in this transaction. The second additionally proves that
			// history and the first idempotency key were inserted before failure.
			common := fmt.Sprintf(`(SELECT fence FROM works WHERE id='%s')=1
 AND (SELECT used FROM quota WHERE actor='%s' AND day=%d)=%d
 AND (SELECT used FROM quota WHERE actor='global' AND day=%d)=%d`, id, keyID(worker), testTime/86400, cost, testTime/86400, globalBefore+cost)
			trigger := `CREATE TRIGGER fail_work_after_charge BEFORE INSERT ON work_transitions WHEN ` + common + ` BEGIN SELECT RAISE(ABORT,'fixture_after_work_charge'); END`
			if stage == "second_request_insert" {
				trigger = `CREATE TRIGGER fail_work_after_charge BEFORE INSERT ON requests WHEN NEW.request_key='nonce:atomic-claim-nonce' AND ` + common + fmt.Sprintf(`
 AND (SELECT count(*) FROM work_transitions WHERE work_id='%s')=2
 AND EXISTS(SELECT 1 FROM requests WHERE actor='%s' AND request_key='id:atomic-claim')
 BEGIN SELECT RAISE(ABORT,'fixture_after_work_charge'); END`, id, keyID(worker))
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Execute(testContext, claim, "test-origin"); err == nil || !strings.Contains(err.Error(), "fixture_after_work_charge") {
				t.Fatalf("injection was not reached after the expected writes: %v", err)
			}
			if after := snapshotWorkAtomicTables(t, s); !reflect.DeepEqual(before, after) {
				for table := range before {
					if !reflect.DeepEqual(before[table], after[table]) {
						t.Errorf("aborted work mutation changed %s", table)
					}
				}
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_work_after_charge"); err != nil {
				t.Fatal(err)
			}
			run(t, s, claim) // The failed attempt did not burn either dedup key.
			accepted := snapshotWorkAtomicTables(t, s)
			run(t, s, claim)
			if !reflect.DeepEqual(accepted, snapshotWorkAtomicTables(t, s)) {
				t.Fatal("accepted exact retry spent or mutated another row")
			}
			if work := getTestWork(t, s, id); work.State != "claimed" || work.Fence != 1 {
				t.Fatal("successful retry did not allocate exactly one fence")
			}
			if err := s.Integrity(testContext); err != nil {
				t.Fatal(err)
			}
		})
	}
}
