package trust

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

// Params is the trust parameter namespace (RFC0012 §2.7, §4). Every number
// the run uses is here, so re-pricing is a new version, never a code change.
// Amounts are integers: ppm for fractions, the collateral unit for prices,
// flow units (1/UnitPerShare of a fair share) for flow.
//
// Decay and ramp curves never evaluate a float at run time: each curve
// publishes DayFactorPPM = round(1e6 × 2^(−1/half_life_days)) and the run
// applies it once per whole day with floor (see decayPPM). Validation checks
// the factor against the half-life; the run reads only the factor.
type Params struct {
	Version         int64                 `json:"-"`
	Schema          int64                 `json:"schema"`
	CollateralUnit  string                `json:"collateral_unit"`
	Seeds           []string              `json:"seeds"`
	SeedsReason     string                `json:"seeds_reason"`
	ServiceAccounts []string              `json:"service_accounts"`
	Proofs          map[string]ProofPrice `json:"proofs"`
	// DomainSuffixes are public suffixes of two or more labels ("co.uk"); a
	// domain root is the suffix plus one label, otherwise the last two labels.
	DomainSuffixes []string        `json:"domain_suffixes"`
	ProofFreshDays int64           `json:"proof_fresh_days"`
	History        HistoryPrice    `json:"history"`
	WindowDays     int64           `json:"window_days"`
	NMax           int64           `json:"n_max"`
	KOut           int64           `json:"k_out"`
	Edges          map[string]Edge `json:"edges"`
	UnitPerShare   int64           `json:"unit_per_share"`
	EdgeCapPPM     int64           `json:"edge_cap_ppm"`
	LambdaPPM      int64           `json:"lambda_ppm"`
	RootCapShares  int64           `json:"root_cap_shares"`
	FillSteps      int64           `json:"fill_steps"`
	OwnAvgRuns     int64           `json:"own_avg_runs"`
	ActiveDays     int64           `json:"active_days"`   // H counts accounts active in this many days
	ActivityDays   int64           `json:"activity_days"` // the active-day share looks back this far
	SeedsB         AnchorRule      `json:"seeds_b"`
	MaxWork        int64           `json:"max_work"`
	MaxSeconds     int64           `json:"max_seconds"`
	ThetaTrusted   int64           `json:"theta_trusted"` // flow units for tier 1
	ThetaProven    int64           `json:"theta_proven"`  // proof collateral for tier 2
	UnitPrice      int64           `json:"endorsement_unit_price"`
	WeightCapPPM   int64           `json:"weight_cap_ppm"`
	WeightPerUnit  int64           `json:"weight_per_unit_ppm"`
	Detectors      Detectors       `json:"detectors"`
	Liability      Liability       `json:"liability"`
	Sponsor        Sponsor         `json:"sponsor"`
}

// ProofPrice prices one proof kind at min(forge, rent) × curve(age).
type ProofPrice struct {
	Forge        int64  `json:"forge"`
	Rent         int64  `json:"rent"`
	Curve        string `json:"curve"` // "ramp" or "none"
	HalfLifeDays int64  `json:"half_life_days"`
	DayFactorPPM int64  `json:"day_factor_ppm"`
}

// HistoryPrice prices distinct days of public activity that drew engagement
// from another root with standing.
type HistoryPrice struct {
	DayPrice     int64 `json:"day_price"`
	DaysCap      int64 `json:"days_cap"`
	HalfLifeDays int64 `json:"half_life_days"`
	DayFactorPPM int64 `json:"day_factor_ppm"`
}

// Edge is one endorsement edge kind (§4.2).
type Edge struct {
	BasePPM      int64 `json:"base_ppm"`
	HalfLifeDays int64 `json:"half_life_days"`
	DayFactorPPM int64 `json:"day_factor_ppm"`
}

// AnchorRule is seed set B's public rule (§4.3 step 1).
type AnchorRule struct {
	MinRoots    int64 `json:"min_roots"`
	ThetaAnchor int64 `json:"theta_anchor"`
	MinAgeDays  int64 `json:"min_age_days"`
	ActiveDays  int64 `json:"active_days"`
	OfDays      int64 `json:"of_days"`
	MinMembers  int64 `json:"min_members"`
}

// Detectors are the versioned evidence detectors (§4.5).
type Detectors struct {
	Version        int64 `json:"version"`
	FunnelK        int64 `json:"funnel_k"`
	FunnelDays     int64 `json:"funnel_days"`
	FunnelSpendPPM int64 `json:"funnel_spend_ppm"`
	RingMin        int64 `json:"ring_min"`
	RingInsidePPM  int64 `json:"ring_inside_ppm"`
}

