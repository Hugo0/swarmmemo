//go:build linux

package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"syscall"
	"testing"
	"time"
)

// Opt-in read-only profiling AFTER synthetic fixture creation. No ANALYZE,
// optimizer hints, changed indexes, network, or concurrent backup. Normal tests
// skip. The original mixed workload source and measurements are unchanged.
// SWARMMEMO_WORK_QUERY_PROFILE=1 go test ./internal/board -run '^TestWorkQueryProfileLocal$' -count=1 -timeout=15m -v

// Mirror the exact readWork SELECT, WHERE, scope, cursor and argument order.
// Shared workColumns/workEffectiveSQL remain the runtime's constants. This copy
// is diagnostic only, never an alternative application read implementation.
func workProfileQuery(t *testing.T, s *Store, c Command) (string, []any) {
	t.Helper()
	scope, _ := json.Marshal([]string{c.Room, c.Kind, c.Query})
	cursor, err := s.decodeConversationCursor(c.Cursor, "works.list", string(scope))
	if err != nil {
		t.Fatal("profile_cursor", mixedCode(err))
	}
	where := `e.hidden=0 AND w.id>?`
	args := []any{cursor.Page}
	if c.Room != "" {
		where += ` AND e.room=?`
		args = append(args, c.Room)
	} else {
		where += ` AND r.visibility='public' AND e.kind<>'simulation'`
	}
	if c.Kind != "" {
		where += ` AND (` + workEffectiveSQL + `)=?`
		args = append(args, testTime, s.generation, testTime, c.Kind)
	}
	if c.Query != "" {
		where += ` AND (instr(lower(w.title),lower(?))>0 OR EXISTS(SELECT 1 FROM json_each(w.capabilities) cap WHERE cap.value=lower(?)))`
		args = append(args, c.Query, c.Query)
	}
	args = append(args, c.Limit+1)
	return `SELECT ` + workColumns + ` FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room WHERE ` + where + ` ORDER BY w.id LIMIT ?`, args
}

type workProfilePlan struct {
	ID, Parent int
	Detail     string
}

func workProfileExplain(t *testing.T, ctx context.Context, s *Store, query string, args []any) []workProfilePlan {
	t.Helper()
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(call, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal("profile_explain", mixedCode(err))
	}
	defer rows.Close()
	plan := []workProfilePlan{}
	for rows.Next() {
		var row workProfilePlan
		var unused int
		if err = rows.Scan(&row.ID, &row.Parent, &unused, &row.Detail); err != nil || len(row.Detail) > 2048 || len(plan) >= 64 {
			t.Fatal("profile_plan_bound")
		}
		plan = append(plan, row)
	}
	if rows.Err() != nil || len(plan) == 0 {
		t.Fatal("profile_plan_read")
	}
	return plan
}

func workProfileDuration(sample map[string]float64, name string, began time.Time) {
	sample[name] += float64(time.Since(began)) / float64(time.Millisecond)
}

func workProfileSample(t *testing.T, phase context.Context, s *Store, f mixedFixture, c Command, query string, args []any) (map[string]float64, map[string]int) {
	t.Helper()
	sample := map[string]float64{}
	counts := map[string]int{}
	began := time.Now()
	r, err := mixedExecute(phase, s, c)
	workProfileDuration(sample, "full_execute", began)
	if err != nil {
		t.Fatal("profile_full_execute", mixedCode(err))
	}
	if _, err = mixedReadCheck(f, c, r, 0); err != nil {
		t.Fatal("profile_full_projection")
	}
	expected := r.Data["works"].([]Work)
	ctx, cancel := context.WithTimeout(phase, 5*time.Second)
	defer cancel()
	began = time.Now()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	workProfileDuration(sample, "diagnostic_begin_tx", began)
	if err != nil {
		t.Fatal("profile_begin", mixedCode(err))
	}
	defer tx.Rollback()
	began = time.Now()
	_, err = roomAccess(ctx, tx, c.Room, actor{})
	workProfileDuration(sample, "initial_room_access", began)
	if err != nil {
		t.Fatal("profile_room_scope", mixedCode(err))
	}
	began = time.Now()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatal("profile_select", mixedCode(err))
	}
	stored := []workRow{}
	for rows.Next() {
		w, scanErr := scanWork(rows)
		if scanErr != nil || len(stored) >= c.Limit+1 {
			rows.Close()
			t.Fatal("profile_scan_bound", mixedCode(scanErr))
		}
		stored = append(stored, w)
	}
	err = rows.Err()
	rows.Close()
	workProfileDuration(sample, "main_select_and_scan", began)
	if err != nil {
		t.Fatal("profile_scan", mixedCode(err))
	}
	counts["selected_including_lookahead"] = len(stored)
	more := len(stored) > c.Limit
	if more {
		stored = stored[:c.Limit]
	}
	if len(stored) != len(expected) || r.Data["has_more"] != more {
		t.Fatal("profile_main_select_differs_from_runtime")
	}
	counts["projected"] = len(stored)
	for index, w := range stored {
		began = time.Now()
		root, e := visibleWorkRoot(ctx, tx, w.ID, actor{})
		workProfileDuration(sample, "visible_work_root_total", began)
		if e != nil {
			t.Fatal("profile_visible_root", mixedCode(e))
		}
		began = time.Now()
		projected, e := s.projectWork(ctx, tx, w, root, s.generation, testTime)
		workProfileDuration(sample, "project_work_total", began)
		if e != nil || !reflect.DeepEqual(projected, expected[index]) {
			t.Fatal("profile_projection_differs_from_runtime", mixedCode(e))
		}
		// Separate diagnostic replays: these are NOT additive constituents of
		// project_work_total (different timing window/cache state, same tx/rows).
		began = time.Now()
		state, e := effectiveWork(ctx, tx, w.ID, s.generation, testTime)
		workProfileDuration(sample, "replay_effective_state_total", began)
		if e != nil || state != projected.State {
			t.Fatal("profile_effective_state", mixedCode(e))
		}
		began = time.Now()
		requester, e := currentWorkIdentity(ctx, tx, w.Requester)
		workProfileDuration(sample, "replay_requester_agent_total", began)
		if e != nil || !reflect.DeepEqual(requester, projected.Requester) {
			t.Fatal("profile_requester_agent", mixedCode(e))
		}
		if w.Worker != "" {
			counts["worker_agent_reads"]++
			began = time.Now()
			worker, e := currentWorkIdentity(ctx, tx, w.Worker)
			workProfileDuration(sample, "replay_worker_agent_total", began)
			if e != nil || projected.Worker == nil || !reflect.DeepEqual(worker, *projected.Worker) {
				t.Fatal("profile_worker_agent", mixedCode(e))
			}
		}
		if w.Result != "" {
			counts["result_eligibility_reads"]++
			began = time.Now()
			eligible, e := eligibleWorkResult(ctx, tx, w.Result, w, root)
			workProfileDuration(sample, "replay_result_eligibility_total", began)
			if e != nil || eligible != projected.ResultAvailable {
				t.Fatal("profile_result_eligibility", mixedCode(e))
			}
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("profile_read_transaction_close", mixedCode(err))
	}
	return sample, counts
}

