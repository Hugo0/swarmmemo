package board

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/find"
	"swarmmemo/internal/judge"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services/servicestest"
	"swarmmemo/internal/trust"
)

// The reputation (C304) on the real store: it is exactly its parts, each
// computed by its own function (the drift test); owned subjects count and
// posts do not; its own root's judgements are left out; thin is the
// judge's; a backer is independent, judges above 50 and has weight; and
// every surface shows the same numbers.

// reputationProtocolSection is PROTOCOL.md's generated reputation section:
// the rule and the fields from their one source (reputation.go).
func reputationProtocolSection() string {
	return "- **Reputation.** " + ReputationRule + "\n" +
		"- **Fields.** `accountable` {`tier`, `tier_name`, `standing_cents`, `fake_cost`, `band`, `run`}; `reviewed` {`" + judge.ScoreField + "`, `judgements`, `thin`, `agent`, `sells`, `owned`}, each subject in it {`subject`, `id`, `url`, `" + judge.ScoreField + "`, `judgements`, `thin`}; `delivered`: the `work_record`; `backed_by` {`count`, `backers` [{`account`, `handle`, `tier`, `tier_name`, `act`, `x`, `weight_ppm`, `at`}]}; `line`, the page's header. A Find agent hit carries `reputation` {`tier`, `" + judge.ScoreField + "`, `thin`, `accepted_tasks`, `paid_tasks`, `backers`}.\n"
}

// reputationTestStore runs reviews, offerings, vouches and the inbox.
func reputationTestStore(t *testing.T) *Store {
	t.Helper()
	c := updatesConfig()
	c.Features = Features{Reviews: true, InboxEntries: InboxRead, VoteRecords: true, ExportEndorsements: true}
	c.Offerings = topupConfig(t, &fakeFacilitator{}, "")
	s := openTest(t, c)
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	t.Cleanup(s.stopOfferings)
	return s
}

// trustRows puts accounts in a done trust run (as setStanding does), each
// with its root and standing cents.
func trustRows(t *testing.T, s *Store, rows map[string][2]any) {
	t.Helper()
	cents := map[string]int64{}
	for account, r := range rows {
		cents[account] = r[1].(int64)
	}
	run := setStanding(t, s, cents)
	for account, r := range rows {
		if _, err := s.db.Exec("UPDATE trust_scores SET root=? WHERE run_id=? AND account=?", r[0], run, account); err != nil {
			t.Fatal(err)
		}
	}
}

