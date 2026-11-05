package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"html"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/reviews"
)

// Reviews on every surface (C200): the agent page's reviews block, the
// "Rated on SwarmMemo" badges and their copy-paste snippets, Message the
// reviewer on each card, the first-review path on /s?q=, and the subject
// page's AggregateRating from evidence-backed reviews only.

// wellFormedXML parses body as one XML document.
func wellFormedXML(t *testing.T, body []byte) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		if _, err := dec.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, body)
		}
	}
}

func TestBadgeSVGIsSafe(t *testing.T) {
	for _, f := range []badgeFacts{
		{Label: "SwarmMemo", Value: "3 reviews · mostly good", Tone: "good", Spoken: "Rated on SwarmMemo: 3 reviews, mostly good"},
		{Label: "SwarmMemo lab", Value: "B 78 · proceed", Tone: "good", Spoken: "x"},
		{Label: `<script>alert(1)</script>`, Value: `"&'<>]]>`, Tone: "nonsense", Spoken: `"><script>alert(1)</script>`},
	} {
		body := badgeSVG(f)
		wellFormedXML(t, body)
		s := string(body)
		for _, bad := range []string{"<script", "javascript:", "href", "<foreignObject", "onload", "@import", "url(http"} {
			if strings.Contains(strings.ToLower(s), strings.ToLower(bad)) {
				t.Fatalf("badge carries %q:\n%s", bad, s)
			}
		}
		if !strings.Contains(s, `role="img"`) || !strings.Contains(s, "<title>"+html.EscapeString(f.Spoken)+"</title>") || !strings.Contains(s, "prefers-color-scheme:dark") {
			t.Fatalf("badge lacks its accessible name or dark mode:\n%s", s)
		}
	}
}

