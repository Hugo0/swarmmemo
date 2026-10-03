package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"swarmmemo/internal/graphmodel"
)

type universeBody struct {
	Generation string                   `json:"generation"`
	Datasets   []graphmodel.DatasetInfo `json:"datasets"`
	Nodes      graphmodel.Nodes         `json:"nodes"`
	Bridges    []graphmodel.BridgeView  `json:"bridges"`
}

// TestGraphCommunitySummary summarizes a whole galaxy from its statistics
// and a sample of its public exchanges, quoted as data, and caches it.
func TestGraphCommunitySummary(t *testing.T) {
	f := newGraphFixtureT(t)
	fake := &fakeSummarizer{}
	s := New(f.store, nil, Config{ServiceID: "swarmmemo.com", GraphSummary: &GraphSummaryConfig{Provider: fake}})
	var u universeBody
	_ = json.Unmarshal(graphGet(s, "/api/graph/universe", nil).Body.Bytes(), &u)
	body := fmt.Sprintf(`{"nodes":[%d],"gen":%q}`, u.Datasets[0].Node, u.Generation)
	w := graphPost(s, body, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AI summary") {
		t.Fatalf("community summary: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(fake.user, "<stats>") || !strings.Contains(fake.user, "PUBLIC reply") || strings.Contains(fake.user, "PRIVATE") || strings.Count(fake.user, "</messages>") != 1 {
		t.Fatalf("prompt: %s", fake.user)
	}
	if w := graphPost(s, body, nil); w.Code != 200 || fake.calls != 1 {
		t.Fatalf("cached: %d after %d calls", w.Code, fake.calls)
	}
	// A galaxy with no published text is summarized from its numbers alone.
	if w := graphPost(s, fmt.Sprintf(`{"nodes":[%d],"gen":%q}`, u.Datasets[len(u.Datasets)-2].Node, u.Generation), nil); w.Code != 200 || !strings.Contains(fake.user, "<messages>\n</messages>") {
		t.Fatalf("stats-only summary: %d %s", w.Code, fake.user)
	}
	if w := graphPost(s, `{"nodes":[999999],"gen":"`+u.Generation+`"}`, nil); w.Code != 404 {
		t.Fatalf("unknown node: %d", w.Code)
	}
}

// TestGraphLevelsServePublicHierarchy walks the semantic-zoom reads: the
// universe, a galaxy's children down to agents, a node's stats, search,
// locate and a bridge, and checks nothing private reaches any of them.
func TestGraphLevelsServePublicHierarchy(t *testing.T) {
	f := newGraphFixtureT(t)
	s := New(f.store, nil, Config{ServiceID: "swarmmemo.com"})
	w := graphGet(s, "/api/graph/universe", nil)
	if w.Code != 200 {
		t.Fatalf("universe: %d %s", w.Code, w.Body)
	}
	var u universeBody
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if len(u.Datasets) < 3 || u.Datasets[0].ID != "swarmmemo" || !u.Datasets[0].Live || len(u.Bridges) == 0 || u.Generation == "" {
		t.Fatalf("universe: %d datasets, %d bridges", len(u.Datasets), len(u.Bridges))
	}
	read := func(path string) string {
		t.Helper()
		w := graphGet(s, path, nil)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	// Open SwarmMemo down to its items.
	var all strings.Builder
	all.WriteString(w.Body.String())
	frontier := []int32{u.Datasets[0].Node}
	var items []int32
	for depth := 0; depth < 8 && len(frontier) > 0; depth++ {
		ids := make([]string, len(frontier))
		for i, id := range frontier {
			ids[i] = fmt.Sprint(id)
		}
		body := read("/api/graph/children?gen=" + u.Generation + "&ids=" + strings.Join(ids, ","))
		all.WriteString(body)
		var c struct {
			Nodes graphmodel.Nodes `json:"nodes"`
		}
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}
		frontier = nil
		for i, id := range c.Nodes.ID {
			if c.Nodes.Children[i] > 0 {
				frontier = append(frontier, id)
			} else if c.Nodes.Kind[i] < graphmodel.KindCommunity {
				items = append(items, id)
			}
		}
	}
	text := all.String()
	for _, want := range []string{f.alice.id, f.bob.id, "alice-graph", "#lobby"} {
		if !strings.Contains(text, want) {
			t.Errorf("the SwarmMemo galaxy lacks %q", want)
		}
	}
	for _, banned := range []string{"PUBLIC", "PRIVATE", "ADDRESSED", "vault", f.secret.id, f.dm.id} {
		if strings.Contains(text, banned) {
			t.Errorf("the hierarchy contains %q", banned)
		}
	}
	if len(items) == 0 {
		t.Fatal("no items reached")
	}
	node := read(fmt.Sprintf("/api/graph/node?gen=%s&id=%d", u.Generation, u.Datasets[0].Node))
	if !strings.Contains(node, `"members":`) || !strings.Contains(node, `"weekly":`) || strings.Contains(node, "PRIVATE") {
		t.Fatalf("node: %s", node)
	}
	if body := read("/api/graph/search?gen=" + u.Generation + "&q=alice-gr"); !strings.Contains(body, f.alice.id) {
		t.Fatalf("search: %s", body)
	}
	locate := read("/api/graph/locate?gen=" + u.Generation + "&keys=" + f.alice.id + "," + f.secret.id + "," + f.dm.id)
	if !strings.Contains(locate, f.alice.id) || strings.Contains(locate, f.secret.id) || strings.Contains(locate, f.dm.id) {
		t.Fatalf("locate: %s", locate)
	}
	if body := read("/api/graph/bridge?gen=" + u.Generation + "&id=0"); !strings.Contains(body, `"evidence"`) {
		t.Fatalf("bridge: %s", body)
	}
	stats := read(fmt.Sprintf("/api/graph/stats?gen=%s&ids=%d,%d", u.Generation, items[0], u.Datasets[1].Node))
	if !strings.Contains(stats, `"members":`) {
		t.Fatalf("stats: %s", stats)
	}
	for path, code := range map[string]int{
		"/api/graph/children?gen=nope&ids=1": 409, "/api/graph/children?ids=x": 400, "/api/graph/node?id=999999": 404,
		"/api/graph/search?q=a": 400, "/api/graph/universe?x=1": 400, "/api/graph/bridge?id=99999": 404,
	} {
		if w := graphGet(s, path, nil); w.Code != code {
			t.Errorf("%s answered %d, want %d", path, w.Code, code)
		}
	}
}

// TestGraphReplayServesAGalaxyTimeline returns every public item of a
// galaxy with its position, times and weekly posts, and nothing private.
func TestGraphReplayServesAGalaxyTimeline(t *testing.T) {
	f := newGraphFixtureT(t)
	s := New(f.store, nil, Config{ServiceID: "swarmmemo.com"})
	var u universeBody
	_ = json.Unmarshal(graphGet(s, "/api/graph/universe", nil).Body.Bytes(), &u)
	w := graphGet(s, fmt.Sprintf("/api/graph/replay?gen=%s&id=%d", u.Generation, u.Datasets[0].Node), nil)
	if w.Code != 200 {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	var r struct {
		Replay graphmodel.ReplayView `json:"replay"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	v := r.Replay
	if len(v.ID) == 0 || len(v.ID) != len(v.X) || len(v.ID) != len(v.Weeks) || len(v.ID) != len(v.First) || v.Total != len(v.ID) {
		t.Fatalf("replay arrays: %+v", v)
	}
	weeks := 0
	for _, ws := range v.Weeks {
		weeks += len(ws)
	}
	if weeks == 0 {
		t.Fatal("no weekly activity")
	}
	for _, banned := range []string{"PRIVATE", "ADDRESSED", "vault", f.secret.id, f.dm.id, "alice-graph"} {
		if strings.Contains(w.Body.String(), banned) {
			t.Errorf("replay contains %q", banned)
		}
	}
	for _, path := range []string{"/api/graph/replay?id=0&gen=" + u.Generation, "/api/graph/replay?id=999999&gen=" + u.Generation} {
		if w := graphGet(s, path, nil); w.Code != 404 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

// TestGraphAgentSheet answers an agent's sheet (places, counterparts) for an
// item, 404 for a container, and nothing private.
func TestGraphAgentSheet(t *testing.T) {
	f := newGraphFixtureT(t)
	s := New(f.store, nil, Config{ServiceID: "swarmmemo.com"})
	var u universeBody
	_ = json.Unmarshal(graphGet(s, "/api/graph/universe", nil).Body.Bytes(), &u)
	var loc struct {
		Paths map[string][]int32 `json:"paths"`
	}
	_ = json.Unmarshal(graphGet(s, "/api/graph/locate?gen="+u.Generation+"&keys="+f.alice.id, nil).Body.Bytes(), &loc)
	p := loc.Paths[f.alice.id]
	if len(p) == 0 {
		t.Fatal("alice not located")
	}
	w := graphGet(s, fmt.Sprintf("/api/graph/agent?gen=%s&id=%d", u.Generation, p[len(p)-1]), nil)
	if w.Code != 200 {
		t.Fatalf("agent: %d %s", w.Code, w.Body)
	}
	var r struct {
		Agent graphmodel.AgentSheet `json:"agent"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	a := r.Agent
	if a.Key != f.alice.id || a.Dataset != "swarmmemo" || len(a.Places) == 0 || len(a.Peers) == 0 || len(a.Excerpts) != 0 {
		t.Fatalf("sheet %+v", a)
	}
	for _, banned := range []string{"PRIVATE", "ADDRESSED", "vault", f.secret.id, f.dm.id} {
		if strings.Contains(w.Body.String(), banned) {
			t.Errorf("agent sheet contains %q", banned)
		}
	}
	if w := graphGet(s, fmt.Sprintf("/api/graph/agent?gen=%s&id=%d", u.Generation, u.Datasets[0].Node), nil); w.Code != 404 {
		t.Errorf("a galaxy's sheet answered %d", w.Code)
	}
}
