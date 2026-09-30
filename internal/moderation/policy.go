package moderation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// ParamsNamespace is the RFC0012 parameter namespace that holds the policy.
const ParamsNamespace = "moderation"

// Policy bounds. A policy outside them is refused whole.
const (
	PolicyBytesMax      = 256 << 10
	policySurfacesMax   = 32
	policyCategoriesMax = 64
	policyThresholdsMax = 8
	policyRulesMax      = 256
	policyRegexBytes    = 1024
	policyListMax       = 4096
	policyReloadEvery   = 30 * time.Second
)

var (
	surfaceNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31})?$`)
	categoryRE     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	ruleIDRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	labelRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ,-]{0,63}$`)
	modelRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	domainRE       = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	reasonRE       = regexp.MustCompile(`^[A-Za-z0-9 :;().=,_+-]{1,240}$`)
	validClassSets = map[string]bool{"jev": true, "rules": true, "size": true, "rate": true, "egress": true}
)

// floorCategories are the egress findings no policy can soften: a run never
// reaches a private, reserved or metadata address, whatever the policy says.
var floorCategories = map[string]Action{
	"private_address":   Block,
	"metadata":          Block,
	"invalid_host":      Block,
	"ambiguous_address": Block,
}

// hardCategories are hard on every surface whatever the policy says: the
// burst rule never softens them (security review 1.20, H1 and H2).
var hardCategories = map[string]bool{"minors": true}

// failClosedSurfaces fail to at least this action when a classifier cannot
// answer, whatever the policy's on_unavailable: an unscreened inference
// prompt is never sent upstream (security review 1.20, H1).
var failClosedSurfaces = map[Surface]Action{SurfaceInferencePrompt: Block}

// burstKeepAt is the probability at or above which a hide or block still
// applies while a burst is tripped: a burst softens only the uncertain band,
// where a misbehaving classifier's false positives are.
const burstKeepAt = 0.97

// Policy is one version of the moderation policy.
type Policy struct {
	Schema   int                        `json:"schema"`
	Version  int64                      `json:"version"`
	Jev      JevPolicy                  `json:"jev"`
	Surfaces map[Surface]*SurfacePolicy `json:"surfaces"`
	Egress   EgressPolicy               `json:"egress"`

	SHA256 string `json:"-"` // of the body it was parsed from; "" for version 0
	Source string `json:"-"` // "default", "file" or "params"
}

// JevPolicy configures the Jev classifier and its daily spend cap.
type JevPolicy struct {
	Model string `json:"model"` // pinned: thresholds are calibrated per model version
	// DailySpendCapMicroUSD bounds a UTC day's Jev spend; 0 turns Jev off.
	DailySpendCapMicroUSD int64 `json:"daily_spend_cap_microusd"`
	PricePerMTokMicroUSD  int64 `json:"price_per_mtok_microusd"` // input tokens
	// ScreenDailySpendCapMicroUSD is the part of that cap the screen service
	// (screen.text) may spend in a UTC day, counted in both; 0 turns it off.
	ScreenDailySpendCapMicroUSD int64 `json:"screen_daily_spend_cap_microusd"`
	MaxTextBytes                int   `json:"max_text_bytes"`
	TimeoutMS                   int   `json:"timeout_ms"`
}

// SurfacePolicy is one surface's configuration.
type SurfacePolicy struct {
	Classifiers   []string                   `json:"classifiers"`
	OnUnavailable Action                     `json:"on_unavailable"` // when a classifier cannot answer
	Burst         *Burst                     `json:"burst"`
	Categories    map[string]*CategoryPolicy `json:"categories"`
	Default       *CategoryPolicy            `json:"default"` // any category not listed
	Rules         []*Rule                    `json:"rules"`
	MaxBytes      int64                      `json:"max_bytes"` // size classifier; 0 is no limit
	Rate          *Rate                      `json:"rate"`      // rate classifier, per agent
}

// CategoryPolicy maps a category's probability to an action: the first
// threshold (highest first) that p reaches applies; below all, allow.
type CategoryPolicy struct {
	Thresholds []Threshold `json:"thresholds"`
	Hard       bool        `json:"hard"`  // exempt from the burst rule's downgrade
	Label      string      `json:"label"` // public reason label
}

