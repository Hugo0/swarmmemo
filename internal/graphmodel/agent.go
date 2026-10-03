package graphmodel

import (
	_ "embed"
	"encoding/json"
	"math"
	"sort"
	"sync"
)

// An agent sheet: everything /graph shows about one item. Where it went (the
// rooms, pages or board communities it posted in, with counts and dates), who
// it talked to, which identities elsewhere it is linked to and on what
// evidence, and, for the shipped boards, short excerpts of its most recent
// public posts with links to the originals (datasets/agents.json, written by
// scripts/graph_datasets.py). SwarmMemo's own text is read live elsewhere;
// AI Village and wiki text is never shipped.

//go:embed datasets/agents.json
var agentsJSON []byte

// AgentPlace is a room, page or board community an item posted in. ID is
// its node when the place is on the map (-1 otherwise).
type AgentPlace struct {
	ID    int32  `json:"id"`
	Label string `json:"label"`
	N     int64  `json:"n"`
	First int64  `json:"first"`
	Last  int64  `json:"last"`
}

// AgentPeer is an item this one interacted with.
type AgentPeer struct {
	ID       int32   `json:"id"`
	Label    string  `json:"label"`
	Dataset  string  `json:"dataset"`
	Sent     float64 `json:"sent"`
	Received float64 `json:"received"`
	First    int64   `json:"first"`
	Last     int64   `json:"last"`
}

// AgentLink is a bridge from this item to another, with its evidence in brief.
type AgentLink struct {
	Bridge  int32    `json:"bridge"`
	ID      int32    `json:"id"`
	Label   string   `json:"label"`
	Dataset string   `json:"dataset"`
	Kind    string   `json:"kind"`
	Sub     []string `json:"sub,omitempty"`
	Conf    float64  `json:"conf"`
	Dashed  bool     `json:"dashed"`
	Why     []string `json:"why,omitempty"`
}

// AgentPost is one public post on a shipped board: an excerpt and a link.
type AgentPost struct {
	At    int64  `json:"at"`
	Text  string `json:"text"`
	URL   string `json:"url"`
	Place string `json:"place,omitempty"`
	Reply bool   `json:"reply"`
}

// AgentGoal is an AI Village goal an agent worked on, as a message count.
type AgentGoal struct {
	Label string `json:"label"`
	N     int64  `json:"n"`
	First int64  `json:"first"`
	Last  int64  `json:"last"`
}

// AgentSheet is one item's sheet.
type AgentSheet struct {
	ID          int32        `json:"id"`
	Kind        uint8        `json:"kind"`
	Label       string       `json:"label"`
	Key         string       `json:"key"`
	Dataset     string       `json:"dataset"`
	Title       string       `json:"dataset_title"`
	Posts       int64        `json:"posts"`
	First       int64        `json:"first"`
	Last        int64        `json:"last"`
	Counterpart int          `json:"counterparts"`
	Received    float64      `json:"received"`
	Engagement  float64      `json:"engagement"`
	Path        []int32      `json:"path"`
	Places      []AgentPlace `json:"places"`
	Peers       []AgentPeer  `json:"peers"`
	Links       []AgentLink  `json:"links"`
	Excerpts    []AgentPost  `json:"excerpts"`
	Goals       []AgentGoal  `json:"goals"`
}

type agentDetail struct {
	Places [][4]json.RawMessage `json:"places"` // [name, n, first, last]
	Posts  [][5]json.RawMessage `json:"posts"`  // [at, excerpt, url, place, reply]
	Goals  [][4]json.RawMessage `json:"goals"`  // [label, n, first, last]
}

var (
	agentDetailsOnce sync.Once
	agentDetails     map[string]map[string]agentDetail
)

func details(dataset, key string) (agentDetail, bool) {
	agentDetailsOnce.Do(func() {
		var f struct {
			Datasets map[string]map[string]agentDetail `json:"datasets"`
		}
		if json.Unmarshal(agentsJSON, &f) == nil {
			agentDetails = f.Datasets
		}
	})
	d, ok := agentDetails[dataset][key]
	return d, ok
}

func rawString(r json.RawMessage) string {
	var s string
	_ = json.Unmarshal(r, &s)
	return s
}

func rawInt(r json.RawMessage) int64 {
	var f float64
	_ = json.Unmarshal(r, &f)
	return int64(f)
}

// engagement is what an item's size shows: how many others it interacted
// with, plus the interactions it received. Posting alone, however much,
// adds nothing.
func (m *Model) engagement(di int16, item int32) (peers int, received float64) {
	d := m.data[di]
	seen := map[int32]bool{}
	for _, ei := range m.out[di][item] {
		if dst := d.Edges[ei].Dst; dst != item {
			seen[dst] = true
		}
	}
	for _, ei := range m.inc[di][item] {
		e := d.Edges[ei]
		if e.Src != item {
			seen[e.Src] = true
			received += e.W
		}
	}
	return len(seen), received
}

// Engagement is an item's size measure: distinct counterparts plus
// interactions received.
func (m *Model) Engagement(id int32) float64 {
	if !m.Valid(id) || m.Nodes[id].Item < 0 {
		return 0
	}
	n := &m.Nodes[id]
	p, r := m.engagement(n.Dataset, n.Item)
	return float64(p) + r
}

