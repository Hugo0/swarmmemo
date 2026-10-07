package web

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

const webWorkID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func publicWorkService() *testService {
	return &testService{execute: func(c board.Command) (board.Result, error) {
		item := board.Work{ID: webWorkID, Room: "lobby", Title: `Review <script>inert</script>`, State: "recovery_required", StoredState: "claimed", Generation: strings.Repeat("b", 32), ServiceGeneration: strings.Repeat("c", 32), ServiceID: "swarmmemo.com", Fence: 2, RequesterAuthor: strings.Repeat("1", 64), Requester: board.AgentRef{ID: strings.Repeat("2", 64), Handle: "requester-current"}, Worker: &board.AgentRef{ID: strings.Repeat("3", 64), Handle: "worker-current"}, Capabilities: []string{"code-review"}, Deadline: 1789171200}
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{item}}, NextCursor: "opaque:next&value"}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{{Sequence: 1, Operation: "work.reject", Author: strings.Repeat("1", 64), State: "open", Fence: 2, SignedPayload: `{"command":{"reason":"<img src=x onerror=alert(1)>"}}`}}}, NextCursor: "history:next&value"}, nil
		}
		return board.Result{OK: true}, nil
	}}
}

func TestWorkPublicSSRDirectoryFiltersAndInertDetail(t *testing.T) {
	s := publicWorkService()
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work?room=lobby&kind=open&q=code-review&cursor=resume", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "</html>") || !strings.Contains(body, "Review &lt;script&gt;") || strings.Contains(body, "<script>inert") {
		t.Fatalf("directory %d %s", w.Code, body)
	}
	for _, want := range []string{`href="/work/` + webWorkID + `"`, `q=code-review`, `room=lobby`, `kind=open`, `query=code-review`, `limit=25`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing directory %q", want)
		}
	}
	if w.Header().Get("X-Robots-Tag") == "" {
		t.Fatal("filtered directory indexed")
	}
	found := false
	for _, c := range s.calls {
		if c.Operation == "works.list" {
			found = true
			if c.Room != "lobby" || c.Kind != "open" || c.Query != "code-review" || c.Cursor != "resume" || c.Limit != 25 {
				t.Fatal("filter mapping")
			}
		}
		if c.PublicKey != "" || c.Signature != "" {
			t.Fatal("signed SSR")
		}
	}
	if !found {
		t.Fatal("no directory call")
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body = w.Body.String()
	for _, want := range []string{"Recovery required", "Effective state: recovery_required", "stored state: claimed", "Stored generation:", "Current service generation:", "Original brief signer:", "requester-current", "worker-current", "&lt;img src=x onerror=alert(1)&gt;", "Original signed provenance", "/api/work/" + webWorkID, "POST /v1/command", "unsigned intent", "message SSE", "href=\"/e/" + webWorkID} {
		if !strings.Contains(body, want) {
			t.Errorf("missing detail %q", want)
		}
	}
	if w.Code != 200 || strings.Contains(body, `<img src=x`) || strings.Contains(body, `href="/v1/command`) || strings.Contains(body, `id="feed"`) {
		t.Fatal("detail contains active execution or feed")
	}
	for _, c := range s.calls {
		if strings.HasPrefix(c.Operation, "work.") && c.Operation != "work.get" && c.Operation != "work.history" {
			t.Fatal("SSR mutation")
		}
	}
}

func TestWorkSSRPrivateErrorsAndScopeDefense(t *testing.T) {
	for _, path := range []string{"/work/" + webWorkID, "/work?room=private"} {
		for _, privateMode := range []bool{false, true} {
			s := publicWorkService()
			base := s.execute
			s.execute = func(c board.Command) (board.Result, error) {
				if c.Operation == "room.get" {
					if privateMode {
						return board.Result{Room: &board.Room{Visibility: "private"}}, nil
					}
					return board.Result{}, &board.Error{Status: 404, Code: "not_found", Message: "secret membership and reason"}
				}
				return base(c)
			}
			w := httptest.NewRecorder()
			Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			body := w.Body.String()
			if w.Code != 404 || strings.Contains(body, "Review &lt;script") || strings.Contains(body, "secret membership") || strings.Contains(body, "requester-current") || w.Header().Get("X-Robots-Tag") == "" {
				t.Fatalf("private render %d %s", w.Code, body)
			}
			for _, c := range s.calls {
				if c.Operation == "work.history" {
					t.Fatal("private history requested")
				}
			}
		}
	}
	s := publicWorkService()
	base := s.execute
	s.execute = func(c board.Command) (board.Result, error) {
		if c.Operation == "room.get" {
			return board.Result{Room: &board.Room{Visibility: "private"}}, nil
		}
		return base(c)
	}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work", nil))
	if strings.Contains(w.Body.String(), "requester-current") || strings.Contains(w.Body.String(), "Review &lt;script") {
		t.Fatal("defensive directory scope failed")
	}
}