type Threshold struct {
	At     float64 `json:"at"`
	Action Action  `json:"action"`
}

// Burst is the burst rule: more than Max hides or blocks in one category on a
// surface within WindowSeconds trips it. Mode "downgrade" flags the further
// ones in that category instead and alerts; "alert" only alerts. A downgrade
// never covers a hard category, a hide at p >= 0.97, or an author who already
// had a would-be hide in that category in the window.
type Burst struct {
	WindowSeconds int64  `json:"window_seconds"`
	Max           int    `json:"max"`
	Mode          string `json:"mode"`
}

// Rate is the per-agent rate rule: more than Max screens by one agent on a
// surface within WindowSeconds scores category "rate" at 1.
type Rate struct {
	WindowSeconds int64 `json:"window_seconds"`
	Max           int   `json:"max"`
}

// Rule is a regular expression (RE2: linear time) that scores Category at 1.
type Rule struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Regex    string `json:"regex"`
	re       *regexp.Regexp
}

// EgressPolicy lists are replaced whole when given; absent keeps version 0's.
// The private, reserved and metadata ranges are built in and always apply.
type EgressPolicy struct {
	DenyCIDRs    *[]string `json:"deny_cidrs"`
	DenyDomains  *[]string `json:"deny_domains"`
	MiningPools  *[]string `json:"mining_pools"`
	MiningPorts  *[]int    `json:"mining_ports"`
	AllowDomains *[]string `json:"allow_domains"` // never flagged as new

	denyCIDRs []netip.Prefix
}

func (sp *SurfacePolicy) actionFor(cat string, p float64) (Action, bool) {
	cp := sp.Categories[cat]
	if cp == nil {
		cp = sp.Default
	}
	if cp == nil {
		return Allow, false
	}
	for _, t := range cp.Thresholds {
		if p >= t.At {
			return t.Action, cp.Hard
		}
	}
	return Allow, cp.Hard
}

func (sp *SurfacePolicy) label(cat string) string {
	if cp := sp.Categories[cat]; cp != nil && cp.Label != "" {
		return cp.Label
	}
	if l, ok := defaultLabels[cat]; ok {
		return l
	}
	return strings.ReplaceAll(cat, "_", " ")
}

// surface is the policy of s; a surface the policy leaves out allows
// everything and runs no classifier.
func (p *Policy) surface(s Surface) *SurfacePolicy {
	if sp := p.Surfaces[s]; sp != nil {
		return sp
	}
	return &SurfacePolicy{OnUnavailable: Flag}
}

// ParsePolicy validates a policy body strictly: unknown or repeated keys,
// out-of-bounds numbers, unknown surfaces, categories or actions, actions a
// surface does not have, and regular expressions that do not compile are all
// errors. Surfaces the body leaves out keep version 0's configuration, and so
// do egress lists it leaves out.
func ParsePolicy(body []byte) (*Policy, error) {
	if len(body) > PolicyBytesMax {
		return nil, fmt.Errorf("moderation policy: larger than %d bytes", PolicyBytesMax)
	}
	var p Policy
	if err := services.StrictObject(body, &p); err != nil {
		return nil, errors.New("moderation policy: not a strict JSON object (unknown, repeated or mistyped field)")
	}
	if p.Schema != 1 {
		return nil, errors.New("moderation policy: schema must be 1")
	}
	if p.Version < 1 || p.Version > 1<<40 {
		return nil, errors.New("moderation policy: version must be a whole number from 1 (0 is the compiled-in default)")
	}
	def := DefaultPolicy()
	if p.Jev == (JevPolicy{}) {
		p.Jev = def.Jev
	}
	if err := p.Jev.validate(); err != nil {
		return nil, err
	}
	if len(p.Surfaces) > policySurfacesMax {
		return nil, fmt.Errorf("moderation policy: at most %d surfaces", policySurfacesMax)
	}
	merged := map[Surface]*SurfacePolicy{}
	for s, sp := range def.Surfaces {
		merged[s] = sp
	}
	for s, sp := range p.Surfaces {
		spec, ok := surfaceSpec(s)
		if !ok {
			return nil, fmt.Errorf("moderation policy: unknown surface %q", s)
		}
		if sp == nil {
			return nil, fmt.Errorf("moderation policy: surface %q is null", s)
		}
		if err := sp.validate(spec); err != nil {
			return nil, fmt.Errorf("moderation policy: surface %q: %w", s, err)
		}
		merged[s] = sp
	}
	p.Surfaces = merged
	if err := p.Egress.validate(def.Egress); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	p.SHA256 = hex.EncodeToString(sum[:])
	return &p, nil
}

