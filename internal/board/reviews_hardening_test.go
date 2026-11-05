package board

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/reviews"
	"swarmmemo/internal/rubric"
	"swarmmemo/internal/trust"
)

// The C186 security review's findings, each pinned (C197): lab keys and
// recomputed records (H1), the operator's room (H2), records recomputed
// after commit and capped (H3), paid work (M1), bound stamps (M2), listing
// by verified owners (M3), records bound to subject and time (M4), large
// subjects read in SQL (M5), no lockout (L1), erasure order (ops).

// labObservations is rubric testdata obs-healthy.json moved in time so its
// as_of is at, about subject key (an x402 key), with edit applied.
func labObservations(t *testing.T, at int64, key string, edit func(map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "rubric", "testdata", "obs-healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	asOf, err := time.Parse(time.RFC3339, doc["as_of"].(string))
	if err != nil {
		t.Fatal(err)
	}
	shift := time.Unix(at, 0).Sub(asOf)
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		case string:
			if tm, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return tm.Add(shift).UTC().Format(time.RFC3339)
			}
		}
		return v
	}
	walk(doc)
	doc["subject"].(map[string]any)["key"] = key
	if edit != nil {
		edit(doc)
	}
	out, _ := json.Marshal(doc)
	return out
}

// labRecord stores obs as a doc of key's and returns a lab review's fields
// claiming exactly what obs scores to, with obs as its record.
func labRecord(t *testing.T, s *Store, key ed25519.PrivateKey, obs []byte) map[string]any {
	t.Helper()
	rb := reviews.LookupRubric("x402-endpoint", "")
	parsed, err := rubric.ParseObservations(obs, rb.Rubric)
	if err != nil {
		t.Fatal(err)
	}
	want := rubric.Score(rb.Rubric, parsed)
	sum, doc := sha256Hex(obs), randomID()
	if _, err = s.db.Exec("INSERT INTO docs(id,account,title,version,hash,bytes,stored,created_at,updated_at,visibility) VALUES(?,?,'lab record',1,?,?,?,1,1,'unlisted')", doc, keyID(key), sum, len(obs), len(obs)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO doc_versions(id,doc,version,title,text,bytes,hash,author,created_at) VALUES(?,?,1,'lab record',?,?,?,?,1)", randomID(), doc, string(obs), len(obs), sum, keyID(key)); err != nil {
		t.Fatal(err)
	}
	f := map[string]any{"relationship": "lab", "verdict": reviews.UniversalVerdict(want.Verdict), "rubric": rubricRef("x402-endpoint"),
		"score": want.Score, "band": []int{want.Band[0], want.Band[1]}, "grade": want.Grade, "record": map[string]any{"sha256": sum, "doc": doc}}
	if len(want.Flags) > 0 {
		f["flags"] = want.Flags
	}
	// The scorer's dimensions, na ones left out (as the lab writes them):
	// a grade shows only while every one is measured (C752).
	dims := map[string]int{}
	for _, d := range want.Dimensions {
		if !d.NA {
			dims[d.ID] = d.Score
		}
	}
	if len(dims) > 0 {
		f["dimensions"] = dims
	}
	return f
}

func recordRow(t *testing.T, cards []ReviewCard) ReviewEvidence {
	t.Helper()
	for _, e := range cards[0].Evidence {
		if strings.HasPrefix(e.ID, RecordEvidencePrefix) {
			return e
		}
	}
	t.Fatalf("no record row: %+v", cards[0].Evidence)
	return ReviewEvidence{}
}