// Liability is the endorser penalty rule (§4.5).
type Liability struct {
	PhiPPM      int64 `json:"phi_ppm"`
	PhiMaxPPM   int64 `json:"phi_max_ppm"`
	PenaltyDays int64 `json:"penalty_days"`
	EdgeDays    int64 `json:"edge_days"`
}

// Sponsor is the sponsor dividend rule (§4.5).
type Sponsor struct {
	WindowDays    int64  `json:"window_days"`
	SlotsPerShare int64  `json:"slots_per_share"`
	DividendPPM   int64  `json:"dividend_ppm"`
	DividendDays  int64  `json:"dividend_days"`
	DailyCap      int64  `json:"daily_cap"`
	Resource      string `json:"resource"`
}

// ParamsNamespace is the parameter namespace of the trust module (§2.7).
const ParamsNamespace = "trust"

// DefaultVersion is the compiled-in trust parameter version: version 0 (the
// RFC defaults) plus seed set A (§2.7, §13).
const DefaultVersion = SeedsAVersion

// DayFactor is round(1e6 × 2^(−1/h)), the published daily factor of a curve
// with half-life h days. It is evaluated when parameters are made or checked,
// never during a run.
func DayFactor(halfLifeDays int64) int64 {
	if halfLifeDays <= 0 {
		return 0
	}
	return int64(math.Round(1e6 * math.Pow(2, -1/float64(halfLifeDays))))
}

// DefaultParams is the compiled-in trust parameter set, version 1: the RFC
// defaults with seed set A and the service accounts.
func DefaultParams() Params {
	edge := func(base, h int64) Edge { return Edge{BasePPM: base, HalfLifeDays: h, DayFactorPPM: DayFactor(h)} }
	free := ProofPrice{Curve: "none"}
	return Params{
		Version:         DefaultVersion,
		Schema:          1,
		CollateralUnit:  "usd_cent",
		Seeds:           slices.Clone(SeedsA),
		SeedsReason:     SeedsAReason,
		ServiceAccounts: slices.Clone(ServiceAccountsA),
		Proofs: map[string]ProofPrice{
			"domain":  {Forge: 1200, Rent: 400, Curve: "ramp", HalfLifeDays: 180, DayFactorPPM: DayFactor(180)},
			"ed25519": free,
			"board":   free,
			"url":     free,
			"nostr":   free,
		},
		DomainSuffixes: []string{"ac.uk", "co.jp", "co.nz", "co.uk", "co.za", "com.au", "com.br", "com.cn", "github.io", "gov.uk", "net.au", "org.au", "org.uk", "pages.dev", "vercel.app"},
		ProofFreshDays: 30,
		History:        HistoryPrice{DayPrice: 10, DaysCap: 90, HalfLifeDays: 90, DayFactorPPM: DayFactor(90)},
		WindowDays:     90,
		NMax:           20000,
		KOut:           64,
		Edges: map[string]Edge{
			"vote":        edge(600000, 14),
			"vouch":       edge(1000000, 30),
			"reply":       edge(300000, 14),
			"legacy_vote": edge(0, 14),
		},
		UnitPerShare:  20,
		EdgeCapPPM:    1000000,
		LambdaPPM:     500000,
		RootCapShares: 3,
		FillSteps:     5,
		OwnAvgRuns:    30,
		ActiveDays:    7,
		ActivityDays:  30,
		SeedsB:        AnchorRule{MinRoots: 2, ThetaAnchor: 1000, MinAgeDays: 14, ActiveDays: 5, OfDays: 30, MinMembers: 3},
		MaxWork:       200000000,
		MaxSeconds:    120,
		ThetaTrusted:  10,
		ThetaProven:   200,
		UnitPrice:     25,
		WeightCapPPM:  2000000,
		WeightPerUnit: 1000,
		Detectors:     Detectors{Version: 1, FunnelK: 5, FunnelDays: 7, FunnelSpendPPM: 100000, RingMin: 3, RingInsidePPM: 800000},
		Liability:     Liability{PhiPPM: 1000000, PhiMaxPPM: 1000000, PenaltyDays: 30, EdgeDays: 30},
		Sponsor:       Sponsor{WindowDays: 7, SlotsPerShare: 3, DividendPPM: 1000000, DividendDays: 90, DailyCap: 100, Resource: "credit"},
	}
}

