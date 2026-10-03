package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// The public graph behind /graph and /api/graph: who posts where and who
// replies to whom, from public metadata only. Each node is an identity (the
// sha256 fingerprint of its signing key), one anonymous pool per room for
// unsigned posts, or a room; each edge is a reply (author to the parent's
// author, weighted) or a membership (author to room). It never carries text.
//
// Only visible messages in public rooms count: no private room, no
// conversation (DMs and groups, sealed or not), no addressed message, nothing
// hidden and no edit of a hidden original. A reply edge is drawn only when its
// parent is in the same public set, so a private parent is never inferred.
const (
	// GraphTTL is how long one computed graph is served before it is
	// recomputed: requests within it share one result.
	GraphTTL = 30 * time.Second
	// graphCacheEntries bounds the distinct (room, since) results kept.
	graphCacheEntries = 64
	graphTimeout      = 20 * time.Second
)

// graphFrom selects the public set. It matches activityFrom and adds the
// conversation and addressed-message exclusions.
const graphFrom = " FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0 AND e.recipient='' AND substr(e.room,1,1)<>'~' AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.room=e.room) AND NOT EXISTS (SELECT 1 FROM events h WHERE h.id=e.origin AND h.hidden=1)"

// Graph node kinds.
const (
	GraphIdentity = 0
	GraphAnonPool = 1
	GraphRoom     = 2
)

// GraphSnapshot is one computed graph, encoded once and shared by every
// request until it expires. Callers must not modify it.
type GraphSnapshot struct {
	Generated time.Time
	JSON      []byte
	ETag      string
	Messages  int
}

type graphState struct {
	gate  chan struct{}
	once  sync.Once
	cache map[graphKey]*GraphSnapshot
}

type graphKey struct {
	room  string
	since int64
}

type graphEdges struct {
	Src   []int   `json:"src"`
	Dst   []int   `json:"dst"`
	W     []int   `json:"w"`
	First []int64 `json:"first"`
	Last  []int64 `json:"last"`
}

type graphNodes struct {
	Key       []any    `json:"key"`
	Kind      []int    `json:"kind"`
	Label     []string `json:"label"`
	Posts     []int    `json:"posts"`
	First     []int64  `json:"first"`
	Last      []int64  `json:"last"`
	Rooms     [][]int  `json:"rooms"`
	Community []int    `json:"community"`
}

type graphDocument struct {
	OK   bool `json:"ok"`
	Meta struct {
		Generated int64    `json:"generated"`
		Messages  int      `json:"messages"`
		T0        int64    `json:"t0"`
		T1        int64    `json:"t1"`
		Room      string   `json:"room,omitempty"`
		Since     int64    `json:"since,omitempty"`
		TTL       int      `json:"ttl_seconds"`
		Identity  string   `json:"identity"`
		Notes     []string `json:"notes"`
	} `json:"meta"`
	Rooms []string   `json:"rooms"`
	Nodes graphNodes `json:"nodes"`
	Edges struct {
		Reply  graphEdges `json:"reply"`
		Member graphEdges `json:"member"`
	} `json:"edges"`
}

// ReadGraph returns the public graph, optionally of one room and of messages
// created at or after since (unix seconds, 0 for all time). Results are
// shared for GraphTTL; one computation runs at a time.
func (s *Store) ReadGraph(ctx context.Context, room string, since int64) (*GraphSnapshot, error) {
	g := &s.graph
	g.once.Do(func() {
		g.gate = make(chan struct{}, 1)
		g.cache = map[graphKey]*GraphSnapshot{}
	})
	select {
	case g.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-g.gate }()
	now := s.now()
	key := graphKey{room, since}
	if c := g.cache[key]; c != nil && now.Sub(c.Generated) < GraphTTL && !now.Before(c.Generated) {
		return c, nil
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), graphTimeout)
	defer cancel()
	snap, err := s.computeGraph(work, room, since, now)
	if err != nil {
		return nil, err
	}
	if len(g.cache) >= graphCacheEntries {
		for k, c := range g.cache {
			if now.Sub(c.Generated) >= GraphTTL || now.Before(c.Generated) {
				delete(g.cache, k)
			}
		}
		if len(g.cache) >= graphCacheEntries {
			clear(g.cache)
		}
	}
	g.cache[key] = snap
	return snap, nil
}

