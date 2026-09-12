//go:build linux

package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Diagnostic SQL only: no application runtime/index/schema/ANALYZE change.
// SWARMMEMO_WORK_QUERY_CANDIDATE=1 go test ./internal/board -run '^TestWorkQueryCandidateLocal$' -count=1 -timeout=15m -v
func workCandidateSQL(t *testing.T, s *Store, c Command, shape string) (string, []any) {
	t.Helper()
	original, args := workProfileQuery(t, s, c)
	if shape == "original" {
		return original, args
	}
	if shape == "cross" {
		return strings.Replace(original, "FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room", "FROM works w CROSS JOIN events e ON e.id=w.id CROSS JOIN rooms r ON r.name=e.room", 1), args
	}
	if shape != "exists" {
		t.Fatal("unknown_diagnostic_shape")
	}
	scope, _ := json.Marshal([]string{c.Room, c.Kind, c.Query})
	cursor, err := s.decodeConversationCursor(c.Cursor, "works.list", string(scope))
	if err != nil {
		t.Fatal("candidate_cursor", mixedCode(err))
	}
	where := `w.id>? AND EXISTS (SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=w.id AND e.hidden=0`
	args = []any{cursor.Page}
	if c.Room != "" {
		where += ` AND e.room=?`
		args = append(args, c.Room)
	} else {
		where += ` AND r.visibility='public' AND e.kind<>'simulation'`
	}
	where += `)`
	if c.Kind != "" {
		where += ` AND (` + workEffectiveSQL + `)=?`
		args = append(args, testTime, s.generation, testTime, c.Kind)
	}
	if c.Query != "" {
		where += ` AND (instr(lower(w.title),lower(?))>0 OR EXISTS(SELECT 1 FROM json_each(w.capabilities) cap WHERE cap.value=lower(?)))`
		args = append(args, c.Query, c.Query)
	}
	args = append(args, c.Limit+1)
	return `SELECT ` + workColumns + ` FROM works w WHERE ` + where + ` ORDER BY w.id LIMIT ?`, args
}

type workCandidateTiming struct {
	Total, Select, Projection float64
	Selected                  bool
}

// Restricted fixture caller, not a replacement runtime authority implementation.
// Only known, unrotated ordinary identities/anonymous fixtures are supported;
// all delegated/private-read grant contexts are rejected, not silently omitted.
func workCandidateRead(phase context.Context, s *Store, c Command, query string, args []any) (result Result, timing workCandidateTiming, err error) {
	began := time.Now()
	defer func() { timing.Total = float64(time.Since(began)) / float64(time.Millisecond) }()
	if c.Operation != "works.list" || c.Delegation != nil || c.PrivateRead != nil || c.Limit != 25 {
		return result, timing, errors.New("unsupported_candidate_fixture")
	}
	ctx, cancel := context.WithTimeout(phase, 5*time.Second)
	defer cancel()
	a, err := s.authenticate(c, "local-synthetic-mixed")
	if err != nil {
		return result, timing, err
	}
	if err = validateCommandFields(c); err != nil {
		return result, timing, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, timing, err
	}
	defer tx.Rollback()
	if a.signed {
		var successor string
		if err = tx.QueryRowContext(ctx, "SELECT account,successor FROM identities WHERE id=?", a.id).Scan(&a.account, &successor); err != nil || successor != "" || c.Timestamp != testTime {
			return result, timing, errors.New("unsupported_candidate_agent")
		}
	}
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	var generation string
	if err = tx.QueryRowContext(readCtx, "SELECT value FROM meta WHERE key='generation'").Scan(&generation); err != nil {
		return result, timing, err
	}
	if c.Room != "" {
		if _, err = roomAccess(readCtx, tx, c.Room, a); err != nil {
			return result, timing, err // Permission denial MUST precede candidate SQL.
		}
	}
	timing.Selected = true
	selectStart := time.Now()
	rows, err := tx.QueryContext(readCtx, query, args...)
	if err != nil {
		return result, timing, err
	}
	stored := []workRow{}
	for rows.Next() {
		w, e := scanWork(rows)
		if e != nil || len(stored) >= c.Limit+1 {
			rows.Close()
			return result, timing, errors.New("candidate_scan_bound")
		}
		stored = append(stored, w)
	}
	err = rows.Err()
	rows.Close()
	timing.Select = float64(time.Since(selectStart)) / float64(time.Millisecond)
	if err != nil {
		return result, timing, err
	}
	more := len(stored) > c.Limit
	if more {
		stored = stored[:c.Limit]
	}
	projectStart := time.Now()
	works := []Work{}
	for _, w := range stored {
		root, e := visibleWorkRoot(readCtx, tx, w.ID, a)
		if e != nil {
			return result, timing, e
		}
		p, e := s.projectWork(readCtx, tx, w, root, generation, testTime)
		if e != nil {
			return result, timing, e
		}
		works = append(works, p)
	}
	timing.Projection = float64(time.Since(projectStart)) / float64(time.Millisecond)
	result = Result{OK: true, Data: map[string]any{"works": works, "has_more": more}}
	if more {
		scope, _ := json.Marshal([]string{c.Room, c.Kind, c.Query})
		result.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "works.list", Scope: string(scope), Page: stored[len(stored)-1].ID})
	}
	err = tx.Commit()
	return result, timing, err
}

