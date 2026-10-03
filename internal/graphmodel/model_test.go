package graphmodel

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cliques builds k groups of size n, dense inside and joined by one edge each.
func cliques(id string, k, n int) *Dataset {
	d := &Dataset{ID: id, Title: id}
	for g := 0; g < k; g++ {
		for i := 0; i < n; i++ {
			d.Items = append(d.Items, Item{Key: fmt.Sprintf("%s-%d-%d", id, g, i), Label: fmt.Sprintf("a%d.%d", g, i), Posts: int64(1 + i), First: 100, Last: 200,
				Weeks: map[int32]int32{0: int32(1 + i)}})
		}
	}
	for g := 0; g < k; g++ {
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				if i != j {
					d.Edges = append(d.Edges, Edge{Src: int32(g*n + i), Dst: int32(g*n + j), W: 3, First: 100, Last: 200})
				}
			}
		}
		d.Edges = append(d.Edges, Edge{Src: int32(g * n), Dst: int32(((g + 1) % k) * n), W: 1})
	}
	return d
}

func inside(m *Model, child, parent int32) bool {
	c, p := m.Nodes[child], m.Nodes[parent]
	return math.Hypot(c.X-p.X, c.Y-p.Y)+c.R <= p.R*1.0001+1e-6
}

func TestBuildFindsCommunitiesAndNestsThem(t *testing.T) {
	a, b := cliques("alpha", 4, 6), cliques("beta", 3, 5)
	m := Build([]*Dataset{a, b}, []Bridge{{A: BridgeEnd{"alpha", "alpha-0-0"}, B: BridgeEnd{"beta", "beta-1-2"}, Kind: "explicit", Conf: 0.9}, {A: BridgeEnd{"alpha", "nope"}, B: BridgeEnd{"beta", "beta-0-0"}}}, Options{})
	if len(m.Datasets) != 2 || len(m.Nodes[0].Children) != 2 {
		t.Fatalf("galaxies: %+v", m.Datasets)
	}
	g := m.Datasets[0].Node
	if got := len(m.Nodes[g].Children); got != 4 {
		t.Fatalf("alpha has %d top communities, want the 4 cliques", got)
	}
	for _, c := range m.Nodes[g].Children {
		if m.Nodes[c].Kind != KindCommunity || len(m.Nodes[c].Children) != 6 || m.Nodes[c].Members != 6 {
			t.Fatalf("community %+v", m.Nodes[c])
		}
	}
	// Every node lies inside its parent, and siblings never overlap.
	for _, n := range m.Nodes {
		if n.Parent < 0 {
			continue
		}
		if !inside(m, n.ID, n.Parent) {
			t.Fatalf("node %d (%s) outside its parent", n.ID, n.Label)
		}
		for _, s := range m.Nodes[n.Parent].Children {
			if s > n.ID {
				o := m.Nodes[s]
				if math.Hypot(o.X-n.X, o.Y-n.Y) < (o.R+n.R)*0.999-1e-6 {
					t.Fatalf("siblings %d and %d overlap", n.ID, s)
				}
			}
		}
		if n.X < 0 || n.Y < 0 || n.X > Space || n.Y > Space {
			t.Fatalf("node outside space: %+v", n)
		}
	}
	// Inter-clique edges become flows between communities under the galaxy.
	if len(m.Flows[g]) != 4 {
		t.Fatalf("galaxy flows %+v", m.Flows[g])
	}
	// An end outside the sample attaches to its galaxy.
	if len(m.Bridges) != 2 || m.Nodes[m.Bridges[0].A].Key != "alpha-0-0" || m.Bridges[1].A != m.Datasets[0].Node {
		t.Fatalf("bridges %+v", m.Bridges)
	}
	// The build is deterministic.
	again := Build([]*Dataset{cliques("alpha", 4, 6), cliques("beta", 3, 5)}, nil, Options{})
	for i := range again.Nodes {
		if again.Nodes[i].X != m.Nodes[i].X || again.Nodes[i].Y != m.Nodes[i].Y {
			t.Fatal("layout differs between identical builds")
		}
	}
	s := m.Stats([]int32{m.Nodes[g].Children[0]})
	if s.Members != 6 || s.Reciprocity < 0.99 || s.Density < 0.99 || len(s.TopPairs) == 0 || len(s.Bridges) != 2 {
		t.Fatalf("stats %+v", s)
	}
	nodes, flows, cut := m.Children([]int32{g}, 2)
	if !cut || len(nodes.ID) != 2 || len(flows.A) > 1 {
		t.Fatalf("budgeted children: %+v %+v", nodes.ID, flows)
	}
	if res := m.Search("a2.5", 5); len(res) != 1 || res[0].Path[0] != 0 {
		t.Fatalf("search %+v", res)
	}
	if p, ok := m.Locate("beta", "beta-1-2"); !ok || len(p) < 3 {
		t.Fatalf("locate %v", p)
	}
}

