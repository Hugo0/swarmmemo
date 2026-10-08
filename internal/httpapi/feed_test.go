package httpapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// GET /api/feed, MCP read_feed and /capabilities feeds: the default is the
// hot view, an override reorders it, and bad options are named.
func TestFeedOverHTTPAndMCP(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "feed.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	s := New(store, nil, Config{ServiceID: "swarmmemo.com"})
	for i := range 6 {
		for _, room := range []string{"lobby", "research"} {
			if _, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: room, Text: fmt.Sprintf("%s %d", room, i)}, "fixture"); err != nil {
				t.Fatal(err)
			}
		}
	}
	get := func(path string) board.Result {
		t.Helper()
		w := makeRequest(s, "GET", path, "", "")
		var res board.Result
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return res
	}
	ids := func(r board.Result) string {
		var out []string
		for _, m := range r.Messages {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}
	hot := get("/api/messages?sort=hot&limit=20")
	feed := get("/api/feed?limit=20")
	if ids(feed) == "" || ids(feed) != ids(hot) || feed.Data["sort"] != "feed" {
		t.Fatalf("default feed %s\nhot %s", ids(feed), ids(hot))
	}
	override := url.Values{"override": {`{"sources":{"front":false,"rooms":[{"room":"research"}]}}`}, "explain": {"true"}, "limit": {"20"}}
	research := get("/api/feed?" + override.Encode())
	if len(research.Messages) != 6 || research.Data["explain"] == nil {
		t.Fatalf("research feed: %d posts, data %v", len(research.Messages), research.Data)
	}
	for _, m := range research.Messages {
		if m.Room != "research" {
			t.Fatalf("a %s post in a research-only feed", m.Room)
		}
	}
	// Paging by cursor over GET.
	first := get("/api/feed?limit=5")
	next := get("/api/feed?limit=5&cursor=" + url.QueryEscape(first.NextCursor))
	if len(next.Messages) != 5 || strings.Contains(ids(first), next.Messages[0].ID) {
		t.Fatalf("second page %s after %s", ids(next), ids(first))
	}
	for path, says := range map[string]string{
		"/api/feed?override=" + url.QueryEscape(`{"weights":{"vote":1}}`):   "override.weights.vote is not an argument this method takes",
		"/api/feed?override=" + url.QueryEscape(`{"weights":{"votes":20}}`): "`override.weights.votes` must be 0 to 10",
		"/api/feed?override=nope":          "override is a JSON object",
		"/api/feed?explain=maybe":          "explain is true or false",
		"/api/feed?offset=1&data=%7B%7D":   "not both",
		"/api/feed?profile=default&room=x": "room",
	} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), says) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	// MCP.
	out := mustTool(t, s, "/mcp", "", "read_feed", map[string]any{"override": map[string]any{"weights": map[string]any{"votes": 2}}, "limit": 3})
	body, _ := json.Marshal(out)
	var res board.Result
	if json.Unmarshal(body, &res) != nil || len(res.Messages) != 3 || res.Data["overridden"] != true {
		t.Fatalf("read_feed: %s", body)
	}
	// Capabilities.
	w := makeRequest(s, "GET", "/capabilities", "", "")
	var caps struct {
		Feeds struct {
			DefaultHash string         `json:"default_hash"`
			Limits      map[string]int `json:"limits"`
			Operation   string         `json:"operation"`
		} `json:"feeds"`
	}
	if json.Unmarshal(w.Body.Bytes(), &caps) != nil || caps.Feeds.DefaultHash != board.FeedProfileHash(board.DefaultFeedProfile()) || caps.Feeds.Limits["rooms_max"] != board.FeedRoomsMax || caps.Feeds.Operation != "feed.get" {
		t.Fatalf("capabilities feeds: %+v", caps.Feeds)
	}
}
