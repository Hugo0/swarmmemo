package graphmodel

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options tune a build.
type Options struct {
	MaxLevels    int // Louvain levels kept per dataset (default 4)
	FlowsPerNode int // flows kept per parent (default 3000)
	Now          time.Time
}

// Build makes the hierarchy of the datasets, in order, and resolves the
// bridges between their items. Bridges whose ends are unknown are dropped.
func Build(datasets []*Dataset, bridges []Bridge, opt Options) *Model {
	start := time.Now()
	if opt.MaxLevels <= 0 {
		opt.MaxLevels = 4
	}
	if opt.FlowsPerNode <= 0 {
		opt.FlowsPerNode = 3000
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	m := &Model{Generated: opt.Now, Flows: map[int32][]Flow{}, byKey: map[string]int32{}, byLabel: map[string]int32{}, galaxyOf: map[string]int32{}, data: datasets, bridgesOf: map[int32][]int32{}, affinity: map[string]float64{}}
	for _, b := range bridges {
		w := 1.0
		if !b.Dashed {
			w = 3 // explicit evidence pulls a galaxy closer than a weak hint
		}
		m.affinity[b.A.Dataset+"\x00"+b.B.Dataset] += w
		m.affinity[b.B.Dataset+"\x00"+b.A.Dataset] += w
	}
	root := m.add(Node{Parent: -1, Kind: KindUniverse, Dataset: -1, Item: -1, Label: "Universe"})
	m.itemNode = make([][]int32, len(datasets))
	m.out = make([][][]int32, len(datasets))
	m.member = make([][][]int32, len(datasets))
	m.inc = make([][][]int32, len(datasets))
	for di, d := range datasets {
		g := m.buildDataset(int16(di), d, root, opt)
		info := DatasetInfo{ID: d.ID, Title: d.Title, Description: d.Description, Citation: d.Citation, URL: d.URL, Live: d.Live, Node: g, Items: len(d.Items)}
		for _, it := range d.Items {
			info.Posts += it.Posts
			if it.First > 0 && (info.T0 == 0 || it.First < info.T0) {
				info.T0 = it.First
			}
			if it.Last > info.T1 {
				info.T1 = it.Last
			}
		}
		m.Datasets = append(m.Datasets, info)
	}
	m.lastWeek = make([]int64, len(datasets))
	for di := range datasets {
		m.lastWeek[di] = m.datasetLastWeek(int16(di))
	}
	m.aggregate(root)
	m.layout(root)
	for di, d := range datasets {
		m.flows(int16(di), d, opt.FlowsPerNode)
	}
	m.resolveBridges(bridges)
	m.BuildTime = time.Since(start)
	return m
}

func (m *Model) add(n Node) int32 {
	n.ID = int32(len(m.Nodes))
	m.Nodes = append(m.Nodes, n)
	if n.Parent >= 0 {
		p := &m.Nodes[n.Parent]
		p.Children = append(p.Children, n.ID)
		n.Depth = p.Depth + 1
		m.Nodes[n.ID].Depth = n.Depth
	}
	return n.ID
}

// interactionWeight and memberWeight shape what Louvain groups by: who talks
// to whom first, where they post second.
func interactionWeight(w float64) float64 { return w }
func memberWeight(w float64) float64      { return 0.5 * math.Sqrt(w) }

func (m *Model) buildDataset(di int16, d *Dataset, root int32, opt Options) int32 {
	n := len(d.Items)
	edges := make([]wedge, 0, len(d.Edges))
	m.out[di] = make([][]int32, n)
	m.member[di] = make([][]int32, n)
	m.inc[di] = make([][]int32, n)
	for i, e := range d.Edges {
		if e.Src < 0 || e.Dst < 0 || int(e.Src) >= n || int(e.Dst) >= n {
			continue
		}
		w := interactionWeight(e.W)
		if e.Member {
			w = memberWeight(e.W)
			m.member[di][e.Src] = append(m.member[di][e.Src], int32(i))
		} else {
			m.out[di][e.Src] = append(m.out[di][e.Src], int32(i))
			m.inc[di][e.Dst] = append(m.inc[di][e.Dst], int32(i))
		}
		if w > 0 {
			edges = append(edges, wedge{e.Src, e.Dst, w})
		}
	}
	g := m.add(Node{Parent: root, Kind: KindGalaxy, Dataset: di, Item: -1, Label: d.Title, Key: d.ID})
	m.galaxyOf[d.ID] = g
	parent := make([]int32, n)
	if groups, labels := clusterGroups(d, edges); groups != nil {
		// Precomputed clusters are the top communities; Louvain splits each.
		local := make([]int32, n)
		byGroup := make([][]wedge, len(groups))
		group := make([]int32, n)
		for gi, members := range groups {
			for j, it := range members {
				local[it], group[it] = int32(j), int32(gi)
			}
		}
		for _, e := range edges {
			if ga := group[e.a]; ga == group[e.b] {
				byGroup[ga] = append(byGroup[ga], wedge{local[e.a], local[e.b], e.w})
			}
		}
		for gi, members := range groups {
			c := m.add(Node{Parent: g, Kind: KindCommunity, Dataset: di, Item: -1, Label: labels[gi]})
			m.communities(di, c, members, byGroup[gi], opt.MaxLevels-1, parent)
		}
	} else {
		all := make([]int32, n)
		for i := range all {
			all[i] = int32(i)
		}
		m.communities(di, g, all, edges, opt.MaxLevels, parent)
	}
	parentOf := func(item int) int32 { return parent[item] }
	m.itemNode[di] = make([]int32, n)
	for i, it := range d.Items {
		id := m.add(Node{Parent: parentOf(i), Kind: it.Kind, Dataset: di, Item: int32(i), Label: it.Label, Key: it.Key,
			Posts: it.Posts, First: it.First, Last: it.Last})
		m.itemNode[di][i] = id
		m.byKey[d.ID+"/"+it.Key] = id
		if l := d.ID + "/" + strings.ToLower(it.Label); it.Label != "" {
			if _, dup := m.byLabel[l]; !dup {
				m.byLabel[l] = id
			}
		}
	}
	m.collapse(g)
	return g
}

// communities splits members (item indices) under a parent by multi-level
// Louvain over edges (between members, in local indices), adding community
// nodes coarsest first, and records each member's parent.
func (m *Model) communities(di int16, under int32, members []int32, edges []wedge, maxLevels int, parent []int32) {
	var levels [][]int32
	if maxLevels > 0 && len(members) > 1 {
		levels = louvain(len(members), edges, maxLevels)
	}
	if len(levels) == 0 {
		for _, it := range members {
			parent[it] = under
		}
		return
	}
	comm := make([][]int32, len(levels)) // level -> community -> node
	for l := len(levels) - 1; l >= 0; l-- {
		k := int32(0)
		for _, c := range levels[l] {
			if c+1 > k {
				k = c + 1
			}
		}
		comm[l] = make([]int32, k)
		for c := range comm[l] {
			comm[l][c] = -1
		}
		for j, c := range levels[l] {
			if comm[l][c] >= 0 {
				continue
			}
			p := under
			if l+1 < len(levels) {
				p = comm[l+1][levels[l+1][j]]
			}
			comm[l][c] = m.add(Node{Parent: p, Kind: KindCommunity, Dataset: di, Item: -1})
		}
	}
	for j, it := range members {
		parent[it] = comm[0][levels[0][j]]
	}
}

// clusterGroups turns a dataset's precomputed clusters into groups of items,
// biggest first, with their labels. An unclustered item joins the cluster it
// is most strongly tied to (two rounds, so ties propagate); what stays
// unclustered forms one more group. Two clusters tied to each other at least
// half as strongly as the stronger one holds together are one community.
// Nil when the dataset has no clusters.
func clusterGroups(d *Dataset, edges []wedge) ([][]int32, []string) {
	if !d.Clustered {
		return nil, nil
	}
	n := len(d.Items)
	dense := map[int32]int32{}
	var ids []int32
	of := make([]int32, n)
	for i, it := range d.Items {
		of[i] = -1
		if it.Cluster < 0 {
			continue
		}
		c, ok := dense[it.Cluster]
		if !ok {
			c = int32(len(ids))
			dense[it.Cluster] = c
			ids = append(ids, it.Cluster)
		}
		of[i] = c
	}
	if len(ids) == 0 {
		return nil, nil
	}
	orig := append([]int32(nil), of...) // merges are judged on the source's own assignment
	for round := 0; round < 2; round++ {
		score := map[[2]int32]float64{}
		for _, e := range edges {
			if of[e.a] < 0 && of[e.b] >= 0 {
				score[[2]int32{e.a, of[e.b]}] += e.w
			}
			if of[e.b] < 0 && of[e.a] >= 0 {
				score[[2]int32{e.b, of[e.a]}] += e.w
			}
		}
		best := map[int32][2]float64{}
		for k, w := range score {
			b, ok := best[k[0]]
			if !ok || w > b[1] || (w == b[1] && float64(k[1]) < b[0]) {
				best[k[0]] = [2]float64{float64(k[1]), w}
			}
		}
		for it, b := range best {
			of[it] = int32(b[0])
		}
	}
	k := int32(len(ids))
	intra := make([]float64, k)
	inter := map[[2]int32]float64{}
	for _, e := range edges {
		a, b := orig[e.a], orig[e.b]
		if a < 0 || b < 0 {
			continue
		}
		if a == b {
			intra[a] += e.w
		} else {
			if a > b {
				a, b = b, a
			}
			inter[[2]int32{a, b}] += e.w
		}
	}
	root := make([]int32, k)
	for i := range root {
		root[i] = int32(i)
	}
	var find func(int32) int32
	find = func(x int32) int32 {
		for root[x] != x {
			root[x] = root[root[x]]
			x = root[x]
		}
		return x
	}
	pairs := make([][2]int32, 0, len(inter))
	for p := range inter {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i][0] < pairs[j][0] || (pairs[i][0] == pairs[j][0] && pairs[i][1] < pairs[j][1])
	})
	for _, p := range pairs {
		if hi := math.Max(intra[p[0]], intra[p[1]]); hi > 0 && inter[p] >= 0.5*hi {
			a, b := find(p[0]), find(p[1])
			if a != b {
				root[b] = a
			}
		}
	}
	slot := map[int32]int{}
	var groups [][]int32
	var labels []string
	var rest []int32
	for i := 0; i < n; i++ {
		if of[i] < 0 {
			rest = append(rest, int32(i))
			continue
		}
		r := find(of[i])
		g, ok := slot[r]
		if !ok {
			g = len(groups)
			slot[r] = g
			groups = append(groups, nil)
			labels = append(labels, "")
		}
		groups[g] = append(groups[g], int32(i))
	}
	// A group is named after its clusters' families, each with its cluster
	// ids: "relay-coordination · C4+C5".
	type fam struct {
		name string
		ids  []string
	}
	names := make([][]fam, len(groups))
	for c := int32(0); c < k; c++ {
		g, ok := slot[find(c)]
		if !ok {
			continue
		}
		l := d.ClusterLabels[ids[c]]
		if l == "" {
			l = "cluster"
		}
		id := d.ClusterTags[ids[c]]
		if id == "" {
			id = "C" + strconv.Itoa(int(ids[c]))
		}
		found := false
		for i := range names[g] {
			if names[g][i].name == l {
				names[g][i].ids = append(names[g][i].ids, id)
				found = true
			}
		}
		if !found {
			names[g] = append(names[g], fam{l, []string{id}})
		}
	}
	for g, fs := range names {
		parts := make([]string, 0, len(fs))
		for _, f := range fs {
			sort.Strings(f.ids)
			parts = append(parts, f.name+" · "+strings.Join(f.ids, "+"))
		}
		labels[g] = strings.Join(parts, ", ")
	}
	if len(rest) > 0 {
		groups, labels = append(groups, rest), append(labels, "Unclustered")
	}
	order := make([]int, len(groups))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return len(groups[order[i]]) > len(groups[order[j]]) })
	og, ol := make([][]int32, len(groups)), make([]string, len(groups))
	for i, o := range order {
		og[i], ol[i] = groups[o], labels[o]
	}
	return og, ol
}

