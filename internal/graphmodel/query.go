package graphmodel

import (
	"math"
	"sort"
	"strings"
)

// Nodes is a compact set of nodes as parallel arrays, as /api/graph/levels
// serves them.
type Nodes struct {
	ID       []int32   `json:"id"`
	Parent   []int32   `json:"parent"`
	Kind     []int     `json:"kind"`
	Dataset  []int16   `json:"dataset"`
	Label    []string  `json:"label"`
	Key      []string  `json:"key"`
	X        []float32 `json:"x"`
	Y        []float32 `json:"y"`
	R        []float32 `json:"r"`
	Posts    []int64   `json:"posts"`
	Members  []int64   `json:"members"`
	First    []int64   `json:"first"`
	Last     []int64   `json:"last"`
	Recent   []int64   `json:"recent"`
	Children []int32   `json:"children"`
}

// Flows is a compact list of flows between sibling nodes.
type Flows struct {
	A      []int32   `json:"a"`
	B      []int32   `json:"b"`
	AB     []float32 `json:"ab"`
	BA     []float32 `json:"ba"`
	First  []int64   `json:"first"`
	Last   []int64   `json:"last"`
	Member []bool    `json:"member"`
}

// BridgeView is a bridge as the client draws it: both ends with their
// ancestor paths, so it can attach to whatever ancestor is on screen.
type BridgeView struct {
	ID     int32    `json:"id"`
	A      []int32  `json:"a"` // path from the universe to the item
	B      []int32  `json:"b"`
	Kind   string   `json:"kind"`
	Sub    []string `json:"sub,omitempty"`
	Conf   float64  `json:"conf"`
	Dashed bool     `json:"dashed"`
}

func (n *Nodes) add(m *Model, id int32) {
	x := &m.Nodes[id]
	key := ""
	if x.Item >= 0 || x.Kind == KindGalaxy {
		key = x.Key
	}
	n.ID = append(n.ID, x.ID)
	n.Parent = append(n.Parent, x.Parent)
	n.Kind = append(n.Kind, int(x.Kind))
	n.Dataset = append(n.Dataset, x.Dataset)
	n.Label = append(n.Label, x.Label)
	n.Key = append(n.Key, key)
	n.X = append(n.X, float32(x.X))
	n.Y = append(n.Y, float32(x.Y))
	n.R = append(n.R, float32(x.R))
	n.Posts = append(n.Posts, x.Posts)
	n.Members = append(n.Members, x.Members)
	n.First = append(n.First, x.First)
	n.Last = append(n.Last, x.Last)
	n.Recent = append(n.Recent, x.Recent)
	n.Children = append(n.Children, int32(len(x.Children)))
}

func newNodes() Nodes {
	return Nodes{ID: []int32{}, Parent: []int32{}, Kind: []int{}, Dataset: []int16{}, Label: []string{}, Key: []string{}, X: []float32{}, Y: []float32{}, R: []float32{},
		Posts: []int64{}, Members: []int64{}, First: []int64{}, Last: []int64{}, Recent: []int64{}, Children: []int32{}}
}

func newFlows() Flows {
	return Flows{A: []int32{}, B: []int32{}, AB: []float32{}, BA: []float32{}, First: []int64{}, Last: []int64{}, Member: []bool{}}
}

// Valid reports whether id names a node of the hierarchy.
func (m *Model) Valid(id int32) bool {
	return id >= 0 && int(id) < len(m.Nodes) && m.Nodes[id].Parent != -2
}

// Children returns the children of each named node and the flows between
// them, up to budget nodes in all; a parent's busiest children come first.
// It reports whether any parent was cut.
func (m *Model) Children(ids []int32, budget int) (Nodes, Flows, bool) {
	out, flows := newNodes(), newFlows()
	cut := false
	for _, id := range ids {
		if !m.Valid(id) || len(m.Nodes[id].Children) == 0 {
			continue
		}
		kids := m.Nodes[id].Children
		if len(out.ID)+len(kids) > budget {
			kids = append([]int32(nil), kids...)
			sort.Slice(kids, func(i, j int) bool { return m.Nodes[kids[i]].Posts > m.Nodes[kids[j]].Posts })
			left := budget - len(out.ID)
			if left < 0 {
				left = 0
			}
			kids = kids[:left]
			cut = true
		}
		in := make(map[int32]bool, len(kids))
		for _, c := range kids {
			out.add(m, c)
			in[c] = true
		}
		for _, f := range m.Flows[id] {
			if in[f.A] && in[f.B] {
				flows.A = append(flows.A, f.A)
				flows.B = append(flows.B, f.B)
				flows.AB = append(flows.AB, float32(f.AB))
				flows.BA = append(flows.BA, float32(f.BA))
				flows.First = append(flows.First, f.First)
				flows.Last = append(flows.Last, f.Last)
				flows.Member = append(flows.Member, f.Member)
			}
		}
	}
	return out, flows, cut
}

