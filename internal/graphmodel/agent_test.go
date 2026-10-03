package graphmodel

import (
	"strings"
	"testing"
)

func TestAgentSheet(t *testing.T) {
	d := &Dataset{ID: "board", Title: "Board", Items: []Item{
		{Key: "talker", Label: "talker", Posts: 5, First: 100, Last: 400},
		{Key: "peer", Label: "peer", Posts: 3, First: 120, Last: 300},
		{Key: "spammer", Label: "spammer", Posts: 5000, First: 100, Last: 900},
		{Key: "#lobby", Label: "#lobby", Kind: KindRoom},
	}, Edges: []Edge{
		{Src: 0, Dst: 1, W: 4, First: 150, Last: 250},
		{Src: 1, Dst: 0, W: 2, First: 160, Last: 260},
		{Src: 0, Dst: 3, W: 5, First: 100, Last: 400, Member: true},
		{Src: 2, Dst: 3, W: 5000, First: 100, Last: 900, Member: true},
	}}
	m := Build([]*Dataset{d}, nil, Options{})
	node := func(key string) int32 { id, _ := m.Locate("board", key); return id[len(id)-1] }
	s, ok := m.Agent(node("talker"))
	if !ok || s.Label != "talker" || s.Dataset != "board" || s.Counterpart != 1 || s.Received != 2 {
		t.Fatalf("sheet %+v", s)
	}
	if len(s.Places) != 1 || s.Places[0].Label != "#lobby" || s.Places[0].N != 5 || s.Places[0].First != 100 || s.Places[0].Last != 400 {
		t.Fatalf("places %+v", s.Places)
	}
	if len(s.Peers) != 1 || s.Peers[0].Label != "peer" || s.Peers[0].Sent != 4 || s.Peers[0].Received != 2 {
		t.Fatalf("peers %+v", s.Peers)
	}
	if _, ok := m.Agent(m.Datasets[0].Node); ok {
		t.Fatal("a galaxy has no agent sheet")
	}
	// Size follows engagement, not volume: a talker outgrows a spammer with 1000x its posts.
	if a, b := m.Nodes[node("talker")].R, m.Nodes[node("spammer")].R; a <= b {
		t.Fatalf("talker r=%.2f, spammer r=%.2f", a, b)
	}
}

func TestShippedAgentSheets(t *testing.T) {
	ds, bridges, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	m := Build(ds, bridges, Options{})
	var excerpts, goals, pages int
	for id := range m.Nodes {
		n := &m.Nodes[id]
		if n.Item < 0 || n.Kind != KindIdentity {
			continue
		}
		s, _ := m.Agent(int32(id))
		if len(s.Excerpts) > 10 {
			t.Fatalf("%s has %d excerpts; the cap is 10", s.Label, len(s.Excerpts))
		}
		for _, p := range s.Excerpts {
			if len([]rune(p.Text)) > 200 || !strings.HasPrefix(p.URL, "https://") {
				t.Fatalf("excerpt %+v", p)
			}
		}
		switch s.Dataset {
		case "aivillage", "collusionwiki":
			if len(s.Excerpts) > 0 {
				t.Fatalf("%s ships text for %s", s.Dataset, s.Label)
			}
		}
		excerpts += len(s.Excerpts)
		goals += len(s.Goals)
		if s.Dataset == "collusionwiki" {
			pages += len(s.Places)
		}
	}
	if excerpts == 0 || goals == 0 || pages == 0 {
		t.Fatalf("excerpts %d, goals %d, wiki pages %d", excerpts, goals, pages)
	}
}
