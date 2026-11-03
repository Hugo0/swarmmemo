package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C701a: a heavy standing-task worker's signed work reads. The fixture is
// tasks open standing tasks, each with two rejected results from other
// workers (a burst sibling of the signer, or an unlinked worker on the
// signer's network), and the signer's own claim (made before the rejections) on the first
// claims of them; every row's eligibility then runs the claim cool-down's link test.
type cooldownFixture struct {
	s      *Store
	worker ed25519.PrivateKey
	ids    []string
}

func newCooldownFixture(tb testing.TB, tasks, claims int) cooldownFixture {
	tb.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		tb.Skip("the ledger mode is overridden for the whole suite")
	}
	c := rewardConfig()
	c.DailyBytes, c.AnonymousDailyBytes, c.GlobalDailyBytes = 1<<30, 1<<30, 1<<30
	s := openTest(tb, c)
	tb.Cleanup(s.stopServices)
	setRewardParams(tb, s)
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	exec := func(c Command) Result {
		tb.Helper()
		r, err := s.Execute(testContext, c, "test-origin")
		if err != nil || !r.OK {
			tb.Fatalf("%s: %v", c.Operation, err)
		}
		return r
	}
	worker := keyFor(222)
	// Two burst siblings of the worker (among other clusters), two workers
	// on its home network, one on a cloud network too shared to link anyone
	// (past SignalLinkGroupMax accounts) and one on its own. The worker
	// writes a lot, as a heavy worker does.
	siblings := []ed25519.PrivateKey{keyFor(223), keyFor(224)}
	others := []ed25519.PrivateKey{keyFor(225), keyFor(226), keyFor(227), keyFor(228)}
	burst := BurstResult{ComputedAt: testTime}
	for c := range 40 {
		cl := BurstCluster{ID: c + 2, From: testTime, To: testTime}
		for m := range 10 {
			cl.Members = append(cl.Members, BurstMember{Account: fmt.Sprintf("burst-%d-%d", c, m), CreatedAt: testTime, Family: "scripts"})
		}
		burst.Clusters = append(burst.Clusters, cl)
	}
	storeBurstTB(tb, s, burst, append([]ed25519.PrivateKey{worker}, siblings...)...)
	tx, err := s.db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	signal := func(account, net string, n int) {
		for i := range n {
			if _, err := tx.Exec("INSERT INTO write_signals(created_at,operation,account,via,hash_key,ip_hash_24) VALUES(?,'post',?,'http',?,?)", testTime-int64(i*60), account, s.signals.id, net); err != nil {
				tb.Fatal(err)
			}
		}
	}
	signal(keyID(worker), "net-home", 150)
	signal(keyID(worker), "net-cloud", 150)
	signal(keyID(worker), "net-own-worker", 100)
	signal(keyID(others[0]), "net-home", 50)
	signal(keyID(others[1]), "net-home", 50)
	signal(keyID(others[2]), "net-cloud", 50)
	signal(keyID(others[3]), "net-own-228", 50)
	for a := range SignalLinkGroupMax + 6 {
		signal(fmt.Sprintf("cloud-%d", a), "net-cloud", 5)
	}
	if err = tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	workers := append(append([]ed25519.PrivateKey{}, siblings...), others...)
	f := cooldownFixture{s: s, worker: worker}
	for i := range tasks {
		s.now = func() time.Time { return time.Unix(testTime+int64(i), 0) }
		// One requester holds at most WorkRewardsHeldMax rewards.
		owner := keyFor(byte(150 + i/(WorkRewardsHeldMax-2)))
		if i%(WorkRewardsHeldMax-2) == 0 {
			mintCredit(tb, s, keyID(owner), allowance.Paid, 5000)
		}
		id := exec(signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: fmt.Sprintf("Standing review %d", i), Timestamp: s.now().Unix()})).Receipt.ID
		exec(standingCreate(s, owner, id, map[string]any{"seats": 5, "reward": 10, "per_agent_day": 5, "day_max": 50}, 20*86400))
		if i < claims {
			reply := exec(signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: fmt.Sprintf("Mine %d", i), Timestamp: s.now().Unix()})).Receipt.ID
			exec(workWith(s, worker, Command{Operation: "work.claim", MessageID: id, Target: reply}, nil))
		}
		for j := range 2 {
			k := workers[(i+j)%len(workers)]
			reply := exec(signed(k, Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: fmt.Sprintf("Result %d.%d", i, j), Timestamp: s.now().Unix()})).Receipt.ID
			exec(workWith(s, k, Command{Operation: "work.claim", MessageID: id, Target: reply}, nil))
			exec(workCommand(s, owner, Command{Operation: "work.reject", MessageID: reply, Amount: 1, Reason: "not a result"}))
		}
		f.ids = append(f.ids, id)
	}
	s.now = func() time.Time { return time.Unix(testTime+int64(tasks)+60, 0) }
	return f
}