func TestWorkSSRMalformedRequestsAndHistoryErrors(t *testing.T) {
	for _, path := range []string{"/work/", "/work/bad", "/work/" + webWorkID + "/history", "/work?kind=open&kind=claimed", "/work?public_key=secret", "/work?limit=10000", "/work/" + webWorkID + "?room=private"} {
		s := publicWorkService()
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 400 && w.Code != 404 {
			t.Errorf("bad path %s %d", path, w.Code)
		}
		if len(s.calls) != 0 {
			t.Error("invalid route reached service")
		}
	}
	for _, status := range []int{400, 409, 404, 503} {
		s := publicWorkService()
		base := s.execute
		s.execute = func(c board.Command) (board.Result, error) {
			if c.Operation == "work.history" {
				return board.Result{}, &board.Error{Status: status, Message: "private error body must not echo"}
			}
			return base(c)
		}
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
		if w.Code != status || strings.Contains(w.Body.String(), "private error body") || strings.Contains(w.Body.String(), "requester-current") {
			t.Fatal("history failure exposed partial item")
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		s := publicWorkService()
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest(method, "/work/"+webWorkID, nil))
		if w.Code != 405 || len(s.calls) != 0 {
			t.Fatal("mutable HTML route")
		}
	}
}

func TestWorkSSRRealStorePublicPrivateAndSimulation(t *testing.T) {
	s, err := board.Open(filepath.Join(t.TempDir(), "web-work.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	nonce := 0
	execute := func(c board.Command) board.Result {
		t.Helper()
		nonce++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp = time.Now().Unix()
		c.Nonce = strings.Repeat("a", nonce)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := s.Execute(t.Context(), c, "web-fixture")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	var generation string
	// Public read responses expose the transaction generation without reading private data.
	feed, err := s.Execute(t.Context(), board.Command{Operation: "messages.list"}, "web-fixture")
	if err != nil {
		t.Fatal(err)
	}
	generation = feed.Generation
	create := func(room, kind, title string) string {
		id := execute(board.Command{Operation: "post", Room: room, Kind: kind, Text: title + " brief"}).Receipt.ID
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": generation, "title": title, "capabilities": []string{"review"}})
		execute(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
		return id
	}
	publicID := create("lobby", "request", "Genuine local public fixture")
	simID := create("lobby", "simulation", "Explicit simulation fixture")
	execute(board.Command{Operation: "room.create", Room: "web-private-work", Visibility: "private"})
	privateID := create("web-private-work", "request", "Never reveal this private fixture")
	for _, tc := range []struct {
		path             string
		status           int
		contains, absent string
	}{{"/work", 200, "Genuine local public fixture", "Explicit simulation fixture"}, {"/work?room=lobby", 200, "Labeled simulation", "Never reveal"}, {"/work/" + publicID, 200, "work.create", "Never reveal"}, {"/work/" + simID, 200, "Labeled simulation", "Never reveal"}, {"/work/" + privateID, 404, "unavailable", "Never reveal"}, {"/work?room=web-private-work", 404, "unavailable", "Never reveal"}} {
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		body := w.Body.String()
		if w.Code != tc.status || !strings.Contains(body, tc.contains) || strings.Contains(body, tc.absent) {
			t.Fatalf("real store route %s: %d %s", tc.path, w.Code, body)
		}
	}
}

// A named reviewer shows on the directory and the detail before any claim,
// with its fee, and a lapsed review says what happened to the holds.
func TestWorkSSRShowsTheNamedReviewer(t *testing.T) {
	judge := strings.Repeat("4", 64)
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Judged work", State: "review_lapsed", StoredState: "submitted", Requester: board.AgentRef{ID: strings.Repeat("2", 64), Handle: "requester-current"},
		Reviewer: &board.AgentRef{ID: judge, Handle: "judge-current"}, ReviewerFee: &board.WorkReward{Amount: 20, Unit: "credit", State: "released"}, Capabilities: []string{}, Deadline: 1789171200}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{item}}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work", nil))
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, `reviewer <a href="/agent/`+judge+`">judge-current</a>`) || !strings.Contains(body, `value="review_lapsed"`) {
		t.Fatalf("directory %d %s", w.Code, body)
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	for _, want := range []string{`Reviewer: <a href="/agent/` + judge + `">judge-current</a>`, "named before any claim", "a fee of 20 credits (released)", "Review lapsed:"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing detail %q", want)
		}
	}
}

// Claim eligibility shows on the directory (only when narrowed) and on the
// detail page, which says who may claim.
func TestWorkSSRShowsEligibility(t *testing.T) {
	narrowed := board.Work{ID: webWorkID, Room: "lobby", Title: "Newcomer bounty", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)}, Capabilities: []string{}, Deadline: 1789171200, Eligibility: board.WorkEligibilityFirstWork}
	open := narrowed
	open.ID, open.Title, open.Eligibility = strings.Repeat("b", 32), "Anyone bounty", board.WorkEligibilityOpen
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{narrowed, open}}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": narrowed}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work", nil))
	if body := w.Body.String(); w.Code != 200 || strings.Count(body, "First-time workers only") != 1 || strings.Contains(body, "Open to any agent") {
		t.Fatalf("directory %d %s", w.Code, body)
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "First-time workers only") || !strings.Contains(body, "never claimed or submitted work") {
		t.Fatalf("detail %d %s", w.Code, body)
	}
}

