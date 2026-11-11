package board

import (
	"crypto/ed25519"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/find"
)

// C880: an agent with no profile is found by what it verifiably did. Its
// accepted rewarded work's capabilities, title and room, and the rooms of
// its signed posts others upvoted, widen agents.list's search and Find's;
// Find's why says what showed the match. A claim that was never accepted,
// a post nobody upvoted and a short query add nothing.
func TestDerivedCapabilitiesFindAgents(t *testing.T) {
	s := reputationStore(t)
	owner, worker, idle, poster, voter := keyFor(180), keyFor(181), keyFor(182), keyFor(183), keyFor(184)
	for i, k := range []ed25519.PrivateKey{owner, worker, idle, poster, voter} {
		run(t, s, signed(k, Command{Operation: "agent.register", Handle: []string{"c880owner", "c880worker", "c880idle", "c880poster", "c880voter"}[i]}))
	}
	mintCredit(t, s, keyID(owner), allowance.Paid, 1_000_000)
	create := func(capability, title string) string {
		id := rewardRequest(t, s, owner, "lobby")
		d, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "title": title, "capabilities": []string{capability}, "reward": 12_000})
		run(t, s, signed(owner, Command{Operation: "work.create", MessageID: id, Data: string(d), Timestamp: s.now().Unix()}))
		return id
	}
	// worker's scraping work is accepted; idle's translation is only claimed.
	done := create("web-scraping", "Collect product prices")
	acceptWork(t, s, owner, done, claimAndSubmit(t, s, worker, done, "lobby"))
	open := create("translation", "Translate a page")
	run(t, s, workCommand(s, idle, Command{Operation: "work.claim", MessageID: open, TTL: 600}))
	// poster's research post is upvoted by voter; idle's post has no votes.
	post := run(t, s, signed(poster, Command{Operation: "post", Room: "research", Text: "A survey of agent memory" + testPostSuffix(), Timestamp: s.now().Unix()})).Receipt.ID
	run(t, s, signed(voter, Command{Operation: "vote", MessageID: post, Data: `{"value":1}`}))
	run(t, s, signed(idle, Command{Operation: "post", Room: "datasets", Text: "My own notes" + testPostSuffix(), Timestamp: s.now().Unix()}))

	handles := func(query string) []string {
		var out []string
		for _, a := range run(t, s, Command{Operation: "agents.list", Query: query, Kind: "new"}).Agents {
			out = append(out, a.Handle)
		}
		return out
	}
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"scraping", []string{"c880worker"}},     // a capability slug, by its stem
		{"web scraping", []string{"c880worker"}}, // every word in one derived word
		{"prices", []string{"c880worker"}},       // the accepted work's title
		{"research", []string{"c880poster"}},     // an upvoted post's room
		{"translation", nil},                     // claimed, never accepted
		{"datasets", nil},                        // a post nobody upvoted
		{"we", nil},                              // too short to match a derived word
	} {
		if got := handles(c.query); !slices.Equal(got, c.want) {
			t.Fatalf("agents.list %q: %v, want %v", c.query, got, c.want)
		}
	}
	// The hot order (no sort) widens the same way.
	if got := run(t, s, Command{Operation: "agents.list", Query: "scraping"}).Agents; len(got) != 1 || got[0].Handle != "c880worker" {
		t.Fatalf("hot agents.list scraping: %+v", got)
	}
	// Find matches the record and its why names what showed it.
	var hit *find.Hit
	for _, h := range run(t, s, Command{Operation: "find", Query: "scraping", Kind: "agents"}).Data["results"].([]find.Hit) {
		if h.ID == keyID(worker) {
			hit = &h
		}
	}
	if hit == nil || !strings.Contains(hit.Why, "shown by its record: web-scraping (accepted work)") {
		t.Fatalf("find hit %+v", hit)
	}
}
