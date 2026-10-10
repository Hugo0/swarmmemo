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
// (s / (1 + s)) per polarity. From rule 2 (version 4) each act weighs its
// kind's absolute weight (edge_weights: a vote 1, a vouch 10 or the weight
// its author chose, 1 to vouch_weight_max), and a pair's acts of one kind
// saturate at twice one act (w × 2s / (1 + s)). No edge within one root
// (shared verified domain or linked keys), none from a service account or an
// account whose breaker is on.
//
// Seed mass enters only where faking costs money or effort: a priced entity
// (the proof pricing table: min(forge, rent) × curve, saturating within the
// root), credit spent from the paid and earned buckets (never free or
// granted) at its cost, decayed by the half-life, and each account of the published seed list (the arbiter's seeds). An anonymous
// pseudonym has a tiny seed of its own (one network, one day) and no edges.
//
// One propagation: personalized PageRank from the seed mass. Each step a
// node restarts with (1 − pass_ppm) of its seed and passes pass_ppm of what
// it holds along its out-edges in proportion to their weights; a penalised
// share and every oppose share return to the seeds in proportion to their
// seed, and from rule 1 (version 3) so does the pass of a node with no
// out-edges (in version 2 such a node kept it, so casting one vote halved
// the voter's standing). Mass is conserved and never minted (integer floors
// only lose it); from rule 1 standing is reported as c / (1 − pass_ppm), so
// it sums to at most the seed mass / (1 − pass_ppm), and a region with no
// seed receives only what its inbound edges carry to it.
//
// Rule 2 (version 4) makes weights absolute. A node whose out-edges weigh W
// in all passes only pass × W / (K + W) along them (K = keep_weight); the
// rest returns to the seeds with the dangling pass (rule 1 is the W = 0
// case). One vote (W = 1, K = 100) carries 1/101 of the pass, not all of it.
// At the fixed point c = seed + received − seed × T / S, T being what all
// nodes pass along edges: a node keeps its own standing whether it votes or
// not, and what recipients gain is taken from every seed in proportion. So
// standing is reported as c itself, not rescaled.
//
// Rule 3 (version 6) replaces the propagation with stakes (stakes.go): a
// vouch, a work.accept and a verified witness move a share of the
// endorser's own standing to the target, and votes, witness verdicts and
// vouches are positions settled by later independent endorsement, losers
// paying winners. Down votes only rank.
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

	// From parameter version 3 (the trust-model simulation's fixes, C148).
	// Each is absent (zero) in version 2, whose runs they leave byte for byte
	// unchanged.
	//
	// Rule 1 changes the algorithm in three places: a node with no
	// out-edges returns its pass to the seeds (it no longer keeps it, so
	// casting a vote costs the voter nothing) and standing is reported as
	// c / (1 − pass); spend paid to oneself is not seed (the payee shares
	// the spender's root, or one funded the other by a transfer or a bounty
	// reward within FundedDays); and a domain is priced by its registration
	// age when it is known, not by the link's age.
	Rule int64 `json:"rule,omitempty"`
	// VFloorCents gates v0: v(s) is 0 below this standing (VoteWeightPPM).
	VFloorCents int64 `json:"v_floor_cents,omitempty"`
	// SpendCapCents caps an account's seed from spent credit.
	SpendCapCents int64 `json:"spend_cap_cents,omitempty"`
	// FundedDays is how far back a transfer funds its recipient (rule 1).
	FundedDays int64 `json:"funded_days,omitempty"`

	// From parameter version 4 (rule 2, absolute weights, C151). Absent in
	// versions 2 and 3.
	//
	// KeepWeight is K: a node passes pass × W / (K + W) along out-edges of
	// total weight W, in the units of EdgeWeights.
	KeepWeight int64 `json:"keep_weight,omitempty"`
	// EdgeWeights is one act's weight by kind: vote, down_vote, vouch (the
	// default when the vouch names none), work_accept and witness.
	EdgeWeights map[string]int64 `json:"edge_weights,omitempty"`
	// VouchWeightMax caps the weight a vouch's author may choose.
	VouchWeightMax int64 `json:"vouch_weight_max,omitempty"`

	// From parameter version 6 (rule 3, endorsements are stakes, C152).
	// Absent in versions 2 to 4. Rule 3 replaces the propagation (pass_ppm
	// and keep_weight are not read; iterations is the number of settlement
	// steps): standing is the seed, plus what endorsers moved to the account,
	// less what it moved to others, plus what its judgement earned.
	//
	// StakePPM is the share of the endorser's own standing one act of weight
	// 1 commits (edge_weights scale it): a vouch, an accepted work item and a
	// verified witness move it to their target; a vote, a witness verdict on
	// a claim and the vouch itself are positions settled by later
	// independent endorsement.
	StakePPM int64 `json:"stake_ppm,omitempty"`
	// StakeBudgetPPM caps what an account commits in all, as a share of its
	// standing (its acts are scaled down together above it).
	StakeBudgetPPM int64 `json:"stake_budget_ppm,omitempty"`
	// PriorMinPosts is how many posts with independent endorsement an author
	// needs before its own median sets a post's opening price; below it the
	// median over all such posts does.
	PriorMinPosts int64 `json:"prior_min_posts,omitempty"`
	// SignalLinkDays (rule 3, C160; 0 = off, as in version 6) makes two
	// accounts that wrote from the same network with the same User-Agent
	// within that many days before the run not independent: neither
	// confirms the other's stakes. The links come from operator-only request
	// signals and are not in the published snapshot, so a run with it set
	// cannot be recomputed from the snapshot alone.
	SignalLinkDays int64 `json:"signal_link_days,omitempty"`
}

