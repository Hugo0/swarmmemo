package board

// The public trust network (/api/trust/graph, the network on /trust and
// /trust/network): identities sized by standing in the latest trust run, and
// the public edges standing moves along (docs/TRUST_MODEL.md, "The graph
// stays"): vouches, accepted work, verified witnesses, and verified key links
// between accounts. Votes, replies, DMs and anything private are never edges
// here. Only public accounts (publicAccountSQL) are named, as in trust.get
// and the run snapshot. The graph is capped: the top accounts by standing
// (the core), then their neighbours up to a node cap, and the heaviest edges
// up to an edge cap.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

const (
	// TrustGraphCoreDefault and TrustGraphCoreMax bound the core: accounts
	// with standing, highest first (?limit=).
	TrustGraphCoreDefault = 100
	TrustGraphCoreMax     = 250
	// TrustGraphNodesPerCore is the node cap as a multiple of the core:
	// the core and its neighbours.
	TrustGraphNodesPerCore = 2
	// TrustGraphEdgesMax caps the edges returned, heaviest first.
	TrustGraphEdgesMax = 2000
	// trustGraphScanMax caps the rows read per edge kind.
	trustGraphScanMax = 20000
	// trustGraphTTL is how long a built graph is reused, in seconds.
	trustGraphTTL = 300
)

// TrustGraphEdgeKinds are the edge kinds, in the order the legend shows them.
var TrustGraphEdgeKinds = []string{"vouch", "work_accept", "witness", "key_link"}

// TrustGraph is the public trust network as /api/trust/graph serves it.
type TrustGraph struct {
	Mode          string           `json:"mode"`
	Run           int64            `json:"run"`
	AsOf          int64            `json:"as_of"`
	ParamsVersion int64            `json:"params_version"`
	Limits        TrustGraphLimits `json:"limits"`
	Nodes         []TrustGraphNode `json:"nodes"`
	Edges         []TrustGraphEdge `json:"edges"`
	// Truncated counts what the caps left out: accounts with standing
	// beyond the core, neighbours beyond the node cap, edges beyond the
	// edge cap.
	Truncated TrustGraphLimits `json:"truncated"`
	Scope     string           `json:"scope"`
}

// TrustGraphLimits is a node and edge count: the caps, or what they cut.
type TrustGraphLimits struct {
	Core  int `json:"core"`
	Nodes int `json:"nodes"`
	Edges int `json:"edges"`
}

// TrustGraphNode is one identity: its fingerprint (the account), handle,
// standing in the latest run, band, and the kinds of the roots behind it
// (domain, wallet, github, pow, spend, arbiter; never their values).
type TrustGraphNode struct {
	ID            string   `json:"id"`
	Handle        string   `json:"handle"`
	Standing      float64  `json:"standing"`
	StandingCents int64    `json:"standing_cents"`
	FakeCost      string   `json:"fake_cost"`
	Band          int64    `json:"band"`
	BandName      string   `json:"band_name"`
	Roots         []string `json:"roots"`
	Penalised     bool     `json:"penalised"`
	Core          bool     `json:"core"`
}

// TrustGraphEdge is one public edge between two shown identities, the acts
// of one kind from one to the other added up: weight is their summed act
// weight (a vouch its chosen weight, accepted work and a witness 10 each).
type TrustGraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Count  int64  `json:"count"`
	Weight int64  `json:"weight"`
}

// trustGraphCache keeps the last graph per core size for trustGraphTTL.
type trustGraphCache struct {
	mu sync.Mutex
	m  map[int]trustGraphEntry
}

type trustGraphEntry struct {
	at int64
	g  *TrustGraph
}

// BandName names a standing band: 1 trusted, 2 proven, 3 signed.
func BandName(band int64) string {
	switch band {
	case 1:
		return "trusted"
	case 2:
		return "proven"
	}
	return "signed"
}