func TestBadgeRoutes(t *testing.T) {
	f := newReviewFixture(t, true)
	agent, a, b := reviewKey(60), reviewKey(61), reviewKey(62)
	for _, k := range []struct {
		key    ed25519.PrivateKey
		handle string
	}{{agent, "subjectagent"}, {a, "reviewera"}, {b, "reviewerb"}} {
		f.exec(k.key, board.Command{Operation: "agent.register", Handle: k.handle})
	}
	sum := sha256.Sum256(agent.Public().(ed25519.PublicKey))
	fp := hex.EncodeToString(sum[:])
	get := func(path string) (*httptest.ResponseRecorder, string) {
		w := f.get(path)
		return w, w.Body.String()
	}

	// An agent no review names yet: an honest empty badge, cacheable.
	w, body := get("/agent/" + fp + "/badge.svg")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml; charset=utf-8" || !strings.Contains(body, "no reviews yet") ||
		w.Header().Get("Cache-Control") != BadgeCacheControl || w.Header().Get("ETag") == "" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("empty agent badge %d %v\n%s", w.Code, w.Header(), body)
	}
	wellFormedXML(t, []byte(body))
	// The ETag revalidates.
	r := httptest.NewRequest("GET", "/agent/"+fp+"/badge.svg", nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	rec := httptest.NewRecorder()
	Handler(f.store).ServeHTTP(rec, r)
	if rec.Code != 304 {
		t.Fatalf("revalidation %d", rec.Code)
	}

	uri := "agent:" + fp
	f.review(a, uri, "Answered my question <b>fast</b>.", map[string]any{"verdict": "good"})
	f.review(b, uri, "Good, if slow.", map[string]any{"verdict": "good"})
	sub, _ := reviews.Resolve(uri, reviews.Options{})
	for _, path := range []string{"/agent/" + fp + "/badge.svg", "/agent/subjectagent/badge.svg", "/s/" + sub.ID + "/badge.svg"} {
		w, body := get(path)
		if w.Code != 200 || !strings.Contains(body, "2 reviews") {
			t.Fatalf("%s: %d\n%s", path, w.Code, body)
		}
		wellFormedXML(t, []byte(body))
	}
	// A well-formed id no review names, an unknown agent, a bad id.
	if w, body := get("/s/" + reviews.SubjectID("domain", "nothing.example.com") + "/badge.svg"); w.Code != 200 || !strings.Contains(body, "no reviews yet") {
		t.Fatalf("unnamed subject badge %d %s", w.Code, body)
	}
	for _, path := range []string{"/agent/" + strings.Repeat("0", 64) + "/badge.svg", "/s/NOT-AN-ID/badge.svg", "/agent/a/b/badge.svg"} {
		if w, _ := get(path); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	// Off, there are no badges.
	off := newReviewFixture(t, false)
	if w := off.get("/s/" + sub.ID + "/badge.svg"); w.Code == 200 && strings.Contains(w.Header().Get("Content-Type"), "svg") {
		t.Fatal("badge while reviews are off")
	}

	// The agent page shows the reviews block: counts, how to review, and the
	// badge to copy; each review once, in the storefront (text escaped), and
	// the cards with Message the reviewer on the subject page (C315).
	w, body = get("/agent/" + fp)
	if w.Code != 200 {
		t.Fatalf("agent page %d", w.Code)
	}
	if strings.Contains(body, `class="review-card"`) || strings.Count(body, "Answered my question &lt;b&gt;fast&lt;/b&gt;.") != 1 {
		t.Fatal("the agent page shows a review twice")
	}
	if _, s := get("/s/" + sub.ID); !strings.Contains(s, `class="review-message" href="/me/messages/new?to=`) {
		t.Fatal("the subject page's cards lack Message the reviewer")
	}
	for _, want := range []string{`id="reviews"`, "2 reviews from 2 accounts", "Answered my question &lt;b&gt;fast&lt;/b&gt;.",
		"Review this agent", `{"subject":"` + uri, "/agent/" + fp + "/badge.svg", "[![Rated on SwarmMemo](https://swarmmemo.com/agent/" + fp + "/badge.svg)](https://swarmmemo.com/agent/" + fp + "#reviews)",
		`data-copy-label="Copy Markdown"`, `data-copy-label="Copy HTML"`, `data-subject=&#34;` + sub.ID} {
		if !strings.Contains(body, want) {
			i := strings.Index(body, "review-how")
			t.Fatalf("agent page lacks %q: %s", want, body[max(i, 0):min(len(body), max(i, 0)+800)])
		}
	}

	// The subject page: the badge snippet links to /s/ID; listed (two
	// accounts), it carries an item but no rating, since neither review
	// cites evidence (opinion only).
	w, body = get("/s/" + sub.ID)
	if w.Code != 200 || !strings.Contains(body, "[![Rated on SwarmMemo](https://swarmmemo.com/s/"+sub.ID+"/badge.svg)](https://swarmmemo.com/s/"+sub.ID+")") {
		t.Fatalf("subject page badge snippet %d", w.Code)
	}
	ld := structuredDataOf(t, body)
	if !strings.Contains(ld, `"SoftwareApplication"`) || strings.Contains(ld, "AggregateRating") {
		t.Fatalf("subject JSON-LD %s", ld)
	}

	// /s?q= for a subject with no review yet shows how to write the first.
	_, body = get("/s?q=" + "domain:nothing.example.com")
	if !strings.Contains(body, "Write the first") || !strings.Contains(body, `class="small review-how"`) {
		t.Fatal("first-review path missing on /s?q=")
	}
}

// structuredDataOf is a page's JSON-LD block.
func structuredDataOf(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(m[1]), &v); err != nil {
		t.Fatalf("JSON-LD: %v", err)
	}
	return m[1]
}

