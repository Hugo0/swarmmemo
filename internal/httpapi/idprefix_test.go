package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// A unique 8-character prefix of a public message's ID opens it on every
// read route: a page redirects to the full ID's address, an API read answers
// in place with Content-Location naming it. A private post's prefix is not
// found; fewer than 8 characters is 400 (reported by outside agents quoting
// "fcf0ab37").
func TestIDPrefixReads(t *testing.T) {
	store, s, post := postTextFixture(t)
	text := "A short-id post"
	id := post(board.Command{Operation: "post", Room: "lobby", Text: text})
	feed, err := store.Execute(context.Background(), board.Command{Operation: "messages.list"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	wid := post(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: "Summarize a page"})
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Summarize a page", "capabilities": []string{"writing"}})
	post(board.Command{Operation: "work.create", MessageID: wid, Data: string(data)})
	post(board.Command{Operation: "room.create", Room: "secret", Visibility: "private"})
	private := post(board.Command{Operation: "post", Room: "secret", Text: "Private room words"})
	p, wp, pp := id[:8], wid[:8], private[:8]
	if p == pp || wp == pp || p == wp {
		t.Skip("random IDs share a prefix")
	}

	// API reads answer in place.
	for _, tc := range []struct{ path, accept, location, want string }{
		{"/api/work/" + wp, "", "/api/work/" + wid, `"id":"` + wid + `"`},
		{"/api/work/" + wid[:20] + "/history", "", "/api/work/" + wid + "/history", wid},
		{"/api/thread/" + p, "", "/api/thread/" + id, `"root_id":"` + id + `"`},
		{"/e/" + p + "?format=json", "application/json", "/e/" + id, `"id":"` + id + `"`},
		{"/e/" + p + "/text", "", "/e/" + id + "/text", text},
		{"/e/" + p + "/text", "text/html", "/e/" + id + "/text", text},
	} {
		w := get(s, tc.path, tc.accept)
		if w.Code != 200 || w.Header().Get("Content-Location") != tc.location || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: %d %q %.300s", tc.path, w.Code, w.Header().Get("Content-Location"), w.Body)
		}
	}
	// Pages redirect to the full ID, keeping the rest of the path and query.
	for _, tc := range []struct{ path, location string }{
		{"/e/" + p, "/e/" + id},
		{"/e/" + p + "?cursor=x", "/e/" + id + "?cursor=x"},
		{"/e/" + p + "/proof", "/e/" + id + "/proof"},
		{"/e/" + p + "/history", "/e/" + id + "/history"},
		{"/work/" + wp, "/work/" + wid},
	} {
		w := get(s, tc.path, "text/html")
		if w.Code != 302 || w.Header().Get("Location") != tc.location {
			t.Errorf("%s: %d %q", tc.path, w.Code, w.Header().Get("Location"))
		}
	}
	// A full ID is untouched.
	if w := get(s, "/api/work/"+wid, ""); w.Code != 200 || w.Header().Get("Content-Location") != "" {
		t.Errorf("full ID: %d %q", w.Code, w.Header().Get("Content-Location"))
	}
	// A private post's prefix is the not-found of an unknown ID, naming nothing.
	for _, path := range []string{"/e/" + pp + "?format=json", "/e/" + pp + "/text", "/api/thread/" + pp, "/api/work/" + pp} {
		w := get(s, path, "")
		if w.Code != 404 || strings.Contains(w.Body.String(), private) || strings.Contains(w.Body.String(), "Private room") {
			t.Errorf("private %s: %d %s", path, w.Code, w.Body)
		}
	}
	if w := get(s, "/e/"+pp, "text/html"); w.Code != 404 || strings.Contains(w.Body.String(), private) {
		t.Errorf("private page: %d", w.Code)
	}
	// Seven characters is too short.
	for _, path := range []string{"/api/work/" + wid[:7], "/api/thread/" + id[:7], "/e/" + id[:7] + "?format=json", "/e/" + id[:7] + "/text"} {
		w := get(s, path, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_message_id"`) || !strings.Contains(w.Body.String(), "at least 8 hex characters") {
			t.Errorf("short %s: %d %s", path, w.Code, w.Body)
		}
	}
}

// prefixStore answers every prefix with fixed candidates.
type prefixStore struct {
	*board.Store
	ids []string
}

func (p prefixStore) PublicIDsWithPrefix(_ context.Context, _ string, limit int) ([]string, error) {
	return p.ids[:min(limit, len(p.ids))], nil
}

// A prefix several public messages share is 409 ambiguous_id naming up to
// ten of them; a page lists them as links.
func TestIDPrefixAmbiguous(t *testing.T) {
	store, _, _ := postTextFixture(t)
	ids := []string{}
	for i := range 12 {
		ids = append(ids, "abcdef01"+strings.Repeat("0", 22)+string("0123456789ab"[i])+"0")
	}
	for _, n := range []int{2, 12} {
		s := New(prefixStore{store, ids[:n]}, web.Handler(store), Config{PublicURL: "https://example.test", ServiceID: "swarmmemo.com"})
		for _, path := range []string{"/api/work/abcdef01", "/api/thread/abcdef01", "/e/abcdef01?format=json", "/e/abcdef01/text"} {
			w := get(s, path, "")
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						Candidates []string `json:"candidates"`
						More       bool     `json:"more"`
					} `json:"details"`
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			shown := min(n, 10)
			if w.Code != 409 || body.Error.Code != "ambiguous_id" || len(body.Error.Details.Candidates) != shown ||
				body.Error.Details.Candidates[0] != ids[0] || body.Error.Details.More != (n > 10) {
				t.Errorf("%d %s: %d %s", n, path, w.Code, w.Body)
			}
		}
		w := get(s, "/e/abcdef01/proof", "text/html")
		if w.Code != 409 || !strings.Contains(w.Body.String(), `href="/e/`+ids[1]+`/proof"`) {
			t.Errorf("%d page: %d %s", n, w.Code, w.Body)
		}
		if n == 12 && strings.Contains(w.Body.String(), ids[10]) {
			t.Errorf("page lists more than ten: %s", w.Body)
		}
	}
}
