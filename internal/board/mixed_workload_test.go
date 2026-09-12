//go:build linux

package board

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Explicit local-only, closed-loop experiments. No network, external keys,
// concurrent backup, or production configuration changes. Normal tests skip both.
// SWARMMEMO_MIXED_WORKLOAD=1 go test ./internal/board -run '^TestMixedWorkloadLocal$' -count=1 -timeout=15m -v
// SWARMMEMO_MIXED_CORRECTNESS=1 go test -race ./internal/board -run '^TestMixedWorkloadSmallCorrectness$' -count=1 -timeout=2m -v
const mixedRoom = "synthetic-mixed"
const mixedHit = "literal%_ OR marker"
const mixedCanary = "PRIVATE-OR-HIDDEN-SYNTHETIC-CANARY"

type mixedFixture struct {
	parent, child, worker ed25519.PrivateKey
	grant                 *DelegationContext
	historyID             string
	privateID, hiddenID   string
	works                 map[string]Work
}

func mixedCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var be *Error
	if errors.As(err, &be) {
		return be.Code // Core-defined code only; never raw error/response text.
	}
	return "fixture_or_storage_error"
}

func mixedExecute(ctx context.Context, s *Store, c Command) (Result, error) {
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := s.Execute(call, c, "local-synthetic-mixed")
	if err == nil && !r.OK {
		err = errors.New("missing_ok")
	}
	return r, err
}

func mixedOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, Config{DailyBytes: 1 << 30, AnonymousDailyBytes: 1 << 30, GlobalDailyBytes: 1 << 30})
	if err != nil {
		t.Fatal("fixture_open", mixedCode(err))
	}
	// Set once before any concurrent access: no racing clock writes or expiry.
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	return s
}

func mixedDisk(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		i, err := d.Info()
		if err != nil { // SQLite can unlink transient journals between samples.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !i.Mode().IsRegular() {
			return errors.New("unexpected_fixture_file")
		}
		size += i.Size()
		return nil
	})
	return size, err
}

func mixedRSS() int64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			return n * 1024
		}
	}
	return 0
}

