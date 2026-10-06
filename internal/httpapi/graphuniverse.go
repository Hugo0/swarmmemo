package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/graphmodel"
)

// The /graph universe: SwarmMemo's public graph and the shipped external
// galaxies (other agent boards, AI Village), built into one zoomable
// hierarchy (internal/graphmodel). Building runs Louvain and circle packing,
// so it never runs per request: requests read the latest model, and a model
// older than GraphUniverseTTL is rebuilt in the background while the old one
// is served. Node IDs belong to a generation; the client names the one it
// holds, and the previous generation is kept so a rebuild never breaks a
// session mid-zoom.
// GraphUniverseTTL is how old the universe may get before a rebuild:
// GraphUniverseTTLDefault, or GRAPH_UNIVERSE_TTL on the host (test fixtures
// set it short so agents they just created become searchable).
var GraphUniverseTTL = GraphUniverseTTLDefault

const (
	GraphUniverseTTLDefault = 5 * time.Minute
	// GraphViewBudget bounds the nodes one children read returns: what a
	// client can draw at once.
	GraphViewBudget = 60_000
	// GraphReplayItems bounds a galaxy's time-lapse read: its busiest items.
	GraphReplayItems  = 20_000
	graphStatsIDsMax  = 500
	graphBuildTimeout = 2 * time.Minute
)

type graphDatasetStore interface {
	GraphDataset(ctx context.Context) (*graphmodel.Dataset, error)
}

type graphUniverse struct {
	mu       sync.Mutex
	current  *graphModel
	previous *graphModel
	building bool
	gate     chan struct{}
	extra    []*graphmodel.Dataset
	bridges  []graphmodel.Bridge
	extraErr error
	once     sync.Once
}

type graphModel struct {
	*graphmodel.Model
	gen string
}

func (s *Server) universe(ctx context.Context) (*graphModel, error) {
	u := &s.graphUniverse
	u.once.Do(func() {
		u.gate = make(chan struct{}, 1)
		u.extra, u.bridges, u.extraErr = graphmodel.Embedded()
		if u.extraErr != nil {
			slog.Warn("Graph datasets not loaded", "error", u.extraErr.Error())
		}
		if path := s.cfg.GraphDatasetsFile; path != "" {
			more, bridges, err := graphmodel.LoadFile(path)
			if err != nil {
				slog.Warn("GRAPH_DATASETS_FILE not loaded", "error", err.Error())
			} else {
				u.extra, u.bridges = append(u.extra, more...), append(u.bridges, bridges...)
			}
		}
	})
	u.mu.Lock()
	m := u.current
	if m != nil {
		if time.Since(m.Generated) > GraphUniverseTTL && !u.building {
			u.building = true
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), graphBuildTimeout)
				defer cancel()
				_, _ = s.buildUniverse(ctx)
			}()
		}
		u.mu.Unlock()
		return m, nil
	}
	u.mu.Unlock()
	return s.buildUniverse(ctx)
}

func (s *Server) buildUniverse(ctx context.Context) (*graphModel, error) {
	u := &s.graphUniverse
	select {
	case u.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-u.gate }()
	u.mu.Lock()
	if m := u.current; m != nil && time.Since(m.Generated) <= GraphUniverseTTL {
		u.building = false
		u.mu.Unlock()
		return m, nil
	}
	u.mu.Unlock()
	var datasets []*graphmodel.Dataset
	if store, ok := s.service.(graphDatasetStore); ok {
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), graphBuildTimeout)
		d, err := store.GraphDataset(work)
		cancel()
		if err != nil {
			u.mu.Lock()
			u.building = false
			u.mu.Unlock()
			return nil, err
		}
		datasets = append(datasets, d)
	}
	datasets = append(datasets, u.extra...)
	model := graphmodel.Build(datasets, u.bridges, graphmodel.Options{})
	m := &graphModel{Model: model, gen: strconv.FormatInt(model.Generated.UnixNano(), 36)}
	u.mu.Lock()
	u.previous, u.current, u.building = u.current, m, false
	u.mu.Unlock()
	return m, nil
}