// EdgeKinds are the kinds edge_weights prices (rule 2).
var EdgeKinds = []string{"down_vote", "vote", "vouch", "witness", "work_accept"}

// StandingVersion is the first trust parameter version with a standing
// section (RFC0015 phase 1A).
const StandingVersion = 2

// StandingFixVersion is the parameter version with the trust-model
// simulation's fixes (rule 1, the gated v(s), the spend cap; C148).
const StandingFixVersion = 3

// StandingWeightsVersion is the parameter version with absolute edge
// weights and the keep weight (rule 2; C151).
const StandingWeightsVersion = 4

// StandingStakesVersion is the parameter version in which endorsements are
// stakes settled by independent confirmation (rule 3; C152).
const StandingStakesVersion = 6

// Standing modes.
const (
	StandingShadow = "shadow"
	StandingActive = "active"
)

// DefaultStanding is the standing section of parameter version 6, in
// shadow: version 5's (StandingV5) under rule 3. One weight unit commits
// 0.25% of the endorser's standing (a vote 0.25%, a default vouch 2.5%, a
// vouch at 50 12.5%), at most half of it in all; an author's own median
// opens its posts' price from its third post with independent endorsement.
func DefaultStanding() *StandingParams {
	st := StandingV5()
	st.Rule = 3
	st.StakePPM = 2500
	st.StakeBudgetPPM = 500000
	st.PriorMinPosts = 3
	return st
}

// StandingV5 is the standing section of parameter version 5, which only
// added the assessed roots' pricing rows (roots.go): version 4's.
func StandingV5() *StandingParams { return StandingV4() }

// StandingV4 is the standing section of parameter version 4: version 3's
// (StandingV3) with absolute weights (rule 2). K = 100 and a vote weighs 1,
// so one vote carries 1/101 of the voter's pass; a vouch weighs 10 by
// default (about ten votes), its author may choose 1 to 50; work.accept and
// a verified witness weigh 10, a down vote 1.
func StandingV4() *StandingParams {
	st := StandingV3()
	st.Rule = 2
	st.KeepWeight = 100
	st.EdgeWeights = map[string]int64{"vote": 1, "down_vote": 1, "vouch": 10, "work_accept": 10, "witness": 10}
	st.VouchWeightMax = 50
	return st
}

// StandingV3 is the standing section of parameter version 3: version 2's
// (StandingV2) with the simulation's fixes and defaults: pass 0.3 over 20
// steps (20 agree with 200 to a thousandth of a cent), v0 only from 50
// cents, spend seed capped at theta2, and rule 1.
func StandingV3() *StandingParams {
	st := StandingV2()
	st.PassPPM = 300000
	st.Iterations = 20
	st.Rule = 1
	st.VFloorCents = 50
	st.SpendCapCents = st.Theta2Cents
	st.FundedDays = 30
	return st
}