func mixedSeed(t *testing.T, root string, s *Store, retained int) mixedFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := mixedFixture{parent: keyFor(221), child: keyFor(222), worker: keyFor(223), works: map[string]Work{}}
	mutations, publicPosts := 0, 0
	execute := func(c Command) Result {
		mutations++
		if mutations > 12000 {
			t.Fatal("setup_mutation_bound")
		}
		if mutations%100 == 0 {
			size, err := mixedDisk(root)
			if err != nil || size >= 512<<20 {
				t.Fatal("setup_storage_bound")
			}
		}
		r, err := mixedExecute(ctx, s, c)
		if err != nil {
			t.Fatal("setup", c.Operation, mixedCode(err))
		}
		return r
	}
	post := func(key ed25519.PrivateKey, reply string) string {
		text := strings.Repeat("Synthetic local fixture café 雪. ", 32)
		if publicPosts%10 == 0 {
			text += mixedHit
		}
		c := signed(key, Command{Operation: "post", Room: mixedRoom, Page: "main", Kind: "simulation", Text: text, ReplyTo: reply, RequestID: fmt.Sprintf("seed-%d", publicPosts)})
		r := execute(c)
		publicPosts++
		if r.Receipt == nil || r.Receipt.Duplicate {
			t.Fatal("setup_receipt")
		}
		return r.Receipt.ID
	}
	ids := []string{}
	results := map[string]string{}
	for i := 0; i < 100; i++ {
		id := post(f.parent, "")
		ids = append(ids, id)
		execute(workCommand(s, f.parent, Command{Operation: "work.create", MessageID: id}))
		if i%4 > 0 {
			execute(workCommand(s, f.worker, Command{Operation: "work.claim", MessageID: id, TTL: 3600}))
		}
		if i%4 >= 2 {
			result := post(f.worker, id)
			results[id] = result
			execute(workCommand(s, f.worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: result}))
		}
		if i%4 == 3 {
			execute(workCommand(s, f.parent, Command{Operation: "work.accept", MessageID: id, Amount: 1}))
		}
	}
	f.historyID = ids[0]
	for fence := int64(1); fence <= 50; fence++ {
		execute(workCommand(s, f.worker, Command{Operation: "work.claim", MessageID: f.historyID, TTL: 3600}))
		execute(workCommand(s, f.parent, Command{Operation: "work.reject", MessageID: f.historyID, Amount: fence, Reason: "Synthetic reset; no external work"}))
	}
	for publicPosts < retained {
		post(f.parent, "")
	}
	for i, id := range ids {
		r, err := mixedExecute(ctx, s, Command{Operation: "work.get", MessageID: id})
		if err != nil {
			t.Fatal("setup_work_projection", mixedCode(err))
		}
		w, ok := r.Data["work"].(Work)
		state := []string{"open", "claimed", "submitted", "accepted"}[i%4]
		if !ok || w.ID != id || w.Room != mixedRoom || !w.Simulated || w.State != state || w.StoredState != state || w.Requester.ID != keyID(f.parent) || w.RequesterAuthor != keyID(f.parent) || w.ResultID != results[id] || w.ResultAvailable != (i%4 >= 2) {
			t.Fatal("setup_work_expected_state")
		}
		if i%4 == 0 && w.Worker != nil || i%4 != 0 && (w.Worker == nil || w.Worker.ID != keyID(f.worker)) {
			t.Fatal("setup_work_expected_owner")
		}
		f.works[id] = w
	}
	execute(signed(f.parent, Command{Operation: "room.create", Room: "synthetic-private", Visibility: "private"}))
	f.privateID = execute(signed(f.parent, Command{Operation: "post", Room: "synthetic-private", Kind: "simulation", Text: mixedCanary})).Receipt.ID
	f.hiddenID = execute(signed(f.parent, Command{Operation: "post", Room: mixedRoom, Kind: "simulation", Text: mixedCanary})).Receipt.ID
	if err := s.Moderate(ctx, f.hiddenID, "Synthetic hidden fixture", true); err != nil {
		t.Fatal("setup_hide", mixedCode(err))
	}
	execute(grantCommand(s, f.parent, f.child, mixedRoom, 3600, 1<<29, []string{"post"}))
	f.grant = &DelegationContext{Schema: 1, GrantID: keyID(f.child), Generation: s.generation}
	t.Logf("seed retained_public_visible=%d hidden_public=1 private=1 works=100 history_transitions=101 setup_mutations=%d", retained, mutations+1)
	return f
}

func mixedProof(key, sig, payload string) bool {
	k, e1 := base64.RawURLEncoding.DecodeString(key)
	b, e2 := base64.RawURLEncoding.DecodeString(sig)
	return e1 == nil && e2 == nil && len(k) == ed25519.PublicKeySize && len(b) == ed25519.SignatureSize && ed25519.Verify(k, []byte(payload), b)
}

func mixedEvent(f mixedFixture, e Message) bool {
	if e.ID == f.privateID || e.Room != mixedRoom || e.Visibility != "public" {
		return false
	}
	if e.Hidden {
		return e.ID == f.hiddenID && e.Type == "tombstone" && e.Text == "" && e.Signature == "" && e.SignedPayload == ""
	}
	if strings.Contains(e.Text, mixedCanary) || e.Kind != "simulation" || !mixedProof(e.PublicKey, e.Signature, e.SignedPayload) {
		return false
	}
	var envelope struct {
		Version int     `json:"version"`
		Service string  `json:"service"`
		Command Command `json:"command"`
	}
	if json.Unmarshal([]byte(e.SignedPayload), &envelope) != nil {
		return false
	}
	c := envelope.Command
	sum := sha256.Sum256([]byte(e.Text))
	if envelope.Service != "swarmmemo.com" || string(Canonical(envelope.Service, c)) != e.SignedPayload || c.Operation != "post" || c.Room != e.Room || c.Kind != e.Kind || c.Text != e.Text || c.PublicKey != e.PublicKey || c.ReplyTo != e.ReplyTo || hex.EncodeToString(sum[:]) != e.Hash {
		return false
	}
	k, _ := base64.RawURLEncoding.DecodeString(e.PublicKey)
	if e.Author != fingerprint(k) {
		return false
	}
	if c.Delegation != nil {
		return envelope.Version == 2 && *c.Delegation == *f.grant && e.Author == keyID(f.child) && e.DelegationID == f.grant.GrantID
	}
	return envelope.Version == 1 && e.DelegationID == "" && (e.Author == keyID(f.parent) || e.Author == keyID(f.worker))
}