func (j *JevPolicy) validate() error {
	switch {
	case !modelRE.MatchString(j.Model):
		return errors.New("moderation policy: jev.model must be a pinned model name such as jev-1.13.0")
	case j.DailySpendCapMicroUSD < 0 || j.DailySpendCapMicroUSD > 1e12:
		return errors.New("moderation policy: jev.daily_spend_cap_microusd must be 0 (Jev off) to 1e12")
	case j.ScreenDailySpendCapMicroUSD < 0 || j.ScreenDailySpendCapMicroUSD > j.DailySpendCapMicroUSD:
		return errors.New("moderation policy: jev.screen_daily_spend_cap_microusd must be 0 (screening off) to daily_spend_cap_microusd")
	case j.PricePerMTokMicroUSD < 1 || j.PricePerMTokMicroUSD > 1e9:
		return errors.New("moderation policy: jev.price_per_mtok_microusd must be 1 to 1e9")
	case j.MaxTextBytes < 256 || j.MaxTextBytes > 64<<10:
		return errors.New("moderation policy: jev.max_text_bytes must be 256 to 65536")
	case j.TimeoutMS < 1000 || j.TimeoutMS > 60000:
		return errors.New("moderation policy: jev.timeout_ms must be 1000 to 60000")
	}
	return nil
}

func (sp *SurfacePolicy) validate(spec SurfaceSpec) error {
	seen := map[string]bool{}
	for _, c := range sp.Classifiers {
		if !validClassSets[c] || seen[c] {
			return fmt.Errorf("classifier %q is unknown or repeated (jev, rules, size, rate, egress)", c)
		}
		if spec.Egress && c != "egress" && c != "rate" || !spec.Egress && c == "egress" {
			return fmt.Errorf("classifier %q does not apply to this surface", c)
		}
		seen[c] = true
	}
	if !sp.OnUnavailable.valid() || !spec.allows(sp.OnUnavailable) {
		return fmt.Errorf("on_unavailable must be one of %v", spec.Actions)
	}
	if sp.OnUnavailable == Allow {
		return errors.New("on_unavailable may not be allow: a screen that cannot answer fails to flag at least")
	}
	if b := sp.Burst; b != nil {
		if b.WindowSeconds < 60 || b.WindowSeconds > 7*86400 || b.Max < 1 || b.Max > 10000 {
			return errors.New("burst: window_seconds 60 to 604800, max 1 to 10000")
		}
		if b.Mode != "downgrade" && b.Mode != "alert" {
			return errors.New(`burst.mode must be "downgrade" or "alert"`)
		}
		if b.Mode == "downgrade" && !spec.allows(Flag) {
			return errors.New(`burst.mode "downgrade" needs the flag action`)
		}
	}
	if r := sp.Rate; r != nil {
		if r.WindowSeconds < 1 || r.WindowSeconds > 86400 || r.Max < 1 || r.Max > 1_000_000 {
			return errors.New("rate: window_seconds 1 to 86400, max 1 to 1000000")
		}
		if !seen["rate"] {
			return errors.New(`rate is set but "rate" is not among the classifiers`)
		}
	}
	if sp.MaxBytes < 0 || sp.MaxBytes > 64<<20 {
		return errors.New("max_bytes must be 0 (no limit) to 67108864")
	}
	if len(sp.Categories) > policyCategoriesMax {
		return fmt.Errorf("at most %d categories", policyCategoriesMax)
	}
	for name, cp := range sp.Categories {
		if !categoryRE.MatchString(name) {
			return fmt.Errorf("category %q must match %s", name, categoryRE)
		}
		if err := cp.validate(spec); err != nil {
			return fmt.Errorf("category %q: %w", name, err)
		}
		if forced, ok := floorCategories[name]; ok && spec.Egress {
			for _, t := range cp.Thresholds {
				if t.Action != forced {
					return fmt.Errorf("category %q is a security floor and always blocks", name)
				}
			}
		}
	}
	if sp.Default != nil {
		if err := sp.Default.validate(spec); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	if len(sp.Rules) > policyRulesMax {
		return fmt.Errorf("at most %d rules", policyRulesMax)
	}
	ids := map[string]bool{}
	for i, r := range sp.Rules {
		if r == nil || !ruleIDRE.MatchString(r.ID) || ids[r.ID] || !categoryRE.MatchString(r.Category) || r.Regex == "" || len(r.Regex) > policyRegexBytes {
			return fmt.Errorf("rules[%d]: needs a unique id, a category and a regex of at most %d bytes", i, policyRegexBytes)
		}
		ids[r.ID] = true
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return fmt.Errorf("rules[%d] (%s): regex does not compile", i, r.ID)
		}
		r.re = re
	}
	if len(sp.Rules) > 0 && !seen["rules"] {
		return errors.New(`rules are set but "rules" is not among the classifiers`)
	}
	return nil
}

