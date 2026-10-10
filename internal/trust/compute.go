package trust

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Output is one run's published result. Its canonical JSON (Canonical) is
// what a verifier must reproduce byte for byte from the same Snapshot.
type Output struct {
	Schema        int64            `json:"schema"`
	AsOf          int64            `json:"as_of"`
	ParamsVersion int64            `json:"params_version"`
	ParamsSHA256  string           `json:"params_sha256"`
	Inputs        InputsSummary    `json:"inputs"`
	Nodes         int64            `json:"nodes"`
	Edges         int64            `json:"edges"`
	PoolUnits     int64            `json:"pool_units"`
	Active        int64            `json:"active_accounts"`
	SeedsA        []string         `json:"seeds_a"`
	SeedsB        []string         `json:"seeds_b"`
	SeedsBUsed    bool             `json:"seeds_b_used"`
	CaptureBound  CaptureBound     `json:"capture_bound"`
	Scores        []ScoreOut       `json:"scores"`
	Evidence      []EvidenceOut    `json:"evidence"`
	Penalties     []PenaltyOut     `json:"penalties"`
	Sponsorships  []SponsorshipOut `json:"sponsorships"`
	Dividends     []DividendOut    `json:"dividends"`
	// Standing is RFC0015's standing summary: present from parameter
	// version 2 (standing.go), absent before, so earlier runs recompute
	// byte for byte.
	Standing *StandingSummary `json:"standing,omitempty"`
	// Work is the number of edge examinations; a run statistic, not output.
	Work int64 `json:"-"`
}

// InputsSummary says what the run read.
type InputsSummary struct {
	EventsSeq       int64 `json:"events_seq"`
	EndorsementsSeq int64 `json:"endorsements_seq"`
	LedgerSeq       int64 `json:"ledger_seq"`
	PriorRuns       int64 `json:"prior_runs"`
	Accounts        int64 `json:"accounts"`
	Posts           int64 `json:"posts"`
	Endorsements    int64 `json:"endorsements"`
	Proofs          int64 `json:"proofs"`
	Transfers       int64 `json:"transfers"`
}

// CaptureBound is the published sybil bound of the run (§4.3 step 4).
type CaptureBound struct {
	UnitPerShare    int64  `json:"unit_per_share"`
	EdgeCapUnits    int64  `json:"edge_cap_units"`
	LambdaPPM       int64  `json:"lambda_ppm"`
	MaxTransitUnits int64  `json:"max_transit_units"`
	PoolUnits       int64  `json:"pool_units"`
	Statement       string `json:"statement"`
}

// ScoreOut is one trust_scores row.
type ScoreOut struct {
	Account         string `json:"account"`
	Root            string `json:"root"`
	ProofCollateral int64  `json:"proof_collateral"`
	FlowA           int64  `json:"flow_a"`
	FlowB           *int64 `json:"flow_b"`
	Flow            int64  `json:"flow"`
	Collateral      int64  `json:"collateral"`
	Tier            int64  `json:"tier"`
	WeightPPM       int64  `json:"weight_ppm"`
	Parts           Parts  `json:"parts"`
}

// Parts explain a score: never a bare number.
type Parts struct {
	Proofs         []ProofPart    `json:"proofs"`
	Endorsers      []EndorserPart `json:"endorsers"`
	EndorsersTotal int64          `json:"endorsers_total"`
	DownVotes      int64          `json:"down_votes"`
	PenaltyPPM     int64          `json:"penalty_ppm"`
	OwnAvg         int64          `json:"own_avg"`
	Transit        int64          `json:"transit"`
	Seed           string         `json:"seed"` // "", "a", "b" or "ab"
	Reset          bool           `json:"reset"`
	// Standing is the account's RFC0015 standing (parameter version 2).
	Standing *StandingPart `json:"standing,omitempty"`
}

// ProofPart is one priced proof.
type ProofPart struct {
	Kind         string `json:"kind"`
	Value        string `json:"value"`
	Root         string `json:"root"`
	State        string `json:"state"`
	Forge        int64  `json:"forge"`
	Rent         int64  `json:"rent"`
	AgeDays      int64  `json:"age_days"`
	Curve        string `json:"curve"`
	WeightPPM    int64  `json:"weight_ppm"`
	Contribution int64  `json:"contribution"`
	SaturatedBy  string `json:"saturated_by"`
	Note         string `json:"note"`
}