type mixedObservation struct {
	Class        string
	Duration     time.Duration
	Code         string
	Count, Bytes int
	Command      Command // Bounded ephemeral exact envelopes; never logged or exported.
	Receipt      *Receipt
}

func mixedReadCheck(f mixedFixture, c Command, r Result, historyAfter int64) (int, error) {
	bad := errors.New("projection_mismatch")
	switch c.Operation {
	case "messages.list":
		if r.Generation != f.grant.Generation || r.NextCursor == "" {
			return 0, bad
		}
		if c.Query == "no-such-synthetic-mixed-content" && len(r.Messages) != 0 || c.Query == mixedHit && len(r.Messages) == 0 || c.Cursor == "" && c.Query == "" && len(r.Messages) == 0 {
			return 0, bad
		}
		var previous int64
		for _, e := range r.Messages {
			if !mixedEvent(f, e) || e.Sequence <= previous || (c.Query != "" && !strings.Contains(strings.ToLower(e.Text), strings.ToLower(c.Query))) {
				return 0, bad
			}
			previous = e.Sequence
		}
		return len(r.Messages), nil
	case "works.list":
		works, ok := r.Data["works"].([]Work)
		if !ok || len(works) == 0 {
			return 0, bad
		}
		previous := ""
		for _, w := range works {
			if !reflect.DeepEqual(w, f.works[w.ID]) || w.ID <= previous || !w.Simulated || w.Room != mixedRoom || (c.Kind != "" && w.State != c.Kind) {
				return 0, bad
			}
			previous = w.ID
		}
		return len(works), nil
	case "work.history":
		rows, ok := r.Data["transitions"].([]WorkTransition)
		if !ok || len(rows) == 0 || r.Data["work_id"] != f.historyID || r.Data["simulated"] != true || r.Data["service_generation"] != f.grant.Generation {
			return 0, bad
		}
		for _, tr := range rows {
			op, state, key := "work.reject", "open", f.parent
			if tr.Sequence == 1 {
				op = "work.create"
			} else if tr.Sequence%2 == 0 {
				op, state, key = "work.claim", "claimed", f.worker
			}
			var envelope struct {
				Version int     `json:"version"`
				Service string  `json:"service"`
				Command Command `json:"command"`
			}
			if json.Unmarshal([]byte(tr.SignedPayload), &envelope) != nil || envelope.Version != 1 || envelope.Service != "swarmmemo.com" || string(Canonical(envelope.Service, envelope.Command)) != tr.SignedPayload || envelope.Command.Operation != op || envelope.Command.MessageID != f.historyID || envelope.Command.PublicKey != tr.PublicKey {
				return 0, bad
			}
			if tr.Sequence != historyAfter+1 || tr.Sequence > 101 || tr.Generation != f.grant.Generation || tr.Operation != op || tr.State != state || tr.Fence != tr.Sequence/2 || tr.Author != keyID(key) || tr.PublicKey != base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) || tr.DelegationID != "" || tr.AcceptedAt != testTime || !mixedProof(tr.PublicKey, tr.Signature, tr.SignedPayload) {
				return 0, bad
			}
			historyAfter = tr.Sequence
		}
		more := historyAfter < 101
		if r.Data["has_more"] != more || (r.NextCursor != "") != more {
			return 0, bad
		}
		return len(rows), nil
	}
	return 0, bad
}

func mixedQuota(t *testing.T, s *Store, f mixedFixture) [5]int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out [5]int64
	for i, actor := range []string{keyID(f.parent), "global", keyID(f.child)} {
		if err := s.db.QueryRowContext(ctx, "SELECT coalesce(sum(used),0) FROM quota WHERE actor=?", actor).Scan(&out[i]); err != nil {
			t.Fatal("quota_snapshot", mixedCode(err))
		}
	}
	if err := s.db.QueryRowContext(ctx, "SELECT used_bytes FROM delegations WHERE child_id=?", keyID(f.child)).Scan(&out[3]); err != nil {
		t.Fatal("grant_snapshot", mixedCode(err))
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&out[4]); err != nil {
		t.Fatal("message_count", mixedCode(err))
	}
	return out
}