// collapse removes communities with a single child, so every level splits.
func (m *Model) collapse(id int32) {
	n := &m.Nodes[id]
	kids := n.Children
	out := kids[:0]
	for _, c := range kids {
		for m.Nodes[c].Kind == KindCommunity && len(m.Nodes[c].Children) == 1 {
			only := m.Nodes[c].Children[0]
			if m.Nodes[only].Kind == KindCommunity && m.Nodes[only].Label == "" {
				m.Nodes[only].Label = m.Nodes[c].Label // a named cluster keeps its name
			}
			m.Nodes[only].Parent = id
			m.Nodes[c].Children = nil
			m.Nodes[c].Parent = -2 // orphaned
			c = only
		}
		out = append(out, c)
	}
	m.Nodes[id].Children = out
	for _, c := range out {
		if m.Nodes[c].Kind == KindCommunity {
			m.collapse(c)
		}
	}
	// A galaxy with one community holds that community's children itself.
	if n := &m.Nodes[id]; n.Kind == KindGalaxy && len(n.Children) == 1 && m.Nodes[n.Children[0]].Kind == KindCommunity {
		c := n.Children[0]
		n.Children = m.Nodes[c].Children
		for _, k := range n.Children {
			m.Nodes[k].Parent = id
		}
		m.Nodes[c].Children, m.Nodes[c].Parent = nil, -2
	}
}