// EndorserPart is one inbound edge.
type EndorserPart struct {
	Agent     string   `json:"agent"`
	Kinds     []string `json:"kinds"`
	WeightPPM int64    `json:"weight_ppm"`
	Flow      int64    `json:"flow"`
}

// EvidenceOut is one detector finding.
type EvidenceOut struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Members         []string `json:"members"`
	Detail          string   `json:"detail"`
	DetectorVersion int64    `json:"detector_version"`
}

// PenaltyOut is one liability penalty.
type PenaltyOut struct {
	Account     string `json:"account"`
	Evidence    string `json:"evidence"`
	FractionPPM int64  `json:"fraction_ppm"`
	StartsAt    int64  `json:"starts_at"`
	EndsAt      int64  `json:"ends_at"`
}

// SponsorshipOut is one sponsorship and its independent inflow high.
type SponsorshipOut struct {
	Invitee   string `json:"invitee"`
	Sponsor   string `json:"sponsor"`
	RecordSeq int64  `json:"record_seq"`
	CreatedAt int64  `json:"created_at"`
	EndsAt    int64  `json:"ends_at"`
	HighWater int64  `json:"high_water"`
}

// DividendOut is one sponsor dividend of this run.
type DividendOut struct {
	Sponsor string `json:"sponsor"`
	Invitee string `json:"invitee"`
	Units   int64  `json:"units"`
}

// Canonical is the output's canonical JSON.
func (o Output) Canonical() []byte { return Canonical(o) }

const day = 86400

func dayOf(t int64) int64 {
	if t < 0 {
		return -((-t + day - 1) / day)
	}
	return t / day
}

// uf is a union-find over strings.
type uf map[string]string

func (u uf) find(x string) string {
	for {
		p, ok := u[x]
		if !ok || p == x {
			return x
		}
		if gp := u[p]; gp != "" && gp != p {
			u[x] = gp
		}
		x = p
	}
}

func (u uf) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u[ra] = ra
	u[rb] = ra
}

// DomainRoot is domainRoot over a suffix list (Params.DomainSuffixes): the
// root Design 0 gives a proven account, so its tier-2 root folds to the
// registrable domain exactly as trust does.
func DomainRoot(domain string, suffixes []string) string {
	set := make(map[string]bool, len(suffixes))
	for _, s := range suffixes {
		set[s] = true
	}
	return domainRoot(domain, set)
}

// domainRoot is "domain:" + the registrable domain: one label under the
// longest listed multi-label suffix, else the last two labels.
func domainRoot(domain string, suffixes map[string]bool) string {
	labels := strings.Split(strings.ToLower(strings.TrimSuffix(domain, ".")), ".")
	keep := 2
	for i := 1; i < len(labels)-1; i++ {
		if suffixes[strings.Join(labels[i:], ".")] {
			keep = len(labels) - i + 1
			break
		}
	}
	if keep > len(labels) {
		keep = len(labels)
	}
	return "domain:" + strings.Join(labels[len(labels)-keep:], ".")
}

type pairKey struct{ src, dst string }