// StandingV2 is the standing section of parameter version 2 (phase 1A, as
// published in 1.75.0).
func StandingV2() *StandingParams {
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
	in("standing.rule", st.Rule, 0, 3)
	in("standing.v_floor_cents", st.VFloorCents, 0, 1e9)
	in("standing.spend_cap_cents", st.SpendCapCents, 0, 1e9)
	in("standing.funded_days", st.FundedDays, 0, 365)
	if st.Rule < 2 {
		check(st.KeepWeight == 0 && st.EdgeWeights == nil && st.VouchWeightMax == 0, "standing.keep_weight, edge_weights and vouch_weight_max need rule 2")
		return
	}
	// Bounds keep a node's total out-weight (pairs × kinds × 1e4 × 2e6 ppm)
	// and K × 1e6 far inside int64.
	in("standing.keep_weight", st.KeepWeight, 1, 1e6)
	in("standing.vouch_weight_max", st.VouchWeightMax, 1, 1e4)
	check(len(st.EdgeWeights) == len(EdgeKinds), "standing.edge_weights names exactly %s", strings.Join(EdgeKinds, ", "))
	for _, k := range EdgeKinds {
		w, ok := st.EdgeWeights[k]
		check(ok, "standing.edge_weights.%s is required", k)
		in("standing.edge_weights."+k, w, 0, 1e4)
	}
	check(st.EdgeWeights["vouch"] <= st.VouchWeightMax, "standing.edge_weights.vouch must be at most vouch_weight_max")
	if st.Rule < 3 {
		check(st.StakePPM == 0 && st.StakeBudgetPPM == 0 && st.PriorMinPosts == 0 && st.SignalLinkDays == 0, "standing.stake_ppm, stake_budget_ppm, prior_min_posts and signal_link_days need rule 3")
		return
	}
	in("standing.signal_link_days", st.SignalLinkDays, 0, 90)
	in("standing.stake_ppm", st.StakePPM, 1, 1e5)
	in("standing.stake_budget_ppm", st.StakeBudgetPPM, 1, 1e6)
	in("standing.prior_min_posts", st.PriorMinPosts, 1, 1000)
}

// actWeight is one act's weight under rule 2: the kind's, or for a vouch the
// weight its author chose (clamped to 1..vouch_weight_max; 0 is the default).
func (st *StandingParams) actWeight(kind string, chosen int64) int64 {
	if kind == "vouch" && chosen > 0 {
		return min(chosen, st.VouchWeightMax)
	}
	return st.EdgeWeights[kind]
}

// Active reports whether standing is in active mode.
func (st *StandingParams) Active() bool { return st != nil && st.Mode == StandingActive }