// Agent returns the sheet of an item node.
func (m *Model) Agent(id int32) (AgentSheet, bool) {
	if !m.Valid(id) || m.Nodes[id].Item < 0 {
		return AgentSheet{}, false
	}
	n := &m.Nodes[id]
	di, d := n.Dataset, m.data[n.Dataset]
	s := AgentSheet{ID: id, Kind: n.Kind, Label: n.Label, Key: n.Key, Dataset: d.ID, Title: d.Title, Posts: n.Posts, First: n.First, Last: n.Last,
		Path: m.Path(id), Places: []AgentPlace{}, Peers: []AgentPeer{}, Links: []AgentLink{}, Excerpts: []AgentPost{}, Goals: []AgentGoal{}}
	s.Counterpart, s.Received = m.engagement(di, n.Item)
	s.Engagement = float64(s.Counterpart) + s.Received

	span := func(first, last *int64, f, l int64) {
		if f > 0 && (*first == 0 || f < *first) {
			*first = f
		}
		if l > *last {
			*last = l
		}
	}
	// Places on the map: rooms, pages and boards it is a member of.
	places := map[int32]*AgentPlace{}
	for _, ei := range m.member[di][n.Item] {
		e := d.Edges[ei]
		rid := m.itemNode[di][e.Dst]
		p := places[rid]
		if p == nil {
			p = &AgentPlace{ID: rid, Label: m.Nodes[rid].Label}
			places[rid] = p
		}
		p.N += int64(math.Round(e.W))
		span(&p.First, &p.Last, e.First, e.Last)
	}
	for _, p := range places {
		s.Places = append(s.Places, *p)
	}
	// Counterparts, both directions.
	peers := map[int32]*AgentPeer{}
	peer := func(other int32) *AgentPeer {
		oid := m.itemNode[di][other]
		p := peers[oid]
		if p == nil {
			p = &AgentPeer{ID: oid, Label: m.Nodes[oid].Label, Dataset: d.Title}
			peers[oid] = p
		}
		return p
	}
	for _, ei := range m.out[di][n.Item] {
		e := d.Edges[ei]
		if e.Dst == n.Item {
			continue
		}
		p := peer(e.Dst)
		p.Sent += e.W
		span(&p.First, &p.Last, e.First, e.Last)
	}
	for _, ei := range m.inc[di][n.Item] {
		e := d.Edges[ei]
		if e.Src == n.Item {
			continue
		}
		p := peer(e.Src)
		p.Received += e.W
		span(&p.First, &p.Last, e.First, e.Last)
	}
	for _, p := range peers {
		s.Peers = append(s.Peers, *p)
	}
	sort.Slice(s.Peers, func(i, j int) bool {
		a, b := s.Peers[i].Sent+s.Peers[i].Received, s.Peers[j].Sent+s.Peers[j].Received
		if a != b {
			return a > b
		}
		return s.Peers[i].ID < s.Peers[j].ID
	})
	if len(s.Peers) > 12 {
		s.Peers = s.Peers[:12]
	}
	// Bridges: the same agent elsewhere, or one it met.
	for _, bi := range m.bridgesOf[id] {
		b := m.Bridges[bi]
		other := b.A
		if other == id {
			other = b.B
		}
		l := AgentLink{Bridge: b.ID, ID: other, Label: m.Nodes[other].Label, Dataset: m.Datasets[m.Nodes[other].Dataset].Title, Kind: b.Kind, Sub: b.Sub, Conf: b.Conf, Dashed: b.Dashed}
		for _, ev := range b.Evidence {
			for _, k := range []string{"note", "evidence_type", "relation"} {
				if v, ok := ev[k].(string); ok && v != "" && len(l.Why) < 3 && !contains(l.Why, v) {
					l.Why = append(l.Why, v)
				}
			}
		}
		s.Links = append(s.Links, l)
	}
	sort.SliceStable(s.Links, func(i, j int) bool { return s.Links[i].Conf > s.Links[j].Conf })
	if len(s.Links) > 20 {
		s.Links = s.Links[:20]
	}
	// Shipped details: board communities and excerpts, AI Village goals.
	if det, ok := details(d.ID, n.Key); ok && n.Kind == KindIdentity {
		for _, p := range det.Places {
			s.Places = append(s.Places, AgentPlace{ID: -1, Label: rawString(p[0]), N: rawInt(p[1]), First: rawInt(p[2]), Last: rawInt(p[3])})
		}
		for _, p := range det.Posts {
			s.Excerpts = append(s.Excerpts, AgentPost{At: rawInt(p[0]), Text: rawString(p[1]), URL: rawString(p[2]), Place: rawString(p[3]), Reply: rawInt(p[4]) == 1})
		}
		for _, g := range det.Goals {
			s.Goals = append(s.Goals, AgentGoal{Label: rawString(g[0]), N: rawInt(g[1]), First: rawInt(g[2]), Last: rawInt(g[3])})
		}
	}
	sort.SliceStable(s.Places, func(i, j int) bool {
		if s.Places[i].N != s.Places[j].N {
			return s.Places[i].N > s.Places[j].N
		}
		return s.Places[i].Label < s.Places[j].Label
	})
	if len(s.Places) > 30 {
		s.Places = s.Places[:30]
	}
	return s, true
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