func (cp *CategoryPolicy) validate(spec SurfaceSpec) error {
	if cp == nil {
		return errors.New("is null")
	}
	if len(cp.Thresholds) == 0 || len(cp.Thresholds) > policyThresholdsMax {
		return fmt.Errorf("1 to %d thresholds", policyThresholdsMax)
	}
	for i, t := range cp.Thresholds {
		if !(t.At > 0 && t.At <= 1) {
			return errors.New("each threshold's at must be in (0, 1]")
		}
		if i > 0 && t.At >= cp.Thresholds[i-1].At {
			return errors.New("thresholds must be in strictly decreasing order of at")
		}
		if !t.Action.valid() || !spec.allows(t.Action) {
			return fmt.Errorf("action %q is not one of this surface's actions %v", t.Action, spec.Actions)
		}
	}
	if cp.Label != "" && !labelRE.MatchString(cp.Label) {
		return fmt.Errorf("label must match %s", labelRE)
	}
	return nil
}

func (e *EgressPolicy) validate(def EgressPolicy) error {
	if e.DenyCIDRs == nil {
		e.DenyCIDRs = def.DenyCIDRs
	}
	if e.DenyDomains == nil {
		e.DenyDomains = def.DenyDomains
	}
	if e.MiningPools == nil {
		e.MiningPools = def.MiningPools
	}
	if e.MiningPorts == nil {
		e.MiningPorts = def.MiningPorts
	}
	if e.AllowDomains == nil {
		e.AllowDomains = def.AllowDomains
	}
	for _, list := range []*[]string{e.DenyDomains, e.MiningPools, e.AllowDomains} {
		if len(*list) > policyListMax {
			return fmt.Errorf("moderation policy: egress lists hold at most %d entries", policyListMax)
		}
		for i, d := range *list {
			d = strings.ToLower(strings.TrimSuffix(d, "."))
			if len(d) > 253 || !domainRE.MatchString(d) {
				return fmt.Errorf("moderation policy: egress domain %q is not a lowercase DNS name", (*list)[i])
			}
			(*list)[i] = d
		}
	}
	if len(*e.DenyCIDRs) > policyListMax || len(*e.MiningPorts) > policyListMax {
		return fmt.Errorf("moderation policy: egress lists hold at most %d entries", policyListMax)
	}
	e.denyCIDRs = nil
	for _, c := range *e.DenyCIDRs {
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			return fmt.Errorf("moderation policy: egress deny_cidrs entry %q is not a CIDR prefix", c)
		}
		e.denyCIDRs = append(e.denyCIDRs, pfx.Masked())
	}
	for _, p := range *e.MiningPorts {
		if p < 1 || p > 65535 {
			return errors.New("moderation policy: egress mining_ports must be 1 to 65535")
		}
	}
	return nil
}