func recompute(t *testing.T, s *Store) int {
	t.Helper()
	n, err := s.recomputeRecords(testContext)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// H1, H3, M4: only a listed lab key posts a lab assessment; its record is
// recomputed after commit (unknown, pending, until then), and only a
// verified one is the subject's lab assessment, its subjects.list verdict
// and its x402 summary. A record about another subject, or dated after the
// review or more than 30 days before it, fails; Score runs once per record.
func TestReviewLabRecordAfterCommit(t *testing.T) {
	s := reviewsStore(t)
	lab, other, peer := keyFor(70), keyFor(71), keyFor(72)
	for _, k := range []ed25519.PrivateKey{lab, other, peer} {
		register(t, s, k)
	}
	sub := reviewSubjectOf(t, reviewSubject)
	key := strings.TrimPrefix(reviewSubject, "x402:")
	fields := labRecord(t, s, lab, labObservations(t, testTime-3600, key, nil))
	fails(t, s, reviewPost(t, lab, reviewSubject, fields, ""), "review_relationship_reserved")
	allowAccounts(t, s, LabsParamsNamespace, lab)
	first := run(t, s, reviewPost(t, lab, reviewSubject, fields, "")).Receipt.ID
	run(t, s, reviewPost(t, peer, reviewSubject, map[string]any{}, "")) // lists the subject

	// Posted, not yet recomputed: a self-reported run, no lab assessment.
	data := readSubjectData(t, s, sub.ID)
	if _, ok := data["lab"]; ok {
		t.Fatal("a pending record is the lab assessment")
	}
	if e := recordRow(t, data["lab_runs"].([]ReviewCard)); e.State != "unknown" || e.Detail["reason"] != RecordPending {
		t.Fatalf("pending record %+v", e)
	}
	if n := recompute(t, s); n != 1 {
		t.Fatalf("recomputed %d records", n)
	}
	data = readSubjectData(t, s, sub.ID)
	card, ok := data["lab"].(ReviewCard)
	if !ok || card.ID != first {
		t.Fatalf("verified lab assessment %+v", data["lab"])
	}
	if e := recordRow(t, []ReviewCard{card}); e.State != "verified" || e.Detail["matches"] != true || e.Detail["as_of"] == nil {
		t.Fatalf("recomputed record %+v", e)
	}
	sums, err := reviewSummaries(testContext, s.db, []string{sub.ID})
	// good is unmeasured (no paid call): the summary shows no letter, the card keeps the stored one (C752)
	if err != nil || sums[sub.ID].Lab["grade"] != nil || sums[sub.ID].Lab["grade_note"] != "no grade (good not measured)" || card.Grade != fields["grade"] || card.ShownGrade != "" || sums[sub.ID].Reviews != 1 {
		t.Fatalf("summary %+v (%v)", sums, err)
	}
	if l := run(t, s, Command{Operation: "subjects.list"}).Data["subjects"].([]SubjectListing); len(l) != 1 || l[0].Lab != fields["verdict"] {
		t.Fatalf("list %+v", l)
	}
	// The same record again is scored from the cache.
	second := run(t, s, reviewPost(t, lab, reviewSubject, fields, first)).Receipt.ID
	recompute(t, s)
	if card, _ := readSubjectData(t, s, sub.ID)["lab"].(ReviewCard); card.ID != second || s.reviewRecords.scored != 1 {
		t.Fatalf("cached recompute: lab %s, Score ran %d times", card.ID, s.reviewRecords.scored)
	}
	// A claim the record does not support fails and leaves the headline.
	wrong := map[string]any{}
	for k, v := range fields {
		wrong[k] = v
	}
	wrong["grade"] = map[string]string{"A": "B", "B": "C", "C": "D", "D": "E", "E": "F", "F": "A"}[fields["grade"].(string)]
	run(t, s, reviewPost(t, lab, reviewSubject, wrong, second))
	recompute(t, s)
	data = readSubjectData(t, s, sub.ID)
	if _, ok := data["lab"]; ok {
		t.Fatal("a failed record is the lab assessment")
	}
	if e := recordRow(t, data["lab_runs"].([]ReviewCard)); e.State != "failed" || e.Detail["reason"] != "recomputed_result_differs" {
		t.Fatalf("differing claim %+v", e)
	}
	if sums, _ = reviewSummaries(testContext, s.db, []string{sub.ID}); sums[sub.ID].Lab != nil {
		t.Fatalf("a failed record in the summary: %+v", sums[sub.ID])
	}
	// M4: bound to the review's subject and time.
	for i, c := range []struct {
		at     int64
		key    string
		reason string
	}{
		{testTime - 3600, "GET https://api.example.com/v1/other", "observations_of_another_subject"},
		{testTime + 3600, "", "as_of_after_the_review"},
		{testTime - 31*86400, "", "as_of_older_than_30_days"},
	} {
		uri := fmt.Sprintf("x402:GET https://api.example.com/v1/m4-%d", i)
		k := c.key
		if k == "" {
			k = strings.TrimPrefix(uri, "x402:")
		}
		run(t, s, reviewPost(t, lab, uri, labRecord(t, s, lab, labObservations(t, c.at, k, nil)), ""))
		recompute(t, s)
		d := readSubjectData(t, s, uri)
		if e := recordRow(t, d["lab_runs"].([]ReviewCard)); e.State != "failed" || e.Detail["reason"] != c.reason {
			t.Errorf("%s: %+v", c.reason, e)
		}
	}
	// Another account's doc is never the record.
	foreign := labRecord(t, s, other, labObservations(t, testTime-3600, "GET https://api.example.com/v1/foreign", nil))
	run(t, s, reviewPost(t, lab, "x402:GET https://api.example.com/v1/foreign", foreign, ""))
	recompute(t, s)
	if e := recordRow(t, readSubjectData(t, s, "x402:GET https://api.example.com/v1/foreign")["lab_runs"].([]ReviewCard)); e.Detail["reason"] != "not_your_doc_version" {
		t.Fatalf("foreign doc %+v", e)
	}
}

// H3: the background recomputer on the real engine: woken by the commit,
// it reads and writes on the pool outside any transaction.
func TestReviewRecordWorker(t *testing.T) {
	s := reviewsStore(t)
	lab := keyFor(73)
	register(t, s, lab)
	allowAccounts(t, s, LabsParamsNamespace, lab)
	ctx, cancel := context.WithCancel(testContext)
	s.startReviewRecords(ctx)
	defer s.stopReviewRecords()
	defer cancel()
	key := strings.TrimPrefix(reviewSubject, "x402:")
	run(t, s, reviewPost(t, lab, reviewSubject, labRecord(t, s, lab, labObservations(t, testTime-60, key, nil)), ""))
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok := readSubjectData(t, s, reviewSubject)["lab"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the recomputer never verified the record")
		}
	}
}