func readReputation(t *testing.T, s *Store, account string) Reputation {
	t.Helper()
	r, err := s.Reputation(testContext, account)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func reputationJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReputationIsItsParts(t *testing.T) {
	s := reputationTestStore(t)
	seller, r1, r2, r3, rootmate, funded, fresh := keyFor(160), keyFor(161), keyFor(162), keyFor(163), keyFor(164), keyFor(165), keyFor(166)
	provider(t, s, seller, "oracle")
	for _, k := range []ed25519.PrivateKey{r1, r2, r3, rootmate, funded, fresh} {
		register(t, s, k)
	}
	seasoned(t, s, r1, r2, r3, rootmate, funded)
	publish(t, s, seller, listing("forecast", nil))
	account := keyID(seller)
	agentURI := "agent:" + account
	const offeringURI = "x402:POST https://swarmmemo.com/@oracle/forecast"
	trustRows(t, s, map[string][2]any{account: {"domain:oracle.example", int64(250)}, keyID(rootmate): {"domain:oracle.example", int64(0)},
		keyID(r1): {keyID(r1), int64(900)}, keyID(r2): {keyID(r2), int64(300)}, keyID(r3): {keyID(r3), int64(300)}, keyID(funded): {keyID(funded), int64(300)}})

	// The agent: two judges at first (thin), then a third.
	run(t, s, reviewPost(t, r1, agentURI, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 90}}, ""))
	run(t, s, reviewPost(t, r2, agentURI, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 94}}, ""))
	if rep := readReputation(t, s, account); !rep.Reviewed.Agent.Thin || rep.Reviewed.Agent.Judgements != 2 || rep.Reviewed.Agent.Score != 70 {
		t.Fatalf("two judges: %+v", rep.Reviewed.Agent)
	}
	run(t, s, reviewPost(t, r3, agentURI, map[string]any{"verdict": "bad"}, ""))
	// What it sells: its offering, reviewed by two.
	run(t, s, reviewPost(t, r1, offeringURI, map[string]any{"verdict": "good"}, ""))
	run(t, s, reviewPost(t, r2, offeringURI, map[string]any{"verdict": "mixed"}, ""))
	// Backing: r1 vouches (its newest act about the agent); funded vouches
	// but the seller paid it; fresh vouches with no weight.
	for _, k := range []ed25519.PrivateKey{r1, funded, fresh} {
		if _, err := vouchAs(s, k, account, `{"schema":1,"value":1}`, randomID()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO ledger_transfers(id,from_account,to_account,resource,amount,fee,state,created_at,execute_at,done_at) VALUES('t1',?,?,'credit',100,0,'done',?,?,?)",
		account, keyID(funded), testTime-86400, testTime-86400, testTime-86400); err != nil {
		t.Fatal(err)
	}

	rep := readReputation(t, s, account)
	now := s.now().Unix()
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Reviewed: each subject's score is the reviews core's own aggregate.
	agentSub, _ := reviews.Canonical(reviews.TypeAgent, account, s.subjectOptions())
	offering := reviewSubjectOf(t, offeringURI)
	aggAgent, _, err := s.communityReviews(testContext, tx, agentSub, now, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	aggOffering, _, err := s.communityReviews(testContext, tx, offering, now, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	pa, na, _ := s.subjectJudgements(testContext, tx, agentSub, now)
	po, no, _ := s.subjectJudgements(testContext, tx, offering, now)
	work, err := workRecords(testContext, tx, []string{account}, now)
	if err != nil {
		t.Fatal(err)
	}
	score, ok, err := trust.Current(testContext, tx, account)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	standing, _, err := trust.CurrentStanding(testContext, tx, account, trust.DefaultParams(), now)
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()

	if rep.Reviewed.Agent.Score != aggAgent.ScoreShadow || rep.Reviewed.Agent.Thin != aggAgent.ScoreThin || rep.Reviewed.Agent.Judgements != 3 || rep.Reviewed.Agent.Subject != agentURI {
		t.Fatalf("agent score %+v, aggregate %d %v", rep.Reviewed.Agent, aggAgent.ScoreShadow, aggAgent.ScoreThin)
	}
	if len(rep.Reviewed.Owned) == 0 || rep.Reviewed.Owned[0].ID != offering.ID || rep.Reviewed.Owned[0].Score != aggOffering.ScoreShadow || rep.Reviewed.Owned[0].Judgements != 2 {
		t.Fatalf("owned %+v, aggregate %d", rep.Reviewed.Owned, aggOffering.ScoreShadow)
	}
	if want := scoreOf(po, no); rep.Reviewed.Sells == nil || *rep.Reviewed.Sells != want {
		t.Fatalf("sells %+v, want %+v", rep.Reviewed.Sells, want)
	}
	if want := scoreOf(append(pa, po...), na+no); rep.Reviewed.ReputationScore != want || want.Judgements != 5 {
		t.Fatalf("reviewed %+v, want %+v", rep.Reviewed.ReputationScore, want)
	}
	// Accountable: the current run's standing and its band.
	if a := rep.Accountable; a.Band != score.Band.Name() || a.Band != "proven" || a.StandingCents != standing.StandingCents || a.StandingCents != 250 ||
		a.FakeCost != standing.FakeCost || a.Band != standing.Band || a.Run != standing.Run || a.Run == 0 {
		t.Fatalf("accountable %+v, standing %+v", a, standing)
	}
	// Delivered: the work record.
	if !reflect.DeepEqual(rep.Delivered, work[account]) {
		t.Fatalf("delivered %+v, want %+v", rep.Delivered, work[account])
	}
	// Backed by: r1 (vouch, newest) and r2 (good review); not r3 (bad), the
	// rootmate (one root), funded (paid by the seller) or fresh (no weight).
	got := map[string]ReputationBacker{}
	for _, b := range rep.BackedBy.Backers {
		got[b.Account] = b
	}
	if rep.BackedBy.Count != 2 || len(got) != 2 || got[keyID(r1)].Act != judge.ActVouch || got[keyID(r1)].Band != "trusted" || got[keyID(r2)].Act != judge.ActReview ||
		rep.BackedBy.Backers[0].Account != keyID(r1) || rep.BackedBy.Backers[0].WeightPPM <= rep.BackedBy.Backers[1].WeightPPM {
		t.Fatalf("backed by %+v", rep.BackedBy)
	}
	if rep.Line != "Accountable: proven (about $2.50 to fake) · Reviewed 5 · Delivered 0 accepted · 0 paid · Backed by 2" || rep.Line != strings.Join(rep.Parts(), " · ") {
		t.Fatalf("line %q", rep.Line)
	}

	// Its own root's judgements are left out: the rootmate's review moves
	// the subject page's aggregate, never the reputation.
	before := reputationJSON(t, rep)
	run(t, s, reviewPost(t, rootmate, agentURI, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 100}}, ""))
	if page := readSubjectData(t, s, agentURI)["aggregate"].(ReviewAggregate); page.Reviews != 4 {
		t.Fatalf("page aggregate %+v", page)
	}
	if after := reputationJSON(t, readReputation(t, s, account)); after != before {
		t.Fatalf("own root moved the reputation:\n%s\n%s", before, after)
	}
	// Posts are not subjects: an up vote on the seller's post changes nothing.
	post := postAs(t, s, seller, Command{Room: "lobby", Text: "Forecasts on request."})
	if _, err := voteAs(s, r2, post, "1", randomID()); err != nil {
		t.Fatal(err)
	}
	if after := reputationJSON(t, readReputation(t, s, account)); after != before {
		t.Fatalf("a vote moved the reputation:\n%s\n%s", before, after)
	}

	// Every surface shows the same numbers.
	want := reputationJSON(t, readReputation(t, s, account))
	agent := run(t, s, Command{Operation: "agent.get", Target: account}).Agent
	if agent.Reputation == nil || reputationJSON(t, agent.Reputation) != want {
		t.Fatalf("agent.get reputation %s", reputationJSON(t, agent.Reputation))
	}
	if got := reputationJSON(t, readSubjectData(t, s, agentURI)["reputation"]); got != want {
		t.Fatalf("subject.get reputation %s", got)
	}
	var round Reputation
	if err := json.Unmarshal([]byte(want), &round); err != nil || reputationJSON(t, round) != want {
		t.Fatalf("round trip %v %s", err, reputationJSON(t, round))
	}
	if !strings.Contains(want, `"`+judge.ScoreField+`":`) {
		t.Fatalf("no %s in %s", judge.ScoreField, want)
	}
	var hit *find.Hit
	for _, h := range run(t, s, Command{Operation: "find", Query: "oracle", Kind: "agents"}).Data["results"].([]find.Hit) {
		if h.ID == account {
			hit = &h
		}
	}
	if hit == nil || !reflect.DeepEqual(hit.Reputation, round.Mark()) || hit.Reputation.Backers != 2 || hit.Reputation.Band != "proven" || hit.Reputation.Score != round.Reviewed.Score {
		t.Fatalf("find hit %+v", hit)
	}
}