func mixedIsolation(t *testing.T, s *Store, f mixedFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := mixedExecute(ctx, s, Command{Operation: "messages.list", Query: mixedCanary})
	if err != nil || len(r.Messages) != 0 {
		t.Fatal("canary_search_leak", mixedCode(err))
	}
	_, err = mixedExecute(ctx, s, Command{Operation: "message.get", MessageID: f.privateID})
	if mixedCode(err) != "not_found" {
		t.Fatal("private_get_scope")
	}
	r, err = mixedExecute(ctx, s, Command{Operation: "message.get", MessageID: f.hiddenID})
	if err != nil || len(r.Messages) != 1 || !mixedEvent(f, r.Messages[0]) {
		t.Fatal("hidden_get_redaction", mixedCode(err))
	}
}

func mixedCase(t *testing.T, root string, s *Store, f mixedFixture, callers, operations int) {
	t.Helper()
	mixedIsolation(t, s, f)
	before := mixedQuota(t, s, f)
	stats := s.db.Stats()
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	phase, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	observations := make([]mixedObservation, operations)
	start := time.Now()
	var wg sync.WaitGroup
	barrier, monitorStop, monitorDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var peakDisk, peakRSS int64
	storageFailed := false // Written only by monitor; read after monitorDone.
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			size, err := mixedDisk(root)
			peakDisk = max(peakDisk, size)
			peakRSS = max(peakRSS, mixedRSS())
			if err != nil || size >= 512<<20 {
				storageFailed = true
				cancel()
				return
			}
			select {
			case <-monitorStop:
				return
			case <-ticker.C:
			}
		}
	}()
	for worker := 0; worker < callers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-barrier
			eventCursor, historyCursor := "start", ""
			var historyAfter int64
			for index := worker; index < operations; index += callers {
				if phase.Err() != nil {
					return
				}
				ob := &observations[index]
				c := Command{Room: mixedRoom, Limit: 25}
				switch index % 10 {
				case 0:
					ob.Class, c.Operation, c.Query = "search_miss", "messages.list", "no-such-synthetic-mixed-content"
				case 1:
					ob.Class, c.Operation = "messages_latest", "messages.list"
				case 2:
					ob.Class, c.Operation = "works_list", "works.list"
				case 3, 8:
					ob.Class = "work_history"
					c = Command{Operation: "work.history", MessageID: f.historyID, Limit: 25, Cursor: historyCursor}
				case 4, 9:
					ob.Class = "signed_post"
					c = Command{Operation: "post", Room: mixedRoom, Page: "main", Kind: "simulation", Visibility: "public", Text: strings.Repeat("Synthetic measured café 雪. ", 32), RequestID: fmt.Sprintf("mixed-%d", index), Timestamp: testTime}
					if index%10 == 9 {
						ob.Class, c.Delegation = "delegated_post", f.grant
					}
				case 5:
					ob.Class, c.Operation, c.Query = "search_hit", "messages.list", mixedHit
				case 6:
					ob.Class, c.Operation, c.Cursor = "messages_history", "messages.list", eventCursor
				case 7:
					ob.Class, c.Operation, c.Kind, c.Query = "works_filtered", "works.list", "open", "review"
				}
				began := time.Now()
				if c.Operation == "post" {
					key := f.parent
					if c.Delegation != nil {
						key = f.child
					}
					c = signed(key, c)
					ob.Command = c // Exact prepared envelope retained before Execute.
				}
				r, err := mixedExecute(phase, s, c)
				ob.Duration, ob.Code = time.Since(began), mixedCode(err)
				if err != nil {
					continue
				}
				if c.Operation == "post" {
					ob.Receipt = r.Receipt
					if r.Receipt == nil || r.Receipt.Duplicate {
						ob.Code = "invalid_fresh_receipt"
						cancel()
					}
					ob.Count = 1
				} else {
					ob.Count, err = mixedReadCheck(f, c, r, historyAfter)
					if err != nil {
						ob.Code = "projection_mismatch"
						cancel()
					}
					if ob.Class == "messages_history" {
						eventCursor = r.NextCursor
						if len(r.Messages) == 0 {
							eventCursor = "start"
						}
					}
					if ob.Class == "work_history" && err == nil {
						rows := r.Data["transitions"].([]WorkTransition)
						historyCursor, historyAfter = r.NextCursor, rows[len(rows)-1].Sequence
						if historyCursor == "" {
							historyAfter = 0
						}
					}
				}
				raw, err := json.Marshal(r) // Correctness/accounting outside latency.
				ob.Bytes = len(raw)
				if err != nil || ob.Bytes > 1<<20 {
					ob.Code = "response_bound"
					cancel()
				}
			}
		}(worker)
	}
	close(barrier)
	wg.Wait()
	phaseDuration := time.Since(start)
	phaseExpired := phaseDuration >= 60*time.Second
	close(monitorStop)
	<-monitorDone
	endStats := s.db.Stats()
	runtime.ReadMemStats(&memAfter)
	classes := map[string][]float64{}
	errorsByClass := map[string]int{}
	errorMaxMS := map[string]float64{}
	counts, byteCounts := map[string]int{}, map[string]int{}
	completed := 0
	for _, ob := range observations {
		if ob.Class == "" {
			continue
		}
		completed++
		if ob.Code != "" {
			errorsByClass[ob.Class+":"+ob.Code]++
			errorMaxMS[ob.Class+":"+ob.Code] = max(errorMaxMS[ob.Class+":"+ob.Code], float64(ob.Duration)/float64(time.Millisecond))
			continue
		}
		classes[ob.Class] = append(classes[ob.Class], float64(ob.Duration)/float64(time.Millisecond))
		counts[ob.Class] += ob.Count
		byteCounts[ob.Class] += ob.Bytes
	}
	summary := map[string]any{"callers": callers, "scheduled": operations, "completed": completed, "errors": errorsByClass, "phase_ms": phaseDuration.Milliseconds(), "pool_wait_count": endStats.WaitCount - stats.WaitCount, "pool_wait_ms": float64(endStats.WaitDuration-stats.WaitDuration) / float64(time.Millisecond), "sampled_fixture_peak_bytes": peakDisk, "sampled_process_peak_rss_bytes": peakRSS, "allocated_bytes": memAfter.TotalAlloc - memBefore.TotalAlloc, "result_counts": counts, "result_bytes": byteCounts}
	latencies := map[string]any{}
	for class, values := range classes {
		sort.Float64s(values)
		n := len(values)
		latencies[class] = map[string]any{"n": n, "p50_ms": values[(n*50+99)/100-1], "p95_ms": values[(n*95+99)/100-1], "max_ms": values[n-1]}
	}
	summary["latencies"] = latencies
	summary["error_max_ms"], summary["phase_expired"], summary["storage_failed"] = errorMaxMS, phaseExpired, storageFailed
	raw, _ := json.Marshal(summary)
	t.Log("mixed_result", string(raw))
	// After all workers have joined, settle at most the original write intents.
	// Exact retries may accept previously unaccepted timed-out writes; distinguish
	// those from historic duplicate acknowledgements and never erase phase errors.
	settle, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	var expectedCost, expectedGrant, writes int64
	seen := map[string]bool{}
	recoveredNew, recoveredDuplicate := 0, 0
	for i := range observations {
		ob := &observations[i]
		if ob.Command.Operation != "post" {
			continue
		}
		writes++
		if writes > 120 {
			t.Fatal("reconciliation_bound")
		}
		c := ob.Command
		if ob.Receipt == nil {
			r, err := mixedExecute(settle, s, c)
			if err != nil || r.Receipt == nil {
				t.Fatal("unresolved_write", mixedCode(err))
			}
			ob.Receipt = r.Receipt
			if r.Receipt.Duplicate {
				recoveredDuplicate++
			} else {
				recoveredNew++
			}
		}
		if seen[ob.Receipt.ID] {
			t.Fatal("duplicate_storage_id")
		}
		seen[ob.Receipt.ID] = true
		r, err := mixedExecute(settle, s, Command{Operation: "message.get", Room: mixedRoom, MessageID: ob.Receipt.ID})
		if err != nil || len(r.Messages) != 1 || !mixedEvent(f, r.Messages[0]) || r.Messages[0].Text != c.Text || r.Messages[0].SignedPayload != string(Canonical("swarmmemo.com", c)) || r.Messages[0].Hash != ob.Receipt.Hash || ob.Receipt.AcceptedAt != testTime {
			t.Fatal("stored_receipt_provenance", mixedCode(err))
		}
		var account string
		if err = s.db.QueryRowContext(settle, "SELECT account FROM events WHERE id=?", ob.Receipt.ID).Scan(&account); err != nil || account != keyID(f.parent) {
			t.Fatal("principal_attribution")
		}
		cost := int64(len(c.Text) + len(c.Room) + len(c.Page) + len(c.Kind) + 512 + len(Canonical("swarmmemo.com", c)))
		expectedCost += cost
		if c.Delegation != nil {
			expectedGrant += cost
		}
		if writes <= 4 { // Additional historical replay check, outside measured time.
			replay, err := mixedExecute(settle, s, c)
			if err != nil || replay.Receipt == nil || !replay.Receipt.Duplicate || replay.Receipt.ID != ob.Receipt.ID || replay.Receipt.Hash != ob.Receipt.Hash {
				t.Fatal("acknowledged_replay", mixedCode(err))
			}
		}
	}
	after := mixedQuota(t, s, f)
	if after[0]-before[0] != expectedCost || after[1]-before[1] != expectedCost || after[2] != before[2] || after[3]-before[3] != expectedGrant || after[4]-before[4] != writes {
		t.Fatal("charge_or_count_mismatch")
	}
	mixedIsolation(t, s, f)
	if err := s.Integrity(settle); err != nil {
		t.Fatal("integrity", mixedCode(err))
	}
	t.Logf("settlement writes=%d recovered_new=%d recovered_duplicate=%d correctness=pass", writes, recoveredNew, recoveredDuplicate)
	if storageFailed || phaseExpired || completed != operations || len(errorsByClass) != 0 {
		t.Fatal("incomplete_mixed_case; no successful capacity claim")
	}
}