// H3: an account's lab records of the day that failed or still wait are
// capped; a lab review without a record is not.
func TestReviewRecordLimit(t *testing.T) {
	s := reviewsStore(t)
	lab := keyFor(74)
	register(t, s, lab)
	allowAccounts(t, s, LabsParamsNamespace, lab)
	post := func(i int) Command {
		f := map[string]any{"relationship": "lab", "rubric": rubricRef("x402-endpoint"), "record": map[string]any{"sha256": strings.Repeat("c", 64), "doc": randomID()}}
		return reviewPost(t, lab, fmt.Sprintf("x402:GET https://api.example.com/v1/limit-%d", i), f, "")
	}
	for i := 0; i < ReviewRecordFailuresPerDay; i++ {
		run(t, s, post(i))
	}
	fails(t, s, post(99), "review_record_limit")
	recompute(t, s) // they fail: no such doc
	fails(t, s, post(99), "review_record_limit")
	run(t, s, reviewPost(t, lab, "x402:GET https://api.example.com/v1/no-record", map[string]any{"relationship": "lab", "rubric": rubricRef("x402-endpoint")}, ""))
}

// H2: a review goes only into the operator's public room reviews.
func TestReviewRoomTaken(t *testing.T) {
	for _, row := range []struct{ visibility, owner string }{{"public", keyID(keyFor(76))}, {"private", ""}} {
		s := reviewsStore(t)
		key := keyFor(75)
		register(t, s, key)
		if _, err := s.db.Exec("INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,?,?,1)", ReviewsRoom, row.visibility, row.owner); err != nil {
			t.Fatal(err)
		}
		fails(t, s, reviewPost(t, key, reviewSubject, map[string]any{}, ""), "review_room_taken")
		var n int
		if err := s.db.QueryRow("SELECT (SELECT count(*) FROM events WHERE room=?)+(SELECT count(*) FROM reviews)", ReviewsRoom).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%+v: %d rows published (%v)", row, n, err)
		}
		if _, err := s.db.Exec("UPDATE rooms SET visibility='public',owner='' WHERE name=?", ReviewsRoom); err != nil {
			t.Fatal(err)
		}
		run(t, s, reviewPost(t, key, reviewSubject, map[string]any{}, ""))
	}
}