// Compute runs the trust module on one snapshot. It is a pure function of
// the snapshot: record order does not matter, and nothing reads the clock
// except to stop at max_seconds.
func Compute(ctx context.Context, snap Snapshot) (Output, error) {
	p := snap.Params
	if err := p.Validate(); err != nil {
		return Output{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.MaxSeconds)*time.Second)
	defer cancel()
	asOf := snap.Meta.AsOf
	D := dayOf(asOf)
	ws := asOf - p.WindowDays*day
	U := p.UnitPerShare
	cv := curves{}

	service := map[string]bool{}
	for _, a := range p.ServiceAccounts {
		service[a] = true
	}
	firstSeen := map[string]int64{}
	for _, r := range snap.Accounts {
		if r.FirstSeen > 0 && (firstSeen[r.Account] == 0 || r.FirstSeen < firstSeen[r.Account]) {
			firstSeen[r.Account] = r.FirstSeen
		}
	}

	// Posts in the window, and activity.
	posts := make([]Record, 0, len(snap.Posts))
	for _, r := range snap.Posts {
		if r.CreatedAt < asOf && r.CreatedAt >= ws && r.Account != "" && !service[r.Account] {
			posts = append(posts, r)
		}
	}
	sort.Slice(posts, func(i, j int) bool {
		x, y := &posts[i], &posts[j]
		if x.CreatedAt != y.CreatedAt {
			return x.CreatedAt < y.CreatedAt
		}
		if x.ID != y.ID {
			return x.ID < y.ID
		}
		if x.Account != y.Account {
			return x.Account < y.Account
		}
		if x.ReplyTo != y.ReplyTo {
			return x.ReplyTo < y.ReplyTo
		}
		return x.ReplyToAccount < y.ReplyToAccount
	})
	postByID := map[string]Record{}
	activity := map[string]map[int64]bool{}
	lastSeen := map[string]int64{}
	active := func(a string, t int64) {
		if activity[a] == nil {
			activity[a] = map[int64]bool{}
		}
		activity[a][dayOf(t)] = true
	}
	seen := func(a string, t int64) {
		if t > lastSeen[a] {
			lastSeen[a] = t
		}
	}
	for _, r := range posts {
		postByID[r.ID] = r
		active(r.Account, r.CreatedAt)
		seen(r.Account, r.CreatedAt)
	}

	// Endorsements: the latest record per vote (voter, message) and per vouch
	// (voter, target) decides.
	ends := make([]*Record, 0, len(snap.Endorsements))
	for i := range snap.Endorsements {
		if r := &snap.Endorsements[i]; r.CreatedAt < asOf && r.Voter != "" && !service[r.Voter] {
			ends = append(ends, r)
		}
	}
	sort.Slice(ends, func(i, j int) bool { return endorsementLess(ends[i], ends[j]) })
	latestVote := map[pairKey]*Record{}
	latestVouch := map[pairKey]*Record{}
	for _, r := range ends {
		switch r.Kind {
		case "vote", "legacy_vote", "unsigned":
			latestVote[pairKey{r.Voter, r.MessageID}] = r
		case "vouch":
			latestVouch[pairKey{r.Voter, r.Target}] = r
		}
		if r.CreatedAt >= ws {
			active(r.Voter, r.CreatedAt)
			seen(r.Voter, r.CreatedAt)
			if r.Target != "" && !service[r.Target] {
				seen(r.Target, r.CreatedAt)
			}
		}
	}
	current := make([]*Record, 0, len(latestVote)+len(latestVouch))
	for _, r := range latestVote {
		current = append(current, r)
	}
	for _, r := range latestVouch {
		current = append(current, r)
	}
	sort.Slice(current, func(i, j int) bool { return endorsementLess(current[i], current[j]) })
	downVotes := map[string]int64{}
	for _, r := range current {
		if r.Kind == "vote" && r.Value == -1 && r.CreatedAt >= ws && r.Target != "" {
			downVotes[r.Target]++
		}
	}

	// Nodes: seeds first, then the most recently active, up to n_max.
	candidates := make([]string, 0, len(lastSeen))
	for a := range lastSeen {
		if !service[a] {
			candidates = append(candidates, a)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if lastSeen[candidates[i]] != lastSeen[candidates[j]] {
			return lastSeen[candidates[i]] > lastSeen[candidates[j]]
		}
		return candidates[i] < candidates[j]
	})
	isNode := map[string]bool{}
	for _, s := range p.Seeds {
		if !service[s] {
			isNode[s] = true
		}
	}
	for _, a := range candidates {
		if int64(len(isNode)) >= p.NMax {
			break
		}
		isNode[a] = true
	}
	nodes := make([]string, 0, len(isNode))
	for a := range isNode {
		nodes = append(nodes, a)
	}
	sort.Strings(nodes)
	index := make(map[string]int32, len(nodes))
	for i, a := range nodes {
		index[a] = int32(i)
	}

	// Roots: verified fresh domains and attached ed25519 keys join accounts.
	suffixes := map[string]bool{}
	for _, s := range p.DomainSuffixes {
		suffixes[s] = true
	}
	proofs := sortRecords(snap.Proofs)
	fresh := asOf - p.ProofFreshDays*day
	counts := func(r Record) bool {
		switch r.Kind {
		case "domain":
			return r.State == "verified" && r.CheckedAt >= fresh && r.CheckedAt <= asOf && r.CreatedAt <= asOf
		case "ed25519":
			return (r.State == "proof_attached" || r.State == "verified") && r.CreatedAt <= asOf
		}
		return r.State == "verified" && r.CreatedAt <= asOf
	}
	roots := uf{}
	for _, r := range proofs {
		if service[r.Account] || !counts(r) {
			continue
		}
		switch r.Kind {
		case "domain":
			roots.union(r.Account, domainRoot(r.LinkValue, suffixes))
		case "ed25519":
			if r.LinkAccount != "" && !service[r.LinkAccount] {
				roots.union(r.Account, r.LinkAccount)
			}
		}
	}
	// A root's label is its smallest domain root, else its smallest account.
	label := map[string]string{}
	for x := range roots {
		rep := roots.find(x)
		cur, ok := label[rep]
		isDomain := strings.HasPrefix(x, "domain:")
		curDomain := strings.HasPrefix(cur, "domain:")
		if !ok || (isDomain && !curDomain) || (isDomain == curDomain && x < cur) {
			label[rep] = x
		}
	}
	rootOf := func(a string) string {
		if _, ok := roots[a]; !ok {
			return a
		}
		return label[roots.find(a)]
	}

	// Priors and standing.
	flowSum := map[string]int64{}
	standing := map[string]bool{}
	for _, r := range snap.Priors {
		flowSum[r.Account] += r.FlowSum
		if r.Standing {
			standing[r.Account] = true
		}
	}

	// Breakers, pause-new-keys and active penalties.
	reset := map[string]bool{}
	for _, r := range snap.Breakers {
		if r.StartedAt <= asOf && r.TrustUntil > asOf {
			reset[r.Account] = true
		}
	}
	if since := snap.Meta.PauseNewKeysSince; since > 0 {
		for a, t := range firstSeen {
			if t > since {
				reset[a] = true
			}
		}
	}
	penalty := map[string]int64{}
	for _, r := range snap.Penalties {
		if r.EndsAt > asOf && r.FractionPPM > penalty[r.Account] {
			penalty[r.Account] = min(r.FractionPPM, 1e6)
		}
	}

	// Edge contributions.
	type contrib struct {
		src, dst, kind string
		at, base       int64
	}
	var contribs []contrib
	for _, r := range current {
		if r.CreatedAt < ws || r.Target == "" || r.Value != 1 {
			continue
		}
		switch r.Kind {
		case "vote", "legacy_vote", "vouch":
			contribs = append(contribs, contrib{r.Voter, r.Target, r.Kind, r.CreatedAt, p.Edges[r.Kind].BasePPM})
		}
	}
	replied := map[string]bool{}
	for _, r := range posts {
		if r.ReplyToAccount == "" || r.ReplyToAccount == r.Account {
			continue
		}
		key := r.Account + "\n" + r.ReplyToAccount + "\n" + strconv.FormatInt(dayOf(r.CreatedAt), 10)
		if replied[key] {
			continue
		}
		replied[key] = true
		contribs = append(contribs, contrib{r.Account, r.ReplyToAccount, "reply", r.CreatedAt, p.Edges["reply"].BasePPM})
	}
	pairSum := map[pairKey]int64{}
	pairRecent := map[pairKey]int64{}
	pairKinds := map[pairKey]map[string]bool{}
	recent := asOf - p.Liability.EdgeDays*day
	for _, c := range contribs {
		if c.src == c.dst || service[c.src] || service[c.dst] || !isNode[c.src] || !isNode[c.dst] {
			continue
		}
		if rootOf(c.src) == rootOf(c.dst) || reset[c.src] || penalty[c.src] >= 1e6 {
			continue
		}
		e := p.Edges[c.kind]
		kind := c.kind
		if kind == "legacy_vote" {
			kind = "vote"
		}
		v := c.base * cv.decay(e.DayFactorPPM, D-dayOf(c.at)) / 1e6
		if v <= 0 {
			continue
		}
		k := pairKey{c.src, c.dst}
		pairSum[k] += v
		if c.at >= recent {
			pairRecent[k] += v
		}
		if pairKinds[k] == nil {
			pairKinds[k] = map[string]bool{}
		}
		pairKinds[k][kind] = true
	}
	saturate := func(s int64) int64 { return 1e6 * s / (1e6 + s) }
	bySrc := map[int32][]gEdge{}
	for k, s := range pairSum {
		if w := saturate(s); w > 0 {
			si := index[k.src]
			bySrc[si] = append(bySrc[si], gEdge{si, index[k.dst], w})
		}
	}
	var edges []gEdge
	for _, list := range bySrc {
		sort.Slice(list, func(i, j int) bool {
			if list[i].w != list[j].w {
				return list[i].w > list[j].w
			}
			return list[i].dst < list[j].dst
		})
		if int64(len(list)) > p.KOut {
			list = list[:p.KOut]
		}
		edges = append(edges, list...)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].src != edges[j].src {
			return edges[i].src < edges[j].src
		}
		return edges[i].dst < edges[j].dst
	})

	// Own average and transit.
	activeDays := func(a string, from, to int64) int64 {
		var n int64
		for d := range activity[a] {
			if d >= from && d < to {
				n++
			}
		}
		return n
	}
	ownAvg := make([]int64, len(nodes))
	transit := make([]int64, len(nodes))
	for i, a := range nodes {
		if snap.Meta.PriorRuns <= 0 {
			continue
		}
		span := p.ActivityDays
		if fs := firstSeen[a]; fs > 0 {
			span = max(1, min(p.ActivityDays, D-dayOf(fs)))
		}
		share := min(1e6, activeDays(a, D-p.ActivityDays, D)*1e6/span)
		avg := min(U, flowSum[a]/snap.Meta.PriorRuns)
		ownAvg[i] = avg * share / 1e6
		if !reset[a] {
			transit[i] = p.LambdaPPM * ownAvg[i] / 1e6 * (1e6 - penalty[a]) / 1e6
		}
	}

	// Proof collateral (history needs roots and standing).
	type proofSet struct {
		parts []ProofPart
		total int64
	}
	accountProofs := map[string]*proofSet{}
	addPart := func(a string, part ProofPart) {
		ps := accountProofs[a]
		if ps == nil {
			ps = &proofSet{}
			accountProofs[a] = ps
		}
		ps.parts = append(ps.parts, part)
	}
	for _, r := range proofs {
		if service[r.Account] || r.CreatedAt > asOf {
			continue
		}
		price := p.Proofs[r.Kind]
		part := ProofPart{Kind: r.Kind, Value: r.LinkValue, State: r.State, Forge: price.Forge, Rent: price.Rent, Curve: price.Curve, AgeDays: max(0, D-dayOf(r.CreatedAt))}
		switch r.Kind {
		case "domain":
			part.Root = domainRoot(r.LinkValue, suffixes)
		case "ed25519":
			part.Root = "key:" + r.LinkAccount
		default:
			part.Root = r.Kind + ":" + r.LinkValue
		}
		switch {
		case !counts(r):
			part.Note = "not counted in this state"
		case price.Curve == "ramp":
			// The verified age is unknown: the link's age bounds it, and an
			// unknown age earns at most half the ramp (never full).
			part.WeightPPM = min(500000, cv.ramp(price.DayFactorPPM, part.AgeDays))
			part.Note = "verified age unknown: at most half weight, bounded by the link's age"
		default:
			part.WeightPPM = 1e6
		}
		part.Contribution = min(price.Forge, price.Rent) * part.WeightPPM / 1e6
		addPart(r.Account, part)
	}
	// History: days with a post that drew a reply or up vote from another
	// root with standing in the previous run.
	qualified := map[string]map[int64]bool{}
	qualify := func(post Record, by string) {
		if by == "" || by == post.Account || service[by] || !standing[by] || rootOf(by) == rootOf(post.Account) {
			return
		}
		if qualified[post.Account] == nil {
			qualified[post.Account] = map[int64]bool{}
		}
		qualified[post.Account][dayOf(post.CreatedAt)] = true
	}
	for _, r := range posts {
		if parent, ok := postByID[r.ReplyTo]; ok && r.ReplyTo != "" {
			qualify(parent, r.Account)
		}
	}
	for _, r := range current {
		if r.Kind == "vote" && r.Value == 1 {
			if post, ok := postByID[r.MessageID]; ok {
				qualify(post, r.Voter)
			}
		}
	}
	for a, days := range qualified {
		first := int64(-1)
		for d := range days {
			if first < 0 || d < first {
				first = d
			}
		}
		h := p.History
		n := min(int64(len(days)), h.DaysCap)
		part := ProofPart{Kind: "history", Value: strconv.FormatInt(int64(len(days)), 10) + " days", Root: "history:" + a, State: "computed",
			Forge: n * h.DayPrice, Rent: n * h.DayPrice, Curve: "ramp", AgeDays: D - first}
		part.WeightPPM = cv.ramp(h.DayFactorPPM, part.AgeDays)
		part.Contribution = part.Forge * part.WeightPPM / 1e6
		part.Note = "lagged one run"
		addPart(a, part)
	}
	// Saturate within a root, sum across roots.
	distinctRoots := map[string]int64{}
	nonDomainMax := map[string]int64{}
	for a, ps := range accountProofs {
		sort.Slice(ps.parts, func(i, j int) bool {
			x, y := ps.parts[i], ps.parts[j]
			if x.Root != y.Root {
				return x.Root < y.Root
			}
			if x.Contribution != y.Contribution {
				return x.Contribution > y.Contribution
			}
			if x.Kind != y.Kind {
				return x.Kind < y.Kind
			}
			return x.Value < y.Value
		})
		for i := range ps.parts {
			part := &ps.parts[i]
			if i > 0 && ps.parts[i-1].Root == part.Root {
				best := ps.parts[i-1]
				if best.SaturatedBy != "" {
					part.SaturatedBy = best.SaturatedBy
				} else {
					part.SaturatedBy = best.Kind + ":" + best.Value
				}
				continue
			}
			ps.total += part.Contribution
			if part.Contribution > 0 {
				distinctRoots[a]++
				if !strings.HasPrefix(part.Root, "domain:") {
					nonDomainMax[a] = max(nonDomainMax[a], part.Contribution)
				}
			}
		}
	}

	// Seed sets.
	var seedsA []int32
	var seedsAList []string
	for _, s := range p.Seeds {
		if i, ok := index[s]; ok {
			seedsA = append(seedsA, i)
			seedsAList = append(seedsAList, s)
		}
	}
	sort.Slice(seedsA, func(i, j int) bool { return seedsA[i] < seedsA[j] })
	sort.Strings(seedsAList)
	var seedsB []int32
	seedsBList := []string{}
	for i, a := range nodes {
		fs := firstSeen[a]
		anchored := distinctRoots[a] >= p.SeedsB.MinRoots || nonDomainMax[a] >= max(1, p.SeedsB.ThetaAnchor)
		if anchored && fs > 0 && fs <= asOf-p.SeedsB.MinAgeDays*day && activeDays(a, D-p.SeedsB.OfDays, D) >= p.SeedsB.ActiveDays && penalty[a] == 0 {
			seedsB = append(seedsB, int32(i))
			seedsBList = append(seedsBList, a)
		}
	}
	useB := int64(len(seedsB)) >= p.SeedsB.MinMembers

	var H int64
	for _, a := range nodes {
		if activeDays(a, D-p.ActiveDays, D) > 0 {
			H++
		}
	}
	pool := U * max(H, 1)

	// Flow graph.
	g := &flowGraph{n: len(nodes), transit: transit, hub: make([]int32, len(nodes)), hubCap: p.RootCapShares * U, nodeCap: U, steps: p.FillSteps}
	groups := map[string][]int32{}
	for i, a := range nodes {
		groups[rootOf(a)] = append(groups[rootOf(a)], int32(i))
		g.hub[i] = -1
	}
	var hubLabels []string
	for l, members := range groups {
		if len(members) >= 2 {
			hubLabels = append(hubLabels, l)
		}
	}
	sort.Strings(hubLabels)
	for j, l := range hubLabels {
		for _, i := range groups[l] {
			g.hub[i] = int32(j)
		}
	}
	g.hubs = len(hubLabels)
	for _, e := range edges {
		g.src = append(g.src, e.src)
		g.dst = append(g.dst, e.dst)
		g.cap = append(g.cap, p.EdgeCapPPM*U*e.w/1e12)
	}
	fa, work, err := runFlow(ctx, g, seedsA, pool, p.MaxWork, 0)
	if err != nil {
		return Output{Work: work}, err
	}
	fb := fa
	if useB {
		if fb, work, err = runFlow(ctx, g, seedsB, pool, p.MaxWork, work); err != nil {
			return Output{Work: work}, err
		}
	}
	flowOf := func(i int32) int64 {
		if useB {
			return min(fa.node[i], fb.node[i])
		}
		return fa.node[i]
	}
	edgeFlow := func(k int) int64 {
		if useB {
			return min(fa.edge[k], fb.edge[k])
		}
		return fa.edge[k]
	}
	inEdges := map[int32][]int{}
	for k, e := range edges {
		inEdges[e.dst] = append(inEdges[e.dst], k)
	}

	// Detectors, liability.
	evidence := detect(p, snap, asOf, D, service, nodes, edges)
	penaltiesOut := []PenaltyOut{}
	for _, ev := range evidence {
		members := map[string]bool{}
		for _, m := range ev.Members {
			members[m] = true
			penaltiesOut = append(penaltiesOut, PenaltyOut{Account: m, Evidence: ev.ID, FractionPPM: 1e6, StartsAt: asOf, EndsAt: asOf + p.Liability.PenaltyDays*day})
		}
		into := map[string]int64{}
		for k, s := range pairRecent {
			if members[k.dst] && !members[k.src] && !service[k.src] {
				into[k.src] += s
			}
		}
		for u, s := range into {
			pen := min(p.Liability.PhiMaxPPM, p.Liability.PhiPPM*saturate(s)/1e6)
			if pen > 0 {
				penaltiesOut = append(penaltiesOut, PenaltyOut{Account: u, Evidence: ev.ID, FractionPPM: pen, StartsAt: asOf, EndsAt: asOf + p.Liability.PenaltyDays*day})
			}
		}
	}
	sort.Slice(penaltiesOut, func(i, j int) bool {
		if penaltiesOut[i].Account != penaltiesOut[j].Account {
			return penaltiesOut[i].Account < penaltiesOut[j].Account
		}
		return penaltiesOut[i].Evidence < penaltiesOut[j].Evidence
	})
	effectivePenalty := map[string]int64{}
	for a, v := range penalty {
		effectivePenalty[a] = v
	}
	for _, pen := range penaltiesOut {
		effectivePenalty[pen.Account] = max(effectivePenalty[pen.Account], pen.FractionPPM)
	}

	// Scores: every node, and every account with a proof.
	scored := map[string]bool{}
	for _, a := range nodes {
		scored[a] = true
	}
	for a := range accountProofs {
		scored[a] = true
	}
	accounts := make([]string, 0, len(scored))
	for a := range scored {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	seedA := map[string]bool{}
	for _, s := range seedsAList {
		seedA[s] = true
	}
	seedB := map[string]bool{}
	if useB {
		for _, s := range seedsBList {
			seedB[s] = true
		}
	}
	flowTotal := map[string]int64{}
	scores := make([]ScoreOut, 0, len(accounts))
	for _, a := range accounts {
		sc := ScoreOut{Account: a, Root: rootOf(a), Parts: Parts{Proofs: []ProofPart{}, Endorsers: []EndorserPart{}, DownVotes: downVotes[a], PenaltyPPM: effectivePenalty[a], Reset: reset[a]}}
		if ps := accountProofs[a]; ps != nil {
			sc.ProofCollateral = ps.total
			sc.Parts.Proofs = ps.parts
		}
		if i, ok := index[a]; ok {
			sc.FlowA = fa.node[i]
			if useB {
				b := fb.node[i]
				sc.FlowB = &b
			}
			sc.Flow = flowOf(i)
			sc.Parts.OwnAvg, sc.Parts.Transit = ownAvg[i], transit[i]
			for _, k := range inEdges[i] {
				e := edges[k]
				src := nodes[e.src]
				var kinds []string
				for kind := range pairKinds[pairKey{src, a}] {
					kinds = append(kinds, kind)
				}
				sort.Strings(kinds)
				sc.Parts.Endorsers = append(sc.Parts.Endorsers, EndorserPart{Agent: src, Kinds: kinds, WeightPPM: e.w, Flow: edgeFlow(k)})
			}
			sort.Slice(sc.Parts.Endorsers, func(x, y int) bool {
				ex, ey := sc.Parts.Endorsers[x], sc.Parts.Endorsers[y]
				if ex.Flow != ey.Flow {
					return ex.Flow > ey.Flow
				}
				if ex.WeightPPM != ey.WeightPPM {
					return ex.WeightPPM > ey.WeightPPM
				}
				return ex.Agent < ey.Agent
			})
			sc.Parts.EndorsersTotal = int64(len(sc.Parts.Endorsers))
			if len(sc.Parts.Endorsers) > 20 {
				sc.Parts.Endorsers = sc.Parts.Endorsers[:20]
			}
		}
		switch {
		case seedA[a] && seedB[a]:
			sc.Parts.Seed = "ab"
		case seedA[a]:
			sc.Parts.Seed = "a"
		case seedB[a]:
			sc.Parts.Seed = "b"
		}
		flowTotal[a] = sc.Flow
		sc.Collateral = sc.ProofCollateral + sc.Flow*p.UnitPrice
		switch {
		case sc.Flow > 0 && sc.Flow >= p.ThetaTrusted:
			sc.Tier = 1
		case sc.ProofCollateral > 0 && sc.ProofCollateral >= p.ThetaProven:
			sc.Tier = 2
		default:
			sc.Tier = 3
		}
		sc.WeightPPM = (1e6 + min(p.WeightCapPPM, sc.Collateral*p.WeightPerUnit)) * (1e6 - effectivePenalty[a]) / 1e6
		scores = append(scores, sc)
	}

	var standingSummary *StandingSummary
	if p.Standing != nil {
		proofParts := make(map[string][]ProofPart, len(accountProofs))
		for a, ps := range accountProofs {
			proofParts[a] = ps.parts
		}
		tiers := make(map[string]int64, len(scores))
		collateral := make(map[string]int64, len(scores))
		for _, sc := range scores {
			tiers[sc.Account], collateral[sc.Account] = sc.Tier, sc.Collateral
		}
		res := computeStanding(standingInput{p: p, snap: snap, asOf: asOf, D: D, service: service, rootOf: rootOf, reset: reset, penalty: effectivePenalty,
			posts: posts, current: current, proofs: proofParts, scored: accounts, tier: tiers, collateral: collateral})
		for i := range scores {
			scores[i].Parts.Standing = res.parts[scores[i].Account]
		}
		// An account whose only evidence is a standing input (credit spent or
		// held) is scored too, at tier 3 and one share.
		for _, a := range res.extra {
			scores = append(scores, ScoreOut{Account: a, Root: rootOf(a), Tier: 3, WeightPPM: 1e6 * (1e6 - effectivePenalty[a]) / 1e6,
				Parts: Parts{Proofs: []ProofPart{}, Endorsers: []EndorserPart{}, DownVotes: downVotes[a], PenaltyPPM: effectivePenalty[a], Reset: reset[a], Standing: res.parts[a]}})
		}
		sort.Slice(scores, func(i, j int) bool { return scores[i].Account < scores[j].Account })
		standingSummary = res.summary
	}

	sponsorships, dividends := sponsor(p, snap, asOf, service, firstSeen, reset, index, ownAvg, rootOf, pairSum, flowTotal, func(invitee string) []endorserFlow {
		i, ok := index[invitee]
		if !ok {
			return nil
		}
		var out []endorserFlow
		for _, k := range inEdges[i] {
			out = append(out, endorserFlow{nodes[edges[k].src], edgeFlow(k)})
		}
		return out
	})

	var maxTransit int64
	for i := range nodes {
		if !seedA[nodes[i]] && !seedB[nodes[i]] {
			maxTransit = max(maxTransit, transit[i])
		}
	}
	summary := InputsSummary{EventsSeq: snap.Meta.EventsSeq, EndorsementsSeq: snap.Meta.EndorsementsSeq, LedgerSeq: snap.Meta.LedgerSeq, PriorRuns: snap.Meta.PriorRuns,
		Accounts: int64(len(snap.Accounts)), Posts: int64(len(posts)), Endorsements: int64(len(ends)), Proofs: int64(len(snap.Proofs)), Transfers: int64(len(snap.Transfers))}
	if seedsAList == nil {
		seedsAList = []string{}
	}
	return Output{
		Schema: 1, AsOf: asOf, ParamsVersion: p.Version, ParamsSHA256: sha256Hex(p.Body()), Inputs: summary,
		Nodes: int64(len(nodes)), Edges: int64(len(edges)), PoolUnits: pool, Active: H,
		SeedsA: seedsAList, SeedsB: seedsBList, SeedsBUsed: useB,
		CaptureBound: CaptureBound{UnitPerShare: U, EdgeCapUnits: p.EdgeCapPPM * U / 1e6, LambdaPPM: p.LambdaPPM, MaxTransitUnits: maxTransit, PoolUnits: pool,
			Statement: "Whatever the number of sybils, a region behind k attack edges receives at most k x edge_cap_units flow units, and at most max_transit_units through any one non-seed endorser."},
		Scores: scores, Evidence: evidence, Penalties: penaltiesOut, Sponsorships: sponsorships, Dividends: dividends, Standing: standingSummary, Work: work,
	}, nil
}

// endorsementLess orders endorsement records by seq, then by content, so
// that even duplicate sequence numbers sort the same way everywhere.
func endorsementLess(a, b *Record) bool {
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Voter != b.Voter {
		return a.Voter < b.Voter
	}
	if a.Target != b.Target {
		return a.Target < b.Target
	}
	if a.MessageID != b.MessageID {
		return a.MessageID < b.MessageID
	}
	if a.Value != b.Value {
		return a.Value < b.Value
	}
	return !a.Sponsor && b.Sponsor
}
