package trust

import (
	"bytes"
	"context"
	"strconv"
	"testing"
)

// FuzzTrustGraph builds random small graphs and checks, on the flow network,
// that whatever reaches a seed-free region is at most what the edges into it
// can carry from their sources' transit, that no node absorbs more than U
// and the total never exceeds the pool; and, on the whole computation, that
// the output is deterministic and a node with no inbound edge has flow 0.
func FuzzTrustGraph(f *testing.F) {
	f.Add([]byte{5, 2, 1, 0, 1, 2, 3, 4, 1, 3, 2, 4, 9, 9, 9})
	f.Add([]byte("seed flow region bound determinism"))
	f.Add(bytes.Repeat([]byte{7, 1, 250, 3}, 40))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 3 {
			return
		}
		n := 3 + int(data[0])%28
		nSeeds := 1 + int(data[1])%3
		region := int(data[2]) % (n - nSeeds + 1) // the last `region` nodes are seed-free
		data = data[3:]
		next := func() int {
			if len(data) == 0 {
				return 0
			}
			v := int(data[0])
			data = data[1:]
			return v
		}
		const U = 20
		g := &flowGraph{n: n, transit: make([]int64, n), hub: make([]int32, n), hubCap: 3 * U, nodeCap: U, steps: 5}
		for i := range g.hub {
			g.hub[i] = -1
			g.transit[i] = int64(next() % 30)
		}
		if n > 4 && next()%2 == 1 { // one shared root
			g.hubs = 1
			g.hub[n-1], g.hub[n-2] = 0, 0
		}
		type pair struct{ s, d int }
		seen := map[pair]bool{}
		var edges []pair
		for k := 0; k < 4*n && len(data) >= 3; k++ {
			s, d := next()%n, next()%n
			c := int64(next() % (U + 1))
			if s == d || seen[pair{s, d}] {
				continue
			}
			seen[pair{s, d}] = true
			edges = append(edges, pair{s, d})
			g.cap = append(g.cap, c)
		}
		// Edges in (src, dst) order, as the run builds them.
		order := make([]int, len(edges))
		for i := range order {
			order[i] = i
		}
		for i := 1; i < len(order); i++ {
			for j := i; j > 0 && (edges[order[j]].s < edges[order[j-1]].s || edges[order[j]].s == edges[order[j-1]].s && edges[order[j]].d < edges[order[j-1]].d); j-- {
				order[j], order[j-1] = order[j-1], order[j]
			}
		}
		caps := g.cap
		g.cap = nil
		for _, i := range order {
			g.src = append(g.src, int32(edges[i].s))
			g.dst = append(g.dst, int32(edges[i].d))
			g.cap = append(g.cap, caps[i])
		}
		var seeds []int32
		for i := 0; i < nSeeds; i++ {
			seeds = append(seeds, int32(i))
		}
		pool := int64(U * n)
		res, _, err := runFlow(context.Background(), g, seeds, pool, 1e8, 0)
		if err != nil {
			t.Fatal(err)
		}
		again, _, _ := runFlow(context.Background(), g, seeds, pool, 1e8, 0)
		inRegion := func(i int32) bool { return int(i) >= n-region }
		var captured, total int64
		for i, v := range res.node {
			if v < 0 || v > U || v != again.node[i] {
				t.Fatalf("node %d absorbs %d (again %d)", i, v, again.node[i])
			}
			total += v
			if inRegion(int32(i)) {
				captured += v
			}
		}
		if total > pool {
			t.Fatalf("total %d above pool %d", total, pool)
		}
		intoRegion := map[int32]int64{}
		for k := range g.src {
			if res.edge[k] > g.cap[k] {
				t.Fatalf("edge %d carries %d over %d", k, res.edge[k], g.cap[k])
			}
			if !inRegion(g.src[k]) && inRegion(g.dst[k]) {
				intoRegion[g.src[k]] += g.cap[k]
			}
		}
		var bound int64
		for s, c := range intoRegion {
			limit := g.transit[s]
			if int(s) < nSeeds {
				limit = pool
			}
			bound += min(c, limit)
		}
		if captured > bound {
			t.Fatalf("region captured %d above its cut bound %d", captured, bound)
		}

		// The whole computation on a snapshot built from the same graph.
		b := newBuilder()
		b.snap.Params.Seeds = nil
		names := make([]string, n)
		for i := range names {
			names[i] = "n" + strconv.Itoa(i)
			b.account(names[i], int64(1+i%40), int64(1+i%9), g.transit[i])
			if i < nSeeds {
				b.snap.Params.Seeds = append(b.snap.Params.Seeds, acct(names[i]))
			}
		}
		inbound := map[string]bool{}
		for k := range g.src {
			kind := []string{"vote", "vouch", "legacy_vote", "unsigned"}[int(g.cap[k])%4]
			b.endorse(kind, names[g.src[k]], names[g.dst[k]], 1, g.cap[k]%20)
			if kind == "vote" || kind == "vouch" {
				inbound[acct(names[g.dst[k]])] = true
			}
		}
		one, err := Compute(context.Background(), b.snap)
		if err != nil {
			t.Fatal(err)
		}
		two, _ := Compute(context.Background(), b.snap)
		if !bytes.Equal(one.Canonical(), two.Canonical()) {
			t.Fatal("not deterministic")
		}
		seed := map[string]bool{}
		for _, s := range b.snap.Params.Seeds {
			seed[s] = true
		}
		for _, sc := range one.Scores {
			if !seed[sc.Account] && !inbound[sc.Account] && sc.Flow != 0 {
				t.Fatalf("%s has flow %d with no inbound edge", sc.Account, sc.Flow)
			}
		}
	})
}