func TestEmbeddedUniverseBuilds(t *testing.T) {
	ds, bridges, err := Embedded()
	if err != nil || len(ds) < 2 || len(bridges) == 0 {
		t.Fatalf("embedded: %v, %d datasets, %d bridges", err, len(ds), len(bridges))
	}
	m := Build(ds, bridges, Options{})
	explicit := 0
	for _, b := range m.Bridges {
		if !b.Dashed {
			explicit++
		}
	}
	if len(m.Bridges) == 0 || explicit == 0 {
		t.Fatalf("bridges resolved: %d, explicit %d", len(m.Bridges), explicit)
	}
	t.Logf("%d galaxies, %d nodes, %d bridges (%d explicit), built in %v", len(m.Datasets), len(m.Nodes), len(m.Bridges), explicit, m.BuildTime)
	for _, d := range m.Datasets {
		if d.ID == "aivillage" && d.Citation == "" {
			t.Fatal("AI Village must carry its citation")
		}
	}
}

// synthetic makes a power-law dataset of n identities whose posts sum to
// about posts, with communities of varied size and mostly local replies.
func synthetic(n int, posts int64, seed int64) *Dataset {
	r := rand.New(rand.NewSource(seed))
	d := &Dataset{ID: "synthetic", Title: "Synthetic", Items: make([]Item, n)}
	weights := make([]float64, n)
	var sum float64
	for i := range weights {
		weights[i] = 1 / math.Pow(float64(i%997+1), 0.9)
		sum += weights[i]
	}
	group := func(i int) int { return i / 500 }
	for i := range d.Items {
		p := int64(float64(posts) * weights[i] / sum)
		if p < 1 {
			p = 1
		}
		d.Items[i] = Item{Key: strconv.Itoa(i), Label: "agent-" + strconv.Itoa(i), Posts: p, First: int64(1_700_000_000 + r.Intn(9_000_000)), Last: 1_790_000_000}
	}
	edges := 3 * n
	d.Edges = make([]Edge, 0, edges)
	for e := 0; e < edges; e++ {
		a := r.Intn(n)
		var b int
		if r.Float64() < 0.9 {
			b = group(a)*500 + r.Intn(500)
			if b >= n {
				b = n - 1
			}
		} else {
			b = r.Intn(n)
		}
		w := float64(1 + r.Intn(int(math.Min(40, float64(d.Items[a].Posts))+1)))
		d.Edges = append(d.Edges, Edge{Src: int32(a), Dst: int32(b), W: w, First: d.Items[a].First, Last: d.Items[a].Last})
	}
	return d
}

// TestScaleMillion builds the model of a synthetic 1M-identity, ~50M-message
// dataset and reports the precompute time, memory and per-view payloads.
// Opt in: GRAPH_SCALE=1000000 (it takes a while; run it at nice 19).
func TestScaleMillion(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("GRAPH_SCALE"))
	if n == 0 {
		t.Skip("set GRAPH_SCALE to run")
	}
	d := synthetic(n, 50_000_000, 1)
	runtime.GC()
	start := time.Now()
	m := Build([]*Dataset{d}, nil, Options{})
	took := time.Since(start)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("%d identities, %d edges: build %v, %d nodes, heap %d MiB", n, len(d.Edges), took.Round(time.Millisecond), len(m.Nodes), ms.HeapAlloc>>20)
	size := func(v any) int { b, _ := json.Marshal(v); return len(b) }
	nodes, flows, bridges := m.Universe(50_000)
	t.Logf("universe view: %d nodes, %d flows, %d bytes", len(nodes.ID), len(flows.A), size([]any{nodes, flows, bridges}))
	// Descend along the busiest child to a leaf, reporting each level's payload.
	id := m.Datasets[0].Node
	for level := 1; len(m.Nodes[id].Children) > 0 && level < 10; level++ {
		kids, fl, cut := m.Children([]int32{id}, 50_000)
		t.Logf("level %d (%s): %d children, %d flows, cut=%v, %d bytes", level, m.Nodes[id].Label, len(kids.ID), len(fl.A), cut, size([]any{kids, fl}))
		best := id
		for _, c := range m.Nodes[id].Children {
			if len(m.Nodes[c].Children) > 0 && (best == id || m.Nodes[c].Posts > m.Nodes[best].Posts) {
				best = c
			}
		}
		if best == id {
			break
		}
		id = best
	}
	s := time.Now()
	st := m.Stats([]int32{m.Nodes[m.Datasets[0].Node].Children[0]})
	t.Logf("stats of a top community: %d members in %v", st.Members, time.Since(s).Round(time.Millisecond))
}