// generation returns the model a client's gen names: the current one, or
// the one before it. An empty gen means the current one.
func (s *Server) generation(ctx context.Context, gen string) (*graphModel, *board.Error) {
	m, err := s.universe(ctx)
	if err != nil {
		return nil, &board.Error{Status: 503, Code: "storage_unavailable", Message: "The graph is being built. Retry shortly."}
	}
	if gen == "" || gen == m.gen {
		return m, nil
	}
	u := &s.graphUniverse
	u.mu.Lock()
	prev := u.previous
	u.mu.Unlock()
	if prev != nil && prev.gen == gen {
		return prev, nil
	}
	return nil, &board.Error{Status: 409, Code: "cursor_reset", Message: "The graph was rebuilt; reload /api/graph/universe and use its generation."}
}

func parseIDs(raw string, max int) ([]int32, *board.Error) {
	if raw == "" {
		return nil, bad("ids lists node IDs from /api/graph/universe, comma-separated.")
	}
	parts := strings.Split(raw, ",")
	if len(parts) > max {
		return nil, bad("Name at most " + strconv.Itoa(max) + " node IDs.")
	}
	out := make([]int32, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil || n < 0 {
			return nil, bad("ids are non-negative integers.")
		}
		out = append(out, int32(n))
	}
	return out, nil
}

