package trust

import (
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// Standing (RFC0015 §3) is what it would cost to fake an identity, in US
// cents, from three primitives and one algorithm.
//
// Nodes are entities of one kind with a type tag and a canonical key:
// identities, which sign and so author edges (their id is the account), and
// passive entities: a domain ("domain:" + the registrable domain, the same
// key a domain subject uses), a linked key ("key:"), a url, a board, a nostr
// key and money spent ("spend:" + account). Later subjects (RFC0015 §4: x402 resources, MCP servers) are the
// same nodes.
//
// Edges come from the act's type, never from reading its tone:
//
//   - CONTROL: an identity link, identity → entity. A passive entity's seed
//     flows to its controller through it whole (split equally between the
//     accounts that control it; the entity holds nothing).
//   - ENDORSE (+): an up vote, a vouch, a work.accept (the accepting key →
//     the worker) and an identity.witness with verdict verified. It carries
//     a conserved share of the author's standing.
//   - OPPOSE (−): a down vote. It takes its share of the author's outflow
//     like an endorsement ("distrust is spent"), but that share returns to
//     the seeds instead of reaching the target, and the target loses it
//     after propagation, locally ("not inherited").
//   - No edge: replies, mentions, DMs, follows and views. Interaction is not
//     judgement.
//
// Each act weighs 1, decayed by the one half-life; a pair's acts saturate
// (s / (1 + s)) per polarity. No edge within one root (shared verified
// domain or linked keys), none from a service account or an account whose
// breaker is on.
//
// Seed mass enters only where faking costs money or effort: a priced entity
// (the proof pricing table: min(forge, rent) × curve, saturating within the
// root), credit spent from the paid and earned buckets (never free or
// granted) at its cost, decayed by the half-life, and each account of the published seed list (the arbiter's seeds). An anonymous
// pseudonym has a tiny seed of its own (one network, one day) and no edges.
//
// One propagation: personalized PageRank from the seed mass. Each step a
// node restarts with (1 − pass_ppm) of its seed and passes pass_ppm of what
// it holds along its out-edges in proportion to their weights (a node with
// no out-edges keeps it); a penalised share and every oppose share return to
// the seeds in proportion to their seed. Mass is conserved and never minted
// (integer floors only lose it), so standing sums to at most the seed mass,
// and a region with no seed receives only what its inbound edges carry to it.
//
// A liability penalty (RFC0012 trust_evidence: a public, logged finding the
// arbiter can lift) is an oppose from the arbiter: the penalised account's
// outflow and its standing are scaled by (1 − fraction).

// StandingParams is the trust parameter section of standing.
type StandingParams struct {
	// Mode is "shadow" (published, read by nothing) or "active" (the
	// allowance share and vote admission read it, above a floor of today's
	// rules: RFC0015 §13).
	Mode string `json:"mode"`
	// HalfLifeDays is the one decay: of an edge's acts and of spent credit.
	HalfLifeDays int64 `json:"half_life_days"`
	DayFactorPPM int64 `json:"day_factor_ppm"`
	// PassPPM is the share of its standing a node passes along its out-edges
	// at each step; Iterations is the number of steps.
	PassPPM    int64 `json:"pass_ppm"`
	Iterations int64 `json:"iterations"`
	// ArbiterSeedCents is the seed of each account of the seed list.
	ArbiterSeedCents int64 `json:"arbiter_seed_cents"`
	// AnonSeedCents is an anonymous pseudonym's standing: one network, one day.
	AnonSeedCents int64 `json:"anon_seed_cents"`
	// CreditsPerCent converts credit to cents (one credit is one micro-USDC).
	CreditsPerCent int64 `json:"credits_per_cent"`
	// Theta1Cents and Theta2Cents are the tier bands: tier 1 at C ≥ theta1,
	// tier 2 at C ≥ theta2, else 3.
	Theta1Cents int64 `json:"theta1_cents"`
	Theta2Cents int64 `json:"theta2_cents"`
	// V0PPM and CRefCents shape the vote weight v(s) (VoteWeightPPM).
	V0PPM     int64 `json:"v0_ppm"`
	CRefCents int64 `json:"c_ref_cents"`
}

// StandingVersion is the first trust parameter version with a standing
// section (RFC0015 phase 1A).
const StandingVersion = 2

// Standing modes.
const (
	StandingShadow = "shadow"
	StandingActive = "active"
)

// DefaultStanding is the standing section of parameter version 2, in shadow.
func DefaultStanding() *StandingParams {
	return &StandingParams{
		Mode:             StandingShadow,
		HalfLifeDays:     90,
		DayFactorPPM:     DayFactor(90),
		PassPPM:          500000,
		Iterations:       40,
		ArbiterSeedCents: 500,
		AnonSeedCents:    1,
		CreditsPerCent:   10000,
		Theta1Cents:      750,
		Theta2Cents:      200,
		V0PPM:            250000,
		CRefCents:        500,
	}
}

func (st *StandingParams) validate(in func(string, int64, int64, int64), check func(bool, string, ...any), curve func(string, int64, int64)) {
	check(st.Mode == StandingShadow || st.Mode == StandingActive, "standing.mode must be shadow or active")
	curve("standing", st.HalfLifeDays, st.DayFactorPPM)
	in("standing.pass_ppm", st.PassPPM, 0, 950000)
	in("standing.iterations", st.Iterations, 1, 200)
	in("standing.arbiter_seed_cents", st.ArbiterSeedCents, 0, 1e7)
	in("standing.anon_seed_cents", st.AnonSeedCents, 0, 1e4)
	in("standing.credits_per_cent", st.CreditsPerCent, 1, 1e9)
	in("standing.theta1_cents", st.Theta1Cents, 1, 1e9)
	in("standing.theta2_cents", st.Theta2Cents, 1, st.Theta1Cents)
	in("standing.v0_ppm", st.V0PPM, 0, 1e6)
	in("standing.c_ref_cents", st.CRefCents, 1, 1e9)
}

// Active reports whether standing is in active mode.
func (st *StandingParams) Active() bool { return st != nil && st.Mode == StandingActive }

// VoteWeightPPM is v(s), the one weight function (RFC0015 §0.3), in ppm:
// 0 for C = 0, else v0 + (1 − v0) × √min(1, C / C_ref). Votes, replies,
// heat, forks and reviews read it; nobody's weighs more than 1e6.
func VoteWeightPPM(cents, v0PPM, cRefCents int64) int64 {
	if cents <= 0 || cRefCents <= 0 {
		return 0
	}
	frac := int64(1e6)
	if cents < cRefCents {
		frac = mulDiv(cents, 1e6, cRefCents)
	}
	root := isqrt(frac * 1e6) // √frac in ppm
	return v0PPM + mulDiv(1e6-v0PPM, root, 1e6)
}

// VoteWeight is v(s) under these parameters.
func (st *StandingParams) VoteWeight(cents int64) int64 {
	return VoteWeightPPM(cents, st.V0PPM, st.CRefCents)
}

// FlooredVoteWeightPPM is the vote weight with today's rule as a floor
// (RFC0015 §13): a seasoned account's vote weighs 1e6 as today; in active
// mode an account that is not seasoned weighs v(s). In shadow it is today's
// weight.
func (st *StandingParams) FlooredVoteWeightPPM(seasoned bool, cents int64) int64 {
	today := int64(0)
	if seasoned {
		today = 1e6
	}
	if !st.Active() {
		return today
	}
	return max(today, st.VoteWeight(cents))
}

// VoteCounts says whether a vote counts while scores are whole votes: the
// floored weight, rounded half up, is 1.
func (st *StandingParams) VoteCounts(seasoned bool, cents int64) bool {
	return st.FlooredVoteWeightPPM(seasoned, cents) >= 500000
}

// ShareWeightPPM is the allowance share standing would set (RFC0015 §0.3):
// 1e6 + min(cap, C × per_unit), the same cap and slope as the trust run's
// weight_ppm.
func ShareWeightPPM(cents int64, p Params) int64 {
	return 1e6 + min(p.WeightCapPPM, max(0, cents)*p.WeightPerUnit)
}

// FlooredShareWeightPPM is the allowance share in active mode with today's
// weight as a floor: standing only adds. In shadow it is today's weight.
func FlooredShareWeightPPM(today, cents int64, p Params) int64 {
	if !p.Standing.Active() {
		return today
	}
	return max(today, ShareWeightPPM(cents, p))
}

// Band is the tier band of C: 1, 2 or 3.
func (st *StandingParams) Band(cents int64) int64 {
	switch {
	case cents >= st.Theta1Cents:
		return 1
	case cents >= st.Theta2Cents:
		return 2
	}
	return 3
}

// mulDiv is ⌊a × b / c⌋ for non-negative a, b and positive c, exact in 128
// bits; a quotient past int64 saturates.
func mulDiv(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return 1<<63 - 1
	}
	q, _ := bits.Div64(hi, lo, uint64(c))
	if q > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(q)
}