// TestShippedUniverse builds the embedded datasets beside a live galaxy: the
// live one sits at the centre of space with the others around it, and
// collusion.wiki is its own galaxy whose top communities are its named
// clusters (the two tied as strongly as they hold together are one), with
// its infrastructure domains as their own kind.
func TestShippedUniverse(t *testing.T) {
	extra, bridges, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	live := &Dataset{ID: "swarmmemo", Title: "SwarmMemo", Live: true, Items: []Item{{Key: "a", Label: "a", Posts: 3}, {Key: "b", Label: "b", Posts: 2}}, Edges: []Edge{{Src: 0, Dst: 1, W: 2}}}
	m := Build(append([]*Dataset{live}, extra...), bridges, Options{})
	g := m.Nodes[m.galaxyOf["swarmmemo"]]
	if math.Abs(g.X-Space/2) > 1e-6 || math.Abs(g.Y-Space/2) > 1e-6 {
		t.Fatalf("the live galaxy is not at the centre: %v,%v", g.X, g.Y)
	}
	for _, id := range m.Nodes[0].Children {
		o := m.Nodes[id]
		if id != g.ID && math.Hypot(o.X-g.X, o.Y-g.Y) < o.R+g.R {
			t.Fatalf("%s overlaps the centre", o.Label)
		}
	}
	cw, ok := m.galaxyOf["collusionwiki"]
	if !ok {
		t.Fatal("no collusion.wiki galaxy")
	}
	var labels []string
	merged := false
	for _, c := range m.Nodes[cw].Children {
		n := m.Nodes[c]
		if n.Kind != KindCommunity {
			continue
		}
		labels = append(labels, n.Label)
		merged = merged || strings.Contains(n.Label, "June 18 welcome-page crowd · C4+C5")
	}
	if len(labels) < 5 || !merged {
		t.Fatalf("collusion.wiki communities: %q", labels)
	}
	infra := 0
	for _, n := range m.Nodes {
		if n.Kind == KindInfra && m.Datasets[n.Dataset].ID == "collusionwiki" {
			infra++
		}
	}
	if infra < 10 {
		t.Fatalf("%d infrastructure nodes", infra)
	}
	t.Logf("collusion.wiki: %d infra, communities %q", infra, labels)
	// The candidate swarms sit beside collusion.wiki, tied to it by a dashed
	// observer-effect bridge, their clusters named with their confidence.
	sh, ok := m.galaxyOf["swarmhunt"]
	if !ok {
		t.Fatal("no candidate swarms galaxy")
	}
	a, b := m.Nodes[sh], m.Nodes[cw]
	near := math.Hypot(a.X-b.X, a.Y-b.Y) - a.R - b.R
	for _, id := range m.Nodes[0].Children {
		if o := m.Nodes[id]; id != sh && id != cw && math.Hypot(a.X-o.X, a.Y-o.Y)-a.R-o.R < near-1 && o.Dataset != 0 {
			t.Logf("%s is nearer the candidate swarms than collusion.wiki", o.Label)
		}
	}
	if near > b.R {
		t.Fatalf("candidate swarms are %.0f from collusion.wiki (radius %.0f)", near, b.R)
	}
	observer := false
	for _, br := range m.Bridges {
		observer = observer || (br.Kind == "observer-effect" && br.Dashed && (br.A == cw || br.B == cw))
	}
	conf := false
	for _, c := range m.Nodes[sh].Children {
		conf = conf || strings.Contains(m.Nodes[c].Label, "confidence) · C2")
	}
	if !observer || !conf {
		t.Fatalf("observer bridge %v, labelled clusters %v", observer, conf)
	}
}