// Path returns the ancestors of id from the universe down to id itself.
func (m *Model) Path(id int32) []int32 {
	var p []int32
	for x := id; x >= 0; x = m.Nodes[x].Parent {
		p = append(p, x)
	}
	for i, j := 0, len(p)-1; i < j; i, j = i+1, j-1 {
		p[i], p[j] = p[j], p[i]
	}
	return p
}

// Root is the universe node.
func (m *Model) Root() int32 { return 0 }

// Universe returns the universe, its galaxies and their first level, and
// every bridge with its paths.
func (m *Model) Universe(budget int) (Nodes, Flows, []BridgeView) {
	nodes := newNodes()
	nodes.add(m, 0)
	galaxies, flows, _ := m.Children([]int32{0}, budget)
	nodes = appendNodes(nodes, galaxies)
	first, f2, _ := m.Children(galaxies.ID, budget-len(nodes.ID))
	nodes = appendNodes(nodes, first)
	flows = appendFlows(flows, f2)
	bridges := make([]BridgeView, 0, len(m.Bridges))
	for _, b := range m.Bridges {
		bridges = append(bridges, BridgeView{ID: b.ID, A: m.Path(b.A), B: m.Path(b.B), Kind: b.Kind, Sub: b.Sub, Conf: b.Conf, Dashed: b.Dashed})
	}
	return nodes, flows, bridges
}

func appendNodes(a, b Nodes) Nodes {
	a.ID = append(a.ID, b.ID...)
	a.Parent = append(a.Parent, b.Parent...)
	a.Kind = append(a.Kind, b.Kind...)
	a.Dataset = append(a.Dataset, b.Dataset...)
	a.Label = append(a.Label, b.Label...)
	a.Key = append(a.Key, b.Key...)
	a.X = append(a.X, b.X...)
	a.Y = append(a.Y, b.Y...)
	a.R = append(a.R, b.R...)
	a.Posts = append(a.Posts, b.Posts...)
	a.Members = append(a.Members, b.Members...)
	a.First = append(a.First, b.First...)
	a.Last = append(a.Last, b.Last...)
	a.Recent = append(a.Recent, b.Recent...)
	a.Children = append(a.Children, b.Children...)
	return a
}

func appendFlows(a, b Flows) Flows {
	a.A = append(a.A, b.A...)
	a.B = append(a.B, b.B...)
	a.AB = append(a.AB, b.AB...)
	a.BA = append(a.BA, b.BA...)
	a.First = append(a.First, b.First...)
	a.Last = append(a.Last, b.Last...)
	a.Member = append(a.Member, b.Member...)
	return a
}

// Bridge returns one bridge with its evidence.
func (m *Model) Bridge(id int32) (ModelBridge, bool) {
	if id < 0 || int(id) >= len(m.Bridges) {
		return ModelBridge{}, false
	}
	return m.Bridges[id], true
}

// SearchResult is one match of Search.
type SearchResult struct {
	ID      int32   `json:"id"`
	Label   string  `json:"label"`
	Key     string  `json:"key,omitempty"`
	Kind    uint8   `json:"kind"`
	Dataset string  `json:"dataset"`
	Posts   int64   `json:"posts"`
	Path    []int32 `json:"path"`
}

