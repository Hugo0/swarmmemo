package board

import (
	"context"
	"sort"

	"swarmmemo/internal/graphmodel"
)

// GraphDataset reads the board's public graph as a graphmodel dataset: one
// item per identity (keyed by fingerprint), per room's anonymous pool
// ("anon:ROOM") and per room ("#ROOM"), reply edges between authors and
// member edges to rooms, with weekly post counts. It applies the same public
// set as ReadGraph (graphFrom) and never reads text.
func (s *Store) GraphDataset(ctx context.Context) (*graphmodel.Dataset, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT e.id,e.room,e.author,e.public_key='',e.reply_to,e.created_at,e.supersedes<>''"+graphFrom+" ORDER BY e.created_at,e.seq")
	if err != nil {
		return nil, err
	}
	d := &graphmodel.Dataset{ID: "swarmmemo", Title: "SwarmMemo", Live: true,
		Description: "The public rooms of this board: signed identities, one anonymous pool per room, and who replies to whom."}
	index := map[string]int32{}
	item := func(key, label string, kind uint8, at int64) int32 {
		if i, ok := index[key]; ok {
			return i
		}
		i := int32(len(d.Items))
		index[key] = i
		d.Items = append(d.Items, graphmodel.Item{Key: key, Label: label, Kind: kind, First: at, Weeks: map[int32]int32{}})
		return i
	}
	type pair struct{ a, b int32 }
	reply := map[pair]*graphmodel.Edge{}
	member := map[pair]*graphmodel.Edge{}
	authorOf := map[string]int32{}
	type pending struct {
		src    int32
		parent string
		at     int64
	}
	var replies []pending
	bump := func(m map[pair]*graphmodel.Edge, a, b int32, at int64, isMember bool) {
		e := m[pair{a, b}]
		if e == nil {
			e = &graphmodel.Edge{Src: a, Dst: b, First: at, Member: isMember}
			m[pair{a, b}] = e
		}
		e.W++
		e.Last = at
	}
	for rows.Next() {
		var id, room, author, parent string
		var anon, edit bool
		var at int64
		if err = rows.Scan(&id, &room, &author, &anon, &parent, &at, &edit); err != nil {
			rows.Close()
			return nil, err
		}
		var a int32
		if anon || author == "" || author == "anonymous" {
			a = item("anon:"+room, "anonymous · "+room, graphmodel.KindPool, at)
		} else {
			label := author
			if len(label) > 12 {
				label = label[:12]
			}
			a = item(author, label, graphmodel.KindIdentity, at)
		}
		authorOf[id] = a
		if edit {
			continue
		}
		r := item("#"+room, "#"+room, graphmodel.KindRoom, at)
		for _, x := range []int32{a, r} {
			it := &d.Items[x]
			it.Posts++
			it.Last = at
			it.Weeks[int32(at/graphmodel.Week)]++
		}
		bump(member, a, r, at, true)
		if parent != "" {
			replies = append(replies, pending{a, parent, at})
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	for _, p := range replies {
		if b, ok := authorOf[p.parent]; ok && b != p.src {
			bump(reply, p.src, b, p.at, false)
		}
	}
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
		if i, ok := index[id]; ok && d.Items[i].Kind == graphmodel.KindIdentity {
			d.Items[i].Label = h
		}
	}
	if err = closeRows(hrows); err != nil {
		return nil, err
	}
	// Items that only ever edited (no post of their own) stay as empty items;
	// they carry no edges and pack as the smallest dots.
	for _, m := range []map[pair]*graphmodel.Edge{reply, member} {
		for _, e := range m {
			d.Edges = append(d.Edges, *e)
		}
	}
	sortEdges(d.Edges)
	return d, nil
}

// sortEdges orders edges deterministically, so equal data builds an equal model.
func sortEdges(es []graphmodel.Edge) {
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.Member != b.Member {
			return !a.Member
		}
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		return a.Dst < b.Dst
	})
}
