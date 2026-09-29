package ledger

// Versioned parameters (RFC0012 §2.7). Every number of the waterfall is a
// parameter, never a constant: the compiled-in set is version 0 of the
// "allowance" namespace; "swarmmemo params set" appends versions, never edits
// one. Bodies are strict JSON: unknown, missing or duplicate keys, trailing
// data and values out of bounds are refused.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"sync"

	"swarmmemo/internal/allowance"
)

// AllowanceNamespace is the namespace of the ledger's parameters.
const AllowanceNamespace = "allowance"

// Bounds shared by the parser and the ledger.
const (
	maxUnits       = int64(1) << 42 // any budget, cap or amount
	ppm            = int64(1_000_000)
	maxHalfLife    = int64(3650)
	maxParamsBytes = 64 << 10
)

// ResourceParams are one resource's waterfall parameters. Per-tier arrays are
// indexed by tier - 1: cap, floor, root_cap and share_max_ppm have four
// entries (tiers 1–4); reserve_min_ppm, headroom_pct and borrow have three
// (tiers 1–3).
type ResourceParams struct {
	Unit           string  `json:"unit"`
	Budget         int64   `json:"budget"`
	SpendCeiling   int64   `json:"spend_ceiling"`
	GrantSharePPM  int64   `json:"grant_share_ppm"`
	Cap            []int64 `json:"cap"`
	Floor          []int64 `json:"floor"`
	RootCap        []int64 `json:"root_cap"`
	ReserveMinPPM  []int64 `json:"reserve_min_ppm"`
	HeadroomPct    []int64 `json:"headroom_pct"`
	ShareMaxPPM    []int64 `json:"share_max_ppm"`
	Borrow         []bool  `json:"borrow"`
	SpillStart     int64   `json:"spill_start"`
	SpillInterval  int64   `json:"spill_interval"`
	TransferFee    int64   `json:"transfer_fee"`
	InboundCap     int64   `json:"inbound_cap"`
	NewKeyFloor    int64   `json:"new_key_floor"`
	ClientSharePPM int64   `json:"client_share_ppm"`
}

// AllowanceParams is the body of the "allowance" namespace.
type AllowanceParams struct {
	Schema                int                                    `json:"schema"`
	Resources             map[allowance.Resource]*ResourceParams `json:"resources"`
	ClaimExpiryDays       int64                                  `json:"claim_expiry_days"`
	ClaimHalfLifeDays     int64                                  `json:"claim_half_life_days"`
	ClaimRatePPM          int64                                  `json:"claim_rate_ppm"`
	GrantedHalfLifeDays   int64                                  `json:"granted_half_life_days"`
	GrantedRatePPM        int64                                  `json:"granted_rate_ppm"`
	EarnedHalfLifeDays    int64                                  `json:"earned_half_life_days"`
	EarnedRatePPM         int64                                  `json:"earned_rate_ppm"`
	Dust                  int64                                  `json:"dust"`
	TransferableFreeTiers []int64                                `json:"transferable_free_tiers"`
	PaidTransferable      bool                                   `json:"paid_transferable"`
	TransferDelay         int64                                  `json:"transfer_delay"`
	DormantDays           int64                                  `json:"dormant_days"`
	SpikeX                int64                                  `json:"spike_x"`
	SpikeFloor            int64                                  `json:"spike_floor"`
	TrustResetDays        int64                                  `json:"trust_reset_days"`
}

// KnownResources is the resource registry (§2.1): a new resource is a
// parameter entry plus a line here.
var KnownResources = map[allowance.Resource]string{
	allowance.PostBytes:   "byte",
	allowance.MemoryBytes: "byte",
	allowance.Credit:      "credit",
}

// RatePPM is the daily demurrage rate of a half-life of h days:
// round((1 − 2^(−1/h)) × 1e6), 0 when h is 0 (no decay).
func RatePPM(h int64) int64 {
	if h <= 0 {
		return 0
	}
	return int64(math.Round((1 - math.Exp2(-1/float64(h))) * 1e6))
}

