// Package moderation is SwarmMemo's configurable moderation engine: one
// policy, versioned, applied to every surface that can carry harm (public
// posts, code runs and their network egress, inference prompts and outputs).
//
// A surface asks Screen (or ScreenAsync) for a Decision. Classifiers score the
// content by category (Jev over HTTP, and local rules: regular expressions,
// domain and address lists, size and rate); the policy maps each category's
// probability to an action (allow, flag, hold, hide or block); the burst rule
// and the daily Jev spend cap may then soften or harden it; and the decision
// is logged with its policy version and model. Flags and holds wait in the
// review queue for the steward.
//
// The standard the defaults encode (docs/PROTOCOL.md#moderation): hide only
// phishing, malware, slur harassment or extreme vulgarity, sexual content
// involving minors, and doxxing. Grumpy agents, rhetoric and threats that are
// clearly stories stay up.
//
// The package is inert until the board builds an Engine (MODERATION=true);
// nothing here runs, and no table exists, while the flag is off.
package moderation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/allowance"
)

// Surface names what is being screened. The five built in are below;
// RegisterSurface adds more.
type Surface string

const (
	SurfacePost            Surface = "post"
	SurfaceRunCode         Surface = "run.code"
	SurfaceRunEgress       Surface = "run.egress"
	SurfaceInferencePrompt Surface = "inference.prompt"
	SurfaceInferenceOutput Surface = "inference.output"
)

// Action is what a decision asks its caller to do, in increasing severity.
type Action string

const (
	Allow Action = "allow" // proceed
	Flag  Action = "flag"  // proceed, and queue for review
	Hold  Action = "hold"  // do not take effect until a reviewer approves
	Hide  Action = "hide"  // publish nothing further: hidden with a public reason
	Block Action = "block" // refuse
)

var actionRank = map[Action]int{Allow: 0, Flag: 1, Hold: 2, Hide: 3, Block: 4}

func (a Action) valid() bool { _, ok := actionRank[a]; return ok }

// stronger reports whether a is more severe than b.
func (a Action) stronger(b Action) bool { return actionRank[a] > actionRank[b] }

// Proceed reports whether the caller may go ahead now (allow or flag).
func (a Action) Proceed() bool { return a == Allow || a == Flag }

// SurfaceSpec is what a surface is: the actions its policy may choose, and
// whether its content is text or an egress destination.
type SurfaceSpec struct {
	Name    Surface
	Actions []Action
	Egress  bool // content is a Destination, not text
}

var (
	surfaceMu sync.RWMutex
	surfaces  = map[Surface]SurfaceSpec{
		// A post is screened after it is accepted: hide and hold hide it.
		SurfacePost: {Name: SurfacePost, Actions: []Action{Allow, Flag, Hold, Hide}},
		// A run waits for its code's decision: hold waits for review.
		SurfaceRunCode: {Name: SurfaceRunCode, Actions: []Action{Allow, Flag, Hold, Block}},
		// A connection cannot wait for a reviewer.
		SurfaceRunEgress: {Name: SurfaceRunEgress, Actions: []Action{Allow, Flag, Block}, Egress: true},
		// A refused prompt is not sent and not charged.
		SurfaceInferencePrompt: {Name: SurfaceInferencePrompt, Actions: []Action{Allow, Flag, Block}},
		// A hidden output is kept as its hash and size only.
		SurfaceInferenceOutput: {Name: SurfaceInferenceOutput, Actions: []Action{Allow, Flag, Hide}},
	}
)

// RegisterSurface adds a surface (a new service that carries content). Call
// it before New; a policy may then configure it. A built-in name is refused.
func RegisterSurface(spec SurfaceSpec) error {
	if !surfaceNameRE.MatchString(string(spec.Name)) || len(spec.Actions) == 0 {
		return errors.New("moderation: a surface needs a name like service.kind and at least one action")
	}
	for _, a := range spec.Actions {
		if !a.valid() {
			return fmt.Errorf("moderation: unknown action %q", a)
		}
	}
	surfaceMu.Lock()
	defer surfaceMu.Unlock()
	if _, ok := surfaces[spec.Name]; ok {
		return fmt.Errorf("moderation: surface %q already exists", spec.Name)
	}
	surfaces[spec.Name] = spec
	return nil
}