// The reputation of an agent nothing names: neutral, thin, no backers.
func TestReputationEmpty(t *testing.T) {
	s := reputationTestStore(t)
	key := keyFor(167)
	register(t, s, key)
	rep := readReputation(t, s, keyID(key))
	if rep.Reviewed.Score != judge.Neutral || !rep.Reviewed.Thin || rep.Reviewed.Sells != nil || len(rep.Reviewed.Owned) != 0 || rep.BackedBy.Count != 0 ||
		rep.Accountable.Band != "signed" || rep.Accountable.Run != 0 || rep.Delivered == nil {
		t.Fatalf("empty reputation %+v", rep)
	}
	if rep.Line != "Accountable: signed (about $0.00 to fake) · Reviewed 0 (thin) · Delivered 0 accepted · 0 paid · Backed by 0" {
		t.Fatalf("line %q", rep.Line)
	}
}

// C312 drift test: one exclusion (RFC 0020 §3.3, judge.OwnSubject through
// ownRootReviewers) for every score. With reviews by the owner and by an
// account of its root on an offering and on the agent subject, subject.get's
// score_shadow (what /s/ID and the API show), the reputation's per-subject
// scores and Find's quality all read the same judgements: exactly those
// left when the own-root reviews are gone. The own-root reviews stay listed,
// labelled and weightless.
func TestOwnRootExclusionIsOneRule(t *testing.T) {
	s := reputationTestStore(t)
	seller, r1, r2, r3, rootmate := keyFor(170), keyFor(171), keyFor(172), keyFor(173), keyFor(174)
	provider(t, s, seller, "augur")
	for _, k := range []ed25519.PrivateKey{r1, r2, r3, rootmate} {
		register(t, s, k)
	}
	seasoned(t, s, seller, r1, r2, r3, rootmate)
	publish(t, s, seller, listing("forecast", nil))
	account := keyID(seller)
	agentURI := "agent:" + account
	const offeringURI = "x402:POST https://swarmmemo.com/@augur/forecast"
	trustRows(t, s, map[string][2]any{account: {"domain:augur.example", int64(250)}, keyID(rootmate): {"domain:augur.example", int64(300)},
		keyID(r1): {keyID(r1), int64(300)}, keyID(r2): {keyID(r2), int64(300)}, keyID(r3): {keyID(r3), int64(300)}})
	for _, uri := range []string{agentURI, offeringURI} {
		run(t, s, reviewPost(t, r1, uri, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 70}}, ""))
		run(t, s, reviewPost(t, r2, uri, map[string]any{"verdict": "mixed"}, ""))
		run(t, s, reviewPost(t, r3, uri, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 75}}, ""))
		run(t, s, reviewPost(t, rootmate, uri, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 100}}, ""))
	}
	run(t, s, reviewPost(t, seller, offeringURI, map[string]any{"verdict": "good", "dimensions": map[string]int{"good": 100}}, ""))

	type surfaces struct {
		agg     ReviewAggregate
		points  []judge.Point
		n       int64
		quality subjectQuality
	}
	read := func(sub reviews.Subject) surfaces {
		t.Helper()
		var out surfaces
		out.agg = readSubjectData(t, s, sub.ID)["aggregate"].(ReviewAggregate)
		tx, err := s.db.BeginTx(testContext, nil)
		if err != nil {
			t.Fatal(err)
		}
		out.points, out.n, err = s.subjectJudgements(testContext, tx, sub, s.now().Unix())
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		q, err := s.findQuality(testContext, s.now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		out.quality = q[sub.ID]
		return out
	}
	agentSub, _ := reviews.Canonical(reviews.TypeAgent, account, s.subjectOptions())
	offering := reviewSubjectOf(t, offeringURI)
	rep := readReputation(t, s, account)
	repScore := map[string]ReputationScore{agentSub.ID: rep.Reviewed.Agent.ReputationScore}
	for _, o := range rep.Reviewed.Owned {
		repScore[o.ID] = o.ReputationScore
	}
	with := map[string]surfaces{}
	for _, sub := range []reviews.Subject{agentSub, offering} {
		got := read(sub)
		with[sub.ID] = got
		want := scoreOf(got.points, got.n)
		if got.agg.ScoreShadow != want.Score || got.agg.ScoreThin != want.Thin || want.Judgements != 3 {
			t.Fatalf("%s: aggregate %d thin %v, judge.ScoreGrouped over the filtered set %+v", sub.URI(), got.agg.ScoreShadow, got.agg.ScoreThin, want)
		}
		if r, ok := repScore[sub.ID]; !ok || r != want {
			t.Fatalf("%s: reputation %+v, want %+v", sub.URI(), r, want)
		}
		if got.quality.Reviews != got.agg.Reviews || got.quality.ReviewMean != got.agg.VerdictMean || got.quality.ReviewWeight != got.agg.WeightTotal {
			t.Fatalf("%s: find quality %+v, aggregate %+v", sub.URI(), got.quality.Quality, got.agg)
		}
		// Listed, labelled and weightless: the rootmate's (and the owner's).
		page := readSubjectData(t, s, sub.ID)["reviews"].([]ReviewCard)
		own := 0
		for _, c := range page {
			if c.Author.Account == keyID(rootmate) || c.Author.Account == account {
				own++
				if c.Order == nil || c.Order.Weight != 0 || c.Order.RelationshipFactor != 0 {
					t.Fatalf("%s: own-root review weighs %+v", sub.URI(), c.Order)
				}
				if c.Author.Account == keyID(rootmate) && (!c.Order.OwnRoot || c.Order.Note != OwnRootNote || c.Order.V == 0 || c.Order.ClassWeight == 0) {
					t.Fatalf("%s: rootmate's review %+v", sub.URI(), c.Order)
				}
			} else if c.Order == nil || c.Order.OwnRoot || c.Order.Weight == 0 {
				t.Fatalf("%s: independent review %+v", sub.URI(), c.Order)
			}
		}
		if want := map[string]int{agentSub.ID: 1, offering.ID: 2}[sub.ID]; own != want || len(page) != got.agg.Reviews {
			t.Fatalf("%s: %d own-root reviews listed of %d", sub.URI(), own, len(page))
		}
	}

	// The filtered set is exactly the set without them: hide the own-root
	// reviews and every number stays.
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id IN (SELECT event_id FROM reviews WHERE account IN (?,?))", keyID(rootmate), account); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []reviews.Subject{agentSub, offering} {
		got, before := read(sub), with[sub.ID]
		if got.agg.ScoreShadow != before.agg.ScoreShadow || got.agg.ScoreThin != before.agg.ScoreThin || got.agg.WeightTotal != before.agg.WeightTotal ||
			got.agg.VerdictMean != before.agg.VerdictMean || !reflect.DeepEqual(got.agg.Dimensions, before.agg.Dimensions) || scoreOf(got.points, got.n) != scoreOf(before.points, before.n) {
			t.Fatalf("%s: without own-root reviews %+v, with them %+v", sub.URI(), got.agg, before.agg)
		}
	}
	if after := reputationJSON(t, readReputation(t, s, account)); after != reputationJSON(t, rep) {
		t.Fatalf("hiding own-root reviews moved the reputation:\n%s\n%s", reputationJSON(t, rep), after)
	}
}