// DefaultAllowanceParams is version 0, the defaults of RFC0012 §2.
func DefaultAllowanceParams() AllowanceParams {
	const mib = 1 << 20
	post := &ResourceParams{
		Unit: "byte", Budget: 64 * mib, SpendCeiling: 128 * mib, GrantSharePPM: 0,
		Cap:           []int64{16 * mib, 8 * mib, 4 * mib, 4 * mib},
		Floor:         []int64{64 << 10, 64 << 10, 64 << 10, 16 << 10},
		RootCap:       []int64{64 * mib, 32 * mib, 16 * mib, 16 * mib},
		ReserveMinPPM: []int64{100_000, 100_000, 200_000},
		HeadroomPct:   []int64{50, 50, 50},
		ShareMaxPPM:   []int64{ppm, ppm, ppm, ppm},
		Borrow:        []bool{true, true, true},
		SpillStart:    3600, SpillInterval: 3600,
		TransferFee: 256, InboundCap: 64 * mib, NewKeyFloor: 0, ClientSharePPM: ppm,
	}
	memory := &ResourceParams{
		Unit: "byte", Budget: 16 * mib, SpendCeiling: 32 * mib, GrantSharePPM: 0,
		Cap:           []int64{4 * mib, 2 * mib, 1 * mib, 0},
		Floor:         []int64{16 << 10, 16 << 10, 16 << 10, 0},
		RootCap:       []int64{16 * mib, 8 * mib, 4 * mib, 0},
		ReserveMinPPM: []int64{100_000, 100_000, 200_000},
		HeadroomPct:   []int64{50, 50, 50},
		ShareMaxPPM:   []int64{ppm, ppm, ppm, 0},
		Borrow:        []bool{true, true, true},
		SpillStart:    3600, SpillInterval: 3600,
		TransferFee: 256, InboundCap: 16 * mib, NewKeyFloor: 0, ClientSharePPM: ppm,
	}
	credit := &ResourceParams{
		Unit: "credit", Budget: 0, SpendCeiling: 0, GrantSharePPM: 0,
		Cap:           []int64{0, 0, 0, 0},
		Floor:         []int64{0, 0, 0, 0},
		RootCap:       []int64{0, 0, 0, 0},
		ReserveMinPPM: []int64{100_000, 100_000, 200_000},
		HeadroomPct:   []int64{50, 50, 50},
		ShareMaxPPM:   []int64{ppm, ppm, ppm, 0},
		Borrow:        []bool{true, true, true},
		SpillStart:    3600, SpillInterval: 3600,
		TransferFee: 1, InboundCap: 0, NewKeyFloor: 0, ClientSharePPM: ppm,
	}
	return AllowanceParams{
		Schema:          1,
		Resources:       map[allowance.Resource]*ResourceParams{allowance.PostBytes: post, allowance.MemoryBytes: memory, allowance.Credit: credit},
		ClaimExpiryDays: 0, ClaimHalfLifeDays: 0, ClaimRatePPM: 0,
		GrantedHalfLifeDays: 14, GrantedRatePPM: RatePPM(14),
		EarnedHalfLifeDays: 30, EarnedRatePPM: RatePPM(30),
		Dust: 64, TransferableFreeTiers: []int64{1, 2, 3, 4}, PaidTransferable: true,
		TransferDelay: 48 * 3600, DormantDays: 30, SpikeX: 4, SpikeFloor: 8, TrustResetDays: 30,
	}
}

// Clone is a deep copy.
func (p AllowanceParams) Clone() AllowanceParams {
	out := p
	out.Resources = make(map[allowance.Resource]*ResourceParams, len(p.Resources))
	for r, rp := range p.Resources {
		c := *rp
		c.Cap, c.Floor, c.RootCap = slices.Clone(rp.Cap), slices.Clone(rp.Floor), slices.Clone(rp.RootCap)
		c.ReserveMinPPM, c.HeadroomPct, c.ShareMaxPPM = slices.Clone(rp.ReserveMinPPM), slices.Clone(rp.HeadroomPct), slices.Clone(rp.ShareMaxPPM)
		c.Borrow = slices.Clone(rp.Borrow)
		out.Resources[r] = &c
	}
	out.TransferableFreeTiers = slices.Clone(p.TransferableFreeTiers)
	return out
}