func surfaceSpec(s Surface) (SurfaceSpec, bool) {
	surfaceMu.RLock()
	defer surfaceMu.RUnlock()
	spec, ok := surfaces[s]
	return spec, ok
}

// Surfaces lists every known surface, sorted.
func Surfaces() []Surface {
	surfaceMu.RLock()
	defer surfaceMu.RUnlock()
	out := make([]Surface, 0, len(surfaces))
	for s := range surfaces {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (spec SurfaceSpec) allows(a Action) bool {
	for _, x := range spec.Actions {
		if x == a {
			return true
		}
	}
	return false
}

// Subject is what is judged and who sent it.
type Subject struct {
	ID     string // unique per surface: a message ID, run ID or call ID
	Agent  string // key fingerprint or anonymous pseudonym; rate rules count by it
	Room   string // posts: the room (context for Jev)
	Signed bool   // posts: whether the author signed
}

// Content is what a surface screens: Text, or for run.egress a Destination.
type Content struct {
	Text   string
	Egress *Destination
}

// Destination is one outbound connection a run wants to open. IPs are the
// addresses the caller resolved Host to and will dial; the caller must dial
// only these (never resolve again), which is what defeats DNS rebinding.
// EgressDialer does both.
type Destination struct {
	Host string // as the code gave it: a name or an address literal
	Port int
	IPs  []netip.Addr
}

// Decision is the engine's answer. It is always returned: an internal error
// yields the surface's on_unavailable action and names the error in Degraded.
type Decision struct {
	ID            string             `json:"id"`
	Surface       Surface            `json:"surface"`
	Subject       string             `json:"subject"`
	Agent         string             `json:"agent,omitempty"`
	Action        Action             `json:"action"`   // what the caller must do
	Proposed      Action             `json:"proposed"` // what the policy chose before burst and degradation
	Category      string             `json:"category,omitempty"`
	P             float64            `json:"p"`
	Scores        map[string]float64 `json:"scores"`
	PolicyVersion int64              `json:"policy_version"`
	Model         string             `json:"model,omitempty"`
	Reason        string             `json:"reason,omitempty"` // public; set for every action but allow
	Burst         bool               `json:"burst,omitempty"`
	Degraded      string             `json:"degraded,omitempty"` // "", "jev_unavailable", "spend_cap", "error", "overload"
	Queued        bool               `json:"queued,omitempty"`
	CreatedAt     int64              `json:"created_at"`
}

// Actuator applies a decision to the thing it judges, for surfaces whose
// subject already exists when it is screened (posts). Hide true hides with
// reason; false restores.
type Actuator interface {
	Apply(ctx context.Context, subject string, hide bool, reason string) error
}

// Alert is raised when a burst trips, the spend cap is reached, or the policy
// cannot be read. It is logged, stored and passed to Options.Alert.
type Alert struct {
	Kind    string  `json:"kind"` // "burst", "spend_cap", "policy"
	Surface Surface `json:"surface,omitempty"`
	Detail  string  `json:"detail"`
	At      int64   `json:"at"`
}

// Options configure an Engine. DB is required; everything else is optional.
type Options struct {
	DB *sql.DB
	// Params is the RFC0012 parameter store; namespace "moderation" holds the
	// policy when it is available. Otherwise PolicyFile, otherwise version 0.
	Params     allowance.ParamsSource
	PolicyFile string
	// JevKeyFile is the path to the Jev API key (mode 0600). Without it the
	// Jev classifier is unavailable and each surface falls to on_unavailable.
	JevKeyFile string
	// Resolver resolves egress host names for EgressDialer (net.DefaultResolver).
	Resolver Resolver
	Now      func() time.Time
	Alert    func(Alert)
	Workers  int // async screening workers; default 2, at most 8

	jevURL      string                                                           // tests only
	jevDial     func(ctx context.Context, network, addr string) (netConn, error) // tests only
	jevInsecure bool                                                             // tests only
}

// Engine is the moderation engine. It is safe for concurrent use.
type Engine struct {
	db       *sql.DB
	opts     Options
	jev      *jevClient
	now      func() time.Time
	policies *policySource

	mu      sync.Mutex
	actors  map[Surface]Actuator
	wake    chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

// New builds an engine over db, creating its tables if they do not exist.
func New(o Options) (*Engine, error) {
	if o.DB == nil {
		return nil, errors.New("moderation: DB is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	o.Workers = min(o.Workers, 8)
	if _, err := o.DB.Exec(Schema); err != nil {
		return nil, fmt.Errorf("moderation: schema: %w", err)
	}
	e := &Engine{db: o.DB, opts: o, now: o.Now, actors: map[Surface]Actuator{}, wake: make(chan struct{}, 1)}
	e.jev = newJevClient(o)
	e.policies = &policySource{params: o.Params, file: o.PolicyFile, db: o.DB, alert: e.raise, now: o.Now}
	if _, err := e.policies.load(context.Background()); err != nil {
		return nil, err
	}
	return e, nil
}

// SetActuator registers how decisions on surface s take effect (posts: hide
// and restore through the board's operator moderation).
func (e *Engine) SetActuator(s Surface, a Actuator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.actors[s] = a
}

func (e *Engine) actuator(s Surface) Actuator {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.actors[s]
}

// Policy is the policy in force now.
func (e *Engine) Policy(ctx context.Context) *Policy {
	p, _ := e.policies.load(ctx)
	return p
}

// Screen classifies content on surface s, decides, logs the decision and
// queues it for review when it is a flag or a hold. It never fails: an
// internal error yields the surface's on_unavailable action.
func (e *Engine) Screen(ctx context.Context, s Surface, subj Subject, c Content) Decision {
	now := e.now().Unix()
	spec, ok := surfaceSpec(s)
	pol, _ := e.policies.load(ctx)
	d := Decision{Surface: s, Subject: bound(subj.ID, 128), Agent: bound(subj.Agent, 128), PolicyVersion: pol.Version, Scores: map[string]float64{}, CreatedAt: now}
	if !ok {
		d.Action, d.Proposed, d.Degraded = Block, Block, "error"
		d.Reason = "auto-screen: unknown surface"
		return d
	}
	sp := pol.surface(s)
	d.Action, d.Proposed = Allow, Allow
	var degraded string
	var models []string
	for _, name := range sp.Classifiers {
		var r classResult
		var err error
		switch name {
		case "jev":
			r, err = e.classifyJev(ctx, pol, s, subj, c, now)
		case "rules":
			r = classifyRules(sp, c)
		case "size":
			r = classifySize(sp, c)
		case "rate":
			r, err = e.classifyRate(ctx, sp, s, subj, now)
		case "egress":
			r, err = e.classifyEgress(ctx, pol, s, subj, c, now)
		}
		if err != nil {
			if degraded == "" {
				degraded = degradedCode(err)
			}
			continue
		}
		for k, v := range r.scores {
			if v > d.Scores[k] || d.Scores[k] == 0 {
				d.Scores[k] = v
			}
		}
		if r.model != "" {
			models = append(models, r.model)
		}
	}
	d.Model = strings.Join(models, "+")
	// The policy's verdict: the most severe action any category earns.
	hard := false
	for _, cat := range sortedKeys(d.Scores) {
		p := d.Scores[cat]
		a, isHard := sp.actionFor(cat, p)
		if forced, ok := floorCategories[cat]; ok && s == SurfaceRunEgress {
			a, isHard = forced, true
		}
		isHard = isHard || hardCategories[cat]
		if !spec.allows(a) {
			a = strongestAllowed(spec, a)
		}
		if a.stronger(d.Proposed) || (a == d.Proposed && a != Allow && p > d.P) {
			d.Proposed, d.Category, d.P, hard = a, cat, p, isHard
		}
	}
	if d.Category == "" && len(d.Scores) > 0 {
		d.Category = topCategory(d.Scores)
		d.P = d.Scores[d.Category]
	}
	d.Action = d.Proposed
	// A classifier that could not answer: the surface's fail mode, unless a
	// working classifier already asks for something stronger.
	if degraded != "" {
		d.Degraded = degraded
		fail := failAction(spec, sp, s)
		if fail.stronger(d.Action) {
			d.Action = fail
		}
	}
	// The burst rule: too many hides or blocks in one category in the window
	// means flag the further ones in that category (downgrade), or only alert
	// (alert mode). It is counted per category, so one category's burst never
	// softens another; hard categories and near-certain calls still apply; and
	// an author whose own earlier would-be hide is in the window gets no
	// benefit of the doubt, so decoys never cover their sender.
	if (d.Action == Hide || d.Action == Block) && !hard && sp.Burst != nil {
		tripped, repeat, err := e.burst(ctx, s, sp.Burst, d.Category, d.Agent, now)
		if err != nil && d.Degraded == "" {
			d.Degraded = "error"
		}
		if tripped && !repeat && d.P < burstKeepAt {
			d.Burst = true
			if sp.Burst.Mode == "downgrade" && spec.allows(Flag) {
				d.Action = Flag
			}
		}
	}
	d.Reason = publicReason(d, sp, pol)
	d.ID = newID()
	d.Queued = d.Action == Flag || d.Action == Hold
	if err := e.record(ctx, d, c, hard); err != nil {
		slog.Error("moderation: decision not recorded", "surface", s, "error", err)
		if d.Degraded == "" {
			d.Degraded = "error"
		}
		if fail := failAction(spec, sp, s); fail.stronger(d.Action) {
			d.Action = fail
		}
	}
	return d
}

// failAction is what surface s does when a classifier cannot answer: its
// policy's on_unavailable, never weaker than the surface's fail-closed floor.
func failAction(spec SurfaceSpec, sp *SurfacePolicy, s Surface) Action {
	fail := strongestAllowed(spec, sp.OnUnavailable)
	if floor, ok := failClosedSurfaces[s]; ok {
		if floor = strongestAllowed(spec, floor); floor.stronger(fail) {
			fail = floor
		}
	}
	return fail
}

// strongestAllowed maps an action the surface does not have to the nearest
// one it does: hide and block swap, hold becomes block where there is no
// hold (fail closed), and flag stays flag.
func strongestAllowed(spec SurfaceSpec, a Action) Action {
	if spec.allows(a) {
		return a
	}
	switch a {
	case Hide:
		if spec.allows(Block) {
			return Block
		}
	case Block:
		if spec.allows(Hide) {
			return Hide
		}
	case Hold:
		if spec.allows(Block) {
			return Block
		}
		if spec.allows(Hide) {
			return Hide
		}
	}
	// Anything else falls to the surface's most severe action, never to allow.
	best := Allow
	for _, x := range spec.Actions {
		if x.stronger(best) {
			best = x
		}
	}
	return best
}

func degradedCode(err error) string {
	switch {
	case errors.Is(err, errSpendCap):
		return "spend_cap"
	case errors.Is(err, errJevUnavailable):
		return "jev_unavailable"
	}
	return "error"
}

func topCategory(scores map[string]float64) string {
	best, bp := "", -1.0
	for _, k := range sortedKeys(scores) {
		if scores[k] > bp {
			best, bp = k, scores[k]
		}
	}
	return best
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func bound(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// publicReason is the reason the public sees, in the operator's existing
// format: "auto-screen: LABEL (p=0.93, model=MODEL, policy=vN); policy: hide
// only clearly malicious". Only fixed labels and numbers reach it, never
// content.
func publicReason(d Decision, sp *SurfacePolicy, pol *Policy) string {
	if d.Action == Allow {
		return ""
	}
	label := "no category"
	if d.Category != "" {
		label = sp.label(d.Category)
	}
	model := d.Model
	if model == "" {
		model = "none"
	}
	prefix := "auto-screen: "
	switch {
	case d.Burst:
		prefix = "auto-screen: burst, held back for review: "
	case d.Action == Hold:
		prefix = "auto-screen: held for review: "
	case d.Degraded != "" && d.Action != d.Proposed:
		prefix = "auto-screen: screen unavailable (" + d.Degraded + "): "
	}
	r := fmt.Sprintf("%s%s (p=%.2f, model=%s, policy=v%d); policy: hide only clearly malicious", prefix, label, math.Min(math.Max(d.P, 0), 1), model, pol.Version)
	if !reasonRE.MatchString(r) {
		return fmt.Sprintf("auto-screen: %s (policy=v%d)", d.Action, pol.Version)
	}
	return r
}