// Body is the canonical JSON of the parameter set (sorted keys, no spaces),
// the form published at /api/params/trust and hashed into every run.
func (p Params) Body() []byte {
	raw, err := json.Marshal(p)
	if err != nil {
		panic(err) // Params holds only strings, integers, slices and maps
	}
	return canonicalize(raw)
}

var accountRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseParams reads a trust parameter body strictly: unknown fields, missing
// sections and values outside their bounds are errors.
func ParseParams(version int64, body []byte) (Params, error) {
	var p Params
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return Params{}, fmt.Errorf("trust params: %w", err)
	}
	if dec.More() {
		return Params{}, fmt.Errorf("trust params: trailing data")
	}
	p.Version = version
	if err := p.Validate(); err != nil {
		return Params{}, err
	}
	return p, nil
}

// Validate checks every bound. A parameter set that passes cannot make a run
// overflow int64 or loop without bound.
func (p Params) Validate() error {
	var errs []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Sprintf(format, args...))
		}
	}
	in := func(name string, v, lo, hi int64) {
		check(v >= lo && v <= hi, "%s must be %d–%d, not %d", name, lo, hi, v)
	}
	curve := func(name string, h, factor int64) {
		in(name+".half_life_days", h, 1, 3650)
		check(factor >= DayFactor(h)-1 && factor <= DayFactor(h)+1, "%s.day_factor_ppm must be round(1e6×2^(−1/%d)) = %d, not %d", name, h, DayFactor(h), factor)
	}
	check(p.Schema == 1, "schema must be 1")
	check(p.CollateralUnit != "" && len(p.CollateralUnit) <= 32, "collateral_unit must be a short name")
	check(len(p.Seeds) >= 1 && len(p.Seeds) <= 32, "seeds must list 1–32 accounts")
	check(p.SeedsReason != "", "seeds_reason is required")
	service := map[string]bool{}
	check(len(p.ServiceAccounts) <= 256, "service_accounts lists at most 256 accounts")
	for _, a := range p.ServiceAccounts {
		check(accountRE.MatchString(a), "service account %q is not a fingerprint", a)
		service[a] = true
	}
	seen := map[string]bool{}
	for _, s := range p.Seeds {
		check(accountRE.MatchString(s), "seed %q is not a fingerprint", s)
		check(!service[s], "seed %s is a service account", s)
		check(!seen[s], "seed %s is listed twice", s)
		seen[s] = true
	}
	for _, kind := range []string{"domain", "ed25519", "board", "url", "nostr"} {
		pr, ok := p.Proofs[kind]
		check(ok, "proofs.%s is required", kind)
		if !ok {
			continue
		}
		in("proofs."+kind+".forge", pr.Forge, 0, 1e9)
		in("proofs."+kind+".rent", pr.Rent, 0, 1e9)
		switch pr.Curve {
		case "ramp":
			curve("proofs."+kind, pr.HalfLifeDays, pr.DayFactorPPM)
		case "none":
			check(pr.HalfLifeDays == 0 && pr.DayFactorPPM == 0, "proofs.%s: curve none takes no half-life", kind)
		default:
			check(false, "proofs.%s.curve must be ramp or none", kind)
		}
	}
	check(len(p.Proofs) == 5, "proofs names exactly domain, ed25519, board, url and nostr")
	check(len(p.DomainSuffixes) <= 1024, "domain_suffixes lists at most 1024 suffixes")
	for _, s := range p.DomainSuffixes {
		check(strings.Count(s, ".") >= 1 && s == strings.ToLower(s) && !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, "."), "domain suffix %q must be lowercase with two or more labels", s)
	}
	in("proof_fresh_days", p.ProofFreshDays, 1, 365)
	in("history.day_price", p.History.DayPrice, 0, 1e7)
	in("history.days_cap", p.History.DaysCap, 0, 3650)
	curve("history", p.History.HalfLifeDays, p.History.DayFactorPPM)
	in("window_days", p.WindowDays, 1, 365)
	in("n_max", p.NMax, 1, 20000)
	in("k_out", p.KOut, 1, 256)
	for _, kind := range []string{"vote", "vouch", "reply", "legacy_vote"} {
		e, ok := p.Edges[kind]
		check(ok, "edges.%s is required", kind)
		if !ok {
			continue
		}
		in("edges."+kind+".base_ppm", e.BasePPM, 0, 1e6)
		curve("edges."+kind, e.HalfLifeDays, e.DayFactorPPM)
	}
	check(len(p.Edges) == 4, "edges names exactly vote, vouch, reply and legacy_vote")
	in("unit_per_share", p.UnitPerShare, 1, 1000)
	in("edge_cap_ppm", p.EdgeCapPPM, 0, 10e6)
	in("lambda_ppm", p.LambdaPPM, 0, 1e6)
	in("root_cap_shares", p.RootCapShares, 1, 100)
	in("fill_steps", p.FillSteps, 1, 20)
	in("own_avg_runs", p.OwnAvgRuns, 1, 90)
	in("active_days", p.ActiveDays, 1, 90)
	in("activity_days", p.ActivityDays, 1, 90)
	in("seeds_b.min_roots", p.SeedsB.MinRoots, 1, 16)
	in("seeds_b.theta_anchor", p.SeedsB.ThetaAnchor, 0, 1e9)
	in("seeds_b.min_age_days", p.SeedsB.MinAgeDays, 0, 3650)
	in("seeds_b.of_days", p.SeedsB.OfDays, 1, 90)
	in("seeds_b.active_days", p.SeedsB.ActiveDays, 0, p.SeedsB.OfDays)
	in("seeds_b.min_members", p.SeedsB.MinMembers, 1, 1000)
	in("max_work", p.MaxWork, 1, 1e10)
	in("max_seconds", p.MaxSeconds, 1, 3600)
	in("theta_trusted", p.ThetaTrusted, 0, 1e6)
	in("theta_proven", p.ThetaProven, 0, 1e9)
	in("endorsement_unit_price", p.UnitPrice, 0, 1e7)
	in("weight_cap_ppm", p.WeightCapPPM, 0, 100e6)
	in("weight_per_unit_ppm", p.WeightPerUnit, 0, 1e6)
	in("detectors.version", p.Detectors.Version, 1, 1e6)
	in("detectors.funnel_k", p.Detectors.FunnelK, 2, 1000)
	in("detectors.funnel_days", p.Detectors.FunnelDays, 1, 90)
	in("detectors.funnel_spend_ppm", p.Detectors.FunnelSpendPPM, 0, 1e6)
	in("detectors.ring_min", p.Detectors.RingMin, 2, 1000)
	in("detectors.ring_inside_ppm", p.Detectors.RingInsidePPM, 0, 1e6)
	in("liability.phi_ppm", p.Liability.PhiPPM, 0, 10e6)
	in("liability.phi_max_ppm", p.Liability.PhiMaxPPM, 0, 1e6)
	in("liability.penalty_days", p.Liability.PenaltyDays, 1, 365)
	in("liability.edge_days", p.Liability.EdgeDays, 1, 365)
	in("sponsor.window_days", p.Sponsor.WindowDays, 0, 90)
	in("sponsor.slots_per_share", p.Sponsor.SlotsPerShare, 0, 100)
	in("sponsor.dividend_ppm", p.Sponsor.DividendPPM, 0, 1e9)
	in("sponsor.dividend_days", p.Sponsor.DividendDays, 0, 365)
	in("sponsor.daily_cap", p.Sponsor.DailyCap, 0, 1e9)
	check(p.Sponsor.Resource == "credit" || p.Sponsor.Resource == "post_bytes" || p.Sponsor.Resource == "memory_bytes", "sponsor.resource must be a metered resource")
	if len(errs) > 0 {
		return fmt.Errorf("trust params: %s", strings.Join(errs, "; "))
	}
	return nil
}

// decayPPM is 1e6 × f^days with f = factor/1e6, applied one whole day at a
// time with floor: v₀ = 1e6, vₙ₊₁ = ⌊vₙ × factor / 1e6⌋. Ages past 3650
// days use 3650. It is the only curve evaluation in a run.
type decayTable struct {
	factor int64
	values []int64
}

func (t *decayTable) at(days int64) int64 {
	if days <= 0 {
		return 1e6
	}
	if days > 3650 {
		days = 3650
	}
	if t.values == nil {
		t.values = []int64{1e6}
	}
	for int64(len(t.values)) <= days {
		last := t.values[len(t.values)-1]
		t.values = append(t.values, last*t.factor/1e6)
	}
	return t.values[days]
}

// curves memoizes decay tables by factor.
type curves map[int64]*decayTable

func (c curves) decay(factor, days int64) int64 {
	t := c[factor]
	if t == nil {
		t = &decayTable{factor: factor}
		c[factor] = t
	}
	return t.at(days)
}

// ramp is 1e6 − decay: the weight a proof earns by surviving.
func (c curves) ramp(factor, days int64) int64 { return 1e6 - c.decay(factor, days) }
