package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
)

// Accepted work as reputation (C300) on the real store, ledger and reviews
// core: the work record counts accepted (paid), rejected and expired claims
// on both sides (an accept counts at once, paid only once its reward is)
// and leaves out tiny rewards, linked keys, one trust root and
// two operator accounts; a rated accept posts exactly one real review
// through post, with verified work evidence, idempotent on a retry; and
// agent.get, subject.get, subject.resolve and Find show the one function's
// numbers.

// workRecordProtocolSection is PROTOCOL.md's generated work-record section:
// the rules from their one source (workreputation.go, reviews.ScoreBands).
func workRecordProtocolSection() string {
	var bands []string
	for _, b := range reviews.ScoreBands {
		bands = append(bands, fmt.Sprintf("`%s` from %d", b.Verdict, b.From))
	}
	return "- **Work record.** " + WorkRecordRule + "\n" +
		"- **Rated accept.** " + WorkRatingRule + fmt.Sprintf(" A rating is 0-%d, 50 neutral; its verdict: %s. An unrated accept stands for a score of %d (RFC 0020).\n", WorkRatingMax, strings.Join(bands, ", "), WorkAcceptDefaultScore)
}

func reputationStore(t *testing.T) *Store {
	t.Helper()
	c := rewardConfig()
	c.Features.Reviews = true
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, c)
	t.Cleanup(s.stopServices)
	setRewardParams(t, s)
	return s
}

// ratedAccept is requester's work.accept of id at fence carrying its signed
// review of worker (WorkRatingPost); data is the accept's data as sent.
func ratedAccept(t *testing.T, s *Store, requester ed25519.PrivateKey, id string, fence int64, worker string, rating int, note string) Command {
	t.Helper()
	post, err := WorkRatingPost(id, worker, rating, note)
	if err != nil {
		t.Fatal(err)
	}
	post.RequestID, post.Timestamp = randomID(), s.now().Unix()
	raw, _ := json.Marshal(signed(requester, post))
	d, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "review": json.RawMessage(raw)})
	return signed(requester, Command{Operation: "work.accept", MessageID: id, Amount: fence, Data: string(d), RequestID: randomID(), Timestamp: s.now().Unix()})
}

func acceptWork(t *testing.T, s *Store, requester ed25519.PrivateKey, id string, fence int64) {
	t.Helper()
	run(t, s, workCommand(s, requester, Command{Operation: "work.accept", MessageID: id, Amount: fence}))
}