// aggregate fills depth, counts and spans bottom-up and labels communities
// after their busiest member.
func (m *Model) aggregate(id int32) {
	n := &m.Nodes[id]
	if n.Parent >= 0 {
		n.Depth = m.Nodes[n.Parent].Depth + 1
	}
	if n.Item >= 0 {
		if n.Kind == KindIdentity || n.Kind == KindPool {
			n.Members = 1
		}
		d := m.data[n.Dataset]
		it := d.Items[n.Item]
		last := m.lastWeek[n.Dataset]
		for w, c := range it.Weeks {
			if int64(w) >= last-1 {
				n.Recent += int64(c)
			}
		}
		return
	}
	var top struct {
		label string
		posts int64
	}
	for _, c := range n.Children {
		m.aggregate(c)
		k := &m.Nodes[c]
		n = &m.Nodes[id]
		n.Posts += k.Posts
		n.Members += k.Members
		n.Recent += k.Recent
		if k.First > 0 && (n.First == 0 || k.First < n.First) {
			n.First = k.First
		}
		if k.Last > n.Last {
			n.Last = k.Last
		}
		if (k.Kind == KindIdentity || k.Kind == KindCommunity) && k.Posts > top.posts {
			top.label, top.posts = k.Label, k.Posts
		}
	}
	if n.Kind == KindCommunity && n.Label == "" {
		n.Label = top.label
	}
}

