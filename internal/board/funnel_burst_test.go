package board

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// burstWorld is a store with a synthetic ring and ordinary newcomers,
// replayed through Execute (the real engine), then recomputed on day 30.
type burstWorld struct {
	*funnelWorld
	ring []string
}

func (w *burstWorld) at(day int, offset int64) { w.day(day, offset) }

// execFrom runs a signed command from its own network, so no per-network
// limit couples the agents.
func (w *burstWorld) execFrom(name string, c Command) Result {
	w.t.Helper()
	ctx := WithVia(context.Background(), "command")
	ctx = WithRequestSignals(ctx, RequestSignals{UserAgent: "Mozilla/5.0 burst-test"})
	c.Timestamp = w.s.now().Unix()
	c = signed(w.keys[name], c)
	res, err := w.s.Execute(ctx, c, fmt.Sprintf("198.51.%d.%d", 100+len(name)%50, 1+int(w.keys[name][0])%250))
	if err != nil {
		w.t.Fatalf("%s %s: %v", name, c.Operation, err)
	}
	return res
}

func (w *burstWorld) say(name, text, replyTo string) string {
	return w.execFrom(name, Command{Operation: "post", Room: "lobby", Text: text, ReplyTo: replyTo, RequestID: randomID()}).Receipt.ID
}

var (
	burstCrowd = []string{"crowd0", "crowd1", "crowd2", "crowd3", "crowd4"}
	burstTrio  = []string{"trio0", "trio1", "trio2"}
)