func TestWorkRecordAndRatedAccept(t *testing.T) {
	s := reputationStore(t)
	owner, w1, w2, puppet, rootmate, op1, op2 := keyFor(210), keyFor(211), keyFor(212), keyFor(213), keyFor(214), keyFor(215), keyFor(216)
	for i, k := range []ed25519.PrivateKey{owner, w1, w2, puppet, rootmate, op1, op2} {
		run(t, s, signed(k, Command{Operation: "agent.register", Handle: "c300agent" + string(rune('a'+i))}))
	}
	requester, worker1, worker2 := keyID(owner), keyID(w1), keyID(w2)
	run(t, s, linkCommand(puppet, "identity.link", "ed25519", pubKey(owner))) // puppet names owner's key
	mintCredit(t, s, requester, allowance.Paid, 1_000_000)
	mintCredit(t, s, keyID(op1), allowance.Paid, 1_000_000)
	create := func(key ed25519.PrivateKey, reward int64) string {
		id := rewardRequest(t, s, key, "lobby")
		run(t, s, rewardCreate(s, key, id, reward, 0))
		return id
	}
	const reward = 12_000

	// A: w1's first result is rejected; w2 claims and lets the claim lapse;
	// w1 claims again and the requester accepts it with a rating.
	a := create(owner, reward)
	fence := claimAndSubmit(t, s, w1, a, "lobby")
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: a, Amount: fence, Reason: "Missing the tests."}))
	run(t, s, workCommand(s, w2, Command{Operation: "work.claim", MessageID: a, TTL: 600}))
	s.now = func() time.Time { return time.Unix(testTime+700, 0) }
	fence = claimAndSubmit(t, s, w1, a, "lobby")

	// A rating that names another subject, or cites other evidence, refuses
	// the whole accept: nothing changes.
	wrong := ratedAccept(t, s, owner, a, fence, worker2, 90, "")
	fails(t, s, wrong, "invalid_work_rating")
	if getTestWork(t, s, a).State != "submitted" {
		t.Fatal("a refused rating left the work changed")
	}
	accept := ratedAccept(t, s, owner, a, fence, worker1, 90, "Fast and correct.")
	res := run(t, s, accept)
	receipt, ok := res.Data["review"].(*Receipt)
	if !ok || receipt.ID == "" || res.Data["ack"].(WorkAck).State != "accepted" {
		t.Fatalf("rated accept result %+v", res.Data)
	}
	// Exactly one review, through the shared path: in room reviews, by the
	// requester, about agent:WORKER, relationship customer, work evidence
	// verified (the work names its worker).
	sub, _ := reviews.Canonical(reviews.TypeAgent, worker1, reviews.Options{})
	if n := sqlCount(t, s, "SELECT count(*) FROM reviews WHERE account=?", requester); n != 1 {
		t.Fatalf("reviews by the requester: %d", n)
	}
	var subject, relationship, class, verdict string
	var good int
	if err := s.db.QueryRow("SELECT subject,relationship,evidence_class,verdict,good FROM reviews WHERE event_id=?", receipt.ID).Scan(&subject, &relationship, &class, &verdict, &good); err != nil {
		t.Fatal(err)
	}
	if subject != sub.ID || relationship != "customer" || class != "work" || verdict != "good" || good != 90 {
		t.Fatalf("review row: %s %s %s", subject, relationship, class)
	}
	msg := run(t, s, Command{Operation: "message.get", MessageID: receipt.ID}).Messages[0]
	if msg.Room != ReviewsRoom || msg.Author != requester || msg.Text != "Fast and correct." || msg.Kind != "review" {
		t.Fatalf("review message %+v", msg)
	}
	if e := evidenceOf(t, s, receipt.ID)["work:"+a]; e.State != "verified" || e.Detail["role"] != "requester" {
		t.Fatalf("work evidence %+v", e)
	}
	// The event is the requester's own signed post: its payload verifies as one.
	var payload string
	if err := s.db.QueryRow("SELECT payload FROM events WHERE id=?", receipt.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"operation":"post"`) || !strings.Contains(payload, `"room":"reviews"`) {
		t.Fatalf("review payload %s", payload)
	}
	// A retry of the same signed accept is the stored result; the signed
	// review sent on its own is a duplicate of itself. Still one review.
	again := run(t, s, accept)
	if again.Data["review"] == nil || sqlCount(t, s, "SELECT count(*) FROM reviews WHERE account=?", requester) != 1 {
		t.Fatalf("retry %+v", again.Data)
	}
	var inner struct {
		Review Command `json:"review"`
	}
	_ = json.Unmarshal([]byte(accept.Data), &inner)
	if r := run(t, s, inner.Review); r.Receipt == nil || !r.Receipt.Duplicate || r.Receipt.ID != receipt.ID {
		t.Fatalf("the review replayed alone: %+v", r.Receipt)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM reviews WHERE account=?", requester); n != 1 {
		t.Fatalf("reviews after replays: %d", n)
	}

	// B: a tiny reward, accepted: not counted.
	b := create(owner, 50)
	acceptWork(t, s, owner, b, claimAndSubmit(t, s, w1, b, "lobby"))
	// C: a worker naming the requester's key as its own: left out.
	c := create(owner, reward)
	acceptWork(t, s, owner, c, claimAndSubmit(t, s, puppet, c, "lobby"))
	// D: a worker the trust run puts under the requester's root: left out.
	if _, err := s.db.Exec("INSERT INTO trust_runs(id,as_of,params_version,state,inputs,started_at) VALUES(1,0,1,'done','',0)"); err != nil {
		t.Fatal(err)
	}
	for _, acct := range []string{requester, keyID(rootmate)} {
		if _, err := s.db.Exec("INSERT INTO trust_scores(run_id,account,root,proof_collateral,flow_a,flow_b,flow,collateral,tier,weight_ppm,parts) VALUES(1,?,'domain:one.example',0,0,0,0,0,3,0,'{}')", acct); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO trust_current(account,run_id) VALUES(?,1)", acct); err != nil {
			t.Fatal(err)
		}
	}
	d := create(owner, reward)
	acceptWork(t, s, owner, d, claimAndSubmit(t, s, rootmate, d, "lobby"))
	// E: two operator accounts (both on the exclude list) paying each other.
	for _, k := range []ed25519.PrivateKey{op1, op2} {
		if _, err := s.FunnelExclude(testContext, keyID(k), true); err != nil {
			t.Fatal(err)
		}
	}
	e := create(op1, reward)
	acceptWork(t, s, op1, e, claimAndSubmit(t, s, op2, e, "lobby"))
	// F: w1's accept whose credit reward is still on hold: accepted, not
	// yet paid (its rating would already show under Backed by).
	f := create(owner, reward)
	acceptWork(t, s, owner, f, claimAndSubmit(t, s, w1, f, "lobby"))
	if _, err := s.db.Exec("UPDATE work_rewards SET state='held' WHERE work_id=?", f); err != nil {
		t.Fatal(err)
	}

	records, err := workRecords(testContext, s.db, nil, s.now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	w1Rec := records[worker1]
	if w1Rec == nil || w1Rec.AsWorker.Accepted != 2 || w1Rec.AsWorker.Paid != 1 || w1Rec.AsWorker.Rejected != 1 || w1Rec.AsWorker.ExpiredClaims != 0 ||
		w1Rec.AsWorker.PaidCredits != reward || w1Rec.AsWorker.PaidUSDC != "0" || w1Rec.AsWorker.LastAcceptedAt != testTime+700 {
		t.Fatalf("w1's record %+v", w1Rec)
	}
	if r := records[worker2]; r == nil || r.AsWorker != (WorkRecordSide{ExpiredClaims: 1, PaidUSDC: "0"}) {
		t.Fatalf("w2's record %+v", r)
	}
	if r := records[requester]; r == nil || r.AsRequester.Accepted != 2 || r.AsRequester.Paid != 1 || r.AsRequester.Rejected != 1 || r.AsRequester.ExpiredClaims != 1 || r.AsRequester.PaidCredits != reward {
		t.Fatalf("the requester's record %+v", r)
	}
	for name, k := range map[string]ed25519.PrivateKey{"puppet": puppet, "rootmate": rootmate, "op1": op1, "op2": op2} {
		if r := records[keyID(k)]; r != nil {
			t.Errorf("%s is counted: %+v", name, r)
		}
	}
	// One account's read (narrowed to its own work, C701) is exactly its
	// line of the board-wide read, and empty for an account left out.
	empty := &WorkRecord{AsWorker: WorkRecordSide{PaidUSDC: "0"}, AsRequester: WorkRecordSide{PaidUSDC: "0"}}
	for _, k := range []ed25519.PrivateKey{puppet, rootmate, op1, op2} {
		if one, err := workRecords(testContext, s.db, []string{keyID(k)}, s.now().Unix()); err != nil || !reflect.DeepEqual(one[keyID(k)], empty) {
			t.Errorf("%s alone %+v (%v), want empty", keyID(k)[:12], one[keyID(k)], err)
		}
	}
	for account, want := range records {
		one, err := workRecords(testContext, s.db, []string{account}, s.now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(one[account], want) {
			t.Errorf("%s alone %+v, board-wide %+v", account[:12], one[account], want)
		}
	}

	// Every surface shows the one function's numbers.
	want := w1Rec
	agent := run(t, s, Command{Operation: "agent.get", Target: worker1}).Agent
	if agent.Reviews == nil || !reflect.DeepEqual(agent.Reviews.WorkRecord, want) {
		t.Fatalf("agent.get work_record %+v", agent.Reviews)
	}
	if got := readSubjectData(t, s, "agent:"+worker1)["work_record"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("subject.get work_record %+v", got)
	}
	resolved := run(t, s, Command{Operation: "subject.resolve", Target: "agent:" + worker1}).Data["reviews"].(ReviewSummary)
	if !reflect.DeepEqual(resolved.WorkRecord, want) || resolved.Reviews != 1 {
		t.Fatalf("subject.resolve %+v", resolved)
	}
	if want.Line() != "Delivered 2 accepted · 1 paid" {
		t.Fatalf("line %q", want.Line())
	}
	var hit *find.Hit
	for _, h := range run(t, s, Command{Operation: "find", Query: "c300agentb", Kind: "agents"}).Data["results"].([]find.Hit) {
		if h.ID == worker1 {
			hit = &h
		}
	}
	if hit == nil || hit.AcceptedTasks != int(want.AsWorker.Accepted) || hit.PaidTasks != int(want.AsWorker.Paid) || !strings.Contains(hit.Why, want.Line()) {
		t.Fatalf("find hit %+v", hit)
	}
}

// A rated accept is the requester's: a named reviewer's accept may not
// carry one, and the rating's review replaces the requester's current
// review of the worker (the newest paid work speaks).
func TestRatedAcceptRules(t *testing.T) {
	s := reputationStore(t)
	owner, worker := keyFor(220), keyFor(221)
	register(t, s, owner)
	register(t, s, worker)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1_000_000)
	var reviewsBy []string
	for i := range 2 {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, rewardCreate(s, owner, id, 12_000, 0))
		fence := claimAndSubmit(t, s, worker, id, "lobby")
		rating := []int{90, 50}[i]
		res := run(t, s, ratedAccept(t, s, owner, id, fence, keyID(worker), rating, ""))
		reviewsBy = append(reviewsBy, res.Data["review"].(*Receipt).ID)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM reviews WHERE account=? AND current=1", keyID(owner)); n != 1 {
		t.Fatalf("current reviews %d", n)
	}
	var current, text string
	if err := s.db.QueryRow("SELECT event_id,verdict FROM reviews WHERE account=? AND current=1", keyID(owner)).Scan(&current, &text); err != nil {
		t.Fatal(err)
	}
	if current != reviewsBy[1] || text != "mixed" {
		t.Fatalf("current review %s %s, want %s", current, text, reviewsBy[1])
	}
	if m := run(t, s, Command{Operation: "message.get", MessageID: current}).Messages[0]; m.Text != WorkRatingTextDefault {
		t.Fatalf("default text %q", m.Text)
	}
	// The worker may not rate (only the requester accepts here anyway); a
	// rating whose signer differs from the accept's is refused.
	id := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, id, 12_000, 0))
	fence := claimAndSubmit(t, s, worker, id, "lobby")
	post, _ := WorkRatingPost(id, keyID(worker), 90, "")
	post.Timestamp = s.now().Unix()
	raw, _ := json.Marshal(signed(worker, post))
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "review": json.RawMessage(raw)})
	fails(t, s, signed(owner, Command{Operation: "work.accept", MessageID: id, Amount: fence, Data: string(data), Timestamp: s.now().Unix()}), "invalid_work_rating")
	// The review field belongs to work.accept only.
	fails(t, s, signed(owner, Command{Operation: "work.reject", MessageID: id, Amount: fence, Reason: "no", Data: string(data), Timestamp: s.now().Unix()}), "invalid_work_data")
}

// A rating is 0-100, 50 neutral (RFC 0020): it is the review's good
// dimension and reads as a verdict by reviews.ScoreVerdict's bands; an
// unrated accept stands for WorkAcceptDefaultScore.
func TestWorkRatingPostScale(t *testing.T) {
	worker, id := keyID(keyFor(230)), strings.Repeat("a", 32)
	for rating, verdict := range map[int]string{100: "good", 65: "good", 64: "mixed", 50: "mixed", 35: "mixed", 34: "bad", 10: "bad", 9: "unusable", 0: "unusable"} {
		post, err := WorkRatingPost(id, worker, rating, "")
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Review struct {
				Verdict    string         `json:"verdict"`
				Dimensions map[string]int `json:"dimensions"`
				Evidence   []string       `json:"evidence"`
			} `json:"review"`
		}
		if err = json.Unmarshal([]byte(post.Data), &d); err != nil || d.Review.Verdict != verdict || d.Review.Dimensions["good"] != rating || d.Review.Evidence[0] != "work:"+id {
			t.Errorf("rating %d: %+v %v", rating, d.Review, err)
		}
	}
	for _, bad := range []int{-1, 101} {
		if _, err := WorkRatingPost(id, worker, bad, ""); err == nil {
			t.Errorf("rating %d accepted", bad)
		}
	}
	if reviews.ScoreVerdict(WorkAcceptDefaultScore) != "good" {
		t.Fatal("the unrated accept's default reads as good")
	}
	// The page the clients compute for agent:ACCOUNT (clients/python
	// agent_subject_id, clients/javascript agentSubjectID test the same vector).
	if got := reviews.SubjectID(reviews.TypeAgent, strings.Repeat("a", 64)); got != "gv3vfww72ravwux2dfdyxmfhbm" {
		t.Fatalf("agent subject id vector: %s", got)
	}
}

// C604: a rated accept's review is bound to its verdict (one per accepted
// result of the requester's own task), so the daily review cap neither
// counts nor refuses it; every other review stays capped, and a review
// that merely cites an accepted work frees nothing more.
func TestRatedAcceptPastReviewCap(t *testing.T) {
	s := reputationStore(t)
	owner, worker := keyFor(240), keyFor(241)
	register(t, s, owner)
	register(t, s, worker)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1_000_000)
	task := func() (string, int64) {
		id := rewardRequest(t, s, owner, "lobby")
		run(t, s, rewardCreate(s, owner, id, 12_000, 0))
		return id, claimAndSubmit(t, s, worker, id, "lobby")
	}
	// One normal review short of the cap, then two rated accepts.
	for i := 0; i < ReviewsPerAccountDay-1; i++ {
		run(t, s, reviewPost(t, owner, fmt.Sprintf("site:https://c604-%d.example.com/", i), map[string]any{}, ""))
	}
	var accepted []string
	for _, rating := range []int{80, 40} {
		id, fence := task()
		res := run(t, s, ratedAccept(t, s, owner, id, fence, keyID(worker), rating, ""))
		receipt := res.Data["review"].(*Receipt)
		var good int
		if err := s.db.QueryRow("SELECT good FROM reviews WHERE event_id=?", receipt.ID).Scan(&good); err != nil || good != rating {
			t.Fatalf("rating stored %d (%v), want %d", good, err, rating)
		}
		accepted = append(accepted, id)
	}
	// The ratings did not count: the cap's last normal review still posts.
	cites := map[string]any{"relationship": "customer", "evidence": []string{"work:" + accepted[0]}}
	run(t, s, reviewPost(t, owner, "site:https://c604-last.example.com/", cites, ""))
	// Now at the cap: a normal review is refused, even one citing an
	// accepted work (each accepted result frees one review, its rating).
	fails(t, s, reviewPost(t, owner, "site:https://c604-over.example.com/", map[string]any{}, ""), "review_daily_limit")
	fails(t, s, reviewPost(t, owner, "site:https://c604-over2.example.com/", map[string]any{"relationship": "customer", "evidence": []string{"work:" + accepted[1]}}, ""), "review_daily_limit")
	// At the cap, a third rated accept still goes through, rating stored.
	id, fence := task()
	res := run(t, s, ratedAccept(t, s, owner, id, fence, keyID(worker), 95, "Spot on."))
	if res.Data["ack"].(WorkAck).State != "accepted" {
		t.Fatalf("rated accept at the cap %+v", res.Data)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM reviews r JOIN review_evidence v ON v.event_id=r.event_id AND v.evidence=? WHERE r.account=? AND r.good=95", "work:"+id, keyID(owner)); n != 1 {
		t.Fatalf("rating at the cap stored %d times", n)
	}
	// The verdict is one per result: accepting it again refuses, no second rating.
	if _, err := s.Execute(testContext, ratedAccept(t, s, owner, id, fence, keyID(worker), 10, ""), "test-origin"); err == nil {
		t.Fatal("a second rated accept of one result went through")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM reviews r JOIN review_evidence v ON v.event_id=r.event_id AND v.evidence=? WHERE r.account=?", "work:"+id, keyID(owner)); n != 1 {
		t.Fatalf("ratings of one result: %d", n)
	}
	// A refused rating says the accept goes through without it.
	id, fence = task()
	msg := failsWith(t, s, ratedAccept(t, s, owner, id, fence, keyID(owner), 90, ""), "invalid_work_rating")
	if !strings.Contains(msg, UnratedAcceptNote) {
		t.Fatalf("refusal %q lacks the unrated-accept note", msg)
	}
	acceptWork(t, s, owner, id, fence)
}
