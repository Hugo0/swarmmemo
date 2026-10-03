package graphmodel

import "sort"

// wgraph is an undirected weighted graph in compressed adjacency form.
// Self-loops carry the weight of edges inside an aggregated node.
type wgraph struct {
	n    int
	off  []int32 // n+1 offsets into nbr/w
	nbr  []int32
	w    []float64
	self []float64 // weight of each node's self-loop (counted once)
}

type wedge struct {
	a, b int32
	w    float64
}

// newWGraph builds a graph from undirected edges; duplicates are summed and
// a == b edges become self-loops.
func newWGraph(n int, edges []wedge) *wgraph {
	g := &wgraph{n: n, off: make([]int32, n+1), self: make([]float64, n)}
	deg := make([]int32, n)
	for _, e := range edges {
		if e.a == e.b {
			continue
		}
		deg[e.a]++
		deg[e.b]++
	}
	for i := 0; i < n; i++ {
		g.off[i+1] = g.off[i] + deg[i]
	}
	g.nbr = make([]int32, g.off[n])
	g.w = make([]float64, g.off[n])
	pos := append([]int32(nil), g.off[:n]...)
	for _, e := range edges {
		if e.a == e.b {
			g.self[e.a] += e.w
			continue
		}
		g.nbr[pos[e.a]], g.w[pos[e.a]] = e.b, e.w
		pos[e.a]++
		g.nbr[pos[e.b]], g.w[pos[e.b]] = e.a, e.w
		pos[e.b]++
	}
	return g
}

// louvain runs multi-level Louvain modularity optimisation and returns one
// partition per level, each mapping the original nodes to a community index
// (0..k-1), finest first. Node order is fixed, so results are deterministic.
// Levels that merge nothing are dropped.
func louvain(n int, edges []wedge, maxLevels int) [][]int32 {
	g := newWGraph(n, edges)
	member := make([]int32, n) // original node -> current aggregated node
	for i := range member {
		member[i] = int32(i)
	}
	var levels [][]int32
	for level := 0; level < maxLevels && g.n > 1; level++ {
		comm, k := louvainPass(g)
		if k == g.n {
			break // nothing merged
		}
		for i := range member {
			member[i] = comm[member[i]]
		}
		levels = append(levels, append([]int32(nil), member...))
		if k <= 1 {
			break
		}
		g = aggregate(g, comm, k)
	}
	return levels
}

// louvainPass moves nodes between communities while modularity improves and
// returns the renumbered communities and their count.
func louvainPass(g *wgraph) ([]int32, int) {
	n := g.n
	k := make([]float64, n) // node strength
	var m2 float64          // 2m
	for i := 0; i < n; i++ {
		s := 2 * g.self[i]
		for j := g.off[i]; j < g.off[i+1]; j++ {
			s += g.w[j]
		}
		k[i] = s
		m2 += s
	}
	comm := make([]int32, n)
	tot := make([]float64, n)
	for i := range comm {
		comm[i] = int32(i)
		tot[i] = k[i]
	}
	if m2 == 0 {
		return comm, n
	}
	links := make([]float64, n)
	touched := make([]int32, 0, 64)
	for pass := 0; pass < 32; pass++ {
		moved := 0
		for i := 0; i < n; i++ {
			ci := comm[i]
			touched = touched[:0]
			for j := g.off[i]; j < g.off[i+1]; j++ {
				c := comm[g.nbr[j]]
				if links[c] == 0 {
					touched = append(touched, c)
				}
				links[c] += g.w[j]
			}
			tot[ci] -= k[i]
			best, bestGain := ci, links[ci]-tot[ci]*k[i]/m2
			for _, c := range touched {
				gain := links[c] - tot[c]*k[i]/m2
				if gain > bestGain+1e-12 {
					best, bestGain = c, gain
				}
			}
			tot[best] += k[i]
			if best != ci {
				comm[i] = best
				moved++
			}
			for _, c := range touched {
				links[c] = 0
			}
			links[ci] = 0
		}
		if moved == 0 {
			break
		}
	}
	// Renumber densely, in order of first appearance.
	re := make([]int32, n)
	for i := range re {
		re[i] = -1
	}
	next := int32(0)
	for i := 0; i < n; i++ {
		c := comm[i]
		if re[c] < 0 {
			re[c] = next
			next++
		}
		comm[i] = re[c]
	}
	return comm, int(next)
}

// aggregate collapses each community into one node.
func aggregate(g *wgraph, comm []int32, k int) *wgraph {
	sum := map[[2]int32]float64{}
	selfW := make([]float64, k)
	for i := 0; i < g.n; i++ {
		ci := comm[i]
		selfW[ci] += g.self[i]
		for j := g.off[i]; j < g.off[i+1]; j++ {
			nb := g.nbr[j]
			if int32(i) > nb {
				continue // each undirected edge once
			}
			cj := comm[nb]
			if ci == cj {
				selfW[ci] += g.w[j]
				continue
			}
			a, b := ci, cj
			if a > b {
				a, b = b, a
			}
			sum[[2]int32{a, b}] += g.w[j]
		}
	}
	keys := make([][2]int32, 0, len(sum))
	for key := range sum {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	edges := make([]wedge, 0, len(keys)+k)
	for _, key := range keys {
		edges = append(edges, wedge{key[0], key[1], sum[key]})
	}
	out := newWGraph(k, edges)
	copy(out.self, selfW)
	return out
}
