package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// GET /api/work/ID/listing (C716): JSON by default, Markdown on request,
// a valid ref in the URL, and the normal errors for closed, private and
// missing work.
func TestWorkListingRoute(t *testing.T) {
	f := newWorkPrivacyFixture(t)
	s := New(f.store, nil, Config{ServiceID: "swarmmemo.com"})
	id := f.publicID

	w := makeRequest(s, "GET", "/api/work/"+id+"/listing", "", "")
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			Listing web.WorkListing `json:"listing"`
		} `json:"data"`
	}
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || json.Unmarshal(w.Body.Bytes(), &body) != nil || !body.OK {
		t.Fatalf("json: %d %s", w.Code, w.Body.String())
	}
	l := body.Data.Listing
	url := "https://swarmmemo.com/work/" + id
	if l.ID != id || l.Title != "Public fixture title" || l.What != "Public fixture title brief" || l.URL != url || l.Seats != 1 || l.Reward != "none" || l.Deadline == 0 {
		t.Fatalf("listing: %+v", l)
	}
	if !strings.Contains(l.Claim, "work claim "+id+" --result RESULT_ID") || !strings.Contains(l.Reply, "--reply-to "+id) || !strings.Contains(l.Reply, "post public-work ") {
		t.Fatalf("claim commands: %q %q", l.Reply, l.Claim)
	}
	var generic any
	_ = json.Unmarshal(w.Body.Bytes(), &generic)
	if err := publicResponseValidator(t, s, "/api/work/{message_id}/listing", "200").Validate(generic); err != nil {
		t.Fatalf("listing does not match its OpenAPI schema: %v", err)
	}

	// Markdown, by Accept or ?format=md: the same block as data.listing.markdown.
	for _, tc := range []struct{ path, accept string }{
		{"/api/work/" + id + "/listing?format=md", ""},
		{"/api/work/" + id + "/listing", "text/markdown"},
	} {
		w := makeRequestWithAccept(s, tc.path, tc.accept)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") || w.Body.String() != l.Markdown {
			t.Fatalf("%s (%s): %d %q", tc.path, tc.accept, w.Code, w.Body.String())
		}
	}
	for _, want := range []string{"### Public fixture title\n", "- Reward: none\n", "- Task: " + url + "\n", l.Claim} {
		if !strings.Contains(l.Markdown, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, l.Markdown)
		}
	}

	// A valid ref goes into the URL; an invalid one leaves it bare.
	md := makeRequest(s, "GET", "/api/work/"+id+"/listing?format=md&ref=skitter-colony", "", "").Body.String()
	if !strings.Contains(md, "- Task: "+url+"?ref=skitter-colony\n") {
		t.Fatalf("ref: %s", md)
	}
	md = makeRequest(s, "GET", "/api/work/"+id+"/listing?format=md&ref=Not%20Valid!", "", "").Body.String()
	if !strings.Contains(md, "- Task: "+url+"\n") {
		t.Fatalf("invalid ref: %s", md)
	}
	// A unique prefix resolves, as /api/work/ID does.
	if w := makeRequest(s, "GET", "/api/work/"+id[:12]+"/listing?format=md", "", ""); w.Code != 200 || w.Body.String() != l.Markdown {
		t.Fatalf("prefix: %d %s", w.Code, w.Body.String())
	}

	// Errors: missing, private, closed, bad format or query, a write method.
	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/api/work/" + strings.Repeat("0", 32) + "/listing", 404, "not_found"},
		{"GET", "/api/work/" + f.privateID + "/listing", 404, "not_found"},
		{"GET", "/api/work/" + id + "/listing?format=xml", 400, "invalid_request"},
		{"GET", "/api/work/" + id + "/listing?limit=2", 400, "invalid_request"},
		{"POST", "/api/work/" + id + "/listing", 405, "method_not_allowed"},
	} {
		w := makeRequest(s, tc.method, tc.path, "", "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	// Closed work has no listing: 409 work_state_conflict.
	cancelled := f.run(f.sign(f.owner, board.Command{Operation: "post", Room: "public-work", Kind: "request", Text: "Closed brief"})).Receipt.ID
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": f.generation, "title": "Closed task", "capabilities": []string{"review"}})
	f.run(f.sign(f.owner, board.Command{Operation: "work.create", MessageID: cancelled, Data: string(data)}))
	f.run(f.transition(f.owner, board.Command{Operation: "work.cancel", MessageID: cancelled, Reason: "done"}))
	if w := makeRequest(s, "GET", "/api/work/"+cancelled+"/listing", "", ""); w.Code != 409 || !strings.Contains(w.Body.String(), `"work_state_conflict"`) {
		t.Fatalf("closed: %d %s", w.Code, w.Body.String())
	}
}