// VoteWeightPPM is v(s), the one weight function (RFC0015 §0.3), in ppm:
// 0 for C = 0 or C below the floor, else v0 + (1 − v0) × √min(1, C / C_ref).
// Votes, replies, heat, forks and reviews read it; nobody's weighs more than
// 1e6. The floor gates v0: without it any C > 0 weighed at least v0, so
// splitting standing into many 1-cent keys multiplied vote weight (the
// simulation: 140× per dollar).
func VoteWeightPPM(cents, v0PPM, cRefCents, floorCents int64) int64 {
	if cents <= 0 || cRefCents <= 0 || cents < floorCents {
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
	return VoteWeightPPM(cents, st.V0PPM, st.CRefCents, st.VFloorCents)
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
	// floor; accounts not seasoned whose vote would weigh at least half.
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
	// Rule 1 prices a domain by its registration age when it is known (the
	// registry's creation date, RDAP), in full: an old domain counts on day
	// one. Otherwise the link's age bounds it, at most half (the table's
	// rule), and the state says so.
	registered := map[string]int64{}
	if st.Rule >= 1 {
		for _, r := range in.snap.Proofs {
			if r.Kind != "domain" || r.RegisteredAt <= 0 || r.RegisteredAt > in.asOf {
				continue
			}
			k := r.Account + "\x00" + r.LinkValue
			if t, ok := registered[k]; !ok || r.RegisteredAt < t {
				registered[k] = r.RegisteredAt
			}
		}
	}
	for _, b := range bests {
		contribution, state := b.part.Contribution, b.part.State
		if st.Rule >= 1 && b.part.Kind == "domain" && b.part.Note != "not counted in this state" {
			if t, ok := registered[b.account+"\x00"+b.part.Value]; ok {
				price := p.Proofs["domain"]
				contribution = min(price.Forge, price.Rent) * cv.ramp(price.DayFactorPPM, max(0, in.D-dayOf(t))) / 1e6
				state += " (registration age)"
			} else {
				state += " (link age: registration date unknown)"
			}
		}
		mass := int64(0)
		if n := claimants[b.part.Root]; n > 0 && contribution > 0 {
			mass = contribution * massPerCent / n
		}
		if b.part.Note == "not counted in this state" {
			state += " (not counted)"
		}
		seeds[b.account] = append(seeds[b.account], seedRoot{b.part.Root, "imported", b.part.Kind, state, mass})
	}
	// Spent credit at cost, decayed. Credit held is not an input: its rent
	// is a negligible signal, and publishing it per account is not.
	// Rule 1: spend paid to oneself is not seed. A spend record names its
	// payee when the payment went to an account (to) or a host (link_value);
	// it is self-dealt when the payee shares the spender's root, or when
	// either funded the other (a transfer or a bounty reward) within
	// funded_days.
	var selfDealt func(r Record) bool
	funded := map[[2]string]bool{}
	if st.Rule >= 1 {
		inputs["spend_self_dealt"] = 0
		from := in.asOf - st.FundedDays*day
		for _, r := range in.snap.Transfers {
			if r.CreatedAt >= from && r.CreatedAt < in.asOf && r.From != "" && r.To != "" && r.From != r.To && r.Amount > 0 {
				funded[[2]string{r.From, r.To}], funded[[2]string{r.To, r.From}] = true, true
			}
		}
		suffixes := make(map[string]bool, len(p.DomainSuffixes))
		for _, x := range p.DomainSuffixes {
			suffixes[x] = true
		}
		selfDealt = func(r Record) bool {
			root := in.rootOf(r.Account)
			if r.To != "" && (r.To == r.Account || in.rootOf(r.To) == root || funded[[2]string{r.Account, r.To}]) {
				return true
			}
			return r.LinkValue != "" && in.rootOf(domainRoot(r.LinkValue, suffixes)) == root
		}
	}
	spend := map[string]int64{}
	for _, r := range sortRecords(in.snap.Spends) {
		if r.Account == "" || in.service[r.Account] || r.Day >= in.D || r.Amount <= 0 {
			continue
		}
		if selfDealt != nil && selfDealt(r) {
			inputs["spend_self_dealt"]++
			continue
		}
		inputs["spend"]++
		spend[r.Account] += mulDiv(min(r.Amount, 1e15), cv.decay(st.DayFactorPPM, in.D-r.Day)*massPerCent, 1e6*st.CreditsPerCent)
	}
	for a, m := range spend {
		if st.SpendCapCents > 0 {
			m = min(m, st.SpendCapCents*massPerCent)
		}
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
		weight         int64  // a vouch's chosen weight (rule 2)
		item           string // rule 3: the voted message, the work item or the witnessed claim
	}
	var acts []act
	for _, r := range in.current {
		if r.CreatedAt < ws || r.Target == "" {
			continue
		}
		switch {
		case (r.Kind == "vote" || r.Kind == "vouch") && r.Value == 1:
			acts = append(acts, act{r.Voter, r.Target, r.Kind, r.CreatedAt, false, r.Weight, r.MessageID})
		case r.Kind == "vote" && r.Value == -1:
			acts = append(acts, act{r.Voter, r.Target, "down_vote", r.CreatedAt, true, 0, r.MessageID})
		}
	}
	for _, r := range sortRecords(in.snap.Acts) {
		if r.CreatedAt >= in.asOf || r.CreatedAt < ws || (r.Kind != "work_accept" && r.Kind != "witness") {
			continue
		}
		// A failed witness verdict (value -1) is read from rule 3 only: a
		// position against the witnessed claim, no edge.
		if r.Value < 0 && (st.Rule < 3 || r.Kind != "witness") {
			continue
		}
		acts = append(acts, act{r.From, r.To, r.Kind, r.CreatedAt, r.Value < 0, 0, r.ID})
	}
	type edgeKey struct {
		src, dst string
		oppose   bool
	}
	// Rule 2 saturates per pair, polarity and kind, at the kind's weight.
	type kindKey struct {
		edgeKey
		kind string
	}
	pairSum := map[edgeKey]int64{}
	kindSum, kindWeight := map[kindKey]int64{}, map[kindKey]int64{}
	var staked []stakeAct
	if st.Rule >= 3 {
		inputs["witness_failed"] = 0
	}
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
		if c.kind == "witness" && c.oppose {
			inputs["witness_failed"]++
		} else {
			inputs[c.kind]++
		}
		if st.Rule >= 3 {
			staked = append(staked, stakeAct{src: c.src, dst: c.dst, kind: c.kind, item: c.item, at: c.at, against: c.oppose,
				load: st.actWeight(c.kind, c.weight) * v})
		}
		if st.Rule >= 2 {
			k := kindKey{edgeKey{c.src, c.dst, c.oppose}, c.kind}
			kindSum[k] += v
			kindWeight[k] = max(kindWeight[k], st.actWeight(c.kind, c.weight))
			continue
		}
		pairSum[edgeKey{c.src, c.dst, c.oppose}] += v
	}
	// A pair's weight under rule 2, in ppm of a weight unit: each kind
	// counts w × 2s / (1 + s), so one fresh act is its weight and repeats
	// saturate at twice it.
	for k, s := range kindSum {
		if w := kindWeight[k] * mulDiv(2e6, s, 1e6+s); w > 0 {
			pairSum[k.edgeKey] += w
		}
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
		w := s // rule 2: already the pair's weight
		if st.Rule < 2 {
			w = 1e6 * s / (1e6 + s)
		}
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

	var stakeOut stakeResult
	if st.Rule >= 3 {
		// C160: accounts that wrote from one network with one client are
		// not independent, while signal_link_days is set.
		linked := map[[2]string]bool{}
		if st.SignalLinkDays > 0 {
			for _, r := range in.snap.SignalLinks {
				if r.Account != "" && r.LinkAccount != "" && r.Account != r.LinkAccount {
					linked[[2]string{r.Account, r.LinkAccount}], linked[[2]string{r.LinkAccount, r.Account}] = true, true
				}
			}
			inputs["signal_links"] = int64(len(linked) / 2)
		}
		stakeOut = runStakes(st, in.asOf, ws, nodes, seed, pen, staked, func(a, b string) bool {
			return a != b && in.rootOf(a) != in.rootOf(b) && !funded[[2]string{a, b}] && !linked[[2]string{a, b}]
		})
	}

	// Personalized PageRank from the seed mass, in integers. Each step a
	// node restarts with (1 − pass) of its seed and passes pass of what it
	// holds along its out-edges; a node with no out-edges keeps it (rule 0)
	// or returns it to the seeds (rule 1), and keeps the rounding. Under
	// rule 2 only pass × W / (K + W) goes along the edges and the rest
	// returns to the seeds. Penalised and oppose shares return to the seeds
	// by seed. At the fixed point the mass of all accounts sums to the seed.
	keep := st.KeepWeight * 1e6
	c := append([]int64(nil), seed...)
	restart := make([]int64, n)
	for i := range seed {
		restart[i] = seed[i] - mulDiv(seed[i], st.PassPPM, 1e6)
	}
	opposed := make([]int64, n)
	for it := int64(0); it < st.Iterations && st.Rule < 3; it++ {
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
				if st.Rule >= 1 {
					back += pass // dangling: returns to the seeds
				} else {
					next[u] += pass
				}
				continue
			}
			if st.Rule >= 2 {
				out := mulDiv(pass, wsum[u], wsum[u]+keep)
				back += pass - out // kept: returns to the seeds
				pass = out
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
	if st.Rule >= 3 {
		c = stakeOut.c
	}
	for i, a := range nodes {
		raw, opp := c[i], opposed[i]
		if st.Rule >= 3 {
			raw = stakeOut.raw[i] // before the penalty; c[i] is after it
		}
		if st.Rule == 1 {
			// Reported as c / (1 − pass): a node's own seed counts whole
			// whether or not it passes anything on. (Rule 2 reports c: a
			// node's own seed returns to it whole, less its seed share of
			// what all nodes pass.)
			raw, opp = mulDiv(raw, 1e6, 1e6-st.PassPPM), mulDiv(opp, 1e6, 1e6-st.PassPPM)
		}
		cents := mulDiv(max(0, raw-opp), 1e6-pen[i], 1e6) / massPerCent
		if st.Rule >= 3 {
			cents = c[i] / massPerCent
		}
		part := &StandingPart{Cents: cents, RawCents: raw / massPerCent, OpposedCents: opp / massPerCent, PenaltyPPM: pen[i], SeedCents: seed[i] / massPerCent,
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
		switch {
		case raw > seed[i] && st.Rule >= 3:
			// What it holds above its seed: what its judgement earned
			// (settled stakes), and what endorsers moved to it.
			above := raw - seed[i]
			judged := min(above, max(0, stakeOut.judged[i]))
			if above-judged >= massPerCent {
				part.Breakdown = append(part.Breakdown, StandingRoot{Root: "endorsements", Kind: "earned", Source: "edges", State: "computed", Contribution: (above - judged) / massPerCent})
			}
			if judged >= massPerCent {
				part.Breakdown = append(part.Breakdown, StandingRoot{Root: "judgement", Kind: "earned", Source: "stakes", State: "computed", Contribution: judged / massPerCent})
			}
		case raw > seed[i]:
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
		active := *st
		active.Mode = StandingActive
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
		// The floor keeps a seasoned voter's vote at one; since C155 any signed
		// key may vote, and an unseasoned voter's vote weighs v(s) alone.
		if seasoned[r.Voter] {
			sum.WouldBe.VoteWeight["votes_floored_ppm"] += max(1e6, st.VoteWeight(cents))
		} else {
			sum.WouldBe.VoteWeight["votes_floored_ppm"] += st.VoteWeight(cents)
		}
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
