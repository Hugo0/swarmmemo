package board

import (
	"crypto/ed25519"
	"encoding/json"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/reviews"
	"swarmmemo/internal/trust"
)

// Subjects and reviews (RFC 0015 phase 1B, RFC 0019) on the real engine:
// the write rules, one current review per account and subject, evidence
// checks, ownership, the default order and its aggregate, lab and outside
// raters kept apart, reply counts and the pinned owner reply.

const reviewSubject = "x402:GET https://api.example.com/v1/search"

func reviewsStore(t *testing.T) *Store {
	t.Helper()
	return openTest(t, Config{Features: Features{Reviews: true}})
}

func reviewSubjectOf(t *testing.T, uri string) reviews.Subject {
	t.Helper()
	s, err := reviews.Resolve(uri, reviews.Options{OwnHosts: []string{"swarmmemo.com"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// reviewPost is a signed review of uri with the given data.review fields.
func reviewPost(t *testing.T, key ed25519.PrivateKey, uri string, fields map[string]any, supersedes string) Command {
	t.Helper()
	fields["subject"] = uri
	if fields["verdict"] == nil {
		fields["verdict"] = "good"
	}
	d := map[string]any{"schema": 1, "review": fields}
	if supersedes != "" {
		d["supersedes"] = supersedes
	}
	raw, _ := json.Marshal(d)
	return signed(key, Command{Operation: "post", Room: ReviewsRoom, Page: reviewSubjectOf(t, uri).ID, Kind: "review", Text: "It answered every call I paid for." + testPostSuffix(), Data: string(raw), RequestID: randomID()})
}

func readSubjectData(t *testing.T, s *Store, target string) map[string]any {
	t.Helper()
	return run(t, s, Command{Operation: "subject.get", Target: target}).Data
}

func TestReviewsOff(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(31)
	register(t, s, key)
	fails(t, s, reviewPost(t, key, reviewSubject, map[string]any{}, ""), "reviews_unavailable")
	fails(t, s, Command{Operation: "subject.get", Target: reviewSubject}, "reviews_unavailable")
	fails(t, s, Command{Operation: "subjects.list"}, "reviews_unavailable")
	// Off, room reviews is an ordinary room.
	run(t, s, signed(key, Command{Operation: "post", Room: ReviewsRoom, Text: "hello", RequestID: randomID()}))
	if s.SubjectsCapabilities() != nil || s.ReviewsCapabilities() != nil {
		t.Fatal("capabilities while off")
	}
}

func TestReviewWriteRules(t *testing.T) {
	s := reviewsStore(t)
	key := keyFor(32)
	register(t, s, key)
	sub := reviewSubjectOf(t, reviewSubject)
	ok := reviewPost(t, key, reviewSubject, map[string]any{}, "")

	// Anonymous posts carry no data, so no review.
	anon := ok
	anon.PublicKey, anon.Signature = "", ""
	fails(t, s, anon, "signature_required")
	for _, c := range []struct {
		mut  func(*Command)
		code string
	}{
		{func(c *Command) { c.Room = "lobby" }, "review_room"},
		{func(c *Command) { c.Kind = "note" }, "review_room"},
		{func(c *Command) { c.Page = "main" }, "review_page"},
		{func(c *Command) { c.Data = strings.Replace(c.Data, "GET https", "get https", 1) }, "invalid_review"},
		{func(c *Command) { c.Data = strings.Replace(c.Data, `"verdict":"good"`, `"verdict":"great"`, 1) }, "invalid_review"},
		{func(c *Command) {
			c.Data = strings.Replace(c.Data, reviewSubject, "x402:GET https://social.example.com/@someone", 1)
			c.Page = subjectID("x402", "GET https://social.example.com/@someone")
		}, "subject_refused"},
	} {
		bad := ok
		bad.Signature = ""
		c.mut(&bad)
		fails(t, s, signed(key, bad), c.code)
	}
	// A top-level post in room reviews is a review; a reply is a comment.
	fails(t, s, signed(key, Command{Operation: "post", Room: ReviewsRoom, Page: sub.ID, Text: "plain", RequestID: randomID()}), "review_required")
	first := run(t, s, ok).Receipt.ID
	run(t, s, signed(keyFor(33), Command{Operation: "post", Room: ReviewsRoom, Page: sub.ID, ReplyTo: first, Text: "Same here.", RequestID: randomID()}))
	// A review cannot be a reply.
	reply := reviewPost(t, key, reviewSubject, map[string]any{}, "")
	reply.Signature, reply.ReplyTo = "", first
	fails(t, s, signed(key, reply), "review_room")

	// One current review per account and subject: a second supersedes the first.
	_, err := s.Execute(testContext, reviewPost(t, key, reviewSubject, map[string]any{"verdict": "bad"}, ""), "test-origin")
	var e *Error
	if !errors.As(err, &e) || e.Code != "review_exists" || !strings.Contains(e.Message, first) || !strings.Contains(e.Message, reviewSubject) || strings.Contains(e.Message, "whole site") {
		t.Fatalf("second review: %v", err)
	}
	// A site subject's refusal says a bare URL names the whole site (C773).
	siteKey := keyFor(39)
	register(t, s, siteKey)
	site := "site:https://api.example.com/"
	siteFirst := run(t, s, reviewPost(t, siteKey, site, map[string]any{}, "")).Receipt.ID
	_, err = s.Execute(testContext, reviewPost(t, siteKey, site, map[string]any{"verdict": "mixed"}, ""), "test-origin")
	if !errors.As(err, &e) || e.Code != "review_exists" || !strings.Contains(e.Message, siteFirst) || !strings.Contains(e.Message, site) || !strings.Contains(e.Message, "whole site") || !strings.Contains(e.Message, "x402:METHOD URL") {
		t.Fatalf("second site review: %v", err)
	}
	second := run(t, s, reviewPost(t, key, reviewSubject, map[string]any{"verdict": "bad"}, first)).Receipt.ID
	var current int
	if err = s.db.QueryRow("SELECT current FROM reviews WHERE event_id=?", first).Scan(&current); err != nil || current != 0 {
		t.Fatalf("superseded review current=%d (%v)", current, err)
	}
	d := readSubjectData(t, s, sub.ID)
	list := d["reviews"].([]ReviewCard)
	if len(list) != 1 || list[0].ID != second || list[0].Verdict != "bad" || list[0].Root != first || list[0].Replies != 1 || list[0].Thread != "/e/"+first {
		t.Fatalf("reviews %+v", list)
	}
	// Supersession keeps the subject: a review of another subject is a new one.
	other := reviewPost(t, key, "domain:example.com", map[string]any{}, "")
	run(t, s, other)

	// The daily cap counts every review the account wrote today.
	for i := 0; i < ReviewsPerAccountDay-3; i++ {
		run(t, s, reviewPost(t, key, fmt.Sprintf("site:https://s%d.example.com/", i), map[string]any{}, ""))
	}
	_, err = s.Execute(testContext, reviewPost(t, key, "site:https://over.example.com/", map[string]any{}, ""), "test-origin")
	if !errors.As(err, &e) || e.Code != "review_daily_limit" || e.Status != 429 || e.RetryAfter <= 0 {
		t.Fatalf("over the daily cap: %v", err)
	}
	// An operator importer key rates a catalogue: its outside ratings have
	// the operator's cap, while its own reviews keep the agent cap.
	stamp := strings.Repeat("cd", 32)
	if _, err = s.db.Exec("INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?)", stamp, testTime, keyID(key)); err != nil {
		t.Fatal(err)
	}
	allowAccounts(t, s, ImportersParamsNamespace, key)
	outside := map[string]any{"relationship": "outside_source", "evidence": []string{"stamp:" + stamp}, "source": map[string]any{"name": "Index", "url": "https://index.example/r/1", "terms": "CC BY", "grade": "OPEN"}}
	run(t, s, reviewPost(t, key, "site:https://imported.example.com/", outside, ""))
	_, err = s.Execute(testContext, reviewPost(t, key, "site:https://over2.example.com/", map[string]any{}, ""), "test-origin")
	if !errors.As(err, &e) || e.Code != "review_daily_limit" {
		t.Fatalf("an importer's own review past the agent cap: %v", err)
	}
}

func subjectID(typ, key string) string { return reviews.SubjectID(typ, key) }

// allowAccounts sets an operator account list (importers or labs) to keys.
func allowAccounts(t *testing.T, s *Store, namespace string, keys ...ed25519.PrivateKey) {
	t.Helper()
	accounts := []string{}
	for _, k := range keys {
		accounts = append(accounts, keyID(k))
	}
	body, _ := json.Marshal(map[string]any{"schema": 1, "accounts": accounts})
	if _, err := s.SetAllowanceParams(testContext, namespace, body, "test "+namespace, 0); err != nil {
		t.Fatal(err)
	}
}

func rubricRef(id string) map[string]any {
	r := reviews.LookupRubric(id, "")
	return map[string]any{"id": r.ID, "version": r.Version, "sha256": r.SHA256}
}

func TestReviewWorkerKeyAndImporter(t *testing.T) {
	s := reviewsStore(t)
	key := keyFor(34)
	register(t, s, key)
	// A worker key (a grant) cannot review, whatever else it may post.
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := parsePostData(`{"schema":1,"review":{"subject":"` + reviewSubject + `","verdict":"good"}}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.checkReview(testContext, tx, Command{Room: ReviewsRoom, Kind: "review", Page: reviewSubjectOf(t, reviewSubject).ID}, actor{id: keyID(key), account: keyID(key), signed: true, grant: &delegationRow{}}, d, testTime)
	_ = tx.Rollback()
	var e *Error
	if !errors.As(err, &e) || e.Code != "review_worker_key" {
		t.Fatalf("worker key review: %v", err)
	}

	// outside_source is the operator's importer keys' only.
	stamp := strings.Repeat("ab", 32)
	if _, err = s.db.Exec("INSERT INTO notary_receipts(hash,time,account) VALUES(?,?,?)", stamp, testTime, keyID(key)); err != nil {
		t.Fatal(err)
	}
	outside := map[string]any{"relationship": "outside_source", "evidence": []string{"stamp:" + stamp}, "source": map[string]any{"name": "x402 Trust", "url": "https://x402trust.example/r/1", "terms": "public grade, attributed", "grade": "A"}}
	fails(t, s, reviewPost(t, key, reviewSubject, outside, ""), "review_relationship_reserved")
	body, _ := json.Marshal(map[string]any{"schema": 1, "accounts": []string{keyID(key)}})
	if _, err = s.SetAllowanceParams(testContext, ImportersParamsNamespace, body, "test importer", 0); err != nil {
		t.Fatal(err)
	}
	// The importers list opens outside_source only, never lab.
	fails(t, s, reviewPost(t, key, reviewSubject, map[string]any{"relationship": "lab", "rubric": rubricRef("x402-endpoint")}, ""), "review_relationship_reserved")
	listed := func() bool {
		t.Helper()
		var l bool
		if err := s.db.QueryRow("SELECT listed_at>0 FROM subjects WHERE id=?", reviewSubjectOf(t, reviewSubject).ID).Scan(&l); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return l
	}
	wasListed := listed()
	if wasListed {
		t.Fatal("the subject is listed before the import; the check below would prove nothing")
	}
	run(t, s, reviewPost(t, key, reviewSubject, outside, ""))
	// An importer's outside rating lists the subject (Find, sitemap).
	if !listed() {
		t.Fatalf("an imported rating did not list the subject (listed before: %v)", wasListed)
	}
	data := readSubjectData(t, s, reviewSubject)
	others, _ := data["other_raters"].([]ReviewCard)
	if len(others) != 1 || others[0].Source == nil || others[0].Source.Grade != "A" || others[0].Evidence[0].State != "verified" {
		t.Fatalf("other raters %+v", others)
	}
	if list := data["reviews"].([]ReviewCard); len(list) != 0 {
		t.Fatalf("an outside rater is in the community reviews: %+v", list)
	}
}

func TestReviewEvidenceAndOrder(t *testing.T) {
	s := reviewsStore(t)
	sub := reviewSubjectOf(t, reviewSubject)
	alice, bob, carol, lab := keyFor(40), keyFor(41), keyFor(42), keyFor(43)
	for _, k := range []ed25519.PrivateKey{alice, bob, carol, lab} {
		register(t, s, k)
	}
	// alice and bob are established: a public post more than a day old and
	// standing of C_ref (v = 1); carol has none.
	s.now = func() time.Time { return time.Unix(testTime-3*86400, 0) }
	run(t, s, signed(alice, Command{Operation: "post", Text: "hello from alice", RequestID: randomID(), Timestamp: testTime - 3*86400}))
	run(t, s, signed(bob, Command{Operation: "post", Text: "hello from bob", RequestID: randomID(), Timestamp: testTime - 3*86400}))
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	cRef := trust.DefaultParams().CRefCents
	setStanding(t, s, map[string]int64{keyID(alice): cRef, keyID(bob): cRef})

	// alice paid for a call to the resource (paid bucket); a call to another
	// resource, someone else's call and an old call all fail.
	if _, err := s.db.Exec("INSERT INTO x402_catalogue(id,bundler,url,method,pay_to,amount,first_seen,last_seen) VALUES('res1','x402','https://api.example.com/v1/search?q=x','GET','0xabc',1000,1,1),('res2','x402','https://other.example.com/','GET','0xabc',1000,1,1)"); err != nil {
		t.Fatal(err)
	}
	call := func(id, account, resource string, created int64, bucket string) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,state,public,prices_version,created_at) VALUES(?,?,'x402','call',?,?,'credit','done',?,1,?)`,
			id, account, "rk-"+id, "hold-"+id, `{"resource":"`+resource+`","http_status":200,"response_bytes":512}`, created); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO ledger_entries(day,created_at,kind,account,resource,bucket,amount,hold_id,params_version) VALUES(1,1,'commit',?,'credit',?,10,?,1)`, account, bucket, "hold-"+id); err != nil {
			t.Fatal(err)
		}
	}
	paid, other, theirs, old := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32), strings.Repeat("4", 32)
	call(paid, keyID(alice), "res1", testTime-3600, "paid")
	call(other, keyID(alice), "res2", testTime-3600, "paid")
	call(theirs, keyID(bob), "res1", testTime-3600, "free")
	call(old, keyID(alice), "res1", testTime-40*86400, "paid")
	// bob's fetch of the endpoint's host; a notary stamp of carol's.
	fetch := strings.Repeat("5", 32)
	if _, err := s.db.Exec(`INSERT INTO service_calls(id,account,service,method,request_key,hold_id,resource,state,body,prices_version,created_at) VALUES(?,?,'fetch','fetch','rk-f','','credit','done',?,1,?)`,
		fetch, keyID(bob), `{"url":"https://api.example.com/docs","raw_sha256":"`+strings.Repeat("e", 64)+`","fetched_at":1}`, testTime-60); err != nil {
		t.Fatal(err)
	}
	ev := func(ids ...string) []string { return ids }
	aliceID := run(t, s, reviewPost(t, alice, reviewSubject, map[string]any{"dimensions": map[string]any{"works": 90}, "relationship": "customer", "evidence": ev("call:"+paid, "call:"+other, "call:"+theirs, "call:"+old, "tx:eip155:8453:0x"+strings.Repeat("f", 64), "work:"+strings.Repeat("6", 32))}, "")).Receipt.ID
	run(t, s, reviewPost(t, bob, reviewSubject, map[string]any{"verdict": "mixed", "dimensions": map[string]any{"works": 50}, "evidence": ev("fetch:" + fetch)}, ""))
	run(t, s, reviewPost(t, carol, reviewSubject, map[string]any{"verdict": "bad"}, ""))
	r := reviews.LookupRubric("x402-endpoint", "")
	allowAccounts(t, s, LabsParamsNamespace, lab)
	run(t, s, reviewPost(t, lab, reviewSubject, map[string]any{"relationship": "lab", "rubric": map[string]any{"id": r.ID, "version": r.Version, "sha256": r.SHA256}, "score": 84, "band": []int{78, 89}, "grade": "B",
		"observations": []any{map[string]any{"check": "R4", "pass": 3, "fail": 0}}}, ""))

	data := readSubjectData(t, s, sub.ID)
	info := data["subject"].(SubjectInfo)
	if !info.Exists || !info.Listed || info.Parent == nil || info.Parent.URI != "domain:example.com" {
		t.Fatalf("subject %+v", info)
	}
	// A lab review without a recomputed record is a self-reported run, never
	// the subject's lab assessment (security review C186 H1).
	if _, ok := data["lab"]; ok {
		t.Fatalf("a lab run without a verified record is the lab assessment: %+v", data["lab"])
	}
	runs, _ := data["lab_runs"].([]ReviewCard)
	if len(runs) != 1 || runs[0].Relationship != "lab" || runs[0].Grade != "B" || *runs[0].Score != 84 || runs[0].LabVerdict != "proceed" || len(runs[0].Observations) != 1 {
		t.Fatalf("lab runs %+v", runs)
	}
	list := data["reviews"].([]ReviewCard)
	if len(list) != 3 || list[0].ID != aliceID {
		t.Fatalf("order %+v", list)
	}
	states := map[string]string{}
	for _, e := range list[0].Evidence {
		states[e.ID] = e.State
	}
	want := map[string]string{"call:" + paid: "verified", "call:" + other: "failed", "call:" + theirs: "failed", "call:" + old: "failed", "tx:eip155:8453:0x" + strings.Repeat("f", 64): "unknown", "work:" + strings.Repeat("6", 32): "failed"}
	for id, st := range want {
		if states[id] != st {
			t.Errorf("evidence %s: %s, want %s", id, states[id], st)
		}
	}
	a := list[0]
	if a.EvidenceClass != "paid_use" || a.Order.V != 1 || a.Order.ClassWeight != 1 || a.Order.Weight <= 0.99 {
		t.Fatalf("alice's order %+v class %s", a.Order, a.EvidenceClass)
	}
	b := list[1]
	if b.Author.Account != keyID(bob) || b.EvidenceClass != "observed" || b.Order.Weight < 0.39 || b.Order.Weight > 0.41 {
		t.Fatalf("bob's order %+v class %s", b.Order, b.EvidenceClass)
	}
	if c := list[2]; c.Order.V != 0 || c.Order.Weight != 0 {
		t.Fatalf("an account without standing weighs 0: %+v", c.Order)
	}
	agg := data["aggregate"].(ReviewAggregate)
	want0 := (1*1 + 0.4*0.5 + 2*0.5) / (1 + 0.4 + 2)
	if agg.Reviews != 3 || agg.Accounts != 3 || agg.ByVerdict["bad"] != 1 || agg.ByClass["paid_use"] != 1 || fmt.Sprintf("%.4f", agg.VerdictMean) != fmt.Sprintf("%.4f", want0) {
		t.Fatalf("aggregate %+v want mean %.4f", agg, want0)
	}
	if agg.Dimensions["works"] < 78 || agg.Dimensions["works"] > 79 {
		t.Fatalf("works mean %v", agg.Dimensions)
	}
	// The x402 summary and subject.resolve read the same index.
	sums, err := reviewSummaries(testContext, s.db, []string{sub.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil || len(sums) != 1 || sums[sub.ID].Reviews != 3 || sums[sub.ID].Accounts != 3 || sums[sub.ID].ByVerdict["mixed"] != 1 || sums[sub.ID].Lab != nil {
		t.Fatalf("summaries %+v (%v)", sums, err)
	}
	res := run(t, s, Command{Operation: "subject.resolve", Target: "GET https://API.example.com/v1/search?q=2"}).Data
	if res["subject"].(SubjectInfo).ID != sub.ID || res["reviews"].(ReviewSummary).Reviews != 3 {
		t.Fatalf("resolve %+v", res)
	}
	// message.get carries the review card part.
	m := run(t, s, Command{Operation: "message.get", MessageID: aliceID}).Messages[0]
	if m.Review == nil || m.Review.Verdict != "good" || m.Review.Subject.ID != sub.ID || len(m.Review.Evidence) != 6 {
		t.Fatalf("message review %+v", m.Review)
	}
	// subjects.list lists it (three accounts); a hidden review leaves the page.
	l := run(t, s, Command{Operation: "subjects.list", Query: "API.EXAMPLE"}).Data["subjects"].([]SubjectListing)
	if len(l) != 1 || l[0].ID != sub.ID || l[0].Reviews != 3 || l[0].Lab != "" {
		t.Fatalf("list %+v", l)
	}
	if _, err = s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", aliceID); err != nil {
		t.Fatal(err)
	}
	if list = readSubjectData(t, s, sub.ID)["reviews"].([]ReviewCard); len(list) != 2 {
		t.Fatalf("hidden review still shown: %d", len(list))
	}
}

func TestReviewOwnerAndThread(t *testing.T) {
	s := reviewsStore(t)
	owner, reviewer := keyFor(50), keyFor(51)
	register(t, s, owner)
	register(t, s, reviewer)
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,'domain','example.com','verified',1,1)", keyID(owner)); err != nil {
		t.Fatal(err)
	}
	sub := reviewSubjectOf(t, reviewSubject)
	id := run(t, s, reviewPost(t, reviewer, reviewSubject, map[string]any{"verdict": "bad"}, "")).Receipt.ID
	// Before any owner reply; then a stranger's and the owner's replies.
	run(t, s, signed(keyFor(52), Command{Operation: "post", Room: ReviewsRoom, Page: sub.ID, ReplyTo: id, Text: "Agreed.", RequestID: randomID()}))
	s.now = func() time.Time { return time.Unix(testTime+10, 0) }
	reply := run(t, s, signed(owner, Command{Operation: "post", Room: ReviewsRoom, Page: sub.ID, ReplyTo: id, Text: "Fixed in v2; please re-test.", RequestID: randomID()})).Receipt.ID
	// The owner's own review is raised to owner and weighs 0; it lists the subject.
	run(t, s, reviewPost(t, owner, reviewSubject, map[string]any{}, ""))
	data := readSubjectData(t, s, reviewSubject)
	info := data["subject"].(SubjectInfo)
	if len(info.Owners) != 1 || info.Owners[0].Account != keyID(owner) || info.Owners[0].Link != "domain:example.com" || !info.Listed {
		t.Fatalf("owners %+v", info)
	}
	var ownCard, theirCard ReviewCard
	for _, c := range data["reviews"].([]ReviewCard) {
		if c.Author.Account == keyID(owner) {
			ownCard = c
		} else {
			theirCard = c
		}
	}
	if ownCard.RelationshipRaised != "owner" || ownCard.Order.RelationshipFactor != 0 {
		t.Fatalf("owner's own review %+v", ownCard)
	}
	if theirCard.Replies != 2 || theirCard.OwnerReply == nil || theirCard.OwnerReply.ID != reply || theirCard.OwnerReply.Text != "Fixed in v2; please re-test." {
		t.Fatalf("thread %+v %+v", theirCard.Replies, theirCard.OwnerReply)
	}
	// An agent subject: the agent owns it; an unknown account is refused.
	fails(t, s, reviewPost(t, reviewer, "agent:"+strings.Repeat("0", 64), map[string]any{}, ""), "subject_not_found")
	run(t, s, reviewPost(t, reviewer, "agent:"+keyID(owner), map[string]any{}, ""))
	if got := readSubjectData(t, s, "agent:"+keyID(owner))["subject"].(SubjectInfo); len(got.Owners) != 1 || got.Owners[0].Link != "self" {
		t.Fatalf("agent owners %+v", got.Owners)
	}
}

func TestSubjectReads(t *testing.T) {
	s := reviewsStore(t)
	// Before any review: resolve and a read by TYPE:KEY answer; by id, 404.
	sub := reviewSubjectOf(t, "site:https://shop.example.com/")
	res := run(t, s, Command{Operation: "subject.resolve", Target: "https://Shop.Example.com/deep?x=1"}).Data["subject"].(SubjectInfo)
	if res.ID != sub.ID || res.Exists || res.URL != "/s/"+sub.ID {
		t.Fatalf("resolve %+v", res)
	}
	d := readSubjectData(t, s, sub.URI())
	if d["subject"].(SubjectInfo).Exists || len(d["reviews"].([]ReviewCard)) != 0 {
		t.Fatalf("unreviewed %+v", d)
	}
	fails(t, s, Command{Operation: "subject.get", Target: sub.ID}, "subject_not_found")
	fails(t, s, Command{Operation: "subject.resolve", Target: "mailto:a@b.example"}, "subject_refused")
	fails(t, s, Command{Operation: "subject.resolve", Target: "nonsense"}, "invalid_subject")
	fails(t, s, Command{Operation: "subjects.list", Kind: "planet"}, "invalid_kind")
	fails(t, s, Command{Operation: "subjects.list", Cursor: "x"}, "invalid_cursor")
	fails(t, s, Command{Operation: "subjects.list", Data: `{"schema":1,"sort":"score"}`}, "invalid_request")
	// One review lists nothing yet; a second account lists it.
	key1, key2 := keyFor(60), keyFor(61)
	register(t, s, key1)
	register(t, s, key2)
	run(t, s, reviewPost(t, key1, sub.URI(), map[string]any{}, ""))
	if l := run(t, s, Command{Operation: "subjects.list"}).Data["subjects"].([]SubjectListing); len(l) != 0 {
		t.Fatalf("listed after one review: %+v", l)
	}
	run(t, s, reviewPost(t, key2, sub.URI(), map[string]any{"verdict": "mixed"}, ""))
	l := run(t, s, Command{Operation: "subjects.list", Kind: "site", Limit: 1, Data: `{"schema":1,"sort":"recent"}`}).Data
	if list := l["subjects"].([]SubjectListing); len(list) != 1 || list[0].Accounts != 2 || l["has_more"] != false {
		t.Fatalf("list %+v", l)
	}
	// The review page pages.
	p := run(t, s, Command{Operation: "subject.get", Target: sub.ID, Limit: 1}).Data
	if len(p["reviews"].([]ReviewCard)) != 1 || p["has_more"] != true || p["next_cursor"] != "1" {
		t.Fatalf("page %+v", p)
	}
	p = run(t, s, Command{Operation: "subject.get", Target: sub.ID, Limit: 1, Cursor: "1"}).Data
	if len(p["reviews"].([]ReviewCard)) != 1 || p["has_more"] != false {
		t.Fatalf("second page %+v", p)
	}
	caps := s.ReviewsCapabilities()
	var rubricIDs []string
	for _, r := range caps["rubrics"].([]map[string]any) {
		rubricIDs = append(rubricIDs, r["id"].(string))
	}
	if strings.Join(rubricIDs, ",") != "mpp-endpoint,x402-endpoint" || s.SubjectsCapabilities()["types"] == nil {
		t.Fatalf("capabilities %+v", caps)
	}
}