func (m *Model) datasetLastWeek(di int16) int64 {
	return m.Datasets[di].T1 / Week
}

// itemRadius is an item's disc, on a log scale of its engagement (distinct
// counterparts plus interactions received) with a little for its posts: an
// agent that talks with many others is large; one that only posts, however
// much, stays small.
func itemRadius(posts int64, engagement float64) float64 {
	return 1.5 + 2.4*math.Log2(1+engagement) + 0.3*math.Log2(1+float64(posts))
}

// galaxyRadius sizes a population by its conversation, damped: the summed
// log engagement of its members, its two-way pairs, and a little for its
// head count, so a board full of one-way posting does not dominate and a
// quiet one stays visible.
func (m *Model) galaxyRadius(di int16) float64 {
	d := m.data[di]
	var logSum float64
	var members int
	for i, it := range d.Items {
		if it.Kind != KindIdentity && it.Kind != KindPool {
			continue
		}
		members++
		p, r := m.engagement(di, int32(i))
		logSum += math.Log2(1 + float64(p) + r)
	}
	pairs := map[[2]int32]uint8{}
	for _, e := range d.Edges {
		if e.Member || e.Src == e.Dst {
			continue
		}
		if e.Src < e.Dst {
			pairs[[2]int32{e.Src, e.Dst}] |= 1
		} else {
			pairs[[2]int32{e.Dst, e.Src}] |= 2
		}
	}
	twoWay := 0
	for _, v := range pairs {
		if v == 3 {
			twoWay++
		}
	}
	return 40 + 10*math.Sqrt(logSum) + 24*math.Sqrt(float64(twoWay)) + 6*math.Sqrt(float64(members))
}