// TrustGraph is the public trust network of the latest finished run with
// standing, limited to core accounts by standing and their neighbours.
func (s *Store) TrustGraph(ctx context.Context, core int) (*TrustGraph, error) {
	if s.config.Features.Trust == TrustOff {
		return nil, allowanceError("service_unavailable")
	}
	if core < 1 || core > TrustGraphCoreMax {
		core = TrustGraphCoreDefault
	}
	now := s.now().Unix()
	cache := &s.trust.graph
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.m == nil {
		cache.m = map[int]trustGraphEntry{}
	}
	if e, ok := cache.m[core]; ok && now-e.at < trustGraphTTL {
		return e.g, nil
	}
	var g *TrustGraph
	err := (trustDB{s}).Read(ctx, func(q allowance.Querier) (err error) {
		g, err = buildTrustGraph(ctx, q, core, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	g.Mode = s.config.Features.Trust.String()
	cache.m[core] = trustGraphEntry{now, g}
	return g, nil
}

type graphPair struct{ from, to, kind string }

func buildTrustGraph(ctx context.Context, q allowance.Querier, core int, now int64) (*TrustGraph, error) {
	g := &TrustGraph{Nodes: []TrustGraphNode{}, Edges: []TrustGraphEdge{},
		Limits: TrustGraphLimits{Core: core, Nodes: core * TrustGraphNodesPerCore, Edges: TrustGraphEdgesMax},
		Scope:  "public accounts only; edges are vouches, accepted work on public items, verified witnesses and verified key links; never votes, replies, mentions, DMs or private rooms"}
	if ok, err := tableExists(ctx, q, "trust_runs"); err != nil || !ok {
		return g, err
	}
	var mode string
	err := q.QueryRowContext(ctx, `SELECT id,as_of,params_version,coalesce(json_extract(inputs,'$.params.standing.mode'),'') FROM trust_runs
 WHERE state='done' ORDER BY id DESC LIMIT 1`).Scan(&g.Run, &g.AsOf, &g.ParamsVersion, &mode)
	if errors.Is(err, sql.ErrNoRows) || err == nil && mode == "" {
		g.Run, g.AsOf, g.ParamsVersion = 0, 0, 0
		return g, nil
	}
	if err != nil {
		return nil, err
	}

	// The core: public accounts with standing, highest first.
	scores := map[string]*trust.StandingPart{}
	var coreIDs []string
	const withStanding = `FROM trust_scores t WHERE t.run_id=? AND coalesce(json_extract(t.parts,'$.standing.cents'),0)>0 AND `
	var total int
	if err = q.QueryRowContext(ctx, "SELECT count(*) "+withStanding+publicAccountSQL("t.account"), g.Run).Scan(&total); err != nil {
		return nil, err
	}
	g.Truncated.Core = max(0, total-core)
	rows, err := q.QueryContext(ctx, "SELECT t.account,t.parts "+withStanding+publicAccountSQL("t.account")+
		" ORDER BY json_extract(t.parts,'$.standing.cents') DESC, t.account LIMIT ?", g.Run, core)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var account, parts string
		if err = rows.Scan(&account, &parts); err != nil {
			rows.Close()
			return nil, err
		}
		var p trust.Parts
		if json.Unmarshal([]byte(parts), &p) == nil && p.Standing != nil {
			scores[account] = p.Standing
		}
		coreIDs = append(coreIDs, account)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	inCore := map[string]bool{}
	for _, a := range coreIDs {
		inCore[a] = true
	}

	// Every public edge touching the core.
	agg := map[graphPair]*TrustGraphEdge{}
	add := func(from, to, kind string, weight int64) {
		if from == "" || to == "" || from == to || !(inCore[from] || inCore[to]) {
			return
		}
		k := graphPair{from, to, kind}
		e := agg[k]
		if e == nil {
			e = &TrustGraphEdge{From: from, To: to, Kind: kind}
			agg[k] = e
		}
		e.Count++
		e.Weight += weight
	}
	scan := func(query string, kind string, args ...any) error {
		r, err := q.QueryContext(ctx, query, append(args, trustGraphScanMax)...)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var from, to, payload string
			if err := r.Scan(&from, &to, &payload); err != nil {
				return err
			}
			w := int64(10)
			if kind == "vouch" {
				if n := vouchWeightOf(payload); n > 0 {
					w = int64(n)
				}
			}
			add(from, to, kind, w)
		}
		return r.Err()
	}
	if ok, err := tableExists(ctx, q, "vouches"); err != nil {
		return nil, err
	} else if ok {
		if err := scan(`SELECT v.voter_account,v.target_account,coalesce(r.signed_payload,'') FROM vouches v LEFT JOIN endorsement_records r ON r.seq=v.record_seq
 WHERE v.value=1 AND `+publicAccountSQL("v.voter_account")+` AND `+publicAccountSQL("v.target_account")+` ORDER BY v.record_seq DESC LIMIT ?`, "vouch"); err != nil {
			return nil, err
		}
	}
	if ok, err := tableExists(ctx, q, "work_transitions"); err != nil {
		return nil, err
	} else if ok {
		if err := scan(`SELECT ia.account,w.worker,'' FROM work_transitions t JOIN works w ON w.id=t.work_id
 JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room JOIN identities ia ON ia.id=t.author
 WHERE t.operation='work.accept' AND w.state='accepted' AND w.worker<>'' AND r.visibility='public' AND e.hidden=0 AND e.kind<>'simulation'
 AND `+publicAccountSQL("ia.account")+` AND `+publicAccountSQL("w.worker")+` ORDER BY t.rowid DESC LIMIT ?`, "work_accept"); err != nil {
			return nil, err
		}
	}
	if ok, err := tableExists(ctx, q, "link_witnesses"); err != nil {
		return nil, err
	} else if ok {
		if err := scan(`SELECT iw.account,ia.account,'' FROM link_witnesses l JOIN identities iw ON iw.id=l.witness JOIN identities ia ON ia.id=l.agent
 WHERE l.verdict='verified' AND l.superseded_at=0 AND `+publicAccountSQL("iw.account")+` AND `+publicAccountSQL("ia.account")+` ORDER BY l.seq DESC LIMIT ?`, "witness"); err != nil {
			return nil, err
		}
	}
	if ok, err := tableExists(ctx, q, "identity_links"); err != nil {
		return nil, err
	} else if ok {
		if err := scan(`SELECT i.account,o.account,'' FROM identity_links l JOIN identities i ON i.id=l.agent JOIN identities o ON o.public_key=l.value
 WHERE l.kind='ed25519' AND l.state='verified' AND i.successor='' AND o.account<>i.account AND `+publicAccountSQL("i.account")+` AND `+publicAccountSQL("o.account")+`
 ORDER BY l.rowid DESC LIMIT ?`, "key_link"); err != nil {
			return nil, err
		}
	}

	// Neighbours of the core, by standing then by ties to the core, up to the node cap.
	ties := map[string]int{}
	for k := range agg {
		for _, a := range []string{k.from, k.to} {
			if !inCore[a] {
				ties[a]++
			}
		}
	}
	var neighbours []string
	for a := range ties {
		neighbours = append(neighbours, a)
	}
	if err := readStandingParts(ctx, q, g.Run, neighbours, scores); err != nil {
		return nil, err
	}
	cents := func(a string) int64 {
		if p := scores[a]; p != nil {
			return p.Cents
		}
		return 0
	}
	sort.Slice(neighbours, func(i, j int) bool {
		a, b := neighbours[i], neighbours[j]
		if cents(a) != cents(b) {
			return cents(a) > cents(b)
		}
		if ties[a] != ties[b] {
			return ties[a] > ties[b]
		}
		return a < b
	})
	room := g.Limits.Nodes - len(coreIDs)
	if len(neighbours) > room {
		g.Truncated.Nodes = len(neighbours) - room
		neighbours = neighbours[:room]
	}
	shown := map[string]bool{}
	ids := append(append([]string{}, coreIDs...), neighbours...)
	for _, a := range ids {
		shown[a] = true
	}

	// Edges between shown identities, heaviest first, up to the edge cap.
	for _, e := range agg {
		if shown[e.From] && shown[e.To] {
			g.Edges = append(g.Edges, *e)
		}
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
	if len(g.Edges) > TrustGraphEdgesMax {
		g.Truncated.Edges = len(g.Edges) - TrustGraphEdgesMax
		g.Edges = g.Edges[:TrustGraphEdgesMax]
	}

	// Node details: handle, penalty, roots by kind.
	handles, err := readHandles(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	penalised := map[string]bool{}
	if ok, err := tableExists(ctx, q, "trust_penalties"); err != nil {
		return nil, err
	} else if ok {
		r, err := q.QueryContext(ctx, `SELECT DISTINCT p.account FROM trust_penalties p JOIN trust_evidence e ON e.id=p.evidence_id WHERE p.ends_at>? AND e.lifted_at=0`, now)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var a string
			if err = r.Scan(&a); err != nil {
				r.Close()
				return nil, err
			}
			penalised[a] = true
		}
		r.Close()
		if err = r.Err(); err != nil {
			return nil, err
		}
	}
	for _, a := range ids {
		n := TrustGraphNode{ID: a, Handle: handles[a], Band: 3, Roots: []string{}, Penalised: penalised[a], Core: inCore[a]}
		if p := scores[a]; p != nil {
			n.StandingCents, n.Band = p.Cents, p.Band
			n.Roots = rootKinds(p.Breakdown)
		}
		if n.Band < 1 || n.Band > 3 {
			n.Band = 3
		}
		n.Standing, n.FakeCost, n.BandName = trust.StandingScore(n.StandingCents), trust.FakeCostText(n.StandingCents), BandName(n.Band)
		g.Nodes = append(g.Nodes, n)
	}
	return g, nil
}

// rootKinds are the kinds of the roots that seed a standing, in a fixed
// order: never their values.
func rootKinds(breakdown []trust.StandingRoot) []string {
	have := map[string]bool{}
	for _, r := range breakdown {
		if r.SeedCents <= 0 {
			continue
		}
		kind := r.Root
		if i := strings.IndexByte(kind, ':'); i > 0 {
			kind = kind[:i]
		}
		have[kind] = true
	}
	out := []string{}
	for _, k := range []string{"domain", "wallet", "github", "pow", "spend", "arbiter"} {
		if have[k] {
			out = append(out, k)
		}
	}
	return out
}

// readStandingParts adds the run's standing parts of these accounts to into.
func readStandingParts(ctx context.Context, q allowance.Querier, run int64, accounts []string, into map[string]*trust.StandingPart) error {
	for start := 0; start < len(accounts); start += 200 {
		chunk := accounts[start:min(len(accounts), start+200)]
		args := []any{run}
		for _, a := range chunk {
			args = append(args, a)
		}
		r, err := q.QueryContext(ctx, "SELECT account,parts FROM trust_scores WHERE run_id=? AND account IN (?"+strings.Repeat(",?", len(chunk)-1)+")", args...)
		if err != nil {
			return err
		}
		for r.Next() {
			var a, parts string
			if err = r.Scan(&a, &parts); err != nil {
				r.Close()
				return err
			}
			var p trust.Parts
			if json.Unmarshal([]byte(parts), &p) == nil && p.Standing != nil {
				into[a] = p.Standing
			}
		}
		r.Close()
		if err = r.Err(); err != nil {
			return err
		}
	}
	return nil
}

// readHandles is each account's handle: the newest of its keys' handles.
func readHandles(ctx context.Context, q allowance.Querier, accounts []string) (map[string]string, error) {
	out := map[string]string{}
	for start := 0; start < len(accounts); start += 200 {
		chunk := accounts[start:min(len(accounts), start+200)]
		args := []any{}
		for _, a := range chunk {
			args = append(args, a)
		}
		r, err := q.QueryContext(ctx, "SELECT account,handle FROM identities WHERE handle<>'' AND account IN (?"+strings.Repeat(",?", len(chunk)-1)+") ORDER BY created_at", args...)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var a, h string
			if err = r.Scan(&a, &h); err != nil {
				r.Close()
				return nil, err
			}
			out[a] = h
		}
		r.Close()
		if err = r.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