// C782: the reputation's work accepts read their workers as one json_each
// set. A one-account IN (?) let SQLite run publicAccountSQL(w.worker) once
// per work_transitions row before the works join, a walk of all of an
// account's events per row when it has no public post (agent.get of a
// sealed-room member past its budget). The read finds the same acts, for
// one worker and for several, and its plan checks the worker only after
// the works row.
func TestReputationWorkerAcceptsSet(t *testing.T) {
	s := openTest(t, Config{Features: Features{Trust: TrustOn}})
	key, worker, other := keyFor(70), keyFor(71), keyFor(72)
	s.now = func() time.Time { return time.Unix(testTime-3*86400, 0) }
	postAs(t, s, key, Command{Room: "lobby", Text: "hello", RequestID: "t-hello"})
	postAs(t, s, worker, Command{Room: "lobby", Text: "for hire", RequestID: "t-hire"})
	item := createTestWork(t, s, key, "lobby", "request", 0)
	run(t, s, workCommand(s, key, Command{Operation: "work.accept", MessageID: item, Amount: claimAndSubmit(t, s, worker, item, "lobby")}))
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	acts := func(workers ...string) []string {
		raw, _ := json.Marshal(workers)
		var got []string
		if err := scanAll(testContext, s.db, workerAcceptActsSQL, []any{string(raw)}, func(r *sql.Rows) error {
			var requester, w string
			var at int64
			err := r.Scan(&requester, &w, &at)
			got = append(got, requester+">"+w)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := []string{keyID(key) + ">" + keyID(worker)}
	if got := acts(keyID(worker)); !reflect.DeepEqual(got, want) {
		t.Fatalf("one worker: %v", got)
	}
	if got := acts(keyID(other), keyID(worker)); !reflect.DeepEqual(got, want) {
		t.Fatalf("two workers: %v", got)
	}
	if got := acts(keyID(other)); len(got) != 0 {
		t.Fatalf("a worker with no accepts: %v", got)
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+workerAcceptActsSQL, `["`+keyID(other)+`"]`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	works, subquery := -1, -1
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if parent == 0 && works < 0 && strings.HasPrefix(detail, "SEARCH w ") {
			works = len(plan)
		}
		if parent == 0 && subquery < 0 && strings.HasPrefix(detail, "CORRELATED SCALAR SUBQUERY") {
			subquery = len(plan)
		}
		plan = append(plan, detail)
	}
	if works < 0 || subquery < works {
		t.Fatalf("the public checks run before the works join: %s", strings.Join(plan, "; "))
	}
}