// layout packs every node's children inside it, bottom-up, then places the
// tree in absolute coordinates scaled into [0, Space].
func (m *Model) layout(root int32) {
	var pack func(id int32) float64
	pack = func(id int32) float64 {
		n := &m.Nodes[id]
		if len(n.Children) == 0 {
			if n.Kind == KindGalaxy {
				n.R = 40 // a population known only through its bridges
			} else if n.Item >= 0 {
				p, r := m.engagement(n.Dataset, n.Item)
				n.R = itemRadius(n.Posts, float64(p)+r)
				if n.Kind == KindRoom {
					n.R = 1 + 0.35*math.Sqrt(float64(n.Posts)) // context, not a participant
				}
			} else {
				n.R = 1
			}
			return n.R
		}
		kids := append([]int32(nil), n.Children...)
		for _, c := range kids {
			pack(c)
		}
		sort.SliceStable(kids, func(i, j int) bool { return m.Nodes[kids[i]].R > m.Nodes[kids[j]].R })
		cs := make([]*circle, len(kids))
		gap := 0.04
		if m.Nodes[id].Kind == KindUniverse {
			gap = 0.35 // galaxies stand apart, so bridges read as bridges
		} else if m.Nodes[id].Kind == KindGalaxy {
			gap = 0.12
		}
		for i, c := range kids {
			cs[i] = &circle{r: m.Nodes[c].R * (1 + gap)}
		}
		var r float64
		if m.Nodes[id].Kind == KindUniverse && len(kids) > 1 {
			r = m.ringPack(kids, cs)
		} else {
			r = packSiblings(cs)
		}
		for i, c := range kids {
			m.Nodes[c].X, m.Nodes[c].Y = cs[i].x, cs[i].y
		}
		n = &m.Nodes[id]
		n.R = r*1.02 + 0.5
		if n.Kind == KindGalaxy {
			m.scale(id, m.galaxyRadius(n.Dataset)/n.R)
		}
		return n.R
	}
	pack(root)
	scale := (Space/2 - 64) / math.Max(m.Nodes[root].R, 1e-9)
	var place func(id int32, x, y float64)
	place = func(id int32, x, y float64) {
		n := &m.Nodes[id]
		n.X, n.Y = x, y
		n.R *= scale
		for _, c := range n.Children {
			k := &m.Nodes[c]
			place(c, x+k.X*scale, y+k.Y*scale)
		}
	}
	m.Nodes[root].X, m.Nodes[root].Y = 0, 0
	place(root, Space/2, Space/2)
}

// ringPack lays the universe out around its centre galaxy (the live one,
// else the first): the centre sits at the origin and the others pack around
// it, those with the most bridges to it first, so the bridges out of the
// centre are the first thing in view. kids and cs are parallel.
func (m *Model) ringPack(kids []int32, cs []*circle) float64 {
	centre := 0
	for i, k := range kids {
		if d := m.Nodes[k].Dataset; d >= 0 && int(d) < len(m.data) && m.data[d].Live {
			centre = i
			break
		}
	}
	if centre == 0 {
		lowest := int16(math.MaxInt16)
		for i, k := range kids {
			if d := m.Nodes[k].Dataset; d < lowest {
				lowest, centre = d, i
			}
		}
	}
	centreID := ""
	if d := m.Nodes[kids[centre]].Dataset; d >= 0 && int(d) < len(m.data) {
		centreID = m.data[d].ID
	}
	type other struct {
		c        *circle
		affinity float64
		id       string
	}
	var rest []other
	for i, c := range cs {
		if i == centre {
			continue
		}
		d := m.Nodes[kids[i]].Dataset
		var a float64
		id := ""
		if d >= 0 && int(d) < len(m.data) {
			id = m.data[d].ID
			a = m.affinity[centreID+"\x00"+id]
		}
		rest = append(rest, other{c, a, id})
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].affinity != rest[j].affinity {
			return rest[i].affinity > rest[j].affinity
		}
		return rest[i].c.r > rest[j].c.r
	})
	// A galaxy with no bridge to the centre packs right after the galaxy it
	// is most bridged to, so it lands beside it.
	var loose []string
	for _, o := range rest {
		if o.affinity == 0 {
			loose = append(loose, o.id)
		}
	}
	for _, id := range loose {
		i := -1
		for k, o := range rest {
			if o.id == id {
				i = k
			}
		}
		best, bw := "", 0.0
		for _, o := range rest {
			if w := m.affinity[id+"\x00"+o.id]; o.id != id && w > bw {
				best, bw = o.id, w
			}
		}
		if i < 0 || best == "" {
			continue
		}
		moved := rest[i]
		rest = append(rest[:i], rest[i+1:]...)
		for k, o := range rest {
			if o.id == best {
				rest = append(rest[:k+1], append([]other{moved}, rest[k+1:]...)...)
				break
			}
		}
	}
	// Front-chain packing grows outward from its first circles, so with the
	// centre first and the most bridged next, it stays in the middle; the
	// result is then moved so the centre is the origin.
	order := []*circle{cs[centre]}
	for _, o := range rest {
		order = append(order, o.c)
	}
	packSiblings(order)
	c0 := cs[centre]
	ox, oy := c0.x, c0.y
	enclose := 0.0
	for _, c := range order {
		c.x, c.y = c.x-ox, c.y-oy
		enclose = math.Max(enclose, math.Hypot(c.x, c.y)+c.r)
	}
	return enclose
}