// Marshal is the canonical body: compact JSON with sorted map keys.
func (p AllowanceParams) Marshal() []byte {
	b, err := json.Marshal(p)
	if err != nil {
		panic(err) // plain structs of ints, bools and strings always encode
	}
	return b
}

// Validate checks every bound. A body that parses and validates is safe to
// run the waterfall on: no division by zero, no overflow past maxUnits.
func (p AllowanceParams) Validate() error {
	if p.Schema != 1 {
		return errors.New("schema must be 1")
	}
	if len(p.Resources) != len(KnownResources) {
		return fmt.Errorf("resources must name exactly %d resources", len(KnownResources))
	}
	for r, unit := range KnownResources {
		rp := p.Resources[r]
		if rp == nil {
			return fmt.Errorf("resources.%s is missing", r)
		}
		if err := rp.validate(unit); err != nil {
			return fmt.Errorf("resources.%s: %w", r, err)
		}
	}
	checks := []struct {
		name      string
		v, lo, hi int64
	}{
		{"claim_expiry_days", p.ClaimExpiryDays, 0, 30},
		{"claim_half_life_days", p.ClaimHalfLifeDays, 0, maxHalfLife},
		{"granted_half_life_days", p.GrantedHalfLifeDays, 1, maxHalfLife},
		{"earned_half_life_days", p.EarnedHalfLifeDays, 1, maxHalfLife},
		{"dust", p.Dust, 0, 1 << 20},
		{"transfer_delay", p.TransferDelay, 0, 30 * 86400},
		{"dormant_days", p.DormantDays, 1, maxHalfLife},
		{"spike_x", p.SpikeX, 1, 1000},
		{"spike_floor", p.SpikeFloor, 1, 1_000_000},
		{"trust_reset_days", p.TrustResetDays, 0, maxHalfLife},
	}
	for _, c := range checks {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("%s must be %d–%d", c.name, c.lo, c.hi)
		}
	}
	for _, rate := range []struct {
		name string
		h, r int64
	}{{"claim_rate_ppm", p.ClaimHalfLifeDays, p.ClaimRatePPM}, {"granted_rate_ppm", p.GrantedHalfLifeDays, p.GrantedRatePPM}, {"earned_rate_ppm", p.EarnedHalfLifeDays, p.EarnedRatePPM}} {
		if rate.r != RatePPM(rate.h) {
			return fmt.Errorf("%s must be %d, round((1 - 2^(-1/h)) * 1e6) of its half-life", rate.name, RatePPM(rate.h))
		}
	}
	if len(p.TransferableFreeTiers) > 4 {
		return errors.New("transferable_free_tiers names at most tiers 1–4")
	}
	for i, t := range p.TransferableFreeTiers {
		if t < 1 || t > 4 || i > 0 && t <= p.TransferableFreeTiers[i-1] {
			return errors.New("transferable_free_tiers must be ascending tiers from 1 to 4")
		}
	}
	return nil
}

