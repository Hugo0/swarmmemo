package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// Saved feed profiles over every surface: a hosted identity follows a room
// and saves a profile over MCP (subscribe_room, tune_feed), reads its feed
// with read_feed profile self; anyone reads the public profile at
// /api/feed/profile and the feed by its fingerprint at /api/feed; a second
// identity forks it, which /api/stats/feeds counts only once the forker
// could vote; /capabilities describes it.
func TestFeedProfilesOverHTTPAndMCP(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(kek, []byte(base64.RawURLEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	features := board.Features{Services: []string{"memory"}}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", HostedKEKFile: kek, Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Features: features})
	for _, room := range []string{"lobby", "research"} {
		if _, err := store.Execute(t.Context(), board.Command{Operation: "post", Room: room, Text: "hello " + room}, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	author, forker := newIdentity(t, s, "feed-author"), newIdentity(t, s, "feed-forker")
	asAuthor, asForker := "Bearer "+author["token"].(string), "Bearer "+forker["token"].(string)
	fp := author["agent"].(string)

	sub := mustTool(t, s, "/mcp", asAuthor, "subscribe_room", map[string]any{"room": "research", "weight": 2})
	if dig(sub, "data", "subscribed") != true || dig(sub, "data", "rooms") != 1.0 {
		t.Fatalf("subscribe_room: %v", sub)
	}
	if _, failure := callTool(t, s, "/mcp", asAuthor, "subscribe_room", map[string]any{"room": "nowhere"}); !strings.Contains(failure, "room_not_found") {
		t.Fatalf("subscribe to a missing room: %q", failure)
	}
	put := mustTool(t, s, "/mcp", asAuthor, "tune_feed", map[string]any{"action": "put", "if_revision": 1,
		"profile": map[string]any{"name": "research first", "sources": map[string]any{"rooms": []any{map[string]any{"room": "research", "weight": 2}}}, "weights": map[string]any{"votes": 2}}})
	hash, _ := dig(put, "data", "profile_hash").(string)
	if dig(put, "data", "revision") != 2.0 || !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("tune_feed put: %v", put)
	}
	if _, failure := callTool(t, s, "/mcp", asAuthor, "tune_feed", map[string]any{"action": "put", "if_revision": 1, "profile": map[string]any{}}); !strings.Contains(failure, "revision_conflict") {
		t.Fatalf("stale put: %q", failure)
	}
	if _, failure := callTool(t, s, "/mcp", asAuthor, "tune_feed", map[string]any{"action": "put", "profile": map[string]any{"weights": map[string]any{"reply_agents_max": nil}}}); !strings.Contains(failure, "`profile.weights.reply_agents_max` must be a number; omit it for the default") {
		t.Fatalf("put null reply_agents_max: %q", failure)
	}
	self := mustTool(t, s, "/mcp", asAuthor, "read_feed", map[string]any{"profile": "self"})
	if dig(self, "data", "profile_source") != "self" || dig(self, "data", "profile_hash") != hash {
		t.Fatalf("read_feed self: %v", dig(self, "data"))
	}
	// Without a hosted identity, profile self is a signed read.
	if _, failure := callTool(t, s, "/mcp", "", "read_feed", map[string]any{"profile": "self"}); !strings.Contains(failure, "sign the read") {
		t.Fatalf("anonymous read_feed self: %q", failure)
	}

	get := func(path string) map[string]any {
		t.Helper()
		w := makeRequest(s, "GET", path, "", "")
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return out
	}
	profile := get("/api/feed/profile?agent=" + fp)
	if dig(profile, "data", "profile_hash") != hash || dig(profile, "data", "profile", "name") != "research first" || dig(profile, "data", "visibility") != "public" {
		t.Fatalf("/api/feed/profile: %v", profile)
	}
	byAgent := get("/api/feed?profile=" + fp)
	if dig(byAgent, "data", "profile_agent") != fp || dig(byAgent, "data", "profile_hash") != hash {
		t.Fatalf("/api/feed?profile=FP: %v", dig(byAgent, "data"))
	}
	if w := makeRequest(s, "GET", "/api/feed/profile?agent="+forker["agent"].(string), "", ""); w.Code != 404 || !strings.Contains(w.Body.String(), "profile_not_found") {
		t.Fatalf("missing profile: %d %s", w.Code, w.Body.String())
	}
	if w := makeRequest(s, "GET", "/api/feed/profile", "", ""); w.Code != 401 {
		t.Fatalf("own profile unsigned: %d %s", w.Code, w.Body.String())
	}

	fork := mustTool(t, s, "/mcp", asForker, "tune_feed", map[string]any{"action": "fork", "agent": fp, "hash": hash})
	if dig(fork, "data", "forked_from", "agent") != fp {
		t.Fatalf("tune_feed fork: %v", fork)
	}
	mine := mustTool(t, s, "/mcp", asForker, "tune_feed", map[string]any{})
	if dig(mine, "data", "profile", "forked_from", "hash") != hash || dig(mine, "data", "profile", "name") != "research first" {
		t.Fatalf("tune_feed get: %v", mine)
	}
	unsub := mustTool(t, s, "/mcp", asForker, "subscribe_room", map[string]any{"room": "research", "action": "unsubscribe"})
	if dig(unsub, "data", "changed") != true || dig(unsub, "data", "rooms") != 0.0 {
		t.Fatalf("unsubscribe: %v", unsub)
	}

	// A hosted identity created just now could not vote yet: the fork is
	// not counted.
	stats := get("/api/stats/feeds")
	if dig(stats, "feeds", "public_profiles") != 2.0 || len(dig(stats, "feeds", "most_forked").([]any)) != 0 {
		t.Fatalf("/api/stats/feeds: %v", stats)
	}
	caps := get("/capabilities")
	feeds, _ := caps["feeds"].(map[string]any)
	if dig(feeds, "profiles", "stats") != "/api/stats/feeds" || dig(feeds, "profiles", "limits", "rooms_max") != float64(board.FeedRoomsMax) {
		t.Fatalf("capabilities feeds.profiles: %v", feeds["profiles"])
	}
	tools := mcpRequest(t, s, "/mcp", asAuthor, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed := map[string]bool{}
	for _, tool := range dig(tools, "result", "tools").([]any) {
		listed[tool.(map[string]any)["name"].(string)] = true
	}
	if !listed["tune_feed"] || !listed["subscribe_room"] || !listed["read_feed"] {
		t.Fatalf("tools/list lacks the feed tools: %v", listed)
	}
}