// scale multiplies a packed subtree's radii and relative positions.
func (m *Model) scale(id int32, f float64) {
	n := &m.Nodes[id]
	n.R *= f
	for _, c := range n.Children {
		k := &m.Nodes[c]
		k.X *= f
		k.Y *= f
		m.scale(c, f)
	}
}

// flows aggregates every edge under the lowest common ancestor of its ends,
// as a flow between that ancestor's two children on the way down.
func (m *Model) flows(di int16, d *Dataset, keep int) {
	type key struct{ parent, a, b int32 }
	acc := map[key]*Flow{}
	internal := map[int32]float64{}
	for _, e := range d.Edges {
		if e.Src < 0 || e.Dst < 0 || int(e.Src) >= len(d.Items) || int(e.Dst) >= len(d.Items) || e.Src == e.Dst {
			continue
		}
		a, b := m.itemNode[di][e.Src], m.itemNode[di][e.Dst]
		// Climb the deeper end first, then both, until they share a parent.
		for m.Nodes[a].Depth > m.Nodes[b].Depth {
			a = m.Nodes[a].Parent
		}
		for m.Nodes[b].Depth > m.Nodes[a].Depth {
			b = m.Nodes[b].Parent
		}
		for m.Nodes[a].Parent != m.Nodes[b].Parent {
			a, b = m.Nodes[a].Parent, m.Nodes[b].Parent
		}
		if a == b {
			continue
		}
		p := m.Nodes[a].Parent
		internal[p] += e.W
		k, forward := key{p, a, b}, true
		if a > b {
			k, forward = key{p, b, a}, false
		}
		k2 := k
		f := acc[k2]
		if f == nil {
			f = &Flow{A: k.a, B: k.b, First: e.First, Last: e.Last, Member: e.Member}
			acc[k2] = f
		}
		if forward {
			f.AB += e.W
		} else {
			f.BA += e.W
		}
		if !e.Member {
			f.Member = false
		}
		if e.First > 0 && (f.First == 0 || e.First < f.First) {
			f.First = e.First
		}
		if e.Last > f.Last {
			f.Last = e.Last
		}
	}
	for k, f := range acc {
		m.Flows[k.parent] = append(m.Flows[k.parent], *f)
	}
	for p, fs := range m.Flows {
		if m.Nodes[p].Dataset != di {
			continue
		}
		sort.Slice(fs, func(i, j int) bool {
			if fs[i].Member != fs[j].Member {
				return !fs[i].Member
			}
			wi, wj := fs[i].AB+fs[i].BA, fs[j].AB+fs[j].BA
			if wi != wj {
				return wi > wj
			}
			if fs[i].A != fs[j].A {
				return fs[i].A < fs[j].A
			}
			return fs[i].B < fs[j].B
		})
		if len(fs) > keep {
			fs = fs[:keep]
		}
		m.Flows[p] = fs
	}
	for p, w := range internal {
		m.Nodes[p].Internal += w
	}
}

// resolve finds a bridge end: an item by key, then by label (bridges often
// name a handle), else the dataset's galaxy itself when the end is the
// dataset or an account outside the collected sample.
func (m *Model) resolve(e BridgeEnd) (int32, bool) {
	if id, ok := m.byKey[e.Dataset+"/"+e.Key]; ok && e.Key != "" {
		return id, true
	}
	if id, ok := m.byLabel[e.Dataset+"/"+strings.ToLower(e.Key)]; ok && e.Key != "" {
		return id, true
	}
	g, ok := m.galaxyOf[e.Dataset]
	return g, ok
}

func (m *Model) resolveBridges(bridges []Bridge) {
	for _, b := range bridges {
		a, okA := m.resolve(b.A)
		c, okB := m.resolve(b.B)
		if !okA || !okB || a == c {
			continue
		}
		id := int32(len(m.Bridges))
		m.Bridges = append(m.Bridges, ModelBridge{ID: id, A: a, B: c, Kind: b.Kind, Sub: b.Sub, Conf: b.Conf, Dashed: b.Dashed, Evidence: b.Evidence})
		m.bridgesOf[a] = append(m.bridgesOf[a], id)
		m.bridgesOf[c] = append(m.bridgesOf[c], id)
	}
}
