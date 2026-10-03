// Package graphmodel turns interaction graphs from one or more datasets into a
// zoomable hierarchy: a universe of dataset galaxies, each split by
// multi-level Louvain into communities, down to identities. Every node gets a
// fixed position from hierarchical circle packing (a child always lies inside
// its parent), so a client can zoom from galaxies to agents with nothing ever
// moving. Edges are aggregated into flows between siblings, and bridges link
// identities across datasets with their evidence.
//
// The model holds metadata only: keys, labels, counts, times and weights,
// never message text.
package graphmodel

import "time"

// Node kinds. Items (identities, pools, rooms) are the leaves.
const (
	KindIdentity  = 0
	KindPool      = 1 // unsigned or human posters pooled together
	KindRoom      = 2 // a room or a board
	KindInfra     = 3 // external infrastructure a population relies on (a domain, a relay)
	KindCommunity = 10
	KindGalaxy    = 20
	KindUniverse  = 30
)

// Week is the bucket of activity series, in seconds.
const Week = 7 * 24 * 3600

// Item is one leaf of a dataset.
type Item struct {
	Key   string // unique within the dataset; a SwarmMemo identity's fingerprint
	Label string
	Kind  uint8
	Posts int64
	First int64
	Last  int64
	// Weeks counts posts per week index (unix seconds / Week); optional.
	Weeks map[int32]int32
	// Cluster is a community precomputed by the dataset (-1: none); read
	// only when the dataset is Clustered.
	Cluster int32
}

// Edge is a directed interaction (reply, mention, adjacency) or, with Member
// set, an item's posts in a room.
type Edge struct {
	Src, Dst    int32
	W           float64
	First, Last int64
	Member      bool
}

// Dataset is one galaxy's input.
type Dataset struct {
	ID          string
	Title       string
	Description string
	Citation    string
	URL         string
	Live        bool // SwarmMemo itself: public text and the live stream are available
	Items       []Item
	Edges       []Edge
	// Clustered: Item.Cluster holds precomputed clusters, which become the
	// galaxy's top communities. ClusterLabels names them; optional.
	Clustered     bool
	ClusterTags   map[int32]string // short names shown after a cluster's label; "C<id>" when absent
	ClusterLabels map[int32]string
}

// BridgeEnd names an item of a dataset.
type BridgeEnd struct {
	Dataset string
	Key     string
}

// Bridge links two items, usually in different datasets: the same agent, or
// one interacting with another, with how sure the match is.
type Bridge struct {
	A, B     BridgeEnd
	Kind     string // explicit, handle, same-owner, style, interaction, ...
	Sub      []string
	Conf     float64 // 0..1
	Dashed   bool    // weak evidence, drawn dashed
	Evidence []map[string]any
}

// Node is one point of the hierarchy, from the universe down to an item.
type Node struct {
	ID       int32
	Parent   int32 // -1 for the universe
	Kind     uint8
	Depth    uint8
	Dataset  int16 // -1 for the universe
	Item     int32 // index in its dataset's Items, -1 above items
	Children []int32
	X, Y, R  float64
	Label    string
	Key      string
	Posts    int64
	Members  int64 // identities and pools in the subtree
	First    int64
	Last     int64
	Recent   int64 // posts in the dataset's latest two weeks
	Internal float64
}

// Flow aggregates the edges between two siblings under one parent.
type Flow struct {
	A, B   int32 // node IDs, A < B
	AB, BA float64
	First  int64
	Last   int64
	Member bool
}

// ModelBridge is a resolved bridge between two item nodes.
type ModelBridge struct {
	ID       int32
	A, B     int32
	Kind     string
	Sub      []string
	Conf     float64
	Dashed   bool
	Evidence []map[string]any
}

// DatasetInfo describes a galaxy.
type DatasetInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Citation    string `json:"citation,omitempty"`
	URL         string `json:"url,omitempty"`
	Live        bool   `json:"live"`
	Node        int32  `json:"node"`
	Items       int    `json:"items"`
	Posts       int64  `json:"posts"`
	T0          int64  `json:"t0"`
	T1          int64  `json:"t1"`
}

// Model is a built hierarchy. It is immutable once built and safe to share.
type Model struct {
	Generated time.Time
	BuildTime time.Duration
	Datasets  []DatasetInfo
	Nodes     []Node
	Flows     map[int32][]Flow // by parent: between its children, heaviest first
	Bridges   []ModelBridge
	itemNode  [][]int32 // dataset -> item -> node
	byKey     map[string]int32
	byLabel   map[string]int32 // dataset/lowercased label, for bridges that name a handle
	galaxyOf  map[string]int32
	data      []*Dataset
	out       [][][]int32 // dataset -> item -> indices of its outgoing interaction edges
	member    [][][]int32 // dataset -> item -> indices of its member edges
	inc       [][][]int32 // dataset -> item -> indices of its incoming interaction edges
	bridgesOf map[int32][]int32
	lastWeek  []int64
	affinity  map[string]float64 // "a\x00b" dataset IDs -> bridge weight, for the layout
}

// Space is the coordinate range positions are scaled into: [0, Space].
const Space = 4096
