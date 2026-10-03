package graphmodel

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// universeJSON is the derived external galaxies (scripts/graph_datasets.py):
// other agent boards and AI Village, as metadata only, with bridges.
//
//go:embed datasets/universe.json
var universeJSON []byte

type fileDataset struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Citation    string `json:"citation"`
	URL         string `json:"url"`
	Items       struct {
		Key     []string     `json:"key"`
		Label   []string     `json:"label"`
		Kind    []uint8      `json:"kind"`
		Posts   []int64      `json:"posts"`
		First   []int64      `json:"first"`
		Last    []int64      `json:"last"`
		Weeks   [][][2]int64 `json:"weeks"`
		Cluster []int32      `json:"cluster"`
	} `json:"items"`
	Edges struct {
		Src    []int32   `json:"src"`
		Dst    []int32   `json:"dst"`
		W      []float64 `json:"w"`
		First  []int64   `json:"first"`
		Last   []int64   `json:"last"`
		Member []bool    `json:"member"`
	} `json:"edges"`
	Clusters []struct {
		ID    int32  `json:"id"`
		Label string `json:"label"`
		Tag   string `json:"tag"`
	} `json:"clusters"`
}

type fileBridge struct {
	A        [2]string        `json:"a"`
	B        [2]string        `json:"b"`
	Kind     string           `json:"kind"`
	Sub      []string         `json:"sub"`
	Conf     float64          `json:"conf"`
	Dashed   bool             `json:"dashed"`
	Evidence []map[string]any `json:"evidence"`
}

type file struct {
	Schema   int           `json:"schema"`
	Datasets []fileDataset `json:"datasets"`
	Bridges  []fileBridge  `json:"bridges"`
}

// Embedded returns the shipped external datasets and bridges.
func Embedded() ([]*Dataset, []Bridge, error) { return Parse(universeJSON) }

// MaxFileBytes caps a datasets file read by LoadFile.
const MaxFileBytes = 512 << 20

// LoadFile reads a datasets file in the same format, up to MaxFileBytes.
func LoadFile(path string) ([]*Dataset, []Bridge, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxFileBytes {
		return nil, nil, fmt.Errorf("datasets file is over %d bytes", MaxFileBytes)
	}
	return Parse(raw)
}

// Parse decodes a datasets file.
func Parse(raw []byte) ([]*Dataset, []Bridge, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, err
	}
	if f.Schema != 1 {
		return nil, nil, errors.New("graph datasets: unsupported schema")
	}
	var out []*Dataset
	for _, fd := range f.Datasets {
		n := len(fd.Items.Key)
		if len(fd.Items.Label) != n || len(fd.Items.Kind) != n || len(fd.Items.Posts) != n || len(fd.Items.First) != n || len(fd.Items.Last) != n {
			return nil, nil, errors.New("graph datasets: item arrays differ in length in " + fd.ID)
		}
		d := &Dataset{ID: fd.ID, Title: fd.Title, Description: fd.Description, Citation: fd.Citation, URL: fd.URL, Items: make([]Item, n), Clustered: len(fd.Items.Cluster) == n && n > 0}
		for i := 0; i < n; i++ {
			it := Item{Key: fd.Items.Key[i], Label: fd.Items.Label[i], Kind: fd.Items.Kind[i], Posts: fd.Items.Posts[i], First: fd.Items.First[i], Last: fd.Items.Last[i], Cluster: -1}
			if i < len(fd.Items.Cluster) {
				it.Cluster = fd.Items.Cluster[i]
			}
			if it.Kind > KindInfra {
				return nil, nil, errors.New("graph datasets: unknown item kind in " + fd.ID)
			}
			if i < len(fd.Items.Weeks) && len(fd.Items.Weeks[i]) > 0 {
				it.Weeks = make(map[int32]int32, len(fd.Items.Weeks[i]))
				for _, wc := range fd.Items.Weeks[i] {
					it.Weeks[int32(wc[0])] += int32(wc[1])
				}
			}
			d.Items[i] = it
		}
		for _, c := range fd.Clusters {
			if c.Label != "" {
				if d.ClusterLabels == nil {
					d.ClusterLabels = map[int32]string{}
				}
				d.ClusterLabels[c.ID] = c.Label
			}
			if c.Tag != "" {
				if d.ClusterTags == nil {
					d.ClusterTags = map[int32]string{}
				}
				d.ClusterTags[c.ID] = c.Tag
			}
		}
		e := fd.Edges
		m := len(e.Src)
		if len(e.Dst) != m || len(e.W) != m {
			return nil, nil, errors.New("graph datasets: edge arrays differ in length in " + fd.ID)
		}
		d.Edges = make([]Edge, 0, m)
		for j := 0; j < m; j++ {
			x := Edge{Src: e.Src[j], Dst: e.Dst[j], W: e.W[j]}
			if j < len(e.First) {
				x.First = e.First[j]
			}
			if j < len(e.Last) {
				x.Last = e.Last[j]
			}
			if j < len(e.Member) {
				x.Member = e.Member[j]
			}
			d.Edges = append(d.Edges, x)
		}
		out = append(out, d)
	}
	var bridges []Bridge
	for _, b := range f.Bridges {
		bridges = append(bridges, Bridge{A: BridgeEnd{b.A[0], b.A[1]}, B: BridgeEnd{b.B[0], b.B[1]}, Kind: b.Kind, Sub: b.Sub, Conf: b.Conf, Dashed: b.Dashed, Evidence: b.Evidence})
	}
	return out, bridges, nil
}