// isqrt is ⌊√n⌋ for n ≥ 0.
func isqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := int64(1) << ((bits.Len64(uint64(n)) + 1) / 2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

// StandingPart is an account's standing in a run (Parts.Standing). Cents
// are rounded down from the run's mass unit (a thousandth of a cent).
type StandingPart struct {
	Cents          int64          `json:"cents"`
	RawCents       int64          `json:"raw_cents"`     // before oppose edges and the penalty
	OpposedCents   int64          `json:"opposed_cents"` // taken by oppose edges (down votes)
	PenaltyPPM     int64          `json:"penalty_ppm"`
	SeedCents      int64          `json:"seed_cents"`
	ReceivedCents  int64          `json:"received_cents"` // held above its own seed: from endorse edges
	Band           int64          `json:"band"`
	VoteWeightPPM  int64          `json:"vote_weight_ppm"`
	ShareWeightPPM int64          `json:"share_weight_ppm"`
	Breakdown      []StandingRoot `json:"breakdown"`
}

// StandingRoot is one line of the breakdown: a root's seed and what of the
// account's standing it accounts for. Kind is "imported" (a credential the
// identity proved) or "earned" (spend, the arbiter's seed, and
// endorsements: what arrived over endorse edges). Root is the entity id.
type StandingRoot struct {
	Root         string `json:"root"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	State        string `json:"state"`
	SeedCents    int64  `json:"seed_cents"`
	Contribution int64  `json:"contribution"`
}

// StandingSummary is what a run publishes about standing: totals, inputs,
// bands, and what each RFC0015 §0.3 row would do if standing were active
// (with the floor of today's rules).
type StandingSummary struct {
	Mode          string           `json:"mode"`
	SeedCents     int64            `json:"seed_cents"`
	StandingCents int64            `json:"standing_cents"`
	Accounts      int64            `json:"accounts"`
	Nonzero       int64            `json:"nonzero"`
	Inputs        map[string]int64 `json:"inputs"`
	Bands         map[string]int64 `json:"bands"`
	WouldBe       StandingWouldBe  `json:"would_be"`
	Anonymous     map[string]int64 `json:"anonymous"`
	// Entities counts control edges and their seed by entity type ("domain",
	// "key", "url", "board", "nostr", "spend", "arbiter").
	Entities     map[string]map[string]int64 `json:"entities"`
	LargestMoves []StandingMove              `json:"largest_moves"`
}

// StandingWouldBe counts the would-be effects of active standing.
type StandingWouldBe struct {
	// AllowanceTier compares the band with the run's tier: raised, lowered,
	// same; floored is how many the floor min(tier, band) raises.
	AllowanceTier map[string]int64 `json:"allowance_tier"`
	// ShareWeight: accounts whose share rises above one share, the ppm
	// added in total and the largest.
	ShareWeight map[string]int64 `json:"share_weight"`
	// VoteWeight: accounts at v = 0, 0 < v < 1 and v = 1; the window's
	// counted votes, their weight under v(s) (ppm summed) and under the
	// floor; accounts not seasoned whose vote would count.
	VoteWeight map[string]int64 `json:"vote_weight"`
	// InboxKnown: accounts at "low" trust today (collateral ≥ theta_proven)
	// and at standing ≥ theta2.
	InboxKnown map[string]int64 `json:"inbox_known"`
}

// StandingMove is one of the largest would-be changes.
type StandingMove struct {
	Account        string `json:"account"`
	Cents          int64  `json:"cents"`
	Band           int64  `json:"band"`
	Tier           int64  `json:"tier"`
	ShareWeightPPM int64  `json:"share_weight_ppm"`
	VoteWeightPPM  int64  `json:"vote_weight_ppm"`
	Seasoned       bool   `json:"seasoned"`
}

// massPerCent is the run's mass unit: a thousandth of a cent.
const massPerCent = 1000

// standingInput is what Compute hands computeStanding.
type standingInput struct {
	p          Params
	snap       Snapshot
	asOf, D    int64
	service    map[string]bool
	rootOf     func(string) string
	reset      map[string]bool
	penalty    map[string]int64 // effective penalties of the run
	posts      []Record         // the window's posts, sorted
	current    []*Record        // the current endorsements, sorted
	proofs     map[string][]ProofPart
	scored     []string
	tier       map[string]int64
	collateral map[string]int64
}

type standingResult struct {
	parts   map[string]*StandingPart
	summary *StandingSummary
	extra   []string // accounts with standing that were not otherwise scored
}

func computeStanding(in standingInput) standingResult {
	p, st := in.p, in.p.Standing
	cv := curves{}
	ws := in.asOf - p.WindowDays*day
	inputs := map[string]int64{"vote": 0, "vouch": 0, "work_accept": 0, "witness": 0, "down_vote": 0, "spend": 0}

	// Seeds, by account and root, in mass units.
	type seedRoot struct {
		root, kind, source, state string
		mass                      int64
	}
	seeds := map[string][]seedRoot{}
	// Priced roots: the best part per root (the pricing table, saturating
	// within the root), split between the accounts that claim it.
	claimants := map[string]int64{}
	type best struct {
		account string
		part    ProofPart
	}
	var bests []best
	accounts := make([]string, 0, len(in.proofs))
	for a := range in.proofs {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	for _, a := range accounts {
		for _, part := range in.proofs[a] {
			if part.SaturatedBy != "" || part.Kind == "history" {
				continue
			}
			bests = append(bests, best{a, part})
			if part.Contribution > 0 {
				claimants[part.Root]++
			}
		}
	}
	for _, b := range bests {
		mass := int64(0)
		if n := claimants[b.part.Root]; n > 0 && b.part.Contribution > 0 {
			mass = b.part.Contribution * massPerCent / n
		}
		state := b.part.State
		if b.part.Note == "not counted in this state" {
			state += " (not counted)"
		}
		seeds[b.account] = append(seeds[b.account], seedRoot{b.part.Root, "imported", b.part.Kind, state, mass})
	}
	// Spent credit at cost, decayed. Credit held is not an input: its rent
	// is a negligible signal, and publishing it per account is not.
	spend := map[string]int64{}
	for _, r := range sortRecords(in.snap.Spends) {
		if r.Account == "" || in.service[r.Account] || r.Day >= in.D || r.Amount <= 0 {
			continue
		}
		inputs["spend"]++
		spend[r.Account] += mulDiv(min(r.Amount, 1e15), cv.decay(st.DayFactorPPM, in.D-r.Day)*massPerCent, 1e6*st.CreditsPerCent)
	}
	for a, m := range spend {
		seeds[a] = append(seeds[a], seedRoot{"spend:" + a, "earned", "spend", "computed", m})
	}
	for _, s := range p.Seeds {
		if !in.service[s] {
			seeds[s] = append(seeds[s], seedRoot{"arbiter", "earned", "arbiter_seed", "seed list", st.ArbiterSeedCents * massPerCent})
		}
	}

	// Edges: author → target, one weight per act, decayed, saturating per
	// pair and polarity.
	type act struct {
		src, dst, kind string
		at             int64
		oppose         bool
	}
	var acts []act
	for _, r := range in.current {
		if r.CreatedAt < ws || r.Target == "" {
			continue
		}
		switch {
		case (r.Kind == "vote" || r.Kind == "vouch") && r.Value == 1:
			acts = append(acts, act{r.Voter, r.Target, r.Kind, r.CreatedAt, false})
		case r.Kind == "vote" && r.Value == -1:
			acts = append(acts, act{r.Voter, r.Target, "down_vote", r.CreatedAt, true})
		}
	}
	for _, r := range sortRecords(in.snap.Acts) {
		if r.CreatedAt >= in.asOf || r.CreatedAt < ws || (r.Kind != "work_accept" && r.Kind != "witness") {
			continue
		}
		acts = append(acts, act{r.From, r.To, r.Kind, r.CreatedAt, false})
	}
	type edgeKey struct {
		src, dst string
		oppose   bool
	}
	pairSum := map[edgeKey]int64{}
	for _, c := range acts {
		if c.src == "" || c.dst == "" || c.src == c.dst || in.service[c.src] || in.service[c.dst] || in.reset[c.src] {
			continue
		}
		if in.rootOf(c.src) == in.rootOf(c.dst) {
			continue
		}
		v := cv.decay(st.DayFactorPPM, in.D-dayOf(c.at))
		if v <= 0 {
			continue
		}
		inputs[c.kind]++
		pairSum[edgeKey{c.src, c.dst, c.oppose}] += v
	}

	// Nodes: every account with a seed or an edge, plus every scored one.
	isNode := map[string]bool{}
	for a := range seeds {
		isNode[a] = true
	}
	for k := range pairSum {
		isNode[k.src], isNode[k.dst] = true, true
	}
	for _, a := range in.scored {
		isNode[a] = true
	}
	nodes := make([]string, 0, len(isNode))
	for a := range isNode {
		nodes = append(nodes, a)
	}
	sort.Strings(nodes)
	index := make(map[string]int, len(nodes))
	for i, a := range nodes {
		index[a] = i
	}
	n := len(nodes)
	seed := make([]int64, n)
	var S int64
	for i, a := range nodes {
		for _, r := range seeds[a] {
			seed[i] += r.mass
		}
		S += seed[i]
	}
	type out struct {
		dst    int
		w      int64
		oppose bool
	}
	outs := make([][]out, n)
	wsum := make([]int64, n)
	for k, s := range pairSum {
		w := 1e6 * s / (1e6 + s)
		if w > 0 {
			i := index[k.src]
			outs[i] = append(outs[i], out{index[k.dst], w, k.oppose})
			wsum[i] += w
		}
	}
	for i := range outs {
		sort.Slice(outs[i], func(x, y int) bool {
			if outs[i][x].dst != outs[i][y].dst {
				return outs[i][x].dst < outs[i][y].dst
			}
			return !outs[i][x].oppose && outs[i][y].oppose
		})
	}
	pen := make([]int64, n)
	for i, a := range nodes {
		pen[i] = min(1e6, in.penalty[a])
	}

	// Personalized PageRank from the seed mass, in integers. Each step a
	// node restarts with (1 − pass) of its seed and passes pass of what it
	// holds along its out-edges; a node with no out-edges keeps it, and so
	// does the rounding. Penalised and oppose shares return to the seeds by
	// seed. At the fixed point the standing of all accounts sums to the seed.
	c := append([]int64(nil), seed...)
	restart := make([]int64, n)
	for i := range seed {
		restart[i] = seed[i] - mulDiv(seed[i], st.PassPPM, 1e6)
	}
	opposed := make([]int64, n)
	for it := int64(0); it < st.Iterations; it++ {
		next := append([]int64(nil), restart...)
		for i := range opposed {
			opposed[i] = 0
		}
		var back int64
		for u := 0; u < n; u++ {
			pass := mulDiv(c[u], st.PassPPM, 1e6)
			held := mulDiv(pass, pen[u], 1e6)
			pass -= held
			back += held
			if wsum[u] == 0 {
				next[u] += pass
				continue
			}
			var sent int64
			for _, e := range outs[u] {
				share := mulDiv(pass, e.w, wsum[u])
				sent += share
				if e.oppose {
					opposed[e.dst] += share
					back += share // spent: returns to the seeds below
					continue
				}
				next[e.dst] += share
			}
			next[u] += pass - sent
		}
		for i := range next {
			if S > 0 && seed[i] > 0 {
				next[i] += mulDiv(back, seed[i], S)
			}
		}
		c = next
	}

	// Parts, summary.
	parts := map[string]*StandingPart{}
	seasoned := map[string]bool{}
	for _, r := range in.posts {
		if r.CreatedAt <= in.asOf-day {
			seasoned[r.Account] = true
		}
	}
	scored := map[string]bool{}
	for _, a := range in.scored {
		scored[a] = true
	}
	sum := &StandingSummary{Mode: st.Mode, SeedCents: S / massPerCent, Inputs: inputs,
		Bands: map[string]int64{"1": 0, "2": 0, "3": 0},
		WouldBe: StandingWouldBe{
			AllowanceTier: map[string]int64{"raised": 0, "lowered": 0, "same": 0},
			ShareWeight:   map[string]int64{"raised": 0, "added_ppm": 0, "max_added_ppm": 0},
			VoteWeight:    map[string]int64{"zero": 0, "partial": 0, "full": 0, "votes": 0, "votes_weight_ppm": 0, "votes_floored_ppm": 0, "admitted_unseasoned": 0},
			InboxKnown:    map[string]int64{"today": 0, "would_be": 0, "raised": 0},
		},
		Anonymous:    map[string]int64{"cents": st.AnonSeedCents, "vote_weight_ppm": st.VoteWeight(st.AnonSeedCents), "share_weight_ppm": ShareWeightPPM(st.AnonSeedCents, p)},
		Entities:     map[string]map[string]int64{},
		LargestMoves: []StandingMove{},
	}
	for _, a := range nodes {
		for _, r := range seeds[a] {
			typ := r.root
			if i := strings.IndexByte(typ, ':'); i >= 0 {
				typ = typ[:i]
			}
			e := sum.Entities[typ]
			if e == nil {
				e = map[string]int64{"controls": 0, "seed_cents": 0}
				sum.Entities[typ] = e
			}
			e["controls"]++
			e["seed_cents"] += r.mass / massPerCent
		}
	}
	var extra []string
	var moves []StandingMove
	var total int64
	for i, a := range nodes {
		raw := c[i]
		cents := mulDiv(max(0, raw-opposed[i]), 1e6-pen[i], 1e6) / massPerCent
		part := &StandingPart{Cents: cents, RawCents: raw / massPerCent, OpposedCents: opposed[i] / massPerCent, PenaltyPPM: pen[i], SeedCents: seed[i] / massPerCent,
			ReceivedCents: max(0, raw-seed[i]) / massPerCent, Band: st.Band(cents), VoteWeightPPM: st.VoteWeight(cents), ShareWeightPPM: ShareWeightPPM(cents, p),
			Breakdown: []StandingRoot{}}
		// The account's own seed counts first, split over its roots by
		// seed; what it holds above its seed arrived over endorse edges.
		own := min(raw, seed[i])
		for _, r := range seeds[a] {
			contribution := int64(0)
			if seed[i] > 0 {
				contribution = mulDiv(own, r.mass, seed[i]) / massPerCent
			}
			part.Breakdown = append(part.Breakdown, StandingRoot{Root: r.root, Kind: r.kind, Source: r.source, State: r.state, SeedCents: r.mass / massPerCent, Contribution: contribution})
		}
		if raw > seed[i] {
			part.Breakdown = append(part.Breakdown, StandingRoot{Root: "endorsements", Kind: "earned", Source: "edges", State: "computed", Contribution: (raw - seed[i]) / massPerCent})
		}
		sort.Slice(part.Breakdown, func(x, y int) bool {
			bx, by := part.Breakdown[x], part.Breakdown[y]
			if bx.Contribution != by.Contribution {
				return bx.Contribution > by.Contribution
			}
			if bx.SeedCents != by.SeedCents {
				return bx.SeedCents > by.SeedCents
			}
			return bx.Root < by.Root
		})
		parts[a] = part
		if !scored[a] {
			if cents == 0 && seed[i] == 0 {
				continue // an edge endpoint with nothing: not scored
			}
			extra = append(extra, a)
		}
		total += cents
		sum.Accounts++
		if cents > 0 {
			sum.Nonzero++
		}
		sum.Bands[strconv.FormatInt(part.Band, 10)]++
		tier, ok := in.tier[a]
		if !ok {
			tier = 3
		}
		switch {
		case part.Band < tier:
			sum.WouldBe.AllowanceTier["raised"]++
		case part.Band > tier:
			sum.WouldBe.AllowanceTier["lowered"]++
		default:
			sum.WouldBe.AllowanceTier["same"]++
		}
		if added := part.ShareWeightPPM - 1e6; added > 0 {
			sum.WouldBe.ShareWeight["raised"]++
			sum.WouldBe.ShareWeight["added_ppm"] += added
			sum.WouldBe.ShareWeight["max_added_ppm"] = max(sum.WouldBe.ShareWeight["max_added_ppm"], added)
		}
		switch v := part.VoteWeightPPM; {
		case v == 0:
			sum.WouldBe.VoteWeight["zero"]++
		case v < 1e6:
			sum.WouldBe.VoteWeight["partial"]++
		default:
			sum.WouldBe.VoteWeight["full"]++
		}
		active := &StandingParams{Mode: StandingActive, V0PPM: st.V0PPM, CRefCents: st.CRefCents}
		if !seasoned[a] && active.VoteCounts(false, cents) {
			sum.WouldBe.VoteWeight["admitted_unseasoned"]++
		}
		today := in.collateral[a] >= p.ThetaProven && in.collateral[a] > 0
		would := cents >= st.Theta2Cents
		if today {
			sum.WouldBe.InboxKnown["today"]++
		}
		if would {
			sum.WouldBe.InboxKnown["would_be"]++
			if !today {
				sum.WouldBe.InboxKnown["raised"]++
			}
		}
		moves = append(moves, StandingMove{Account: a, Cents: cents, Band: part.Band, Tier: tier, ShareWeightPPM: part.ShareWeightPPM, VoteWeightPPM: part.VoteWeightPPM, Seasoned: seasoned[a]})
	}
	sum.StandingCents = total
	for _, r := range in.current {
		if r.Kind != "vote" || r.CreatedAt < ws || (r.Value != 1 && r.Value != -1) {
			continue
		}
		sum.WouldBe.VoteWeight["votes"]++
		cents := int64(0)
		if part := parts[r.Voter]; part != nil {
			cents = part.Cents
		}
		sum.WouldBe.VoteWeight["votes_weight_ppm"] += st.VoteWeight(cents)
		// Every recorded vote was cast by a seasoned account: the floor keeps
		// it at one.
		sum.WouldBe.VoteWeight["votes_floored_ppm"] += max(1e6, st.VoteWeight(cents))
	}
	sort.Slice(moves, func(x, y int) bool {
		mx, my := moves[x], moves[y]
		if mx.ShareWeightPPM != my.ShareWeightPPM {
			return mx.ShareWeightPPM > my.ShareWeightPPM
		}
		if mx.Cents != my.Cents {
			return mx.Cents > my.Cents
		}
		return mx.Account < my.Account
	})
	for _, m := range moves {
		if len(sum.LargestMoves) == 10 || m.ShareWeightPPM <= 1e6 {
			break
		}
		sum.LargestMoves = append(sum.LargestMoves, m)
	}
	sort.Strings(extra)
	return standingResult{parts: parts, summary: sum, extra: extra}
}

// StandingScore is log₁₀(1 + C) rounded to two decimals, the displayed
// standing (RFC0015 §3.1); C in cents.
func StandingScore(cents int64) float64 {
	if cents <= 0 {
		return 0
	}
	return math.Round(math.Log10(float64(cents)+1)*100) / 100
}

// FakeCostText is the dollar figure shown beside the score: "about $2.50
// to fake".
func FakeCostText(cents int64) string {
	d := cents / 100
	c := cents % 100
	s := strconv.FormatInt(c, 10)
	if c < 10 {
		s = "0" + s
	}
	return "about $" + groupThousands(d) + "." + s + " to fake"
}

func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	return b.String()
}