func workCandidateSame(s *Store, c Command, a, b Result) bool {
	if !a.OK || !b.OK || !reflect.DeepEqual(a.Data, b.Data) || (a.NextCursor == "") != (b.NextCursor == "") {
		return false
	}
	if a.NextCursor == "" {
		return true
	}
	scope, _ := json.Marshal([]string{c.Room, c.Kind, c.Query})
	x, ex := s.decodeConversationCursor(a.NextCursor, "works.list", string(scope))
	y, ey := s.decodeConversationCursor(b.NextCursor, "works.list", string(scope))
	return ex == nil && ey == nil && reflect.DeepEqual(x, y)
}

func workCandidateMany(t *testing.T, root string, s *Store, f mixedFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	execute := func(c Command) Result {
		r, err := mixedExecute(ctx, s, c)
		if err != nil {
			t.Fatal("many_work_setup", c.Operation, mixedCode(err))
		}
		return r
	}
	execute(signed(f.parent, Command{Operation: "room.create", Room: "candidate-private", Visibility: "private"}))
	execute(signed(f.parent, Command{Operation: "room.create", Room: "candidate-empty", Visibility: "public"}))
	for i := 0; i < 900; i++ {
		if i%50 == 0 {
			size, err := mixedDisk(root)
			if err != nil || size >= 512<<20 {
				t.Fatal("many_work_storage_bound")
			}
		}
		room, kind := fmt.Sprintf("candidate-bulk-%02d", i%40), "request"
		switch i {
		case 0:
			room = "candidate-sparse"
		case 1:
			room = "candidate-hidden"
		case 2, 3, 4:
			room = "candidate-private"
		case 5:
			room, kind = "candidate-simulation", "simulation"
		}
		id := execute(signed(f.parent, Command{Operation: "post", Room: room, Kind: kind, Text: "Synthetic local query-plan fixture; no user task or external work.", RequestID: fmt.Sprintf("many-work-%d", i)})).Receipt.ID
		execute(workCommand(s, f.parent, Command{Operation: "work.create", MessageID: id}))
		if i == 1 || i == 3 {
			if err := s.Moderate(ctx, id, "Synthetic query-plan hidden work", true); err != nil {
				t.Fatal("many_work_moderation", mixedCode(err))
			}
		}
	}
	var count int
	if s.db.QueryRowContext(ctx, "SELECT count(*) FROM works").Scan(&count) != nil || count != 1000 {
		t.Fatal("many_work_count")
	}
}

type workCandidateCase struct {
	Name    string
	Command Command
	Count   int
	Denied  bool
}

func workCandidateCases(t *testing.T, s *Store, f mixedFixture, many bool) []workCandidateCase {
	t.Helper()
	base := Command{Operation: "works.list", Room: mixedRoom, Limit: 25}
	filtered := base
	filtered.Kind, filtered.Query = "open", "review"
	next := func(c Command, limit int) Command {
		first := c
		first.Limit = limit
		if first.PublicKey != "" {
			first = signed(f.parent, first)
		}
		r, err := mixedExecute(context.Background(), s, first)
		if err != nil || r.NextCursor == "" {
			t.Fatal("candidate_runtime_cursor", mixedCode(err))
		}
		c.Cursor = r.NextCursor
		if c.PublicKey != "" {
			c = signed(f.parent, c)
		}
		return c
	}
	claimed, miss := base, filtered
	claimed.Kind, miss.Query = "claimed", "no-such-work-title"
	global := Command{Operation: "works.list", Limit: 25}
	if !many {
		return []workCandidateCase{
			{"room_first", base, 25, false}, {"room_filtered", filtered, 25, false},
			{"room_cursor", next(base, 10), 25, false}, {"filtered_cursor", next(filtered, 10), 15, false},
			{"claimed", claimed, 25, false}, {"query_miss", miss, 0, false}, {"global_simulation_exclusion", global, 0, false},
		}
	}
	room := func(name string) Command { return Command{Operation: "works.list", Room: name, Limit: 25} }
	private := room("candidate-private")
	owner := signed(f.parent, private)
	noMatch := room("candidate-sparse")
	noMatch.Query = "no-such-work-title"
	stateMiss := room("candidate-sparse")
	stateMiss.Kind = "accepted"
	return []workCandidateCase{
		{"sparse_room", room("candidate-sparse"), 1, false}, {"sparse_query_miss", noMatch, 0, false},
		{"sparse_state_miss", stateMiss, 0, false}, {"empty_room", room("candidate-empty"), 0, false},
		{"hidden_root", room("candidate-hidden"), 0, false}, {"explicit_simulation", room("candidate-simulation"), 1, false},
		{"private_owner", owner, 2, false}, {"private_owner_cursor", next(owner, 1), 1, false},
		{"private_anonymous_denied", private, 0, true}, {"private_nonmember_denied", signed(f.worker, private), 0, true},
		{"global_public_only", global, 25, false}, {"global_member_still_public", signed(f.parent, global), 25, false},
		{"global_cursor", next(global, 10), 25, false},
	}
}

