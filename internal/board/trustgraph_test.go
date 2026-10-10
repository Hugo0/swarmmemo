package board

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The public trust network on a real store: the core is the public accounts
// with standing in the latest run, highest first; edges are current public
// vouches (withdrawn ones are not edges); an account that is not public is
// never named, even when a vouch reaches it; the caps hold and are counted.
func TestTrustGraph(t *testing.T) {
	s, seed, alice, bob, _ := trustFixture(t, nil)
	if _, err := s.db.Exec(endorsementSchema); err != nil {
		t.Fatal(err)
	}
	// A key that never posted in public: an account trust.get calls not found.
	hidden := keyFor(70)
	if _, err := s.db.Exec("INSERT INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,?,?,?,?)", keyID(hidden), "pk-hidden", keyID(hidden), testTime, testTime); err != nil {
		t.Fatal(err)
	}
	// Before any run the graph is empty, not an error.
	g, err := s.TrustGraph(testContext, 0)
	if err != nil || g.Run != 0 || len(g.Nodes) != 0 || g.Nodes == nil || g.Edges == nil || g.Limits.Core != TrustGraphCoreDefault {
		t.Fatalf("before a run: %+v %v", g, err)
	}
	sum, err := s.RunTrust(testContext)
	if err != nil || sum.State != "done" {
		t.Fatalf("run: %+v %v", sum, err)
	}
	for _, v := range []struct {
		from, to string
		value    int
	}{{keyID(seed), keyID(alice), 1}, {keyID(seed), keyID(hidden), 1}, {keyID(alice), keyID(bob), 0}} {
		if _, err := s.db.Exec("INSERT INTO vouches VALUES(?,?,?,0,0,?)", v.from, v.to, v.value, testTime); err != nil {
			t.Fatal(err)
		}
	}
	s.trust.graph.m = nil // the cache holds the empty graph read above
	g, err = s.TrustGraph(testContext, 100)
	if err != nil || g.Run != sum.ID || g.Mode != "shadow" || g.ParamsVersion == 0 {
		t.Fatalf("graph: %+v %v", g, err)
	}
	byID := map[string]TrustGraphNode{}
	core := 0
	for i, n := range g.Nodes {
		byID[n.ID] = n
		if n.Core {
			core++
			if i > 0 && g.Nodes[i-1].StandingCents < n.StandingCents {
				t.Errorf("core not by standing, highest first: %+v", g.Nodes)
			}
		}
		if n.BandName != BandName(n.Band) || n.Roots == nil || !strings.HasPrefix(n.FakeCost, "about $") {
			t.Errorf("node %+v", n)
		}
	}
	sd, ok := byID[keyID(seed)]
	if !ok || !sd.Core || sd.StandingCents <= 0 || len(sd.Roots) != 1 || sd.Roots[0] != "arbiter" {
		t.Fatalf("the seed: %+v", sd)
	}
	if a, ok := byID[keyID(alice)]; !ok || !a.Core || a.StandingCents <= 0 {
		t.Fatalf("alice: %+v", a)
	}
	if _, ok := byID[keyID(hidden)]; ok {
		t.Fatal("an account that is not public is named")
	}
	if len(g.Edges) != 1 || g.Edges[0] != (TrustGraphEdge{From: keyID(seed), To: keyID(alice), Kind: "vouch", Count: 1, Weight: 10}) {
		t.Fatalf("edges: %+v", g.Edges)
	}
	body, _ := json.Marshal(g)
	if strings.Contains(string(body), keyID(hidden)) || strings.Contains(string(body), "arbiter_seed") {
		t.Fatal("the graph names more than root kinds and public accounts")
	}

	// Caps: a core of one keeps the highest and counts the rest.
	g1, err := s.TrustGraph(testContext, 1)
	if err != nil {
		t.Fatal(err)
	}
	n1 := 0
	for _, n := range g1.Nodes {
		if n.Core {
			n1++
			if n.ID != g.Nodes[0].ID {
				t.Errorf("core of one is %s, not the highest", n.ID)
			}
		}
	}
	if n1 != 1 || len(g1.Nodes) > 2 || g1.Truncated.Core != core-1 || g1.Limits.Nodes != TrustGraphNodesPerCore {
		t.Fatalf("core of one: %d core, %d nodes, %+v %+v", n1, len(g1.Nodes), g1.Truncated, g1.Limits)
	}
	// Out-of-range sizes fall back to the default.
	if g, err := s.TrustGraph(testContext, TrustGraphCoreMax+1); err != nil || g.Limits.Core != TrustGraphCoreDefault {
		t.Fatalf("oversized core: %+v %v", g.Limits, err)
	}
	// The graph is cached for a while, then read again.
	if _, err := s.db.Exec("DELETE FROM vouches"); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.TrustGraph(testContext, 100); len(g.Edges) != 1 {
		t.Fatal("not cached")
	}
	now := s.now()
	s.now = func() time.Time { return now.Add(trustGraphTTL * time.Second) }
	if g, _ := s.TrustGraph(testContext, 100); len(g.Edges) != 0 {
		t.Fatal("not read again after the TTL")
	}

	// With trust off there is no graph.
	off := openTest(t, Config{})
	if _, err := off.TrustGraph(testContext, 10); err == nil {
		t.Fatal("trust off serves a graph")
	}
}