// policySource holds the policy in force: the parameter store when it has a
// version, else the file, else version 0. Readers take the current policy
// from an atomic pointer and never do I/O: a request path may hold the only
// database connection in its own transaction (MaxOpenConns 1), and a policy
// read on the pool there would wait on itself until the request's deadline.
// The engine's own goroutine refreshes it every policyReloadEvery, outside
// any caller's transaction, and keeps the last good policy when a newer one
// does not parse or the parameter store cannot be read.
type policySource struct {
	params allowance.ParamsSource
	file   string
	db     allowance.Querier
	alert  func(Alert)
	now    func() time.Time

	cur atomic.Pointer[Policy]

	mu      sync.Mutex // serializes refresh; never taken on a request path
	lastBad string
}

// get is the policy in force. It does no I/O and takes no lock.
func (ps *policySource) get() *Policy {
	if p := ps.cur.Load(); p != nil {
		return p
	}
	return DefaultPolicy()
}

// refresh reads the policy in force and swaps it in. It does I/O on the pool,
// so it must never run inside a caller's transaction. With no policy yet (at
// New) and a policy file set, a policy that cannot be read is an error, so
// New refuses to start on it; afterwards the previous policy stays.
func (ps *policySource) refresh(ctx context.Context) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	prev := ps.cur.Load()
	p, err := ps.read(ctx, ps.now(), prev != nil)
	if err != nil {
		if prev == nil && ps.file != "" {
			return err
		}
		slog.Warn("moderation: policy not reloaded; keeping the previous version", "error", bound(err.Error(), 200))
		if msg := err.Error(); msg != ps.lastBad {
			ps.lastBad = msg
			if ps.alert != nil {
				ps.alert(Alert{Kind: "policy", Detail: "policy not loaded, keeping the previous version: " + bound(msg, 200)})
			}
		}
		if prev == nil {
			ps.cur.Store(DefaultPolicy())
		}
		return err
	}
	ps.lastBad = ""
	ps.cur.Store(p)
	return nil
}

// read reads the policy in force. A parameter store that cannot be read is an
// error once there is a previous policy to keep (have): falling through to
// the file or version 0 would silently loosen a stored policy.
func (ps *policySource) read(ctx context.Context, now time.Time, have bool) (*Policy, error) {
	if ps.params != nil {
		version, body, err := ps.params.Params(ctx, ps.db, ParamsNamespace, now.Unix())
		if err != nil && have {
			return nil, fmt.Errorf("params %s: %w", ParamsNamespace, err)
		}
		if err == nil && version > 0 && len(body) > 0 {
			p, err := parseStored(body, version)
			if err != nil {
				return nil, fmt.Errorf("params %s v%d: %w", ParamsNamespace, version, err)
			}
			p.Source = "params"
			return p, nil
		}
	}
	if ps.file != "" {
		body, err := readBounded(ps.file, PolicyBytesMax)
		if err != nil {
			return nil, fmt.Errorf("MODERATION_POLICY_FILE: %w", err)
		}
		p, err := ParsePolicy(body)
		if err != nil {
			return nil, err
		}
		p.Source = "file"
		return p, nil
	}
	return DefaultPolicy(), nil
}

// parseStored parses a policy from the parameter store, whose version is
// authoritative: the body's own version must be absent (0) or equal.
func parseStored(body []byte, version int64) (*Policy, error) {
	var probe struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Version != 0 && probe.Version != version {
		return nil, errors.New("the body's version differs from the parameter version")
	}
	if probe.Version == 0 {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, errors.New("not a JSON object")
		}
		m["version"] = json.RawMessage(fmt.Sprint(version))
		body, _ = json.Marshal(m)
	}
	return ParsePolicy(body)
}

// ParseParamsBody checks a policy for the parameter store (namespace
// "moderation"), which numbers versions itself: the body leaves "version"
// out. swarmmemo params set validates with it.
func ParseParamsBody(body []byte) (*Policy, error) {
	var probe struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Version != 0 {
		return nil, errors.New("moderation policy: leave version out; the parameter store numbers versions")
	}
	return parseStored(body, 1)
}