// storeBurstTB stores res as the burst detector's result, with keys as
// the members of one more cluster.
func storeBurstTB(tb testing.TB, s *Store, res BurstResult, keys ...ed25519.PrivateKey) {
	tb.Helper()
	c := BurstCluster{ID: 1, From: testTime, To: testTime}
	for _, k := range keys {
		c.Members = append(c.Members, BurstMember{Account: keyID(k), CreatedAt: testTime, Family: "scripts"})
	}
	res.Clusters = append(res.Clusters, c)
	raw, err := json.Marshal(res)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", metaFunnelBurst, string(raw)); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkWorkListHeavyWorker is works.list for the signer with data
// claimable (scans up to workClaimableScanMax rows) and with data summary.
// Run with -run '^$' -bench WorkListHeavyWorker -benchtime=5x.
func BenchmarkWorkListHeavyWorker(b *testing.B) {
	f := newCooldownFixture(b, 500, 41)
	for _, tc := range []struct{ name, data string }{
		{"claimable500", `{"schema":1,"claimable":true}`},
		{"summary100", `{"schema":1,"summary":true}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				c := signed(f.worker, Command{Operation: "works.list", Limit: DirectoryPageMax, Data: tc.data, Timestamp: f.s.now().Unix()})
				if _, err := f.s.Execute(testContext, c, "test-origin"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestWorkEligibilityPageCacheUnchanged: one page's shared facts (and its
// link cache) answer each row's eligibility exactly as fresh facts read
// row by row do: the same answer, refusal code and text.
func TestWorkEligibilityPageCacheUnchanged(t *testing.T) {
	f := newCooldownFixture(t, 24, 6)
	s := f.s
	now := s.now().Unix()
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	accounts := []string{keyID(f.worker), keyID(keyFor(223)), keyID(keyFor(225)), keyID(keyFor(228)), ""}
	paused := 0
	for _, account := range accounts {
		shared := &workEligibilityFacts{account: account}
		for _, id := range f.ids {
			w, err := scanWork(tx.QueryRowContext(testContext, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
			if err != nil {
				t.Fatal(err)
			}
			st, err := projectWorkStanding(testContext, tx, w, w.State, now)
			if err != nil {
				t.Fatal(err)
			}
			st2 := *st
			type answer struct {
				ok           bool
				code, reason string
			}
			var got, want answer
			if got.ok, got.code, got.reason, err = s.workEligibleNow(testContext, tx, shared, w, w.State, st, now); err != nil {
				t.Fatal(err)
			}
			if want.ok, want.code, want.reason, err = s.workEligibleNow(testContext, tx, &workEligibilityFacts{account: account}, w, w.State, &st2, now); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s on %s: cached %+v, uncached %+v", account, id, got, want)
			}
			// The uncached cool-down, straight from its own queries.
			until, err := s.workClaimCooldownUntil(testContext, tx, nil, w, true, account, now)
			if err != nil {
				t.Fatal(err)
			}
			if until > 0 {
				paused++
			}
			if want.code == WorkBlockedClaimPause && (until == 0 || want.reason != workClaimCooldownText(true, until)) {
				t.Fatalf("%s on %s: pause %d, reason %q", account, id, until, want.reason)
			}
		}
	}
	if paused == 0 {
		t.Fatal("the fixture pauses no one: the cool-down path is not exercised")
	}
	// Each link signal on its own: burst siblings (task 6), the home
	// network (task 8); the cloud network links no one (task 9 for 227)
	// and an unlinked pair pauses no one (task 10).
	for _, c := range []struct {
		account string
		task    int
		paused  bool
	}{{keyID(f.worker), 6, true}, {keyID(f.worker), 8, true}, {keyID(f.worker), 10, false}, {keyID(keyFor(227)), 9, false}, {keyID(keyFor(225)), 8, true}} {
		w, err := scanWork(tx.QueryRowContext(testContext, `SELECT `+workColumns+` FROM works w WHERE id=?`, f.ids[c.task]))
		if err != nil {
			t.Fatal(err)
		}
		until, err := s.workClaimCooldownUntil(testContext, tx, nil, w, true, c.account, now)
		if err != nil {
			t.Fatal(err)
		}
		if (until > 0) != c.paused {
			t.Fatalf("%s on task %d: paused until %d, want paused %t", c.account, c.task, until, c.paused)
		}
	}
}