func (rp *ResourceParams) validate(unit string) error {
	if rp.Unit != unit {
		return fmt.Errorf("unit must be %q", unit)
	}
	if rp.Budget < 0 || rp.Budget > maxUnits {
		return fmt.Errorf("budget must be 0–%d", maxUnits)
	}
	if rp.SpendCeiling < rp.Budget || rp.SpendCeiling > 8*rp.Budget {
		return errors.New("spend_ceiling must be from budget to 8 × budget")
	}
	if len(rp.Cap) != 4 || len(rp.Floor) != 4 || len(rp.RootCap) != 4 || len(rp.ShareMaxPPM) != 4 {
		return errors.New("cap, floor, root_cap and share_max_ppm have one entry per tier 1–4")
	}
	if len(rp.ReserveMinPPM) != 3 || len(rp.HeadroomPct) != 3 || len(rp.Borrow) != 3 {
		return errors.New("reserve_min_ppm, headroom_pct and borrow have one entry per tier 1–3")
	}
	if !inPPM(rp.GrantSharePPM) {
		return errors.New("grant_share_ppm must be 0–1000000")
	}
	var reserves int64
	for i := 0; i < 4; i++ {
		if rp.Cap[i] < 0 || rp.Cap[i] > maxUnits {
			return fmt.Errorf("cap[%d] must be 0–%d", i+1, maxUnits)
		}
		if rp.Floor[i] < 0 || rp.Floor[i] > rp.Cap[i] {
			return fmt.Errorf("floor[%d] must be 0–cap", i+1)
		}
		if rp.RootCap[i] < 0 || rp.RootCap[i] > maxUnits {
			return fmt.Errorf("root_cap[%d] must be 0–%d", i+1, maxUnits)
		}
		if !inPPM(rp.ShareMaxPPM[i]) {
			return fmt.Errorf("share_max_ppm[%d] must be 0–1000000", i+1)
		}
		if i < 3 {
			if !inPPM(rp.ReserveMinPPM[i]) {
				return fmt.Errorf("reserve_min_ppm[%d] must be 0–1000000", i+1)
			}
			reserves += rp.ReserveMinPPM[i]
			if rp.HeadroomPct[i] < 0 || rp.HeadroomPct[i] > 1000 {
				return fmt.Errorf("headroom_pct[%d] must be 0–1000", i+1)
			}
		}
	}
	if reserves > ppm {
		return errors.New("reserve_min_ppm must sum to at most 1000000")
	}
	if rp.SpillStart < 0 || rp.SpillStart > 86399 {
		return errors.New("spill_start must be 0–86399 seconds")
	}
	if rp.SpillInterval < 60 || rp.SpillInterval > 86400 {
		return errors.New("spill_interval must be 60–86400 seconds")
	}
	if rp.TransferFee < 0 || rp.TransferFee > maxUnits {
		return errors.New("transfer_fee must be 0 or more")
	}
	if rp.InboundCap < 0 || rp.InboundCap > maxUnits {
		return errors.New("inbound_cap must be 0 or more")
	}
	if rp.NewKeyFloor < 0 || rp.NewKeyFloor > maxUnits {
		return errors.New("new_key_floor must be 0 or more")
	}
	if rp.ClientSharePPM < 1 || rp.ClientSharePPM > ppm {
		return errors.New("client_share_ppm must be 1–1000000")
	}
	return nil
}

func inPPM(v int64) bool { return v >= 0 && v <= ppm }

// ParseAllowanceParams parses and validates a strict body.
func ParseAllowanceParams(body []byte) (AllowanceParams, error) {
	var p AllowanceParams
	if err := strictUnmarshal(body, &p); err != nil {
		return AllowanceParams{}, err
	}
	if err := p.Validate(); err != nil {
		return AllowanceParams{}, err
	}
	return p, nil
}

// strictUnmarshal decodes exactly one JSON value into v, refusing unknown,
// duplicate and missing keys, null values and trailing data.
func strictUnmarshal(body []byte, v any) error {
	if len(body) > maxParamsBytes {
		return fmt.Errorf("body exceeds %d bytes", maxParamsBytes)
	}
	var shape any
	if err := checkNoDuplicates(body, &shape); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	var canonical any
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(encoded, &canonical); err != nil {
		return err
	}
	if path := shapeDiff(shape, canonical, "$"); path != "" {
		return fmt.Errorf("missing or mistyped key at %s", path)
	}
	return nil
}