func mixedRun(t *testing.T, retained []int, callers []int, operations int) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal("temp_permissions")
	}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) != nil || disk.Bavail*uint64(disk.Bsize) < 2<<30 {
		t.Fatal("requires_two_gib_free_local_storage")
	}
	t.Logf("environment go=%s arch=%s gomaxprocs=%d schema=8 quota_bytes=1073741824 fixed_unix=%d linux_filesystem_type=%x", runtime.Version(), runtime.GOARCH, runtime.GOMAXPROCS(0), testTime, uint64(disk.Type))
	for _, n := range retained {
		if !t.Run(fmt.Sprintf("retained_%d", n), func(t *testing.T) {
			dir, err := os.MkdirTemp(root, "seed-")
			if err != nil {
				t.Fatal("seed_directory")
			}
			defer os.RemoveAll(dir) // Exact generated fixture only, never a user path.
			seed := mixedOpen(t, filepath.Join(dir, "seed.sqlite"))
			defer seed.Close()
			f := mixedSeed(t, root, seed, n)
			for _, concurrency := range callers {
				if !t.Run(fmt.Sprintf("callers_%d", concurrency), func(t *testing.T) {
					caseDir, err := os.MkdirTemp(root, "case-")
					if err != nil {
						t.Fatal("case_directory")
					}
					defer os.RemoveAll(caseDir)
					path := filepath.Join(caseDir, "case.sqlite")
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					err = seed.Backup(ctx, path) // Idle setup clone, NEVER interference load.
					cancel()
					if err != nil {
						t.Fatal("idle_clone", mixedCode(err))
					}
					s := mixedOpen(t, path)
					defer func() {
						if err := s.Close(); err != nil {
							t.Error("fixture_close", mixedCode(err))
						}
					}()
					mixedCase(t, root, s, f, concurrency, operations)
				}) {
					return // Do not keep loading after a failed case.
				}
			}
		}) {
			break
		}
	}
}

func TestMixedWorkloadLocal(t *testing.T) {
	if os.Getenv("SWARMMEMO_MIXED_WORKLOAD") != "1" {
		t.Skip("explicit local mixed-workload opt-in required")
	}
	mixedRun(t, []int{1000, 10000}, []int{1, 4}, 600)
}

func TestMixedWorkloadSmallCorrectness(t *testing.T) {
	if os.Getenv("SWARMMEMO_MIXED_CORRECTNESS") != "1" {
		t.Skip("explicit small mixed correctness opt-in required; not capacity evidence")
	}
	mixedRun(t, []int{200}, []int{4}, 40)
}