func TestWorkQueryProfileLocal(t *testing.T) {
	if os.Getenv("SWARMMEMO_WORK_QUERY_PROFILE") != "1" {
		t.Skip("explicit synthetic local query-profile opt-in required")
	}
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("profile_temp_permissions")
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) != nil || disk.Bavail*uint64(disk.Bsize) < 2<<30 {
		t.Fatal("profile_requires_two_gib_free")
	}
	t.Logf("profile go=%s arch=%s gomaxprocs=%d samples_per_variant=10 schema=8 no_analyze=true", runtime.Version(), runtime.GOARCH, runtime.GOMAXPROCS(0))
	for _, retained := range []int{1000, 10000} {
		if !t.Run(fmt.Sprintf("retained_%d", retained), func(t *testing.T) {
			dir, err := os.MkdirTemp(root, "profile-")
			if err != nil {
				t.Fatal("profile_directory")
			}
			defer os.RemoveAll(dir)
			s := mixedOpen(t, filepath.Join(dir, "profile.sqlite"))
			defer func() {
				if err := s.Close(); err != nil {
					t.Error("profile_close", mixedCode(err))
				}
			}()
			f := mixedSeed(t, root, s, retained)
			before := mixedQuota(t, s, f)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var sqliteVersion string
			if err := s.db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVersion); err != nil {
				t.Fatal("profile_sqlite_version", mixedCode(err))
			}
			t.Log("sqlite_version", sqliteVersion)
			base := Command{Operation: "works.list", Room: mixedRoom, Limit: 25}
			filtered := base
			filtered.Kind, filtered.Query = "open", "review"
			cursorCommand := func(c Command) Command {
				first := c
				first.Limit = 10
				r, err := mixedExecute(ctx, s, first)
				if err != nil || r.NextCursor == "" {
					t.Fatal("profile_obtain_runtime_cursor", mixedCode(err))
				}
				c.Cursor = r.NextCursor
				return c
			}
			variants := []struct {
				name    string
				command Command
			}{
				{"room_first", base}, {"room_filtered_first", filtered},
				{"room_cursor", cursorCommand(base)}, {"room_filtered_cursor", cursorCommand(filtered)},
			}
			for _, variant := range variants {
				query, args := workProfileQuery(t, s, variant.command)
				plan := workProfileExplain(t, ctx, s, query, args)
				values := map[string][]float64{}
				var counts map[string]int
				stats := s.db.Stats()
				for i := 0; i < 10; i++ {
					size, err := mixedDisk(root)
					if err != nil || size >= 512<<20 || ctx.Err() != nil {
						t.Fatal("profile_storage_or_time_bound")
					}
					sample, actualCounts := workProfileSample(t, ctx, s, f, variant.command, query, args)
					if counts != nil && !reflect.DeepEqual(counts, actualCounts) {
						t.Fatal("profile_static_page_changed")
					}
					counts = actualCounts
					for k, v := range sample {
						values[k] = append(values[k], v)
					}
				}
				metrics := map[string]any{}
				for k, v := range values {
					sort.Float64s(v)
					metrics[k] = map[string]any{"n": len(v), "median_ms": (v[4] + v[5]) / 2, "min_ms": v[0], "max_ms": v[9]}
				}
				lastStats := s.db.Stats()
				raw, _ := json.Marshal(map[string]any{"variant": variant.name, "plan": plan, "page_counts": counts, "metrics": metrics, "pool_wait_count": lastStats.WaitCount - stats.WaitCount, "pool_wait_ms": float64(lastStats.WaitDuration-stats.WaitDuration) / float64(time.Millisecond)})
				t.Log("query_profile", string(raw))
			}
			if mixedQuota(t, s, f) != before {
				t.Fatal("read_profile_mutated_quota_or_rows")
			}
			mixedIsolation(t, s, f)
			if err := s.Integrity(ctx); err != nil {
				t.Fatal("profile_integrity", mixedCode(err))
			}
		}) {
			break
		}
	}
}
