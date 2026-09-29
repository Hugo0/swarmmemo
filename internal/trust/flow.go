package trust

import (
	"context"
	"errors"
)

// ErrWorkExceeded aborts a run that examined more than max_work edges or ran
// past max_seconds; the previous run stays current (§4.3).
var ErrWorkExceeded = errors.New("trust: max_work or max_seconds exceeded")

// network is a Dinic max-flow network over integer capacities. It is
// specified exactly, because a verifier must reproduce which paths it finds,
// not only the flow value (a max flow's split over sinks is not unique):
//
//   - add(u, v, c) appends the edge u→v (id e, capacity c) and its reverse
//     v→u (id e^1, capacity 0) and appends e to u's list and e^1 to v's list.
//   - maxflow repeats phases until t is unreachable. A phase is a BFS from s
//     over each node's list in insertion order through edges with residual
//     capacity > 0, giving level[]; then, with a per-node pointer it[] starting
//     at 0, it augments single paths until none is found. A path search starts
//     at s: at node u it takes the first edge from it[u] on with residual > 0
//     into level[u]+1; at a dead end it steps back to the parent and advances
//     the parent's pointer by one. Reaching t, it pushes the path's bottleneck;
//     pointers along the path are not advanced. This is the classic recursive
//     dfs(u, f) returning on the first successful child.
//
// work counts every edge examined by BFS and path search; past maxWork, or
// once the context is done, maxflow returns ErrWorkExceeded.
type network struct {
	adj     [][]int32
	to      []int32
	cap     []int64
	work    int64
	maxWork int64
	ctx     context.Context
	level   []int32
	it      []int32
	queue   []int32
	path    []int32
}

func newNetwork(ctx context.Context, n int, maxWork, work int64) *network {
	return &network{adj: make([][]int32, n), ctx: ctx, maxWork: maxWork, work: work,
		level: make([]int32, n), it: make([]int32, n)}
}

func (g *network) add(u, v int, c int64) int {
	id := len(g.to)
	g.to = append(g.to, int32(v), int32(u))
	g.cap = append(g.cap, c, 0)
	g.adj[u] = append(g.adj[u], int32(id))
	g.adj[v] = append(g.adj[v], int32(id+1))
	return id
}

// flowOn is the flow an edge carries: its reverse residual.
func (g *network) flowOn(e int) int64 { return g.cap[e^1] }

func (g *network) tick() error {
	g.work++
	if g.work > g.maxWork {
		return ErrWorkExceeded
	}
	if g.work&0xffff == 0 && g.ctx.Err() != nil {
		return ErrWorkExceeded
	}
	return nil
}

func (g *network) maxflow(s, t int) error {
	for {
		for i := range g.level {
			g.level[i] = -1
		}
		g.level[s] = 0
		g.queue = append(g.queue[:0], int32(s))
		for head := 0; head < len(g.queue); head++ {
			u := g.queue[head]
			for _, e := range g.adj[u] {
				if err := g.tick(); err != nil {
					return err
				}
				if v := g.to[e]; g.cap[e] > 0 && g.level[v] < 0 {
					g.level[v] = g.level[u] + 1
					g.queue = append(g.queue, v)
				}
			}
		}
		if g.level[t] < 0 {
			return nil
		}
		for i := range g.it {
			g.it[i] = 0
		}
		for {
			f, err := g.augment(s, t)
			if err != nil {
				return err
			}
			if f == 0 {
				break
			}
		}
	}
}

func (g *network) augment(s, t int) (int64, error) {
	path := g.path[:0]
	defer func() { g.path = path[:0] }()
	u := int32(s)
	for {
		if u == int32(t) {
			f := int64(1) << 62
			for _, e := range path {
				f = min(f, g.cap[e])
			}
			for _, e := range path {
				g.cap[e] -= f
				g.cap[e^1] += f
			}
			return f, nil
		}
		advanced := false
		for int(g.it[u]) < len(g.adj[u]) {
			if err := g.tick(); err != nil {
				return 0, err
			}
			e := g.adj[u][g.it[u]]
			if v := g.to[e]; g.cap[e] > 0 && g.level[v] == g.level[u]+1 {
				path = append(path, e)
				u = v
				advanced = true
				break
			}
			g.it[u]++
		}
		if advanced {
			continue
		}
		if len(path) == 0 {
			return 0, nil
		}
		e := path[len(path)-1]
		path = path[:len(path)-1]
		u = g.to[e^1]
		g.it[u]++
	}
}

// flowGraph is the endorsement graph one flow run reads: nodes indexed by
// sorted account id, edges in (src, dst) order.
type flowGraph struct {
	n       int
	src     []int32 // edge sources
	dst     []int32 // edge targets
	cap     []int64 // edge capacities in flow units
	transit []int64 // per node: what it may pass on (seeds: the pool)
	hub     []int32 // per node: its root hub index, or -1 to drain straight to T
	hubs    int     // number of hubs
	hubCap  int64   // per hub: root_cap × U
	nodeCap int64   // per node: U
	steps   int64   // progressive filling levels
}

// flowResult is one run of the flow from one seed set.
type flowResult struct {
	node []int64 // absorbed by each node
	edge []int64 // carried by each graph edge
}

// runFlow computes stake-bounded capacity flow from seeds (sorted node
// indices), splitting pool equally over them, with progressive filling so a
// scarce pool is shared max-min fairly (§4.3 step 3). Layout: in-node i,
// out-node n+i, hub j at 2n+j, then S and T. Insertion order: in→out transit
// edges by node; graph edges by (src, dst); sink edges by node (capacity 0,
// raised each level); hub→T edges by hub; S→seed edges by seed.
func runFlow(ctx context.Context, g *flowGraph, seeds []int32, pool int64, maxWork, work int64) (flowResult, int64, error) {
	res := flowResult{node: make([]int64, g.n), edge: make([]int64, len(g.src))}
	if len(seeds) == 0 || g.n == 0 {
		return res, work, nil
	}
	n := g.n
	S, T := 2*n+g.hubs, 2*n+g.hubs+1
	net := newNetwork(ctx, T+1, maxWork, work)
	isSeed := make([]bool, n)
	for _, s := range seeds {
		isSeed[s] = true
	}
	for i := 0; i < n; i++ {
		c := g.transit[i]
		if isSeed[i] {
			c = pool
		}
		net.add(i, n+i, c)
	}
	edgeIDs := make([]int, len(g.src))
	for k := range g.src {
		edgeIDs[k] = -1
		if g.cap[k] > 0 {
			edgeIDs[k] = net.add(n+int(g.src[k]), int(g.dst[k]), g.cap[k])
		}
	}
	sink := make([]int, n)
	for i := 0; i < n; i++ {
		to := T
		if g.hub[i] >= 0 {
			to = 2*n + int(g.hub[i])
		}
		sink[i] = net.add(i, to, 0)
	}
	for j := 0; j < g.hubs; j++ {
		net.add(2*n+j, T, g.hubCap)
	}
	perSeed := max(1, pool/int64(len(seeds)))
	for _, s := range seeds {
		net.add(S, int(s), perSeed)
	}
	var done int64
	for k := int64(1); k <= g.steps; k++ {
		level := g.nodeCap * k / g.steps
		for _, e := range sink {
			net.cap[e] += level - done
		}
		done = level
		if err := net.maxflow(S, T); err != nil {
			return res, net.work, err
		}
	}
	for i, e := range sink {
		res.node[i] = net.flowOn(e)
	}
	for k, e := range edgeIDs {
		if e >= 0 {
			res.edge[k] = net.flowOn(e)
		}
	}
	return res, net.work, nil
}