// checkNoDuplicates walks the token stream and refuses a key repeated in one
// object; it also decodes the generic shape.
func checkNoDuplicates(body []byte, shape *any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > 16 {
			return errors.New("nesting deeper than 16")
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{':
				seen := map[string]bool{}
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					k := key.(string)
					if seen[k] {
						return fmt.Errorf("duplicate key %q", k)
					}
					seen[k] = true
					if err = walk(depth + 1); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			case '[':
				for dec.More() {
					if err = walk(depth + 1); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			}
		case nil:
			return errors.New("null is not a parameter value")
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	return json.Unmarshal(body, shape)
}

// shapeDiff reports the first path where the input's shape (keys, array
// lengths, kinds) differs from the canonical re-encoding, or "".
func shapeDiff(in, canonical any, path string) string {
	switch c := canonical.(type) {
	case map[string]any:
		m, ok := in.(map[string]any)
		if !ok || len(m) != len(c) {
			return path
		}
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, ok := m[k]
			if !ok {
				return path + "." + k
			}
			if p := shapeDiff(v, c[k], path+"."+k); p != "" {
				return p
			}
		}
	case []any:
		a, ok := in.([]any)
		if !ok || len(a) != len(c) {
			return path
		}
		for i := range c {
			if p := shapeDiff(a[i], c[i], fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	default:
		if fmt.Sprintf("%T", in) != fmt.Sprintf("%T", canonical) {
			return path
		}
	}
	return ""
}

// Namespace describes a parameter namespace: its compiled-in first version
// and its strict validator. The ledger registers "allowance"; the services
// and trust packages register theirs with RegisterNamespace.
type Namespace struct {
	Version  int64
	Body     func() []byte
	Validate func([]byte) error
}

var (
	namespacesMu sync.RWMutex
	namespaces   = map[string]Namespace{
		AllowanceNamespace: {Version: 0, Body: func() []byte { return DefaultAllowanceParams().Marshal() }, Validate: func(b []byte) error { _, err := ParseAllowanceParams(b); return err }},
	}
)

// RegisterNamespace adds a namespace (services, trust) to every ParamsStore.
func RegisterNamespace(name string, ns Namespace) {
	namespacesMu.Lock()
	defer namespacesMu.Unlock()
	namespaces[name] = ns
}

// ParamsStore is the versioned parameter table (§2.7); it provides
// allowance.ParamsSource. Overrides replace a registered namespace's
// compiled-in version (the board derives allowance version 0 from its
// configured byte budgets).
type ParamsStore struct {
	Overrides map[string]Namespace
}

func (p ParamsStore) namespace(name string) (Namespace, bool) {
	if ns, ok := p.Overrides[name]; ok {
		return ns, true
	}
	namespacesMu.RLock()
	defer namespacesMu.RUnlock()
	ns, ok := namespaces[name]
	return ns, ok
}

// Params is the version in effect at now: the newest stored version whose
// effective_at has passed, else the compiled-in one. A namespace nobody
// registered reads as version 0 with an empty body, so its owner falls back
// on its own compiled-in defaults.
func (p ParamsStore) Params(ctx context.Context, q allowance.Querier, namespace string, now int64) (int64, []byte, error) {
	ns, ok := p.namespace(namespace)
	var version int64
	var body string
	err := q.QueryRowContext(ctx, "SELECT version,body FROM params WHERE namespace=? AND effective_at<=? ORDER BY version DESC LIMIT 1", namespace, now).Scan(&version, &body)
	if errors.Is(err, sql.ErrNoRows) {
		if !ok {
			return 0, nil, nil
		}
		return ns.Version, ns.Body(), nil
	}
	if err != nil {
		return 0, nil, err
	}
	return version, []byte(body), nil
}

// ParamsVersion is one stored or compiled-in version, as /api/params shows it.
type ParamsVersion struct {
	Namespace   string          `json:"namespace"`
	Version     int64           `json:"version"`
	Body        json.RawMessage `json:"body"`
	SHA256      string          `json:"sha256"`
	EffectiveAt int64           `json:"effective_at"`
	CreatedAt   int64           `json:"created_at"`
	Reason      string          `json:"reason"`
	CompiledIn  bool            `json:"compiled_in"`
}

// Get reads one version (-1: the one in effect at now).
func (p ParamsStore) Get(ctx context.Context, q allowance.Querier, namespace string, version, now int64) (ParamsVersion, error) {
	ns, ok := p.namespace(namespace)
	if !ok {
		return ParamsVersion{}, &allowance.Err{Code: "invalid_resource"}
	}
	var v ParamsVersion
	var body string
	var err error
	if version < 0 {
		err = q.QueryRowContext(ctx, "SELECT version,body,sha256,effective_at,created_at,reason FROM params WHERE namespace=? AND effective_at<=? ORDER BY version DESC LIMIT 1", namespace, now).Scan(&v.Version, &body, &v.SHA256, &v.EffectiveAt, &v.CreatedAt, &v.Reason)
	} else {
		err = q.QueryRowContext(ctx, "SELECT version,body,sha256,effective_at,created_at,reason FROM params WHERE namespace=? AND version=?", namespace, version).Scan(&v.Version, &body, &v.SHA256, &v.EffectiveAt, &v.CreatedAt, &v.Reason)
	}
	if errors.Is(err, sql.ErrNoRows) && (version < 0 || version == ns.Version) {
		b := ns.Body()
		sum := sha256.Sum256(b)
		return ParamsVersion{Namespace: namespace, Version: ns.Version, Body: b, SHA256: hex.EncodeToString(sum[:]), Reason: "compiled-in defaults", CompiledIn: true}, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ParamsVersion{}, ErrNotFound
	}
	if err != nil {
		return ParamsVersion{}, err
	}
	v.Namespace, v.Body = namespace, json.RawMessage(body)
	return v, nil
}

// List is the namespace's versions, newest first, at most 100 stored ones
// followed by the compiled-in version.
func (p ParamsStore) List(ctx context.Context, q allowance.Querier, namespace string) ([]ParamsVersion, error) {
	ns, ok := p.namespace(namespace)
	if !ok {
		return nil, &allowance.Err{Code: "invalid_resource"}
	}
	rows, err := q.QueryContext(ctx, "SELECT version,sha256,effective_at,created_at,reason FROM params WHERE namespace=? ORDER BY version DESC LIMIT 100", namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParamsVersion
	for rows.Next() {
		v := ParamsVersion{Namespace: namespace}
		if err = rows.Scan(&v.Version, &v.SHA256, &v.EffectiveAt, &v.CreatedAt, &v.Reason); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	b := ns.Body()
	sum := sha256.Sum256(b)
	return append(out, ParamsVersion{Namespace: namespace, Version: ns.Version, SHA256: hex.EncodeToString(sum[:]), Reason: "compiled-in defaults", CompiledIn: true}), nil
}

// Set validates body and appends it as the next version. The stored body is
// the compacted input; effective must not be in the past.
func (p ParamsStore) Set(ctx context.Context, q allowance.Querier, namespace string, body []byte, actor, reason string, effective, now int64) (int64, error) {
	ns, ok := p.namespace(namespace)
	if !ok {
		return 0, fmt.Errorf("unknown parameter namespace %q", namespace)
	}
	if reason == "" || len(reason) > 512 {
		return 0, errors.New("a public reason of 1–512 bytes is required")
	}
	if effective < now {
		effective = now
	}
	if ns.Validate == nil {
		return 0, fmt.Errorf("namespace %q has no validator", namespace)
	}
	if err := ns.Validate(body); err != nil {
		return 0, fmt.Errorf("invalid %s parameters: %w", namespace, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return 0, err
	}
	var latest sql.NullInt64
	if err := q.QueryRowContext(ctx, "SELECT max(version) FROM params WHERE namespace=?", namespace).Scan(&latest); err != nil {
		return 0, err
	}
	version := ns.Version + 1
	if latest.Valid && latest.Int64 >= version {
		version = latest.Int64 + 1
	}
	sum := sha256.Sum256(compact.Bytes())
	_, err := q.ExecContext(ctx, "INSERT INTO params(namespace,version,body,sha256,effective_at,created_at,actor,reason) VALUES(?,?,?,?,?,?,?,?)", namespace, version, compact.String(), hex.EncodeToString(sum[:]), effective, now, actor, reason)
	return version, err
}

var _ allowance.ParamsSource = ParamsStore{}