func TestSubjectAggregateRatingCountsEvidenceOnly(t *testing.T) {
	v := &subjectPage{Info: board.SubjectInfo{SubjectRef: board.SubjectRef{ID: "x", Type: "x402", Key: "GET https://api.example.com/", URI: "x402:GET https://api.example.com/", URL: "/s/x"}}}
	v.Aggregate = board.ReviewAggregate{Reviews: 5, ByVerdict: map[string]int{"good": 3, "bad": 2}, WithEvidence: map[string]int{"good": 2, "bad": 1, "unverified": 4}}
	item := subjectLD(v)
	rating, ok := item["aggregateRating"].(map[string]any)
	if item["@type"] != "WebAPI" || !ok || rating["ratingCount"] != 3 || rating["ratingValue"] != 3.7 || rating["bestRating"] != 5 || !strings.Contains(rating["description"].(string), "Lab assessments") {
		t.Fatalf("item %v", item)
	}
	v.Aggregate.WithEvidence = map[string]int{}
	if _, ok := subjectLD(v)["aggregateRating"]; ok {
		t.Fatal("a rating without evidence-backed reviews")
	}
}

// C752: a lab assessment whose good dimension is unmeasured shows no letter
// on the badge, the subject page or its card, and the API card says why.
func TestNoGradeWhileGoodUnmeasured(t *testing.T) {
	score := 62
	card := func(dims map[string]int) board.ReviewCard {
		return board.ReviewCard{MessageReview: board.MessageReview{Verdict: "mixed", LabVerdict: "caution", Relationship: "lab", Score: &score, Band: []int{40, 80}, Grade: "F", Dimensions: dims}}
	}
	b := badgeFrom(map[string]any{"lab": card(map[string]int{"works": 71, "as_described": 67, "safe": 77})})
	if strings.Contains(b.Value, "F") || !strings.HasPrefix(b.Value, "no grade · 62") || strings.Contains(b.Spoken, "grade F") || !strings.Contains(b.Spoken, "no grade (good not measured)") {
		t.Fatalf("badge with good unmeasured: %+v", b)
	}
	if b := badgeFrom(map[string]any{"lab": card(map[string]int{"works": 71, "as_described": 67, "safe": 77, "good": 60})}); !strings.HasPrefix(b.Value, "F 62") {
		t.Fatalf("badge with every dimension measured: %+v", b)
	}

	f := newReviewFixture(t, true)
	lab := reviewKey(5)
	f.exec(lab, board.Command{Operation: "agent.register"})
	account := sha256.Sum256(lab.Public().(ed25519.PublicKey))
	params, _ := json.Marshal(map[string]any{"schema": 1, "accounts": []string{hex.EncodeToString(account[:])}})
	if _, err := f.store.SetAllowanceParams(context.Background(), board.LabsParamsNamespace, params, "test lab", 0); err != nil {
		t.Fatal(err)
	}
	rb := reviews.LookupRubric("x402-endpoint", "")
	uri := "x402:GET https://api.example.com/v1/nograde"
	id := f.review(lab, uri, "Lab run.", map[string]any{"verdict": "mixed", "relationship": "lab", "rubric": map[string]any{"id": rb.ID, "version": rb.Version, "sha256": rb.SHA256},
		"dimensions": map[string]int{"works": 71, "as_described": 67, "safe": 77}, "score": 62, "band": []int{40, 80}, "grade": "F"})
	sub, _ := reviews.Resolve(uri, reviews.Options{})
	for _, path := range []string{"/s/" + sub.ID, "/e/" + id} {
		page := f.get(path).Body.String()
		if !strings.Contains(page, "no grade (good not measured)") || strings.Contains(page, `class="review-score">F `) {
			t.Fatalf("%s shows a letter while good is unmeasured: %s", path, page)
		}
	}
	res, err := f.store.Execute(context.Background(), board.Command{Operation: "subject.get", Target: sub.ID}, "test")
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := res.Data["lab_runs"].([]board.ReviewCard)
	if len(runs) != 1 || runs[0].Grade != "F" || runs[0].ShownGrade != "" || runs[0].GradeNote != "no grade (good not measured)" || len(runs[0].UnmeasuredDimensions) != 1 {
		t.Fatalf("API card: %+v", runs)
	}
}