func workCandidateMedian(values []float64) float64 {
	sort.Float64s(values)
	return (values[4] + values[5]) / 2
}

func TestWorkQueryCandidateLocal(t *testing.T) {
	if os.Getenv("SWARMMEMO_WORK_QUERY_CANDIDATE") != "1" {
		t.Skip("explicit local SQL-candidate diagnostic opt-in required")
	}
	root := t.TempDir()
	if os.Chmod(root, 0700) != nil {
		t.Fatal("candidate_temp_permissions")
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) != nil || disk.Bavail*uint64(disk.Bsize) < 2<<30 {
		t.Fatal("candidate_requires_two_gib_free")
	}
	for _, fixture := range []struct {
		name     string
		retained int
		many     bool
	}{{"messages_1000", 1000, false}, {"messages_10000", 10000, false}, {"works_1000_sparse_rooms", 1000, true}} {
		if !t.Run(fixture.name, func(t *testing.T) {
			dir, err := os.MkdirTemp(root, "candidate-")
			if err != nil {
				t.Fatal("candidate_directory")
			}
			defer os.RemoveAll(dir)
			s := mixedOpen(t, filepath.Join(dir, "candidate.sqlite"))
			defer func() {
				if err := s.Close(); err != nil {
					t.Error("candidate_close", mixedCode(err))
				}
			}()
			f := mixedSeed(t, root, s, fixture.retained)
			if fixture.many {
				workCandidateMany(t, root, s, f)
			}
			before := mixedQuota(t, s, f)
			cases := workCandidateCases(t, s, f, fixture.many)
			phase, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			for _, tc := range cases {
				shapes := []string{"original", "cross", "exists"}
				queries, arguments := map[string]string{}, map[string][]any{}
				plans := map[string][]workProfilePlan{}
				for _, shape := range shapes {
					queries[shape], arguments[shape] = workCandidateSQL(t, s, tc.Command, shape)
					if !tc.Denied {
						plans[shape] = workProfileExplain(t, phase, s, queries[shape], arguments[shape])
					}
				}
				fullTimes := []float64{}
				times := map[string][]workCandidateTiming{}
				for sample := 0; sample < 10; sample++ {
					size, err := mixedDisk(root)
					if err != nil || size >= 512<<20 || phase.Err() != nil {
						t.Fatal("candidate_storage_or_time_bound")
					}
					began := time.Now()
					actual, actualErr := mixedExecute(phase, s, tc.Command)
					fullTimes = append(fullTimes, float64(time.Since(began))/float64(time.Millisecond))
					if tc.Denied {
						if mixedCode(actualErr) != "not_found" {
							t.Fatal("candidate_expected_authority_denial")
						}
					} else {
						if actualErr != nil {
							t.Fatal("candidate_actual_read", mixedCode(actualErr))
						}
						works, ok := actual.Data["works"].([]Work)
						if !ok || len(works) != tc.Count {
							t.Fatal("candidate_expected_page_count")
						}
						for _, w := range works {
							if tc.Command.Room != "" && w.Room != tc.Command.Room || tc.Command.Room == "" && (w.Simulated || w.Room == "candidate-private") {
								t.Fatal("candidate_actual_scope")
							}
						}
					}
					// Rotate diagnostic order to avoid always warming one candidate last.
					for j := 0; j < len(shapes); j++ {
						shape := shapes[(sample+j)%len(shapes)]
						r, timing, err := workCandidateRead(phase, s, tc.Command, queries[shape], arguments[shape])
						times[shape] = append(times[shape], timing)
						if tc.Denied {
							if mixedCode(err) != "not_found" || timing.Selected {
								t.Fatal("candidate_private_fallback")
							}
						} else if err != nil || !timing.Selected || !workCandidateSame(s, tc.Command, actual, r) {
							t.Fatal("candidate_projection_mismatch", shape, mixedCode(err))
						}
					}
				}
				metrics := map[string]any{}
				for shape, values := range times {
					total, selection, projection := []float64{}, []float64{}, []float64{}
					for _, v := range values {
						total = append(total, v.Total)
						selection = append(selection, v.Select)
						projection = append(projection, v.Projection)
					}
					median := workCandidateMedian(total)
					metrics[shape] = map[string]any{"total_median_ms": median, "total_min_ms": total[0], "total_max_ms": total[9], "select_median_ms": workCandidateMedian(selection), "projection_median_ms": workCandidateMedian(projection)}
				}
				raw, _ := json.Marshal(map[string]any{"case": tc.Name, "samples": 10, "rows": tc.Count, "denied_before_select": tc.Denied, "full_execute_median_ms": workCandidateMedian(fullTimes), "metrics": metrics, "plans": plans})
				t.Log("candidate_result", string(raw))
			}
			if mixedQuota(t, s, f) != before {
				t.Fatal("candidate_read_mutated_quota_or_rows")
			}
			mixedIsolation(t, s, f)
			if err := s.Integrity(phase); err != nil {
				t.Fatal("candidate_integrity", mixedCode(err))
			}
		}) {
			break
		}
	}
}