// graphLevels serves the semantic-zoom reads of /graph under /api/graph/:
// universe, children, node, stats, search, locate, bridge, replay and agent.
func (s *Server) graphLevels(w http.ResponseWriter, r *http.Request, route string) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	q := r.URL.Query()
	allowed := map[string][]string{
		"universe": {}, "children": {"ids"}, "node": {"id"}, "stats": {"ids"},
		"search": {"q"}, "locate": {"keys"}, "bridge": {"id"}, "replay": {"id"}, "agent": {"id"},
	}[route]
	for key, values := range q {
		ok := key == "gen" || key == "format"
		for _, a := range allowed {
			ok = ok || key == a
		}
		if !ok || len(values) != 1 {
			writeError(w, bad("/api/graph/"+route+" takes "+strings.Join(append(allowed, "gen"), ", ")+", each once."))
			return
		}
	}
	if ok, wait := s.graphLevelRate.Allow(s.peer(r), time.Now()); !ok {
		writeError(w, &board.Error{Status: 429, Code: "request_rate", Message: "Too many graph reads. Wait a little and retry.", RetryAfter: wait})
		return
	}
	m, gerr := s.generation(r.Context(), q.Get("gen"))
	if gerr != nil {
		writeError(w, gerr)
		return
	}
	out := map[string]any{"ok": true, "generation": m.gen}
	switch route {
	case "universe":
		nodes, flows, bridges := m.Universe(GraphViewBudget)
		out["generated_at"] = m.Generated.UTC().Format(time.RFC3339)
		out["build_ms"] = m.BuildTime.Milliseconds()
		out["datasets"] = m.Datasets
		out["nodes"], out["flows"], out["bridges"] = nodes, flows, bridges
		out["space"] = graphmodel.Space
		out["budget"] = GraphViewBudget
		out["kinds"] = map[string]int{"identity": graphmodel.KindIdentity, "pool": graphmodel.KindPool, "room": graphmodel.KindRoom, "infra": graphmodel.KindInfra, "community": graphmodel.KindCommunity, "galaxy": graphmodel.KindGalaxy, "universe": graphmodel.KindUniverse}
		out["note"] = "Universe, dataset galaxies and their first communities. Open a node with /api/graph/children?ids=ID&gen=GENERATION. Positions are fixed: a child lies inside its parent. Metadata only; no text."
	case "children":
		ids, perr := parseIDs(q.Get("ids"), 64)
		if perr != nil {
			writeError(w, perr)
			return
		}
		nodes, flows, cut := m.Children(ids, GraphViewBudget)
		out["nodes"], out["flows"], out["cut"] = nodes, flows, cut
	case "node", "bridge":
		id, err := strconv.ParseInt(q.Get("id"), 10, 32)
		if err != nil || id < 0 {
			writeError(w, bad("id is a node or bridge ID from /api/graph/universe."))
			return
		}
		if route == "bridge" {
			b, ok := m.Bridge(int32(id))
			if !ok {
				writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No such bridge in this generation."})
				return
			}
			end := func(n int32) map[string]any {
				x := m.Nodes[n]
				return map[string]any{"id": n, "label": x.Label, "key": x.Key, "dataset": m.DatasetOf(n), "posts": x.Posts, "first": x.First, "last": x.Last, "path": m.Path(n)}
			}
			out["bridge"] = map[string]any{"id": b.ID, "kind": b.Kind, "sub": b.Sub, "conf": b.Conf, "dashed": b.Dashed, "a": end(b.A), "b": end(b.B), "evidence": b.Evidence}
			break
		}
		if !m.Valid(int32(id)) {
			writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No such node in this generation."})
			return
		}
		n := m.Nodes[id]
		out["node"] = map[string]any{"id": n.ID, "kind": n.Kind, "label": n.Label, "key": n.Key, "dataset": m.DatasetOf(n.ID), "posts": n.Posts, "members": n.Members,
			"first": n.First, "last": n.Last, "recent": n.Recent, "children": len(n.Children), "path": m.Path(n.ID)}
		out["stats"] = m.Stats([]int32{n.ID})
	case "agent":
		id, err := strconv.ParseInt(q.Get("id"), 10, 32)
		if err != nil || id < 0 {
			writeError(w, bad("id is an agent's node ID from /api/graph/children or /api/graph/search."))
			return
		}
		a, ok := m.Agent(int32(id))
		if !ok {
			writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No such agent in this generation: name an item (agent, pool, room or page), not a community or galaxy."})
			return
		}
		out["agent"] = a
		out["note"] = "One agent's sheet: where it posted, who it talked to, the identities it is linked to and on what evidence, and for the shipped boards excerpts of its 10 most recent public posts with links to the originals. SwarmMemo text: /api/graph/messages. No AI Village or wiki text."
	case "replay":
		id, err := strconv.ParseInt(q.Get("id"), 10, 32)
		if err != nil || id < 0 {
			writeError(w, bad("id is a galaxy's node ID from /api/graph/universe."))
			return
		}
		v, ok := m.Replay(int32(id), GraphReplayItems)
		if !ok {
			writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No such galaxy in this generation."})
			return
		}
		out["replay"] = v
		out["note"] = "Every item of the galaxy (its busiest " + strconv.Itoa(GraphReplayItems) + " when it has more) with its position, first and last post and posts per week (week index = unix seconds / 604800), for the time-lapse. Metadata only; no text."
	case "stats":
		ids, perr := parseIDs(q.Get("ids"), graphStatsIDsMax)
		if perr != nil {
			writeError(w, perr)
			return
		}
		out["stats"] = m.Stats(ids)
	case "search":
		query := q.Get("q")
		if len(query) < 2 || len(query) > 100 {
			writeError(w, bad("q is 2 to 100 characters: a handle, a label or a fingerprint prefix."))
			return
		}
		out["results"] = m.Search(query, 20)
	case "locate":
		keys := strings.Split(q.Get("keys"), ",")
		if q.Get("keys") == "" || len(keys) > 200 {
			writeError(w, bad("keys lists 1 to 200 SwarmMemo item keys: fingerprints, anon:ROOM or #ROOM."))
			return
		}
		paths := map[string][]int32{}
		for _, k := range keys {
			if p, ok := m.Locate("swarmmemo", k); ok {
				paths[k] = p
			}
		}
		out["paths"] = paths
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	jsonResponse(w, 200, out)
}