// Search finds items by label or key: exact and prefix matches first, then
// substrings, busiest first.
func (m *Model) Search(q string, limit int) []SearchResult {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return []SearchResult{}
	}
	type hit struct {
		id    int32
		score int
	}
	var hits []hit
	for di := range m.itemNode {
		for _, id := range m.itemNode[di] {
			n := &m.Nodes[id]
			label, key := strings.ToLower(n.Label), strings.ToLower(n.Key)
			score := -1
			switch {
			case label == q || key == q:
				score = 3
			case strings.HasPrefix(label, q) || (len(q) >= 6 && strings.HasPrefix(key, q)):
				score = 2
			case strings.Contains(label, q):
				score = 1
			}
			if score >= 0 {
				hits = append(hits, hit{id, score})
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return m.Nodes[hits[i].id].Posts > m.Nodes[hits[j].id].Posts
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]SearchResult, 0, len(hits))
	for _, h := range hits {
		n := &m.Nodes[h.id]
		out = append(out, SearchResult{ID: n.ID, Label: n.Label, Key: n.Key, Kind: n.Kind, Dataset: m.Datasets[n.Dataset].ID, Posts: n.Posts, Path: m.Path(n.ID)})
	}
	return out
}

// Locate returns the path of a dataset item by key, if it is in the model.
func (m *Model) Locate(dataset, key string) ([]int32, bool) {
	id, ok := m.byKey[dataset+"/"+key]
	if !ok {
		return nil, false
	}
	return m.Path(id), true
}

// Pair is a weighted pair of nodes.
type Pair struct {
	A      int32   `json:"a"`
	B      int32   `json:"b"`
	ALabel string  `json:"a_label"`
	BLabel string  `json:"b_label"`
	W      float64 `json:"w"`
	Recip  float64 `json:"reciprocal"` // the smaller direction's share of the pair
}

// Count is a labelled number.
type Count struct {
	ID    int32  `json:"id"`
	Label string `json:"label"`
	N     int64  `json:"n"`
	Extra string `json:"extra,omitempty"`
}

// Stats summarises a node or a selection.
type Stats struct {
	Nodes        int        `json:"nodes"`
	Members      int64      `json:"members"`
	Identities   int64      `json:"identities"`
	Rooms        int64      `json:"rooms"`
	Posts        int64      `json:"posts"`
	First        int64      `json:"first"`
	Last         int64      `json:"last"`
	Weekly       [][2]int64 `json:"weekly"` // [week start unix, posts], oldest first, at most 52
	InternalW    float64    `json:"internal_weight"`
	ExternalW    float64    `json:"external_weight"`
	Reciprocity  float64    `json:"reciprocity"`
	Density      float64    `json:"density"`
	Growth       float64    `json:"growth"` // share of members whose first post is in the latest two weeks
	TopPairs     []Pair     `json:"top_pairs"`
	Bridges      []Count    `json:"bridges"`       // where the selection's external interaction goes
	CrossDataset []Count    `json:"cross_dataset"` // bridges to other galaxies
	TopRooms     []Count    `json:"top_rooms"`
	TopMembers   []Count    `json:"top_members"`
	Datasets     []Count    `json:"datasets"`
}

// Items returns the item nodes under the named nodes, at most limit.
func (m *Model) Items(ids []int32, limit int) []int32 {
	var out []int32
	seen := map[int32]bool{}
	stack := append([]int32(nil), ids...)
	for len(stack) > 0 && len(out) < limit {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !m.Valid(id) || seen[id] {
			continue
		}
		seen[id] = true
		n := &m.Nodes[id]
		if n.Item >= 0 {
			out = append(out, id)
			continue
		}
		stack = append(stack, n.Children...)
	}
	return out
}