// work inserts accepted public work of requester's naming the subject, with
// a reward to worker in state reward ("" for none).
func work(t *testing.T, s *Store, requester ed25519.PrivateKey, worker, reward string) string {
	t.Helper()
	id := run(t, s, signed(requester, Command{Operation: "post", Text: "Please test " + reviewSubject, RequestID: randomID()})).Receipt.ID
	if _, err := s.db.Exec("INSERT INTO works(id,requester,title,capabilities,state,generation,created_at,updated_at,deadline,worker) VALUES(?,?,'test it','[]','accepted','g',?,?,?,?)", id, keyID(requester), testTime-100, testTime-50, testTime+86400, worker); err != nil {
		t.Fatal(err)
	}
	if reward != "" {
		if _, err := s.db.Exec("INSERT INTO work_rewards(work_id,requester,amount,fee,hold_id,state,worker,created_at,settled_at) VALUES(?,?,10,0,?,?,?,?,?)", id, keyID(requester), "hold-"+id, reward, worker, testTime-100, testTime-50); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func evidenceOf(t *testing.T, s *Store, id string) map[string]ReviewEvidence {
	t.Helper()
	m := run(t, s, Command{Operation: "message.get", MessageID: id}).Messages[0]
	out := map[string]ReviewEvidence{}
	for _, e := range m.Review.Evidence {
		out[e.ID] = e
	}
	return out
}

// M1: work is evidence only when accepted with its reward paid between two
// accounts; requester and worker may both cite it, and it counts once.
func TestReviewWorkEvidence(t *testing.T) {
	s := reviewsStore(t)
	alice, bob, carol := keyFor(80), keyFor(81), keyFor(82)
	for _, k := range []ed25519.PrivateKey{alice, bob, carol} {
		register(t, s, k)
	}
	paid := work(t, s, alice, keyID(bob), "paid")
	held := work(t, s, alice, keyID(bob), "held")
	self := work(t, s, carol, keyID(carol), "paid")
	bobReview := run(t, s, reviewPost(t, bob, reviewSubject, map[string]any{"evidence": []string{"work:" + paid, "work:" + held}}, "")).Receipt.ID
	got := evidenceOf(t, s, bobReview)
	if e := got["work:"+paid]; e.State != "verified" || e.Detail["role"] != "worker" {
		t.Fatalf("paid work %+v", e)
	}
	if e := got["work:"+held]; e.State != "failed" || e.Detail["reason"] != "work_reward_not_paid" {
		t.Fatalf("unpaid work %+v", e)
	}
	carolReview := run(t, s, reviewPost(t, carol, reviewSubject, map[string]any{"evidence": []string{"work:" + self}}, "")).Receipt.ID
	if e := evidenceOf(t, s, carolReview)["work:"+self]; e.State != "failed" || e.Detail["reason"] != "work_not_accepted_between_two_accounts" {
		t.Fatalf("self-paid work %+v", e)
	}
	aliceReview := run(t, s, reviewPost(t, alice, reviewSubject, map[string]any{"evidence": []string{"work:" + paid}}, "")).Receipt.ID
	if e := evidenceOf(t, s, aliceReview)["work:"+paid]; e.State != "verified" || e.Detail["counted_for"] != bobReview {
		t.Fatalf("the requester's citation %+v", e)
	}
	classes := map[string]string{}
	for _, c := range readSubjectData(t, s, reviewSubject)["reviews"].([]ReviewCard) {
		classes[c.ID] = c.EvidenceClass
		if c.ID == aliceReview && c.Order.Note == "" {
			t.Errorf("alice's order has no note: %+v", c.Order)
		}
	}
	if classes[bobReview] != "work" || classes[aliceReview] != "opinion" || classes[carolReview] != "opinion" {
		t.Fatalf("classes %v", classes)
	}
	// bob's new version keeps the work (alice's citation counted for none).
	bobNext := run(t, s, reviewPost(t, bob, reviewSubject, map[string]any{"evidence": []string{"work:" + paid}}, bobReview)).Receipt.ID
	if e := evidenceOf(t, s, bobNext)["work:"+paid]; e.State != "verified" || e.Detail["counted_for"] != nil {
		t.Fatalf("bob's new version %+v", e)
	}
}

// C319: a rated accept's own review of its worker verifies on the accept
// alone, before the reward is paid (a held credit, USDC owed until
// work.settle): the requester's accept is itself the attestation. Any
// other reviewer, including the worker reviewing the requester over the
// same work, still needs the reward paid.
func TestReviewWorkEvidenceRequesterAttest(t *testing.T) {
	s := reviewsStore(t)
	alice, bob := keyFor(85), keyFor(86)
	register(t, s, alice)
	register(t, s, bob)
	held := work(t, s, alice, keyID(bob), "held")
	aliceOfBob := reviews.TypeAgent + ":" + keyID(bob)
	aliceReview := run(t, s, reviewPost(t, alice, aliceOfBob, map[string]any{"evidence": []string{"work:" + held}}, "")).Receipt.ID
	if e := evidenceOf(t, s, aliceReview)["work:"+held]; e.State != "verified" || e.Detail["role"] != "requester" || e.Detail["reward"] != "owed" {
		t.Fatalf("the requester's own review, unpaid: %+v", e)
	}
	bobOfAlice := reviews.TypeAgent + ":" + keyID(alice)
	bobReview := run(t, s, reviewPost(t, bob, bobOfAlice, map[string]any{"evidence": []string{"work:" + held}}, "")).Receipt.ID
	if e := evidenceOf(t, s, bobReview)["work:"+held]; e.State != "failed" || e.Detail["reason"] != "work_reward_not_paid" {
		t.Fatalf("another reviewer still needs payment: %+v", e)
	}
}

// C319: workEvidenceAttestBackfill re-verifies review_evidence rows that
// failed as work_reward_not_paid before the fix, for the work's own
// requester reviewing its worker, once an accepted work's requester-worker
// evidence qualifies under the new rule; it changes nothing else (another
// account's row, or one that fails for another reason, stays as it was),
// and a second run is a no-op.
func TestWorkEvidenceAttestBackfill(t *testing.T) {
	s := reviewsStore(t)
	alice, bob, carol := keyFor(87), keyFor(88), keyFor(89)
	register(t, s, alice)
	register(t, s, bob)
	register(t, s, carol)
	held := work(t, s, alice, keyID(bob), "held")
	selfPaid := work(t, s, carol, keyID(carol), "paid")
	aliceOfBob := reviews.TypeAgent + ":" + keyID(bob)
	carolOfCarol := reviews.TypeAgent + ":" + keyID(carol)
	// Pre-fix rows: computed as failed before C319 (the requester's own
	// citation, and an unrelated failure that the fix leaves alone).
	aliceReview := run(t, s, reviewPost(t, alice, aliceOfBob, map[string]any{}, "")).Receipt.ID
	carolReview := run(t, s, reviewPost(t, carol, carolOfCarol, map[string]any{}, "")).Receipt.ID
	for _, row := range []struct{ eventID, workID string }{{aliceReview, held}, {carolReview, selfPaid}} {
		if _, err := s.db.Exec("INSERT INTO review_evidence(event_id,evidence,class,state,checked_at,detail) VALUES(?,?,'work','failed',?,?)",
			row.eventID, "work:"+row.workID, testTime, `{"reason":"work_reward_not_paid"}`); err != nil {
			t.Fatal(err)
		}
	}
	// openTest already ran the backfill once, at store open, before these
	// rows existed (recorded in meta, n=0 then); reset the marker so the
	// run below sees them, as a real upgrade's first start would.
	if _, err := s.db.Exec("DELETE FROM meta WHERE key=?", workEvidenceAttestBackfillKey); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	n, err := workEvidenceAttestBackfill(testContext, tx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		var state, detail string
		_ = tx.QueryRow("SELECT state,detail FROM review_evidence WHERE event_id=? AND evidence=?", aliceReview, "work:"+held).Scan(&state, &detail)
		t.Fatalf("backfilled: %d; alice's row now %s %s", n, state, detail)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if e := evidenceOf(t, s, aliceReview)["work:"+held]; e.State != "verified" || e.Detail["role"] != "requester" || e.Detail["reward"] != "owed" {
		t.Fatalf("the requester's row was not re-verified: %+v", e)
	}
	if e := evidenceOf(t, s, carolReview)["work:"+selfPaid]; e.State != "failed" || e.Detail["reason"] != "work_reward_not_paid" {
		t.Fatalf("the self-paid row changed: %+v", e)
	}
	// Idempotent: recorded in meta, a second run changes nothing.
	tx2, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if n, err = workEvidenceAttestBackfill(testContext, tx2); err != nil || n != 0 {
		t.Fatalf("second run: %d, %v", n, err)
	}
	if err = tx2.Commit(); err != nil {
		t.Fatal(err)
	}
}

// M2: a stamp is observed evidence only of a cited fetch's raw bytes.
func TestReviewStampBinding(t *testing.T) {
	s := reviewsStore(t)
	bob, carol := keyFor(83), keyFor(84)
	register(t, s, bob)
	register(t, s, carol)
	raw, other := strings.Repeat("e", 64), strings.Repeat("f", 64)
	fetch := strings.Repeat("5", 32)
	if _, err := s.db.Exec(`INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,state,body,prices_version,created_at) VALUES(?,?,'fetch','fetch','rk-f','','credit','done',?,1,?)`,
		fetch, keyID(bob), `{"url":"https://api.example.com/docs","raw_sha256":"`+raw+`","fetched_at":1}`, testTime-60); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?),(?,?,?)", raw, testTime, keyID(bob), other, testTime, keyID(carol)); err != nil {
		t.Fatal(err)
	}
	bound := run(t, s, reviewPost(t, bob, reviewSubject, map[string]any{"evidence": []string{"fetch:" + fetch, "stamp:" + raw}}, "")).Receipt.ID
	if e := evidenceOf(t, s, bound)["stamp:"+raw]; e.State != "verified" || e.Class != "observed" || e.Detail["fetch"] != "fetch:"+fetch {
		t.Fatalf("bound stamp %+v", e)
	}
	loose := run(t, s, reviewPost(t, carol, reviewSubject, map[string]any{"evidence": []string{"stamp:" + other}}, "")).Receipt.ID
	if e := evidenceOf(t, s, loose)["stamp:"+other]; e.State != "verified" || e.Class != "opinion" || e.Detail["note"] != "stamp_not_of_a_cited_fetch" {
		t.Fatalf("unbound stamp %+v", e)
	}
	for _, c := range readSubjectData(t, s, reviewSubject)["reviews"].([]ReviewCard) {
		if c.ID == loose && c.EvidenceClass != "opinion" || c.ID == bound && c.EvidenceClass != "observed" {
			t.Fatalf("%s class %s", c.ID, c.EvidenceClass)
		}
	}
}

// M3: a declared owner relationship lists nothing; a verified owner does.
func TestReviewDeclaredOwnerNotListed(t *testing.T) {
	s := reviewsStore(t)
	claimer, owner := keyFor(85), keyFor(86)
	register(t, s, claimer)
	register(t, s, owner)
	run(t, s, reviewPost(t, claimer, reviewSubject, map[string]any{"relationship": "owner"}, ""))
	if info := readSubjectData(t, s, reviewSubject)["subject"].(SubjectInfo); info.Listed {
		t.Fatal("a declared owner listed the subject")
	}
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'domain','example.com','verified',1,1)", keyID(owner)); err != nil {
		t.Fatal(err)
	}
	run(t, s, reviewPost(t, owner, "domain:example.com", map[string]any{"relationship": "owner"}, ""))
	if info := readSubjectData(t, s, "domain:example.com")["subject"].(SubjectInfo); !info.Listed {
		t.Fatal("the verified owner's review did not list the subject")
	}
}