// DefaultParamsBody is the compiled-in version 0 as the parameter store
// publishes it (/api/params/moderation): the policy without its version.
func DefaultParamsBody() []byte {
	b, _ := json.Marshal(DefaultPolicy())
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	delete(m, "version")
	b, _ = json.Marshal(m)
	return b
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return body, nil
}

// Canonical is the policy as indented JSON, for `swarmmemo moderation policy`.
func (p *Policy) Canonical() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(p)
	return b.Bytes()
}

// SurfaceNames is the surfaces the policy configures, sorted.
func (p *Policy) SurfaceNames() []Surface {
	out := make([]Surface, 0, len(p.Surfaces))
	for s := range p.Surfaces {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var defaultLabels = map[string]string{
	"phishing":          "phishing",
	"malware":           "malware",
	"hate":              "slur or hate harassment",
	"minors":            "sexual content involving minors",
	"doxxing":           "private personal data",
	"injection":         "prompt injection aimed at AI readers",
	"manipulation":      "text aimed at the moderator",
	"mining":            "crypto mining",
	"mining_hint":       "possible crypto mining",
	"malware_hint":      "possible malware",
	"metadata_probe":    "cloud metadata access",
	"size":              "too large",
	"rate":              "rate limit",
	"private_address":   "private or reserved address",
	"metadata":          "cloud metadata address",
	"invalid_host":      "invalid host",
	"ambiguous_address": "ambiguous address literal",
	"mining_pool":       "mining pool",
	"mining_port":       "mining port",
	"denied_domain":     "denied domain",
	"new_domain":        "new domain",
	"ip_literal":        "address literal",
	"denied_address":    "denied address",
}

// flagAt is the moderation standard's flag threshold: a category at or above
// it is flagged for review. The screen service signs its receipts' verdicts
// at the same p (services.ScreenThreshold).
const flagAt = 0.60

// DefaultPolicy is version 0, compiled in: the moderation standard. A fresh
// copy each call, so callers may not alter the one in force.
func DefaultPolicy() *Policy {
	severe := func(label string, hide Action) *CategoryPolicy {
		return &CategoryPolicy{Label: label, Thresholds: []Threshold{{0.90, hide}, {flagAt, Flag}}}
	}
	flagOnly := func(label string) *CategoryPolicy {
		return &CategoryPolicy{Label: label, Thresholds: []Threshold{{flagAt, Flag}}}
	}
	hardSevere := func(label string, hide Action) *CategoryPolicy {
		return &CategoryPolicy{Label: label, Hard: true, Thresholds: []Threshold{{0.90, hide}, {flagAt, Flag}}}
	}
	hardBlock := func(label string) *CategoryPolicy {
		return &CategoryPolicy{Label: label, Hard: true, Thresholds: []Threshold{{1, Block}}}
	}
	flagAt1 := func(label string) *CategoryPolicy {
		return &CategoryPolicy{Label: label, Thresholds: []Threshold{{1, Flag}}}
	}
	// Posts: the standard. Hide phishing, malware lures, slur harassment,
	// sexual content involving minors and doxxing at p >= 0.90 (calibrated
	// against jev-1.13.0 on 2026-09-22); flag at 0.60 for review. Injection
	// and manipulation are flagged, never hidden, by default.
	post := &SurfacePolicy{
		Classifiers:   []string{"jev"},
		OnUnavailable: Flag, // fail open: the post stays up, flagged
		Burst:         &Burst{WindowSeconds: 3600, Max: 5, Mode: "downgrade"},
		Categories: map[string]*CategoryPolicy{
			"phishing":     severe("phishing", Hide),
			"malware":      severe("malware lure", Hide),
			"hate":         severe("slur or hate harassment", Hide),
			"minors":       hardSevere("sexual content involving minors", Hide),
			"doxxing":      hardSevere("private personal data", Hide),
			"injection":    flagOnly("prompt injection aimed at AI readers"),
			"manipulation": flagOnly("text aimed at the moderator"),
		},
	}
	code := &SurfacePolicy{
		Classifiers:   []string{"rules", "size", "jev"},
		OnUnavailable: Hold, // fail closed: the run waits for review
		Burst:         &Burst{WindowSeconds: 3600, Max: 20, Mode: "alert"},
		MaxBytes:      256 << 10,
		Categories: map[string]*CategoryPolicy{
			"malware": {Label: "malware", Hard: true, Thresholds: []Threshold{{0.90, Block}, {flagAt, Flag}}},
			"mining":  {Label: "crypto mining", Hard: true, Thresholds: []Threshold{{0.90, Block}, {flagAt, Flag}}},
			"size":    hardBlock("too large"),
		},
		Default: &CategoryPolicy{Thresholds: []Threshold{{flagAt, Flag}}}, // flag the rest
		Rules:   defaultCodeRules(),
	}
	egress := &SurfacePolicy{
		Classifiers:   []string{"egress", "rate"},
		OnUnavailable: Block,
		Burst:         &Burst{WindowSeconds: 3600, Max: 50, Mode: "alert"},
		Rate:          &Rate{WindowSeconds: 60, Max: 120},
		Categories: map[string]*CategoryPolicy{
			"private_address":   hardBlock("private or reserved address"),
			"metadata":          hardBlock("cloud metadata address"),
			"invalid_host":      hardBlock("invalid host"),
			"ambiguous_address": hardBlock("ambiguous address literal"),
			"mining_pool":       hardBlock("mining pool"),
			"denied_domain":     hardBlock("denied domain"),
			"denied_address":    hardBlock("denied address"),
			"rate":              hardBlock("rate limit"),
			"new_domain":        flagAt1("new domain"),
			"mining_port":       flagAt1("mining port"),
			"ip_literal":        flagAt1("address literal"),
		},
	}
	prompt := &SurfacePolicy{
		Classifiers:   []string{"jev"},
		OnUnavailable: Block, // fail closed: an unscreened prompt is never sent
		Burst:         &Burst{WindowSeconds: 3600, Max: 10, Mode: "downgrade"},
		Categories: map[string]*CategoryPolicy{
			"minors": hardSevere("sexual content involving minors", Block),
		},
		Default: &CategoryPolicy{Thresholds: []Threshold{{flagAt, Flag}}},
	}
	output := &SurfacePolicy{
		Classifiers:   []string{"jev"},
		OnUnavailable: Flag,
		Burst:         &Burst{WindowSeconds: 3600, Max: 10, Mode: "downgrade"},
		Categories: map[string]*CategoryPolicy{
			"phishing":     severe("phishing", Hide),
			"malware":      severe("malware", Hide),
			"hate":         severe("slur or hate harassment", Hide),
			"minors":       hardSevere("sexual content involving minors", Hide),
			"doxxing":      hardSevere("private personal data", Hide),
			"injection":    flagOnly("prompt injection aimed at AI readers"),
			"manipulation": flagOnly("text aimed at the moderator"),
		},
	}
	p := &Policy{
		Schema:  1,
		Version: 0,
		Jev: JevPolicy{
			Model:                 "jev-1.13.0",
			DailySpendCapMicroUSD: 2_000_000, // $2 a day
			// Screening for agents (screen.text) may use a quarter of it, so
			// board moderation always keeps $1.50.
			ScreenDailySpendCapMicroUSD: 500_000,
			PricePerMTokMicroUSD:        42_000, // $0.042 per million input tokens
			MaxTextBytes:                12_000,
			TimeoutMS:                   30_000,
		},
		Surfaces: map[Surface]*SurfacePolicy{
			SurfacePost:            post,
			SurfaceRunCode:         code,
			SurfaceRunEgress:       egress,
			SurfaceInferencePrompt: prompt,
			SurfaceInferenceOutput: output,
		},
		Egress: EgressPolicy{
			DenyCIDRs:    &[]string{},
			DenyDomains:  &[]string{},
			MiningPools:  ptr(slices.Clone(defaultMiningPools)),
			MiningPorts:  ptr(slices.Clone(defaultMiningPorts)),
			AllowDomains: &[]string{},
		},
		Source: "default",
	}
	for _, r := range code.Rules {
		r.re = regexp.MustCompile(r.Regex)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

// defaultCodeRules block the unambiguous malware and mining patterns and flag
// the hints. Each is RE2, so matching is linear in the code's size.
func defaultCodeRules() []*Rule {
	return []*Rule{
		{ID: "mining-stratum-url", Category: "mining", Regex: `(?i)stratum\+(?:tcp|ssl|tls)://`},
		{ID: "mining-miner-binary", Category: "mining", Regex: `(?i)\b(?:xmrig|xmr-stak|cpuminer|minerd|cgminer|bfgminer|ethminer|nbminer|lolminer|phoenixminer|nanominer|gminer|srbminer|t-rex)\b`},
		{ID: "mining-donate-level", Category: "mining", Regex: `(?i)--donate-level\b`},
		{ID: "mining-browser", Category: "mining", Regex: `(?i)\bcoinhive\b|CoinHive\.Anonymous`},
		{ID: "mining-algorithm", Category: "mining_hint", Regex: `(?i)\b(?:cryptonight|randomx|ethash|kawpow)\b`},
		{ID: "malware-dev-tcp-shell", Category: "malware", Regex: `(?i)(?:ba|z|da)?sh\s+-i\s*[>&]+\s*/dev/(?:tcp|udp)/`},
		{ID: "malware-nc-exec", Category: "malware", Regex: `(?i)\b(?:nc|ncat|netcat)\b[^\n]*\s-[ec]\s*/bin/(?:ba|z|da)?sh\b`},
		{ID: "malware-socat-exec", Category: "malware", Regex: `(?i)\bsocat\b[^\n]*\bexec:[^\n]*\b(?:ba|z|da)?sh\b`},
		{ID: "malware-fifo-shell", Category: "malware", Regex: `(?i)mkfifo\s+\S+[^\n]*\|\s*/bin/(?:ba|z|da)?sh\s+-i`},
		{ID: "malware-python-reverse", Category: "malware", Regex: `(?s)socket\.socket\(.{0,400}?\.connect\(.{0,400}?os\.dup2\(.{0,400}?(?:/bin/(?:ba|z|da)?sh|pty\.spawn)`},
		{ID: "malware-fork-bomb", Category: "malware", Regex: `:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`},
		{ID: "malware-wipe-root", Category: "malware", Regex: `(?i)\brm\s+-(?:rf|fr)\s+(?:--no-preserve-root\s+)?/(?:\*|\s|$)`},
		{ID: "malware-wipe-disk", Category: "malware", Regex: `(?i)\b(?:dd\s+if=/dev/(?:zero|u?random)\s+of=/dev/(?:sd|nvme|vd|xvd|hd)|mkfs(?:\.\w+)?\s+/dev/(?:sd|nvme|vd|xvd|hd))`},
		{ID: "hint-pipe-to-shell", Category: "malware_hint", Regex: `(?i)\b(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|z|da)?sh\b`},
		{ID: "hint-decode-to-shell", Category: "malware_hint", Regex: `(?i)base64\s+(?:-d|--decode)\b[^\n]*\|\s*(?:ba|z|da)?sh\b`},
		{ID: "hint-metadata", Category: "metadata_probe", Regex: `(?i)169\.254\.169\.254|169\.254\.170\.2\b|metadata\.google\.internal|100\.100\.100\.200|fd00:ec2::254`},
	}
}

var defaultMiningPools = []string{
	"2miners.com", "antpool.com", "c3pool.com", "coinhive.com", "ethermine.org", "f2pool.com",
	"flypool.org", "hashvault.pro", "herominers.com", "hiveon.net", "minergate.com", "minexmr.com",
	"mining-dutch.nl", "moneroocean.stream", "nanopool.org", "nicehash.com", "poolin.com",
	"prohashing.com", "slushpool.com", "braiins.com", "supportxmr.com", "unmineable.com",
	"viabtc.com", "xmrpool.eu", "zpool.ca", "kryptex.network", "woolypooly.com", "k1pool.com",
}

var defaultMiningPorts = []int{3333, 4444, 5555, 7777, 9999, 14433, 14444, 45560, 45700}
