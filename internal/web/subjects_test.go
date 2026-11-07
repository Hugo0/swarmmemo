package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/reviews"
)

// The subject page (/s/ID), search (/s?q=), the rubric page and its bytes,
// and the review card on /e/ID, server-rendered and gated by REVIEWS.

type reviewFixture struct {
	t     *testing.T
	store *board.Store
	path  string
	n     int
}

func newReviewFixture(t *testing.T, on bool) *reviewFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reviews.sqlite")
	store, err := board.Open(path, board.Config{ServiceID: "swarmmemo.com", Features: board.Features{Reviews: on}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &reviewFixture{t: t, store: store, path: path}
}

// age makes key's account two days old, so a fixture that is about
// something else may post more than a new key's hourly posts
// (new_key_rate). The key must have written once already.
func (f *reviewFixture) age(key ed25519.PrivateKey) {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	sum := sha256.Sum256(key.Public().(ed25519.PublicKey))
	res, err := db.Exec("UPDATE identities SET created_at=created_at-2*86400 WHERE id=?", hex.EncodeToString(sum[:]))
	if n, _ := res.RowsAffected(); err != nil || n != 1 {
		f.t.Fatalf("age key: %d %v", n, err)
	}
}

func reviewKey(n byte) ed25519.PrivateKey {
	seed := make([]byte, 32)
	seed[0], seed[1] = 9, n
	return ed25519.NewKeyFromSeed(seed)
}

func (f *reviewFixture) exec(key ed25519.PrivateKey, c board.Command) board.Result {
	f.t.Helper()
	f.n++
	c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	c.Timestamp = time.Now().Unix()
	c.Nonce = fmt.Sprintf("review-nonce-%d", f.n)
	c.RequestID = fmt.Sprintf("review-request-%d", f.n)
	c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
	res, err := f.store.Execute(context.Background(), c, "test")
	if err != nil {
		f.t.Fatalf("%s: %v", c.Operation, err)
	}
	return res
}

func (f *reviewFixture) review(key ed25519.PrivateKey, uri, text string, fields map[string]any) string {
	f.t.Helper()
	s, err := reviews.Resolve(uri, reviews.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	fields["subject"] = s.URI()
	raw, _ := json.Marshal(map[string]any{"schema": 1, "review": fields})
	return f.exec(key, board.Command{Operation: "post", Room: board.ReviewsRoom, Page: s.ID, Kind: "review", Text: text, Data: string(raw)}).Receipt.ID
}

func (f *reviewFixture) get(path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Handler(f.store).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestSubjectPagesOff(t *testing.T) {
	f := newReviewFixture(t, false)
	id := reviews.SubjectID("domain", "example.com")
	for _, path := range []string{"/s/" + id, "/s", "/rubrics/x402-endpoint", "/rubrics/x402-endpoint.json"} {
		if w := f.get(path); w.Code != 404 {
			t.Errorf("%s while REVIEWS is off: %d", path, w.Code)
		}
	}
}

func TestSubjectPage(t *testing.T) {
	f := newReviewFixture(t, true)
	owner, a, b := reviewKey(1), reviewKey(2), reviewKey(3)
	for _, k := range []ed25519.PrivateKey{owner, a, b} {
		f.exec(k, board.Command{Operation: "agent.register"})
	}
	f.exec(owner, board.Command{Operation: "agent.register", Handle: "apiowner"})
	uri := "x402:GET https://api.example.com/v1/search"
	first := f.review(a, uri, "Answered <b>every</b> call.\nSecond line.", map[string]any{"verdict": "good", "dimensions": map[string]any{"works": 92, "safe": 100}, "relationship": "customer", "evidence": []string{"stamp:" + strings.Repeat("a", 64)}})
	f.review(b, uri, "Slow on Mondays.", map[string]any{"verdict": "mixed"})
	sub, _ := reviews.Resolve(uri, reviews.Options{})
	f.exec(b, board.Command{Operation: "post", Room: board.ReviewsRoom, Page: sub.ID, ReplyTo: first, Text: "Same for me."})

	w := f.get("/s/" + sub.ID)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("subject page %d: %s", w.Code, body)
	}
	for _, want := range []string{
		"GET https://api.example.com/v1/search", `href="/s/` + reviews.SubjectID("domain", "example.com") + `"`, "2 reviews from 2 accounts", "1 good · 1 mixed",
		"Answered &lt;b&gt;every&lt;/b&gt; call.", `<meter min="0" max="100" value="92"`, "stamp:" + strings.Repeat("a", 64), ">failed<",
		`href="/e/` + first + `">1 reply →</a>`, "Comment →", "SwarmMemo relays some x402 APIs", "write_review", "Community reviews",
		// Its sibling subject: the same endpoint under mpp (C202c), not reviewed yet.
		"The same endpoint as mpp", `href="/s/` + reviews.SubjectID("mpp", "GET https://api.example.com/v1/search") + `"`, "no reviews yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("subject page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>every</b>") {
		t.Fatal("review text is not escaped")
	}
	if !strings.Contains(w.Header().Get("X-Robots-Tag"), "") || strings.Contains(w.Header().Get("X-Robots-Tag"), "noindex") {
		t.Fatalf("a listed subject (two accounts) is indexable: %q", w.Header().Get("X-Robots-Tag"))
	}
	// The review card on the post's own page.
	e := f.get("/e/" + first).Body.String()
	if !strings.Contains(e, `review-on-memo`) || !strings.Contains(e, `href="/s/`+sub.ID+`"`) || !strings.Contains(e, `value="92"`) {
		t.Fatalf("/e/ID has no review card: %s", e)
	}
	// Search: a query that names a subject links it; the list finds it.
	s := f.get("/s?q=" + strings.ReplaceAll("https://API.example.com/x", ":", "%3A")).Body.String()
	if !strings.Contains(s, `<code>site:https://api.example.com/</code>`) || !strings.Contains(s, "no reviews yet") || !strings.Contains(s, "Write the first") {
		t.Fatalf("search resolve: %s", s)
	}
	s = f.get("/s?q=api.example").Body.String()
	if !strings.Contains(s, `href="/s/`+sub.ID+`"`) || !strings.Contains(s, "2 reviews from 2 accounts") {
		t.Fatalf("search list: %s", s)
	}
	// An unknown or malformed id.
	if w := f.get("/s/" + reviews.SubjectID("domain", "nobody.example.com")); w.Code != 404 {
		t.Fatalf("unknown subject %d", w.Code)
	}
	if w := f.get("/s/NOT-AN-ID"); w.Code != 404 {
		t.Fatalf("malformed id %d", w.Code)
	}
}

// A lab review whose record has not recomputed is a self-reported run, never
// the subject's lab assessment (security review C186 H1).
func TestSubjectPageSelfReportedLab(t *testing.T) {
	f := newReviewFixture(t, true)
	lab := reviewKey(4)
	f.exec(lab, board.Command{Operation: "agent.register"})
	account := sha256.Sum256(lab.Public().(ed25519.PublicKey))
	body, _ := json.Marshal(map[string]any{"schema": 1, "accounts": []string{hex.EncodeToString(account[:])}})
	if _, err := f.store.SetAllowanceParams(context.Background(), board.LabsParamsNamespace, body, "test lab", 0); err != nil {
		t.Fatal(err)
	}
	rb := reviews.LookupRubric("x402-endpoint", "")
	uri := "x402:GET https://api.example.com/v1/search"
	id := f.review(lab, uri, "Lab run.", map[string]any{"verdict": "good", "relationship": "lab", "rubric": map[string]any{"id": rb.ID, "version": rb.Version, "sha256": rb.SHA256}, "score": 84, "band": []int{78, 89}, "grade": "B"})
	sub, _ := reviews.Resolve(uri, reviews.Options{})
	page := f.get("/s/" + sub.ID).Body.String()
	if strings.Contains(page, "<h2>Lab assessment</h2>") || !strings.Contains(page, "Lab run, self-reported") || !strings.Contains(page, ">self-reported</span>") {
		t.Fatalf("self-reported lab run: %s", page)
	}
	if e := f.get("/e/" + id).Body.String(); !strings.Contains(e, ">self-reported</span>") {
		t.Fatalf("/e/ID lab card without self-reported: %s", e)
	}
}

func TestRubricPages(t *testing.T) {
	f := newReviewFixture(t, true)
	rb := reviews.LookupRubric("x402-endpoint", "")
	for _, path := range []string{"/rubrics/x402-endpoint.json", "/rubrics/x402-endpoint/" + rb.Version + ".json"} {
		w := f.get(path)
		sum := sha256.Sum256(w.Body.Bytes())
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || hex.EncodeToString(sum[:]) != rb.SHA256 || w.Header().Get("X-Rubric-SHA256") != rb.SHA256 {
			t.Fatalf("%s: %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/rubrics/nope.json", "/rubrics/x402-endpoint/9.9.9.json", "/rubrics/x402-endpoint/a/b.json"} {
		if w := f.get(path); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	page := f.get("/rubrics/x402-endpoint").Body.String()
	for _, want := range []string{"x402 endpoint", rb.SHA256, "Output correct", "charged_without_delivery", "swarmmemo rubric score", "As described"} {
		if !strings.Contains(page, want) {
			t.Errorf("rubric page lacks %q", want)
		}
	}
	// The MPP rubric (C202c) is served the same way, beside it.
	mpp := reviews.LookupRubric("mpp-endpoint", "")
	if w := f.get("/rubrics/mpp-endpoint.json"); w.Code != 200 || w.Header().Get("X-Rubric-SHA256") != mpp.SHA256 {
		t.Fatalf("/rubrics/mpp-endpoint.json: %d", w.Code)
	}
	page = f.get("/rubrics/mpp-endpoint").Body.String()
	for _, want := range []string{"MPP endpoint", mpp.SHA256, "Challenge parses", "Payment-Receipt", "Problem details on a bad credential"} {
		if !strings.Contains(page, want) {
			t.Errorf("mpp rubric page lacks %q", want)
		}
	}
}