type graphEdge struct {
	w           int
	first, last int64
}

type graphNode struct {
	key, room   string // room is the pool's room for an anonymous pool
	anon        bool
	posts       int
	first, last int64
	rooms       map[string]int
}

func (s *Store) computeGraph(ctx context.Context, room string, since int64, now time.Time) (*GraphSnapshot, error) {
	query := "SELECT e.id,e.room,e.author,e.public_key='',e.reply_to,e.created_at,e.supersedes<>''" + graphFrom
	var args []any
	if room != "" {
		query += " AND e.room=?"
		args = append(args, room)
	}
	if since > 0 {
		query += " AND e.created_at>=?"
		args = append(args, since)
	}
	query += " ORDER BY e.created_at,e.seq"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var (
		nodes      []*graphNode
		byKey      = map[string]int{}
		authorOf   = map[string]int{} // message ID -> node
		reply      = map[[2]int]*graphEdge{}
		member     = map[[2]int]*graphEdge{} // [node, room index placeholder]
		roomPosts  = map[string]int{}
		roomFirst  = map[string]int64{}
		roomLast   = map[string]int64{}
		memberRoom = map[[2]int]string{}
		messages   int
		t0, t1     int64
	)
	type pending struct {
		node   int
		parent string
		at     int64
	}
	var replies []pending
	roomID := map[string]int{}
	for rows.Next() {
		var id, rm, author, parent string
		var anon, edit bool
		var at int64
		if err = rows.Scan(&id, &rm, &author, &anon, &parent, &at, &edit); err != nil {
			rows.Close()
			return nil, err
		}
		key := author
		if anon || author == "" || author == "anonymous" {
			key, anon = "anon:"+rm, true
		}
		i, ok := byKey[key]
		if !ok {
			i = len(nodes)
			byKey[key] = i
			n := &graphNode{key: key, anon: anon, first: at, rooms: map[string]int{}}
			if anon {
				n.room = rm
			}
			nodes = append(nodes, n)
		}
		authorOf[id] = i
		if edit {
			continue // an edit is not a second post, but replies to it still resolve
		}
		n := nodes[i]
		n.posts++
		n.last = at
		n.rooms[rm]++
		if messages == 0 {
			t0 = at
		}
		messages++
		t1 = at
		roomPosts[rm]++
		if _, seen := roomFirst[rm]; !seen {
			roomFirst[rm] = at
		}
		roomLast[rm] = at
		r, ok := roomID[rm]
		if !ok {
			r = len(roomID)
			roomID[rm] = r
		}
		k := [2]int{i, r}
		if e := member[k]; e != nil {
			e.w++
			e.last = at
		} else {
			member[k] = &graphEdge{1, at, at}
			memberRoom[k] = rm
		}
		if parent != "" {
			replies = append(replies, pending{i, parent, at})
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	for _, p := range replies {
		j, ok := authorOf[p.parent]
		if !ok {
			continue // the parent is not public (or not in this window): no edge
		}
		k := [2]int{p.node, j}
		if e := reply[k]; e != nil {
			e.w++
			e.last = p.at
		} else {
			reply[k] = &graphEdge{1, p.at, p.at}
		}
	}
	// Drop nodes that only authored edits (no posts of their own in the set).
	keep := make([]int, len(nodes))
	var live []*graphNode
	for i, n := range nodes {
		if n.posts == 0 {
			keep[i] = -1
			continue
		}
		keep[i] = len(live)
		live = append(live, n)
	}
	handles := map[string]string{}
	hrows, err := s.db.QueryContext(ctx, "SELECT id,handle FROM identities WHERE handle<>''")
	if err != nil {
		return nil, err
	}
	for hrows.Next() {
		var id, h string
		if err = hrows.Scan(&id, &h); err != nil {
			hrows.Close()
			return nil, err
		}
		if _, ok := byKey[id]; ok {
			handles[id] = h
		}
	}
	if err = closeRows(hrows); err != nil {
		return nil, err
	}

	rooms := make([]string, 0, len(roomPosts))
	for r := range roomPosts {
		rooms = append(rooms, r)
	}
	sort.Strings(rooms)
	rix := make(map[string]int, len(rooms))
	for i, r := range rooms {
		rix[r] = i
	}
	N := len(live)
	var doc graphDocument
	doc.OK = true
	doc.Rooms = rooms
	nd := &doc.Nodes
	total := N + len(rooms)
	*nd = graphNodes{Key: make([]any, 0, total), Kind: make([]int, 0, total), Label: make([]string, 0, total), Posts: make([]int, 0, total), First: make([]int64, 0, total), Last: make([]int64, 0, total), Rooms: make([][]int, 0, total), Community: make([]int, 0, total)}
	for _, n := range live {
		type rc struct {
			room  string
			count int
		}
		var rs []rc
		for r, c := range n.rooms {
			rs = append(rs, rc{r, c})
		}
		sort.Slice(rs, func(a, b int) bool {
			if rs[a].count != rs[b].count {
				return rs[a].count > rs[b].count
			}
			return rs[a].room < rs[b].room
		})
		list := make([]int, len(rs))
		for i, r := range rs {
			list[i] = rix[r.room]
		}
		if n.anon {
			nd.Key = append(nd.Key, nil)
			nd.Kind = append(nd.Kind, GraphAnonPool)
			nd.Label = append(nd.Label, "anonymous · "+n.room)
		} else {
			nd.Key = append(nd.Key, n.key)
			nd.Kind = append(nd.Kind, GraphIdentity)
			label := handles[n.key]
			if label == "" {
				label = n.key
				if len(label) > 12 {
					label = label[:12]
				}
			}
			nd.Label = append(nd.Label, label)
		}
		nd.Posts = append(nd.Posts, n.posts)
		nd.First = append(nd.First, n.first)
		nd.Last = append(nd.Last, n.last)
		nd.Rooms = append(nd.Rooms, list)
		nd.Community = append(nd.Community, list[0])
	}
	for i, r := range rooms {
		nd.Key = append(nd.Key, nil)
		nd.Kind = append(nd.Kind, GraphRoom)
		nd.Label = append(nd.Label, "#"+r)
		nd.Posts = append(nd.Posts, roomPosts[r])
		nd.First = append(nd.First, roomFirst[r])
		nd.Last = append(nd.Last, roomLast[r])
		nd.Rooms = append(nd.Rooms, []int{i})
		nd.Community = append(nd.Community, i)
	}
	type packed struct {
		src, dst int
		e        *graphEdge
	}
	pack := func(in []packed) graphEdges {
		sort.Slice(in, func(a, b int) bool {
			if in[a].e.first != in[b].e.first {
				return in[a].e.first < in[b].e.first
			}
			if in[a].src != in[b].src {
				return in[a].src < in[b].src
			}
			return in[a].dst < in[b].dst
		})
		out := graphEdges{Src: make([]int, len(in)), Dst: make([]int, len(in)), W: make([]int, len(in)), First: make([]int64, len(in)), Last: make([]int64, len(in))}
		for i, p := range in {
			out.Src[i], out.Dst[i], out.W[i], out.First[i], out.Last[i] = p.src, p.dst, p.e.w, p.e.first, p.e.last
		}
		return out
	}
	var rp, mp []packed
	for k, e := range reply {
		a, b := keep[k[0]], keep[k[1]]
		if a < 0 || b < 0 {
			continue // a parent known only as an edit's author
		}
		rp = append(rp, packed{a, b, e})
	}
	for k, e := range member {
		mp = append(mp, packed{keep[k[0]], N + rix[memberRoom[k]], e})
	}
	doc.Edges.Reply = pack(rp)
	doc.Edges.Member = pack(mp)
	m := &doc.Meta
	m.Generated, m.Messages, m.T0, m.T1, m.Room, m.Since, m.TTL = now.Unix(), messages, t0, t1, room, since, int(GraphTTL/time.Second)
	m.Identity = "sha256 fingerprint of the signing key; unsigned posts are pooled per room"
	m.Notes = []string{
		"Visible messages in public rooms only: no private rooms, conversations, addressed messages, hidden posts or text.",
		"A reply edge is drawn only when the parent message is in the same public set. An edit is not a second post.",
		"Recomputed at most every 30 seconds.",
	}
	if doc.Rooms == nil {
		doc.Rooms = []string{}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	return &GraphSnapshot{Generated: now, JSON: body, ETag: `"` + hex.EncodeToString(sum[:8]) + `"`, Messages: messages}, nil
}