// Stats aggregates the named nodes (communities, galaxies or items).
func (m *Model) Stats(ids []int32) Stats {
	var s Stats
	items := m.Items(ids, 2_000_000)
	in := make(map[int32]bool, len(items))
	for _, id := range items {
		in[id] = true
	}
	s.Nodes = len(ids)
	weeks := map[int64]int64{}
	pairs := map[[2]int32]*[2]float64{}
	ext := map[int32]float64{}
	rooms := map[int32]int64{}
	ds := map[int16]int64{}
	cross := map[int32]int64{}
	depth := 255
	for _, id := range ids {
		if m.Valid(id) && int(m.Nodes[id].Depth) < depth {
			depth = int(m.Nodes[id].Depth)
		}
	}
	var latest int64
	for _, id := range items {
		if l := m.lastWeek[m.Nodes[id].Dataset]; l > latest {
			latest = l
		}
	}
	type member struct {
		id    int32
		posts int64
	}
	var members []member
	for _, id := range items {
		n := &m.Nodes[id]
		d := m.data[n.Dataset]
		it := &d.Items[n.Item]
		s.Posts += n.Posts
		ds[n.Dataset] += n.Posts
		if n.Kind == KindIdentity || n.Kind == KindPool {
			s.Members++
			members = append(members, member{id, n.Posts})
			if n.Kind == KindIdentity {
				s.Identities++
			}
			if n.First/Week >= m.lastWeek[n.Dataset]-1 {
				s.Growth++
			}
		} else {
			s.Rooms++
		}
		if n.First > 0 && (s.First == 0 || n.First < s.First) {
			s.First = n.First
		}
		if n.Last > s.Last {
			s.Last = n.Last
		}
		for w, c := range it.Weeks {
			weeks[int64(w)] += int64(c)
		}
		for _, ei := range m.out[n.Dataset][n.Item] {
			e := d.Edges[ei]
			dst := m.itemNode[n.Dataset][e.Dst]
			if in[dst] {
				if dst == id {
					continue
				}
				a, b, fw := id, dst, 0
				if a > b {
					a, b, fw = b, a, 1
				}
				p := pairs[[2]int32{a, b}]
				if p == nil {
					p = &[2]float64{}
					pairs[[2]int32{a, b}] = p
				}
				p[fw] += e.W
				s.InternalW += e.W
			} else {
				s.ExternalW += e.W
				ext[m.ancestorAt(dst, depth)] += e.W
			}
		}
		for _, ei := range m.inc[n.Dataset][n.Item] {
			e := d.Edges[ei]
			if src := m.itemNode[n.Dataset][e.Src]; !in[src] {
				s.ExternalW += e.W
				ext[m.ancestorAt(src, depth)] += e.W
			}
		}
		for _, ei := range m.member[n.Dataset][n.Item] {
			e := d.Edges[ei]
			rooms[m.itemNode[n.Dataset][e.Dst]] += int64(e.W)
		}
		for _, bi := range m.bridgesOf[id] {
			b := m.Bridges[bi]
			other := b.A
			if other == id {
				other = b.B
			}
			if !in[other] {
				cross[m.ancestorAt(other, 1)]++
			}
		}
	}
	if s.Members > 0 {
		s.Growth /= float64(s.Members)
	}
	var recipMin, recipAll float64
	plist := make([]Pair, 0, len(pairs))
	for k, p := range pairs {
		recipMin += 2 * math.Min(p[0], p[1])
		recipAll += p[0] + p[1]
		plist = append(plist, Pair{A: k[0], B: k[1], W: p[0] + p[1], Recip: math.Min(p[0], p[1]) / math.Max(1e-9, p[0]+p[1])})
	}
	if recipAll > 0 {
		s.Reciprocity = recipMin / recipAll
	}
	if s.Members > 1 {
		s.Density = float64(len(pairs)) / (float64(s.Members) * float64(s.Members-1) / 2)
	}
	sort.Slice(plist, func(i, j int) bool {
		if plist[i].W != plist[j].W {
			return plist[i].W > plist[j].W
		}
		return plist[i].A < plist[j].A
	})
	if len(plist) > 8 {
		plist = plist[:8]
	}
	for i := range plist {
		plist[i].ALabel, plist[i].BLabel = m.Nodes[plist[i].A].Label, m.Nodes[plist[i].B].Label
	}
	s.TopPairs = plist
	s.Bridges = m.topCounts(ext, 8, func(f float64) int64 { return int64(math.Round(f)) })
	s.CrossDataset = m.topInt(cross, 8)
	s.TopRooms = m.topInt(rooms, 8)
	sort.Slice(members, func(i, j int) bool { return members[i].posts > members[j].posts })
	for i := 0; i < len(members) && i < 12; i++ {
		s.TopMembers = append(s.TopMembers, Count{ID: members[i].id, Label: m.Nodes[members[i].id].Label, N: members[i].posts})
	}
	for d, n := range ds {
		s.Datasets = append(s.Datasets, Count{ID: m.Datasets[d].Node, Label: m.Datasets[d].Title, N: n})
	}
	sort.Slice(s.Datasets, func(i, j int) bool { return s.Datasets[i].N > s.Datasets[j].N })
	var wk []int64
	for w := range weeks {
		wk = append(wk, w)
	}
	sort.Slice(wk, func(i, j int) bool { return wk[i] < wk[j] })
	if len(wk) > 52 {
		wk = wk[len(wk)-52:]
	}
	s.Weekly = make([][2]int64, 0, len(wk))
	for _, w := range wk {
		s.Weekly = append(s.Weekly, [2]int64{w * Week, weeks[w]})
	}
	if s.TopPairs == nil {
		s.TopPairs = []Pair{}
	}
	if s.TopMembers == nil {
		s.TopMembers = []Count{}
	}
	return s
}