// L1: a current review that can take no new version (hidden, or at
// MaxVersions) is replaced by a fresh review.
func TestReviewReplacesFrozenCurrent(t *testing.T) {
	s := reviewsStore(t)
	key := keyFor(87)
	register(t, s, key)
	hidden := run(t, s, reviewPost(t, key, reviewSubject, map[string]any{}, "")).Receipt.ID
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", hidden); err != nil {
		t.Fatal(err)
	}
	fails(t, s, reviewPost(t, key, reviewSubject, map[string]any{}, hidden), "supersede_hidden")
	fresh := run(t, s, reviewPost(t, key, reviewSubject, map[string]any{"verdict": "mixed"}, "")).Receipt.ID
	if list := readSubjectData(t, s, reviewSubject)["reviews"].([]ReviewCard); len(list) != 1 || list[0].ID != fresh {
		t.Fatalf("after a hidden review %+v", list)
	}
	// A review with MaxVersions versions.
	uri := "domain:example.com"
	maxed := run(t, s, reviewPost(t, key, uri, map[string]any{}, "")).Receipt.ID
	fails(t, s, reviewPost(t, key, uri, map[string]any{}, ""), "review_exists")
	for i := 1; i < MaxVersions; i++ {
		if _, err := s.db.Exec(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,origin)
 SELECT display_seq,?,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,id FROM events WHERE id=?`, randomID(), maxed); err != nil {
			t.Fatal(err)
		}
	}
	run(t, s, reviewPost(t, key, uri, map[string]any{"verdict": "bad"}, ""))
	if list := readSubjectData(t, s, uri)["reviews"].([]ReviewCard); len(list) != 1 || list[0].Verdict != "bad" {
		t.Fatalf("after a version-maxed review %+v", list)
	}
}

// M5: a subject with 50,000 current reviews reads quickly and whole: the
// order and aggregate run in SQL, a page loads only its cards, and id lists
// go in chunks. They are all fresh opinions of one day by accounts with
// standing, so the burst cap holds the day's weight to ReviewBurstCap.
func TestReviewManyReviews(t *testing.T) {
	if testing.Short() {
		t.Skip("large fixture: 50k reviews under a scale time bound; runs in full without -short")
	}
	s := reviewsStore(t)
	key := keyFor(88)
	register(t, s, key)
	run(t, s, reviewPost(t, key, reviewSubject, map[string]any{"dimensions": map[string]any{"works": 80}}, ""))
	sub := reviewSubjectOf(t, reviewSubject)
	const n = 50000
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	event, err := tx.Prepare(`INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient) VALUES(?,?,?,?,'fixture review','review',?,?,'','','','',?,'','','')`)
	if err != nil {
		t.Fatal(err)
	}
	review, err := tx.Prepare(`INSERT INTO reviews(event_id,root_event,subject,account,author,root,verdict,relationship,works,evidence_class,seasoned,review,created_at) VALUES(?,?,?,?,?,?,?,'customer',?,'opinion',1,'{}',?)`)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	mixed := 0
	for i := range n {
		id, account := fmt.Sprintf("%032x", i+1), fmt.Sprintf("%064x", i+1)
		ids[i] = id
		verdict := []string{"good", "mixed", "bad"}[i%3]
		if verdict == "mixed" {
			mixed++
		}
		if _, err = event.Exec(1000000+i, id, ReviewsRoom, sub.ID, account, account, testTime-3600); err != nil {
			t.Fatal(err)
		}
		if _, err = review.Exec(id, id, sub.ID, account, account, account, verdict, 50, testTime-3600); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Each fixture reviewer holds standing of C_ref (v = 1) in the latest run;
	// key, whose review is of another day, has none.
	runID := setStanding(t, s, map[string]int64{})
	if _, err = s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO trust_scores(run_id,account,root,proof_collateral,flow_a,flow_b,flow,collateral,tier,weight_ppm,parts) SELECT ?,printf('%064x',i),printf('%064x',i),0,0,0,0,?,2,1000000,'{}' FROM n`,
		n, runID, trust.DefaultParams().CRefCents); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d := readSubjectData(t, s, sub.ID)
	took := time.Since(start)
	agg := d["aggregate"].(ReviewAggregate)
	page := d["reviews"].([]ReviewCard)
	if agg.Reviews != n+1 || agg.Accounts != n+1 || agg.ByVerdict["mixed"] != mixed || len(page) != ReviewPageDefault || d["has_more"] != true || d["next_cursor"] != fmt.Sprint(ReviewPageDefault) {
		t.Fatalf("aggregate %+v, page %d, has_more %v", agg, len(page), d["has_more"])
	}
	if agg.WeightTotal < ReviewBurstCap-0.01 || agg.WeightTotal > ReviewBurstCap+0.01 || page[0].Order.BurstScale >= 1 {
		t.Fatalf("burst cap: weight total %v, first %+v", agg.WeightTotal, page[0].Order)
	}
	t.Logf("subject.get over %d reviews: %v", n+1, took)
	if took > 10*time.Second {
		t.Fatalf("subject.get took %v", took)
	}
	deep := run(t, s, Command{Operation: "subject.get", Target: sub.ID, Cursor: fmt.Sprint(reviewOffsetCursorMax), Limit: ReviewPageMax}).Data
	if len(deep["reviews"].([]ReviewCard)) != ReviewPageMax {
		t.Fatalf("deep page %d", len(deep["reviews"].([]ReviewCard)))
	}
	sums, err := reviewSummaries(testContext, s.db, append(ids[:1200:1200], sub.ID))
	if err != nil || sums[sub.ID].Reviews != n+1 || sums[sub.ID].Accounts != n+1 {
		t.Fatalf("summaries over chunks %+v (%v)", sums[sub.ID], err)
	}
	rtx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer rtx.Rollback()
	cards, err := cardsByID(testContext, rtx, ids[:1200])
	if err != nil || len(cards) != 1200 || cards[1199].ID != ids[1199] {
		t.Fatalf("cards over chunks %d (%v)", len(cards), err)
	}
}

// Ops: review rows reference their event, so with foreign keys on, an
// event with a review cannot be deleted before its review rows; and no
// code path deletes events today (an erasure added later deletes
// review_evidence, then reviews, first: see reviewSchema).
func TestReviewRowsBlockEventErasure(t *testing.T) {
	s := reviewsStore(t)
	key := keyFor(89)
	register(t, s, key)
	id := run(t, s, reviewPost(t, key, reviewSubject, map[string]any{}, "")).Receipt.ID
	if _, err := s.db.Exec("DELETE FROM events WHERE id=?", id); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("an event with review rows was deleted: %v", err)
	}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(strings.ToUpper(string(b)), "DELETE FROM EVENTS") {
			t.Errorf("%s deletes events: delete their review_evidence and reviews rows first, and update this test", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