func burstScenario(t *testing.T) *burstWorld {
	s := openTest(t, Config{})
	s.funnel.lastFlush = time.Unix(1<<40, 0)
	fw := &funnelWorld{t: t, s: s, base: testTime - testTime%86400, keys: map[string]ed25519.PrivateKey{}}
	w := &burstWorld{funnelWorld: fw}
	key := byte(10)
	add := func(name string) {
		w.keys[name] = keyFor(key)
		key++
	}
	// The ring: FunnelMinimumN keys created 20 s apart, each posting the
	// moment its key exists, then praising the one before.
	for i := 0; i < FunnelMinimumN; i++ {
		name := fmt.Sprintf("ring%02d", i)
		add(name)
		w.ring = append(w.ring, name)
	}
	for _, n := range []string{"alice", "bob", "pair0", "pair1", "dave", "erin"} {
		add(n)
	}
	for _, n := range append(slices.Clone(burstCrowd), burstTrio...) {
		add(n)
	}
	var posts []string
	for i, name := range w.ring {
		w.at(0, 3600+int64(i)*20)
		posts = append(posts, w.say(name, "an essay on minerals"+testPostSuffix(), ""))
	}
	w.at(0, 3600+600)
	for i, name := range w.ring[1:] {
		w.say(name, "what a fine essay"+testPostSuffix(), posts[i])
	}
	// alice: a key registered first, her first post an hour later, then a
	// task: an ordinary newcomer who activates.
	w.at(0, 7200)
	if _, err := s.db.Exec("INSERT INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,?,?,?,?)",
		keyID(w.keys["alice"]), base64.RawURLEncoding.EncodeToString(w.keys["alice"].Public().(ed25519.PublicKey)), keyID(w.keys["alice"]), s.now().Unix(), s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	w.at(0, 10800)
	task := w.execFrom("alice", Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Please review my parser", RequestID: randomID()}).Receipt.ID
	w.work("alice", "work.create", task, nil)
	// bob: fast (his first write is a post) but alone, hours from anyone.
	w.at(0, 30000)
	bobPost := w.say("bob", "hello, I write Go", "")
	// dave and erin: fast, a day apart, and they talk: a conversation, not
	// a burst (no creation link).
	w.at(1, 3600)
	davePost := w.say("dave", "anyone building parsers?", "")
	// alice comes back on her day 1.
	w.at(1, 7200)
	w.say("alice", "the parser is fixed", "")
	w.at(2, 3600)
	w.say("erin", "me", davePost)
	w.say("erin", "nice to meet you bob", bobPost)
	// pair0 and pair1: created 5 s apart and each answering the other.
	w.at(3, 3600)
	p0 := w.say("pair0", "first", "")
	w.at(3, 3605)
	p1 := w.say("pair1", "agreed", p0)
	w.say("pair0", "thank you", p1)
	// The trio: fast, a day apart each, replying to each other: they talk
	// but did not arrive together.
	var prev string
	for i, n := range burstTrio {
		w.at(4+i, 3600)
		prev = w.say(n, "a thought", prev)
	}
	// The crowd: five fast keys of one family created a minute apart, none
	// replying to another: a busy hour, not a ring.
	for i, n := range burstCrowd {
		w.at(8, 3600+int64(i)*60)
		w.say(n, "hello board", "")
	}
	if err := s.FlushFunnel(testContext); err != nil {
		t.Fatal(err)
	}
	w.at(30, 3600)
	if _, err := s.FunnelBackfill(testContext, w.base); err != nil {
		t.Fatal(err)
	}
	return w
}

// The ring is flagged with its reasons; the ordinary newcomers are not.
func TestBurstClustersDetected(t *testing.T) {
	w := burstScenario(t)
	s := w.s
	res, err := s.FunnelBursts(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Clusters) != 2 {
		t.Fatalf("clusters %+v", res.Clusters)
	}
	ring := res.Clusters[0]
	if ring.ID != 1 || len(ring.Members) != len(w.ring) {
		t.Fatalf("ring cluster %+v", ring)
	}
	for i, m := range ring.Members {
		if m.Account != keyID(w.keys[w.ring[i]]) || m.FirstPost > BurstFastSeconds {
			t.Fatalf("member %d %+v", i, m)
		}
		if !slices.Equal(m.Reasons, []string{"creation_linked", "reply_linked"}) {
			t.Fatalf("member %d reasons %v", i, m.Reasons)
		}
	}
	pair := res.Clusters[1]
	if len(pair.Members) != 2 || pair.Members[0].Account != keyID(w.keys["pair0"]) || pair.Members[1].Account != keyID(w.keys["pair1"]) {
		t.Fatalf("pair cluster %+v", pair)
	}
	members := res.Members()
	for _, n := range append([]string{"alice", "bob", "dave", "erin"}, append(slices.Clone(burstCrowd), burstTrio...)...) {
		if members[keyID(w.keys[n])] != nil {
			t.Fatalf("%s flagged: %v", n, members[keyID(w.keys[n])])
		}
	}
	// The detector is deterministic, and the funnel rows carry the reasons.
	again, err := s.DetectBurstClusters(testContext)
	if err != nil {
		t.Fatal(err)
	}
	again.ComputedAt = res.ComputedAt
	if fmt.Sprint(again) != fmt.Sprint(res) {
		t.Fatalf("second run differs:\n%+v\n%+v", again, res)
	}
	rows, err := s.FunnelRows(testContext)
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for _, r := range rows {
		if r.Burst != "" {
			flagged++
			if members[r.Account] == nil {
				t.Fatalf("row %s burst %q not in the result", r.Account, r.Burst)
			}
		}
	}
	if flagged != len(w.ring)+2 {
		t.Fatalf("rows flagged %d", flagged)
	}
}

// The report shows rates over everyone and excluding burst members, and
// they differ; /stats shows the ring as its own kind of identity.
func TestBurstClustersInReport(t *testing.T) {
	w := burstScenario(t)
	s := w.s
	rep, err := s.FunnelReport(testContext, FunnelQuery{From: w.base, To: s.now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	b := rep.Burst
	normal := 4 + len(burstCrowd) + len(burstTrio)
	total := len(w.ring) + 2 + normal
	if b.Clusters != 2 || b.Members != len(w.ring)+2 || b.InRange != len(w.ring)+2 || b.Overall.N != total || b.ExcludingBurst.N != normal {
		t.Fatalf("burst view %+v", b)
	}
	// alice's task activates her; pair0's thank-you answers pair1's reply,
	// a two-way thread, so the pair adds one activation the burst view
	// leaves out.
	if b.Overall.Activated != RateOf(2, total) || b.ExcludingBurst.Activated != RateOf(1, normal) {
		t.Fatalf("activated: all %+v, excluding %+v", b.Overall.Activated, b.ExcludingBurst.Activated)
	}
	if d1 := b.ExcludingBurst.Retention["d1"]; d1.K < 1 || d1.Pct <= b.Overall.Retention["d1"].Pct {
		t.Fatalf("d1: all %+v, excluding %+v", b.Overall.Retention["d1"], d1)
	}
	cohorts, _, err := s.FunnelPublic(testContext, 8)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range cohorts {
		kinds = append(kinds, c.Kind)
		if c.Kind == BurstKind && c.N < len(w.ring) {
			t.Fatalf("public burst line %+v", c)
		}
	}
	if !slices.Contains(kinds, BurstKind) {
		t.Fatalf("public kinds %v (the ring counts as its own kind)", kinds)
	}
}

// The detector's lookups use indexes, never a scan of events.
func TestBurstLookupsIndexed(t *testing.T) {
	w := burstScenario(t)
	for _, q := range []struct {
		sql  string
		args []any
	}{{burstAccountsSQL, nil}, {burstRepliesSQL, []any{`["a","b"]`}}} {
		rows, err := w.s.db.Query("EXPLAIN QUERY PLAN "+q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err = rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"SCAN e", "SCAN p", "SCAN d"} {
				if detail == table || strings.HasPrefix(detail, table+" ") {
					t.Errorf("%s: %s", q.sql, detail)
				}
			}
		}
		rows.Close()
	}
}