// ancestorAt returns id's ancestor at the given depth (or id if shallower).
func (m *Model) ancestorAt(id int32, depth int) int32 {
	for int(m.Nodes[id].Depth) > depth && m.Nodes[id].Parent >= 0 {
		id = m.Nodes[id].Parent
	}
	return id
}

func (m *Model) topCounts(c map[int32]float64, k int, conv func(float64) int64) []Count {
	out := make([]Count, 0, len(c))
	for id, v := range c {
		out = append(out, Count{ID: id, Label: m.Nodes[id].Label, N: conv(v), Extra: m.Datasets[m.Nodes[id].Dataset].Title})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func (m *Model) topInt(c map[int32]int64, k int) []Count {
	f := make(map[int32]float64, len(c))
	for id, v := range c {
		f[id] = float64(v)
	}
	return m.topCounts(f, k, func(v float64) int64 { return int64(v) })
}

// MemberKeys returns the keys of the busiest identities under the named
// nodes in one dataset, for reading their public text.
func (m *Model) MemberKeys(ids []int32, dataset string, limit int) []string {
	type kp struct {
		key   string
		posts int64
	}
	var all []kp
	for _, id := range m.Items(ids, 2_000_000) {
		n := &m.Nodes[id]
		if m.Datasets[n.Dataset].ID != dataset || (n.Kind != KindIdentity && n.Kind != KindPool) {
			continue
		}
		all = append(all, kp{n.Key, n.Posts})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].posts > all[j].posts })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]string, len(all))
	for i, k := range all {
		out[i] = k.key
	}
	return out
}

// DatasetOf names the dataset a node belongs to ("" for the universe).
func (m *Model) DatasetOf(id int32) string {
	if !m.Valid(id) || m.Nodes[id].Dataset < 0 {
		return ""
	}
	return m.Datasets[m.Nodes[id].Dataset].ID
}

// ReplayView is a galaxy's items for a time-lapse: where each sits, when it
// first and last posted, and its posts per week, as parallel arrays.
// Metadata only. Weeks pairs are [week index (unix seconds / Week), posts].
type ReplayView struct {
	Galaxy int32        `json:"galaxy"`
	Total  int          `json:"total"`
	ID     []int32      `json:"id"`
	Kind   []uint8      `json:"kind"`
	X      []float32    `json:"x"`
	Y      []float32    `json:"y"`
	R      []float32    `json:"r"`
	First  []int64      `json:"first"`
	Last   []int64      `json:"last"`
	Weeks  [][][2]int32 `json:"weeks"`
}

// Replay returns the items of a galaxy, its busiest max when it has more.
func (m *Model) Replay(galaxy int32, max int) (ReplayView, bool) {
	if !m.Valid(galaxy) || m.Nodes[galaxy].Kind != KindGalaxy {
		return ReplayView{}, false
	}
	di := m.Nodes[galaxy].Dataset
	d := m.data[di]
	idx := make([]int, 0, len(d.Items))
	for i := range d.Items {
		if m.Valid(m.itemNode[di][i]) {
			idx = append(idx, i)
		}
	}
	v := ReplayView{Galaxy: galaxy, Total: len(idx)}
	if len(idx) > max {
		sort.SliceStable(idx, func(a, b int) bool { return d.Items[idx[a]].Posts > d.Items[idx[b]].Posts })
		idx = idx[:max]
	}
	for _, i := range idx {
		it, n := d.Items[i], m.Nodes[m.itemNode[di][i]]
		weeks := make([][2]int32, 0, len(it.Weeks))
		for w, c := range it.Weeks {
			weeks = append(weeks, [2]int32{w, c})
		}
		sort.Slice(weeks, func(a, b int) bool { return weeks[a][0] < weeks[b][0] })
		v.ID = append(v.ID, n.ID)
		v.Kind = append(v.Kind, n.Kind)
		v.X, v.Y, v.R = append(v.X, float32(n.X)), append(v.Y, float32(n.Y)), append(v.R, float32(n.R))
		v.First, v.Last = append(v.First, it.First), append(v.Last, it.Last)
		v.Weeks = append(v.Weeks, weeks)
	}
	return v, true
}
