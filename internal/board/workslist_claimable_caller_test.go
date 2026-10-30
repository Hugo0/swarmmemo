package board

import (
	"database/sql"
	"sync/atomic"
	"testing"

	"swarmmemo/internal/allowance"
)

// C646: works.list read by an identified agent says claimable as its claim
// would answer: a standing task whose per-agent-day seat the agent used is
// claimable false with work.get's eligible_reason, and left out of its
// claimable listings; another agent and an anonymous read still see it
// claimable. The fix reads nothing more than the eligibility each row
// already answers.
func TestWorksListClaimableForCaller(t *testing.T) {
	s := rewardStore(t)
	owner, worker, other := keyFor(242), keyFor(243), keyFor(244)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	standing := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, standing, map[string]any{"seats": 3, "reward": 300, "per_agent_day": 1}, 10*86400))
	r := run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: standing, Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: standing, Target: r}, nil))
	run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: r, Amount: 1}))

	row := func(c Command) (Work, bool) {
		t.Helper()
		for _, w := range run(t, s, c).Data["works"].([]Work) {
			if w.ID == standing {
				return w, true
			}
		}
		return Work{}, false
	}
	get := run(t, s, signed(worker, Command{Operation: "work.get", MessageID: standing})).Data["work"].(Work)
	if get.Eligible == nil || *get.Eligible {
		t.Fatalf("work.get eligible: %+v", get.Eligible)
	}
	for _, kind := range []string{"", "open", WorkKindAll} {
		w, ok := row(signed(worker, Command{Operation: "works.list", Kind: kind, Timestamp: s.now().Unix()}))
		if !ok || w.Claimable || w.Eligible == nil || *w.Eligible || w.EligibleReason != get.EligibleReason {
			t.Fatalf("kind %q, seat used: listed %v claimable %v eligible %v %q (work.get %q)", kind, ok, w.Claimable, w.Eligible, w.EligibleReason, get.EligibleReason)
		}
	}
	for _, c := range []Command{
		signed(worker, Command{Operation: "works.list", Kind: WorkKindRewarded, Timestamp: s.now().Unix()}),
		signed(worker, Command{Operation: "works.list", Data: `{"schema":1,"claimable":true}`, Timestamp: s.now().Unix()}),
	} {
		if _, ok := row(c); ok {
			t.Fatalf("%s %q lists a claim the caller's seat refuses", c.Kind, c.Data)
		}
	}
	fails(t, s, workWith(s, worker, Command{Operation: "work.claim", MessageID: standing, Target: run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: standing, Text: "Again " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt.ID}, nil), "seat_limit")

	// Another agent, and an anonymous read, still see a seat open.
	if w, ok := row(signed(other, Command{Operation: "works.list", Kind: WorkKindRewarded, Timestamp: s.now().Unix()})); !ok || !w.Claimable || w.Eligible == nil || !*w.Eligible {
		t.Fatalf("other agent: listed %v claimable %v eligible %v", ok, w.Claimable, w.Eligible)
	}
	if w, ok := row(Command{Operation: "works.list", Kind: WorkKindRewarded}); !ok || !w.Claimable || w.Eligible != nil {
		t.Fatalf("anonymous: listed %v claimable %v eligible %v", ok, w.Claimable, w.Eligible)
	}
	// A preview for the worker keeps the work's own claimable, eligible false.
	if w, ok := row(signed(other, Command{Operation: "works.list", Kind: WorkKindAll, Data: `{"schema":1,"eligible_for":"` + keyID(worker) + `"}`, Timestamp: s.now().Unix()})); !ok || !w.Claimable || w.Eligible == nil || *w.Eligible || !w.EligiblePreview {
		t.Fatalf("preview: listed %v claimable %v eligible %v", ok, w.Claimable, w.Eligible)
	}

	// Statements: the worker's own listing prepares no more than another
	// agent's preview of the worker's eligibility on the same page (which
	// adds only the named identity's lookup), so the claimable fix costs no
	// read of its own; and both stay bounded.
	var path string
	if err := s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	db := sql.OpenDB(countingConnector{name: path, d: countingDriver{inner: s.db.Driver(), n: &n}})
	defer db.Close()
	count := func(c Command, a actor) int64 {
		t.Helper()
		tx, err := db.BeginTx(testContext, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		n.Store(0)
		if _, err := s.readWork(testContext, tx, c, a, s.now().Unix()); err != nil {
			t.Fatal(err)
		}
		return n.Load()
	}
	own := count(Command{Operation: "works.list", Kind: WorkKindAll}, actor{id: keyID(worker), account: keyID(worker), signed: true})
	preview := count(Command{Operation: "works.list", Kind: WorkKindAll, Data: `{"schema":1,"eligible_for":"` + keyID(worker) + `"}`}, actor{id: keyID(other), account: keyID(other), signed: true})
	if own > preview || own > 60 {
		t.Fatalf("own listing prepared %d statements, a preview %d", own, preview)
	}
}
