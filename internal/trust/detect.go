package trust

import (
	"sort"
	"strconv"
	"strings"
)

// gEdge is one final endorsement edge between node indices, saturated weight w.
type gEdge struct {
	src, dst int32
	w        int64
}

func evidenceID(kind string, D int64, members []string) string {
	return kind + "-" + sha256Hex([]byte(kind + "\n" + strconv.FormatInt(D, 10) + "\n" + strings.Join(members, ",")))[:24]
}

// detect runs the versioned detectors (§4.5). Evidence is mechanical and
// public only: ledger transfers, claims and the endorsement graph. Reports,
// hides and human judgement are never inputs.
//
// funnel: one account receives transfers from ≥ funnel_k distinct accounts in
// the last funnel_days, each of which spent ≤ funnel_spend_ppm of what it
// claimed on those days. Members: the recipient and those senders.
//
// ring: a strongly connected set of ≥ ring_min nodes in which every member
// takes ≥ ring_inside_ppm of its inbound edge weight from inside the set, and
// which shares a member with a funnel. Members: the set.
func detect(p Params, snap Snapshot, asOf, D int64, service map[string]bool, nodes []string, edges []gEdge) []EvidenceOut {
	d := p.Detectors
	out := []EvidenceOut{}
	from := asOf - d.FunnelDays*day
	claimed := map[string]int64{}
	spent := map[string]int64{}
	for _, r := range snap.Claims {
		if r.Day >= D-d.FunnelDays && r.Day < D {
			claimed[r.Account] += r.Claimed
			spent[r.Account] += r.Spent
		}
	}
	senders := map[string]map[string]bool{}
	for _, r := range snap.Transfers {
		if r.CreatedAt < from || r.CreatedAt >= asOf || r.From == r.To || r.From == "" || r.To == "" || service[r.From] || service[r.To] || r.Amount <= 0 {
			continue
		}
		if senders[r.To] == nil {
			senders[r.To] = map[string]bool{}
		}
		senders[r.To][r.From] = true
	}
	recipients := make([]string, 0, len(senders))
	for r := range senders {
		recipients = append(recipients, r)
	}
	sort.Strings(recipients)
	inFunnel := map[string]string{}
	for _, to := range recipients {
		var farm []string
		for s := range senders[to] {
			if claimed[s] > 0 && spent[s]*1e6 <= d.FunnelSpendPPM*claimed[s] {
				farm = append(farm, s)
			}
		}
		if int64(len(farm)) < d.FunnelK {
			continue
		}
		members := append(farm, to)
		sort.Strings(members)
		ev := EvidenceOut{ID: evidenceID("funnel", D, members), Kind: "funnel", Members: members, DetectorVersion: d.Version,
			Detail: string(Canonical(map[string]any{"recipient": to, "senders": len(farm), "window_days": d.FunnelDays}))}
		for _, m := range members {
			if inFunnel[m] == "" {
				inFunnel[m] = ev.ID
			}
		}
		out = append(out, ev)
	}
	if len(inFunnel) == 0 {
		return out
	}
	for _, scc := range stronglyConnected(len(nodes), edges) {
		if int64(len(scc)) < d.RingMin {
			continue
		}
		inside := map[int32]bool{}
		for _, v := range scc {
			inside[v] = true
		}
		in, total := map[int32]int64{}, map[int32]int64{}
		for _, e := range edges {
			if inside[e.dst] {
				total[e.dst] += e.w
				if inside[e.src] {
					in[e.dst] += e.w
				}
			}
		}
		ok, funnel := true, ""
		members := make([]string, 0, len(scc))
		for _, v := range scc {
			if total[v] == 0 || in[v]*1e6 < d.RingInsidePPM*total[v] {
				ok = false
			}
			members = append(members, nodes[v])
		}
		sort.Strings(members)
		for _, m := range members {
			if id := inFunnel[m]; id != "" && (funnel == "" || id < funnel) {
				funnel = id
			}
		}
		if !ok || funnel == "" {
			continue
		}
		out = append(out, EvidenceOut{ID: evidenceID("ring", D, members), Kind: "ring", Members: members, DetectorVersion: d.Version,
			Detail: string(Canonical(map[string]any{"size": len(members), "funnel": funnel}))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// stronglyConnected returns the strongly connected components (Tarjan).
func stronglyConnected(n int, edges []gEdge) [][]int32 {
	adj := make([][]int32, n)
	for _, e := range edges {
		adj[e.src] = append(adj[e.src], e.dst)
	}
	index := make([]int32, n)
	low := make([]int32, n)
	on := make([]bool, n)
	for i := range index {
		index[i] = -1
	}
	var stack []int32
	var out [][]int32
	var next int32
	var visit func(v int32)
	visit = func(v int32) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		on[v] = true
		for _, w := range adj[v] {
			if index[w] < 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []int32
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			out = append(out, comp)
		}
	}
	for v := int32(0); v < int32(n); v++ {
		if index[v] < 0 {
			visit(v)
		}
	}
	return out
}

type endorserFlow struct {
	agent string
	flow  int64
}

// sponsor derives sponsorships and this run's dividends (§4.5). A vouch with
// sponsor:true within the invitee's first window_days records a sponsorship
// (the earliest wins), within the sponsor's daily slots: round(slots_per_share
// × min(1, own_avg/U)), zero while its breaker is active. The invitee's
// independent inflow is the flow over edges from endorsers outside both roots
// with no edge from the sponsor. A new high within dividend_days earns
// dividend_ppm × (new high − previous high), at most daily_cap per sponsor.
func sponsor(p Params, snap Snapshot, asOf int64, service map[string]bool, firstSeen map[string]int64, reset map[string]bool,
	index map[string]int32, ownAvg []int64, rootOf func(string) string, pairSum map[pairKey]int64, flowTotal map[string]int64,
	inflow func(string) []endorserFlow) ([]SponsorshipOut, []DividendOut) {
	U := p.UnitPerShare
	sp := p.Sponsor
	prev := map[pairKey]int64{} // (invitee, sponsor) → the highest recorded high water
	for _, r := range snap.Sponsorships {
		k := pairKey{r.Invitee, r.SponsorOf}
		prev[k] = max(prev[k], r.HighWater)
	}
	vouches := []*Record{}
	for i := range snap.Endorsements {
		if r := &snap.Endorsements[i]; r.Kind == "vouch" && r.Sponsor && r.Value == 1 && r.CreatedAt < asOf && r.Voter != r.Target && r.Voter != "" && r.Target != "" && !service[r.Voter] && !service[r.Target] {
			vouches = append(vouches, r)
		}
	}
	sort.Slice(vouches, func(i, j int) bool { return endorsementLess(vouches[i], vouches[j]) })
	used := map[string]int64{}
	chosen := map[string]*Record{}
	for _, r := range vouches {
		fs := firstSeen[r.Target]
		if fs == 0 || r.CreatedAt > fs+sp.WindowDays*day || rootOf(r.Voter) == rootOf(r.Target) {
			continue
		}
		if _, ok := chosen[r.Target]; ok {
			continue
		}
		var own int64
		if i, ok := index[r.Voter]; ok {
			own = ownAvg[i]
		}
		slots := (2*sp.SlotsPerShare*min(U, own) + U) / (2 * U)
		if reset[r.Voter] {
			slots = 0
		}
		key := r.Voter + "\n" + strconv.FormatInt(dayOf(r.CreatedAt), 10)
		if used[key] >= slots {
			continue
		}
		used[key]++
		chosen[r.Target] = r
	}
	invitees := make([]string, 0, len(chosen))
	for a := range chosen {
		invitees = append(invitees, a)
	}
	sort.Strings(invitees)
	ships := []SponsorshipOut{}
	divs := []DividendOut{}
	paid := map[string]int64{}
	for _, x := range invitees {
		r := chosen[x]
		s := r.Voter
		hw := prev[pairKey{x, s}]
		var indep int64
		for _, e := range inflow(x) {
			if e.agent == s || rootOf(e.agent) == rootOf(s) || rootOf(e.agent) == rootOf(x) || pairSum[pairKey{s, e.agent}] > 0 {
				continue
			}
			indep += e.flow
		}
		indep = min(indep, flowTotal[x])
		ends := r.CreatedAt + sp.DividendDays*day
		if asOf < ends && indep > hw {
			units := min(sp.DividendPPM*(indep-hw)/1e6, sp.DailyCap-paid[s])
			if units > 0 {
				paid[s] += units
				divs = append(divs, DividendOut{Sponsor: s, Invitee: x, Units: units})
			}
		}
		ships = append(ships, SponsorshipOut{Invitee: x, Sponsor: s, RecordSeq: r.Seq, CreatedAt: r.CreatedAt, EndsAt: ends, HighWater: max(hw, indep)})
	}
	return ships, divs
}