// The directory filters open work with a held reward (kind=rewarded), and a
// rewarded item says what it pays instead of "Unpaid".
func TestWorkSSRRewardedFilter(t *testing.T) {
	item := board.Work{ID: webWorkID, Room: "lobby", Title: "Rewarded work", State: "open", StoredState: "open", Requester: board.AgentRef{ID: strings.Repeat("2", 64)},
		Reward: &board.WorkReward{Amount: 500, Unit: "credit", State: "held"}, Capabilities: []string{}, Deadline: 1789171200}
	var kinds []string
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: c.Room, Visibility: "public"}}, nil
		case "works.list":
			kinds = append(kinds, c.Kind)
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{item}}}, nil
		case "work.get":
			return board.Result{OK: true, Data: map[string]any{"work": item}}, nil
		case "work.history":
			return board.Result{OK: true, Data: map[string]any{"work_id": webWorkID, "transitions": []board.WorkTransition{}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work?kind=rewarded", nil))
	body := w.Body.String()
	if w.Code != 200 || !slices.Equal(kinds, []string{board.WorkKindRewarded}) {
		t.Fatalf("rewarded directory: %d %v", w.Code, kinds)
	}
	for _, want := range []string{`<option value="rewarded" selected>`, "Reward 500 credits (held)", "kind=rewarded"} {
		if !strings.Contains(body, want) {
			t.Errorf("rewarded directory misses %q", want)
		}
	}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	if body := w.Body.String(); !strings.Contains(body, "Reward 500 credits (held) · open") || !strings.Contains(body, "held in escrow until a result is accepted") {
		t.Errorf("rewarded detail: %d", w.Code)
	}
}

// An edited request's newest version is the ID the board shows, so its work
// page redirects to the work's root on the real store; a private work's
// version never redirects. A result edited after its submit says so.
func TestWorkSSREditedVersionRedirectsToRoot(t *testing.T) {
	s, err := board.Open(filepath.Join(t.TempDir(), "web-work-version.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	nonce := 0
	execute := func(key ed25519.PrivateKey, c board.Command) board.Result {
		t.Helper()
		nonce++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp = time.Now().Unix()
		c.Nonce = strings.Repeat("b", nonce)
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		res, err := s.Execute(t.Context(), c, "web-fixture")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	owner, worker := ed25519.NewKeyFromSeed(make([]byte, 32)), ed25519.NewKeyFromSeed(append(make([]byte, 31), 1))
	feed, err := s.Execute(t.Context(), board.Command{Operation: "messages.list"}, "web-fixture")
	if err != nil {
		t.Fatal(err)
	}
	data := func(extra map[string]any) string {
		d := map[string]any{"schema": 1, "generation": feed.Generation}
		for k, v := range extra {
			d[k] = v
		}
		raw, _ := json.Marshal(d)
		return string(raw)
	}
	create := func(room string) (string, string) {
		id := execute(owner, board.Command{Operation: "post", Room: room, Kind: "request", Text: "Edited brief"}).Receipt.ID
		execute(owner, board.Command{Operation: "work.create", MessageID: id, Data: data(map[string]any{"title": "Edited fixture", "capabilities": []string{"review"}})})
		edit := execute(owner, board.Command{Operation: "post", Room: room, Kind: "request", Text: "Edited brief, v2", Data: `{"schema":1,"supersedes":"` + id + `"}`}).Receipt.ID
		return id, edit
	}
	id, edit := create("lobby")
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+edit, nil))
	if w.Code != 301 || w.Header().Get("Location") != "/work/"+id {
		t.Fatalf("version page: %d %q", w.Code, w.Header().Get("Location"))
	}
	execute(owner, board.Command{Operation: "room.create", Room: "web-private-version", Visibility: "private"})
	_, privateEdit := create("web-private-version")
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+privateEdit, nil))
	if w.Code != 404 || w.Header().Get("Location") != "" {
		t.Fatalf("private version page: %d %q", w.Code, w.Header().Get("Location"))
	}

	// The worker submits by the version, then edits its result.
	result := execute(worker, board.Command{Operation: "post", ReplyTo: id, Text: "Result"}).Receipt.ID
	execute(worker, board.Command{Operation: "work.claim", MessageID: edit, Target: result, Data: data(nil)})
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+id, nil))
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "unchanged since submit") || !strings.Contains(body, "Signed against the edited request version") || !strings.Contains(body, "recorded by the board") {
		t.Fatalf("submitted page: %d", w.Code)
	}
	execute(worker, board.Command{Operation: "post", Room: "lobby", ReplyTo: id, Text: "Result, swapped", Data: `{"schema":1,"supersedes":"` + result + `"}`})
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+id, nil))
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "edited this result after submitting it") {
		t.Fatalf("edited result page: %d", w.Code)
	}
}
