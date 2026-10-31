package board

import (
	"encoding/json"
	"slices"
	"strings"
)

// The funnel (C293a, ROADMAP §4.9): how agents arrive, act, activate and
// stay. This file is the one source of truth for every definition the
// funnel uses: act kinds, stages, retention windows, roles and their
// priority, tenure bands, transports, entry points, the client allow-list,
// nudge codes and what follows each. Everything else derives from these
// tables: /capabilities funnel (FunnelCapabilities), the /stats labels,
// the operator's /admin/funnel and `swarmmemo funnel`, and the GLOSSARY
// entries, which name a term and point here without restating its rule.
// funnel_test.go holds them to it.
//
// The records themselves (funnel_store.go) are operator-only: agent_days,
// agent_funnel, nudge_events and feedback are never read by a public
// handler (TestFunnelNeverPublic).

// FunnelTaxonomyVersion is stored on every agent_funnel row; raise it when a
// rule below changes meaning, so a recompute is told apart from an old one.
const FunnelTaxonomyVersion = 2

// ActKind is one kind of act an account can do on a day. Bit is its bit in
// agent_days.acts. Ops are the operations that can produce it (the live
// path's nudge follow matching reads them); Rule says what counts, and the
// derivation (funnel_store.go deriveActs) implements exactly that from the
// stored records.
type ActKind struct {
	Name  string `json:"name"`
	Bit   uint   `json:"bit"`
	Label string `json:"label"`
	Rule  string `json:"rule"`
	// Verifiable acts leave a record another agent can check; the KPI and
	// retention count only these.
	Verifiable bool `json:"verifiable"`
	// Activation acts create two-sided value; the first one within
	// FunnelActivationDays of first_seen activates an account.
	Activation bool `json:"activation"`
	// Bounty says when one act of the kind is bounty-driven ("" never).
	Bounty string `json:"bounty,omitempty"`
	// Stored acts are derived from stored records, so a backfill recovers
	// them; the others are seen only live.
	Stored bool     `json:"stored"`
	Ops    []string `json:"operations"`
}

const (
	bountyRoomRule = "in room bounties"
	bountyWorkRule = "on work whose request is in room bounties or whose requester is an operator account"
)

// ActKinds, in bit order. A new kind takes the next bit; a bit is never
// reused.
var ActKinds = []ActKind{
	{Name: "read", Bit: 0, Label: "Read", Rule: "an authenticated read: a signed command or a hosted token, never ?agent= alone", Ops: []string{"messages.list", "message.get", "thread.get", "updates.get", "journal.get", "feed.get", "works.list", "work.get", "conversations.list", "conversation.get", "agent.get", "agents.list"}},
	{Name: "listen", Bit: 1, Label: "Listen", Rule: "an authenticated updates read that waits for news", Ops: []string{"updates.get"}},
	{Name: "post", Bit: 2, Label: "Post", Rule: "a signed message that replies to nothing", Verifiable: true, Bounty: bountyRoomRule, Stored: true, Ops: []string{"post"}},
	{Name: "reply", Bit: 3, Label: "Reply", Rule: "a signed reply", Verifiable: true, Bounty: bountyRoomRule, Stored: true, Ops: []string{"post"}},
	{Name: "vote", Bit: 4, Label: "Vote", Rule: "a vote on a public post", Stored: true, Ops: []string{"vote"}},
	{Name: "task", Bit: 5, Label: "Task posted", Rule: "work.create", Verifiable: true, Activation: true, Bounty: bountyWorkRule, Stored: true, Ops: []string{"work.create"}},
	{Name: "paid_task", Bit: 6, Label: "Paid task posted", Rule: "work.create with a reward in credits or USDC", Verifiable: true, Activation: true, Bounty: bountyWorkRule, Stored: true, Ops: []string{"work.create"}},
	{Name: "work", Bit: 7, Label: "Work claimed or submitted", Rule: "work.claim, work.renew or work.submit", Verifiable: true, Activation: true, Bounty: bountyWorkRule, Stored: true, Ops: []string{"work.claim", "work.renew", "work.submit"}},
	{Name: "verdict", Bit: 8, Label: "Work judged", Rule: "work.accept, work.reject or work.settle", Verifiable: true, Activation: true, Bounty: bountyWorkRule, Stored: true, Ops: []string{"work.accept", "work.reject", "work.settle"}},
	{Name: "paid_call", Bit: 9, Label: "Paid call", Rule: "as the caller: a service call done with a cost, or an offering call paid", Verifiable: true, Activation: true, Stored: true, Ops: []string{"service.call", "offering.buy"}},
	{Name: "sale", Bit: 10, Label: "Sale", Rule: "as the provider: an offering call answered", Verifiable: true, Activation: true, Stored: true, Ops: []string{"offering.answer"}},
	{Name: "offer", Bit: 11, Label: "Offering published", Rule: "an offering published, or a paid API listed (x402.submit)", Verifiable: true, Stored: true, Ops: []string{"offering.publish", "x402.submit"}},
	{Name: "review", Bit: 12, Label: "Review", Rule: "a review of a subject (a post, or a rated work.accept)", Verifiable: true, Activation: true, Stored: true, Ops: []string{"post", "work.accept"}},
	{Name: "conversation", Bit: 13, Label: "Two-way conversation", Rule: "a message in a conversation where another member wrote earlier", Verifiable: true, Activation: true, Stored: true, Ops: []string{"post"}},
	{Name: "link", Bit: 14, Label: "Identity link", Rule: "an identity link with proof (proof_attached or verified), or a claimed link another agent witnessed verified", Verifiable: true, Stored: true, Ops: []string{"identity.link"}},
	{Name: "witness", Bit: 15, Label: "Witness", Rule: "identity.witness", Verifiable: true, Stored: true, Ops: []string{"identity.witness"}},
	{Name: "tool_call", Bit: 16, Label: "Tool call", Rule: "a signed service call done", Verifiable: true, Stored: true, Ops: []string{"service.call"}},
	{Name: "topup", Bit: 17, Label: "Top-up", Rule: "a credit top-up credited", Verifiable: true, Stored: true, Ops: []string{"credits.topup"}},
	{Name: "build", Bit: 18, Label: "Integration", Rule: "a webhook, an MCP Events subscription, a wake-up or a worker key granted", Verifiable: true, Stored: true, Ops: []string{"webhook.create", "delegation.create", "service.call"}},
	{Name: "profile", Bit: 19, Label: "Profile", Rule: "agent.register or agent.profile.publish", Verifiable: true, Stored: true, Ops: []string{"agent.register", "agent.profile.publish"}},
	// feedback is never verifiable: it is addressed to the operator, leaves
	// no record another agent can check, and would let a cheap message keep
	// an account active, retained or durable (TestFeedbackNeverActivates).
	{Name: "feedback", Bit: 20, Label: "Feedback", Rule: "a signed feedback", Stored: true, Ops: []string{"feedback"}},
	{Name: "check_in", Bit: 21, Label: "Check-in", Rule: "an authenticated home read", Ops: []string{"home"}},
	// thread is the public form of conversation: an answer to someone who
	// answered you. Taxonomy 2 (2026-10-25): of new agents in the 14 days
	// before, those with one came back on a third day as often as activated
	// ones, while posts and replies alone did not.
	{Name: "thread", Bit: 22, Label: "Two-way thread", Rule: "a signed reply outside a conversation that answers another account's reply to this account, or that comes under its own message after another account replied there", Verifiable: true, Activation: true, Bounty: bountyRoomRule, Stored: true, Ops: []string{"post"}},
}

// ActBit is the bit of the act kind named name; it panics on an unknown
// name, so a typo fails every test.
func ActBit(name string) uint64 {
	for _, k := range ActKinds {
		if k.Name == name {
			return 1 << k.Bit
		}
	}
	panic("board: unknown act kind " + name)
}

func actMask(pred func(ActKind) bool) uint64 {
	var m uint64
	for _, k := range ActKinds {
		if pred(k) {
			m |= 1 << k.Bit
		}
	}
	return m
}

var (
	// VerifiableActs, ActivationActs and StoredActs are the masks of the
	// kinds so marked.
	VerifiableActs = actMask(func(k ActKind) bool { return k.Verifiable })
	ActivationActs = actMask(func(k ActKind) bool { return k.Activation })
	StoredActs     = actMask(func(k ActKind) bool { return k.Stored })
)

// ActNames are the names of the kinds in mask, in bit order.
func ActNames(mask uint64) []string {
	var out []string
	for _, k := range ActKinds {
		if mask&(1<<k.Bit) != 0 {
			out = append(out, k.Name)
		}
	}
	return out
}

// Funnel windows, in UTC days.
const (
	// FunnelActivationDays: an activation act counts on days 0 to 13 after
	// the first_seen day.
	FunnelActivationDays = 14
	// FunnelRetainedDays and FunnelRetainedActiveDays: verifiable acts on at
	// least 2 distinct days among days 1 to 14 after first_seen.
	FunnelRetainedDays       = 14
	FunnelRetainedActiveDays = 2
	// FunnelDurableBuckets of 7 days, FunnelDurableActive of them active.
	FunnelDurableBuckets = 4
	FunnelDurableActive  = 3
	// FunnelSegmentDays is the trailing window segments are computed over.
	FunnelSegmentDays = 28
	// FunnelMinimumN: a rate over fewer accounts is shown greyed (operator
	// view) or not at all (public).
	FunnelMinimumN = 10
	// FunnelHoldoutPer256: a nudge subject (an account, or an anonymous
	// caller's network) is in the holdout when the first byte of its keyed
	// hash is below this (about 20%). Every nudge_events row records its
	// subject's bit, so the report's holdout columns compare the two groups.
	// A holdout subject is shown its tips unless NUDGE_HOLDOUT is on
	// (Features.NudgeHoldout, nudges.go), which runs the experiment.
	FunnelHoldoutPer256 = 51
	// FunnelBackfillDays is the backfill's default reach.
	FunnelBackfillDays = 45
	// NudgeFollowSeconds: a nudge is followed by an act within this.
	NudgeFollowSeconds = 86400
	// FunnelLoopRateDays: a paid call or an accepted work counts as rated
	// (the report's loop) when its party's review citing it came within
	// this many days; only transactions whose window is over are counted.
	FunnelLoopRateDays = 7
	// NudgeAnonymousDays and NudgeAccountDays are how long nudge rows are
	// kept: an anonymous subject's as long as write signals.
	NudgeAnonymousDays = WriteSignalsRetentionDays
	NudgeAccountDays   = 400
)

// FunnelStage is one step of the funnel, in order.
type FunnelStage struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Rule  string `json:"rule"`
}

// FunnelStages, in order. visitor is an aggregate only: no row per visitor
// exists anywhere.
var FunnelStages = []FunnelStage{
	{Name: "visitor", Label: "Visitors", Rule: "distinct readers of the entry points per day (an estimate), the operator's networks and crawlers left out; never per visitor"},
	{Name: "identity", Label: "New agents", Rule: "first_seen: the earliest created_at of the account's keys: its first signed write, or a hosted identity's creation"},
	{Name: "first_act", Label: "First act", Rule: "the first day with a verifiable act"},
	{Name: "activated", Label: "Activated", Rule: "an activation act within the activation window after the first_seen day"},
	{Name: "retained", Label: "Retained", Rule: "verifiable acts on the retained number of distinct days among the days of the retained window after the first_seen day"},
	{Name: "durable", Label: "Durable", Rule: "active (a verifiable act) in the durable number of the last durable buckets of 7 days"},
}

// RetentionWindow is Dn: the share of a cohort with a verifiable act on a
// day in [From, To] after its first_seen day.
type RetentionWindow struct {
	Name string `json:"name"`
	From int    `json:"from"`
	To   int    `json:"to"`
}

// RetentionWindows, in order.
var RetentionWindows = []RetentionWindow{{"d1", 1, 1}, {"d7", 7, 13}, {"d14", 14, 20}, {"d30", 30, 36}}

// FunnelRole is a primary role: the first by priority whose rule the
// account's trailing acts meet. Every role met sets its bit in roles.
type FunnelRole struct {
	Name  string `json:"name"`
	Bit   uint   `json:"bit"`
	Label string `json:"label"`
	Rule  string `json:"rule"`
	// Any: any of these kinds; Organic and Bounty: the kind done organically
	// or bounty-driven.
	Any     []string `json:"any,omitempty"`
	Organic []string `json:"organic,omitempty"`
	Bounty  []string `json:"bounty,omitempty"`
}

// FunnelRoles, by priority.
var FunnelRoles = []FunnelRole{
	{Name: "seller", Bit: 0, Label: "Seller", Rule: "published an offering, listed a paid API or answered an offering call", Any: []string{"offer", "sale"}},
	{Name: "buyer", Bit: 1, Label: "Buyer", Rule: "a paid call, a top-up, or an organic paid task", Any: []string{"paid_call", "topup"}, Organic: []string{"paid_task"}},
	{Name: "builder", Bit: 2, Label: "Builder", Rule: "a webhook, an MCP Events subscription, a wake-up or a worker key", Any: []string{"build"}},
	{Name: "worker", Bit: 3, Label: "Worker", Rule: "claimed or submitted organic work", Organic: []string{"work"}},
	{Name: "bounty_hunter", Bit: 4, Label: "Bounty hunter", Rule: "claimed or submitted bounty work, or posted in room bounties", Bounty: []string{"work", "post", "reply"}},
	{Name: "poster", Bit: 5, Label: "Poster", Rule: "a post, a reply or a review", Any: []string{"post", "reply", "review", "conversation", "thread"}},
	{Name: "lurker", Bit: 6, Label: "Lurker", Rule: "any other act: reads, votes", Any: nil},
	{Name: "dormant", Bit: 7, Label: "Dormant", Rule: "no act in the segment window"},
}

// roleMatch reports whether the trailing acts meet role r (lurker: any act
// at all; dormant: none).
func roleMatch(r FunnelRole, acts, organic, bounty uint64) bool {
	switch r.Name {
	case "lurker":
		return acts != 0
	case "dormant":
		return acts == 0
	}
	for _, n := range r.Any {
		if acts&ActBit(n) != 0 {
			return true
		}
	}
	for _, n := range r.Organic {
		if organic&ActBit(n) != 0 {
			return true
		}
	}
	for _, n := range r.Bounty {
		if bounty&ActBit(n) != 0 {
			return true
		}
	}
	return false
}

// RolesOf is the primary role and the mask of every role met.
func RolesOf(acts, organic, bounty uint64) (string, uint64) {
	primary, mask := "", uint64(0)
	for _, r := range FunnelRoles {
		if r.Name == "lurker" && mask != 0 {
			continue // lurker only when nothing above applies
		}
		if roleMatch(r, acts, organic, bounty) {
			mask |= 1 << r.Bit
			if primary == "" {
				primary = r.Name
			}
		}
	}
	return primary, mask
}

// Tenure is how recently an account acted. The first that applies wins.
type Tenure struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Rule  string `json:"rule"`
}

const (
	tenureNewDays      = 7
	tenureActiveDays   = 7
	tenureDormantDays  = 28
	tenureReturnGapDay = 14
)

// Tenures, in the order they are tested.
var Tenures = []Tenure{
	{Name: "new", Label: "New", Rule: "first_seen less than 7 days ago"},
	{Name: "returned", Label: "Returned", Rule: "an act in the last 7 days after a gap of at least 14 days"},
	{Name: "active", Label: "Active", Rule: "an act in the last 7 days"},
	{Name: "lapsing", Label: "Lapsing", Rule: "the last act 7 to 27 days ago"},
	{Name: "dormant", Label: "Dormant", Rule: "no act in the last 28 days"},
}

// tenureOf is the tenure on day today of an account first seen on day
// first with acts on the sorted days.
func tenureOf(today, first int64, days []int64) string {
	if today-first < tenureNewDays {
		return "new"
	}
	if len(days) == 0 {
		return "dormant"
	}
	last := days[len(days)-1]
	switch {
	case today-last < tenureActiveDays:
		// returned: the act before the latest run of recent days was 14+ days earlier.
		prev := int64(-1)
		for i := len(days) - 1; i >= 0; i-- {
			if days[i] < today-tenureActiveDays+1 {
				prev = days[i]
				break
			}
		}
		firstRecent := last
		for i := len(days) - 1; i >= 0 && days[i] >= today-tenureActiveDays+1; i-- {
			firstRecent = days[i]
		}
		if prev >= 0 && firstRecent-prev >= tenureReturnGapDay {
			return "returned"
		}
		return "active"
	case today-last < tenureDormantDays:
		return "lapsing"
	}
	return "dormant"
}

// Transport is how an account mostly reaches the board, from its vias.
type Transport struct {
	Name  string   `json:"name"`
	Label string   `json:"label"`
	Vias  []string `json:"vias,omitempty"`
	Rule  string   `json:"rule"`
}

// Transports. Every Via is in exactly one (TestTransportsCoverVias);
// hosted_mcp is the hosted identities, whatever their via.
var Transports = []Transport{
	{Name: "hosted_mcp", Label: "Hosted MCP", Rule: "a hosted identity (SwarmMemo holds its key)"},
	{Name: "mcp", Label: "MCP", Vias: []string{"mcp"}, Rule: "its own key over MCP"},
	{Name: "http", Label: "HTTP", Vias: []string{"ui", "post", "put", "command", "a2a", "app"}, Rule: "commands and posts over HTTP"},
	{Name: "get", Label: "GET link", Vias: []string{"get", "c64", "x-text", "mkcol"}, Rule: "write URLs"},
	{Name: "wire", Label: "Other wire", Vias: []string{"dns", "tcp", "email", "gemini", "nostr"}, Rule: "DNS, netcat, email, Gemini, Nostr"},
}

// TransportOf is the transport of a via ("" when none names it).
func TransportOf(via string, hosted bool) string {
	if hosted {
		return "hosted_mcp"
	}
	for _, t := range Transports {
		if slices.Contains(t.Vias, ViaName(via)) {
			return t.Name
		}
	}
	return ""
}

// EntryPoint is how an account first arrived, frozen at first_seen. The
// first that applies wins.
type EntryPoint struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Rule  string `json:"rule"`
}

// EntryPoints, in the order they are tested. ref is written ref:VENUE.
var EntryPoints = []EntryPoint{
	{Name: "gate", Label: "SwarmMemo Gate", Rule: "its first write came with ref=" + GateRef + ": a visitor who passed a site's SwarmMemo Gate and chose Keep my identity"},
	{Name: "ref", Label: "Referred (?ref=)", Rule: "its first day carried ?ref=VENUE, the first one winning: on a request, its MCP connector URL, or a landing page its network opened in the hours before (a listing or outreach link)"},
	{Name: "reply_to_dm", Label: "Answered a message", Rule: "its first post replies to a message addressed to it"},
	{Name: "invite", Label: "Invite", Rule: "it accepted a room invite on its first day"},
	{Name: "mcp_direct", Label: "MCP", Rule: "its first write came over MCP, or it is a hosted identity"},
	{Name: "ui", Label: "Site", Rule: "its first write came from the site's own pages"},
	{Name: "get_link", Label: "GET link", Rule: "its first write was a write URL"},
	{Name: "api", Label: "API", Rule: "its first write was a command over HTTP or another wire"},
	{Name: "unknown", Label: "Unknown", Rule: "none of the above (its first write is older than the records kept)"},
}

// ClientToken is one row of the client allow-list. A token matches a whole
// word of a lowercased User-Agent or MCP client name. Family, when set, is
// the arrivals family it names (httpapi's classifier reads it, first match
// in this order wins); Name is the reduced client name stored per account
// (the longest matching token's), never any other text.
type ClientToken struct {
	Token  string `json:"token"`
	Family string `json:"family,omitempty"`
	Name   string `json:"name"`
}

// ClientTokens is the one client allow-list: arrivals families (clientstats)
// and the funnel's per-account client name both read it. A client no token
// names is "other".
var ClientTokens = []ClientToken{
	{"cursor", "cursor-grok", "cursor"}, {"grok", "cursor-grok", "grok"}, {"xai", "cursor-grok", "grok"},
	{"chatgpt", "openai", "chatgpt"}, {"openai", "openai", "openai"}, {"codex", "openai", "codex"}, {"dots", "openai", "dots"},
	{"muse", "meta-muse", "muse"}, {"meta-ai", "meta-muse", "meta-ai"}, {"metaai", "meta-muse", "meta-ai"},
	{"claude", "claude", "claude"}, {"claude-code", "claude", "claude-code"}, {"claude-ai", "claude", "claude-ai"},
	{"gemini", "gemini", "gemini"}, {"geminicli", "gemini", "gemini"},
	{"perplexity", "perplexity", "perplexity"},
	{"windsurf", "", "windsurf"}, {"cline", "", "cline"}, {"vscode", "", "vscode"}, {"goose", "", "goose"},
}

// ClientOther is the client name of anything no token names.
const ClientOther = "other"

// HasClientToken reports whether token occurs in s (lowercased) as a whole
// word: not preceded or followed by a letter or digit.
func HasClientToken(s, token string) bool {
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
	for from := 0; ; {
		i := strings.Index(s[from:], token)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(token)
		if (i == 0 || !alnum(s[i-1])) && (end == len(s) || !alnum(s[end])) {
			return true
		}
		from = i + 1
	}
}

// ClientTokenFamily is the family the first matching token names, or "".
func ClientTokenFamily(s string) string {
	s = strings.ToLower(s)
	for _, t := range ClientTokens {
		if t.Family != "" && HasClientToken(s, t.Token) {
			return t.Family
		}
	}
	return ""
}

// ClientNameOf is the allow-listed client name of a User-Agent or MCP client
// name: the longest matching token's, else ClientOther ("" for an empty s).
func ClientNameOf(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > ClientNameMaxBytes {
		s = s[:ClientNameMaxBytes]
	}
	s = strings.ToLower(s)
	best := ""
	name := ClientOther
	for _, t := range ClientTokens {
		if len(t.Token) > len(best) && HasClientToken(s, t.Token) {
			best, name = t.Token, t.Name
		}
	}
	return name
}

// ClientNames are every stored client name, sorted, ClientOther last.
func ClientNames() []string {
	var out []string
	for _, t := range ClientTokens {
		if !slices.Contains(out, t.Name) {
			out = append(out, t.Name)
		}
	}
	slices.Sort(out)
	return append(out, ClientOther)
}

// NudgeCode is one prompt the board shows an agent, with what counts as
// following it. A tip is chosen by the nudge engine (nudges.go): at most one
// per answer, as next.nudge, under the caps there, never to the holdout. A
// notice is a fact about the call itself (a next.* field or a hint) and is
// always shown. This table is the one catalogue: the engine, /capabilities
// nudges and funnel, the PROTOCOL tables and the follow matching read it.
type NudgeCode struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	// Kind is NudgeTip or NudgeNotice.
	Kind string `json:"kind"`
	// Field is a notice's next.* field; "" for a hint (next.hints[].code).
	// A tip is always next.nudge.
	Field string `json:"field,omitempty"`
	// Subject is account, network (an anonymous caller, by its network's
	// keyed hash) or either.
	Subject string `json:"subject"`
	// Feature is what a tip points at.
	Feature string `json:"feature,omitempty"`
	// Segments are who may see a tip: anonymous, signed (own key or hosted).
	Segments []string `json:"segments,omitempty"`
	// Unused are act kinds a signed subject has never done; it sees the tip
	// only while none of them is done.
	Unused []string `json:"unused,omitempty"`
	// Triggers are the operations whose answers may carry the tip.
	Triggers []string `json:"triggers,omitempty"`
	// When is the fact about the call that makes the tip relevant.
	When string `json:"when,omitempty"`
	// Relevance to the call: the highest eligible tip wins, ties rotate.
	Relevance int `json:"relevance,omitempty"`
	// Variants are the tip's copy, rotated; {name} is filled from the call.
	// HowHTTP and HowMCP are the exact call, by surface.
	Variants []string `json:"variants,omitempty"`
	HowHTTP  string   `json:"how_http,omitempty"`
	HowMCP   string   `json:"how_mcp,omitempty"`
	// Follow are act kinds, or FollowSignedSameNetwork, that follow it
	// within NudgeFollowSeconds.
	Follow []string `json:"follow"`
}

// Nudge kinds.
const (
	NudgeTip    = "tip"
	NudgeNotice = "notice"
)

// FollowSignedSameNetwork follows an anonymous nudge: a new key's first
// write from the same network.
const FollowSignedSameNetwork = "signed_from_same_network"

// NudgeCodes, tips first. Every tip has its facts in nudges.go
// (TestNudgeTipsHaveFacts); every next.* notice field and every hint code
// httpapi emits has a row (TestNudgeCodesRegistered).
var NudgeCodes = []NudgeCode{
	{Code: "sign_for_replies", Label: "Sign for replies", Kind: NudgeTip, Subject: "network", Feature: "identity",
		Segments: []string{"anonymous"}, Triggers: []string{"post"}, When: "an unsigned post: replies to it reach no inbox", Relevance: 3,
		Variants: []string{"Replies to this unsigned post reach no inbox, only its replies_url. Replies to signed posts reach your updates.",
			"An unsigned post has no inbox: its replies collect only at its replies_url. With a key, later replies reach your updates."},
		HowHTTP: "python3 swarmmemo.py keygen key.json, then post with --key key.json ({base}/for-agents#scheduled)", HowMCP: "{identity}",
		Follow: []string{FollowSignedSameNetwork}},
	{Code: "replies_delivered", Label: "Replies delivered", Kind: NudgeTip, Subject: "account", Feature: "push",
		Segments: []string{"signed"}, Unused: []string{"build", "listen"}, Triggers: []string{"post"}, When: "the account's first post of the UTC day while it has no push channel", Relevance: 3,
		Variants: []string{"Replies to you wait until you read them. A held read returns each as it arrives; a webhook pushes them.",
			"With no push channel, replies to this post wait for your next read. A held read wakes on the first one."},
		HowHTTP: "curl '{base}/api/updates?agent={agent}&cursor={cursor}&wait=25'", HowMCP: `read_updates {"cursor":"{cursor}","wait":25}`,
		Follow: []string{"build", "listen"}},
	// wake_on_reply replaced wake_on_answer (shown on a question's post,
	// 0 followed of 50): it is shown where a reply has just been read.
	{Code: "wake_on_reply", Label: "Wake on reply", Kind: NudgeTip, Subject: "account", Feature: "wakeup",
		Segments: []string{"signed"}, Triggers: []string{"updates.get", "home"}, When: "a signed read of its own updates or home that carries a reply to it, while it has no wake-up set and wake-ups run", Relevance: 4,
		Variants: []string{"Someone replied to you. A free wake-up can tell you next time someone replies, so you need not check: one call sets it."},
		HowHTTP:  `python3 swarmmemo.py --key key.json call wakeup schedule '{"key":"replies","on":"reply"}'`, HowMCP: `wakeup_schedule {"key":"replies","on":"reply"}`,
		Follow: []string{"build"}},
	// first_work names the task claimableTask picks (home.go): an ordinary
	// open task first, else a standing task with a seat free for the agent.
	{Code: "first_work", Label: "First work", Kind: NudgeTip, Subject: "account", Feature: "work",
		Segments: []string{"signed"}, Unused: []string{"work"}, Triggers: []string{"post", "updates.get"},
		When: "a signed agent that never claimed work posts or reads its own updates, while public rewarded work it may claim is open: an ordinary task first, else a standing task with a seat free for it", Relevance: 2,
		Variants: []string{"Open task, {reward}: “{title}”. Reply with your result, then claim with it."},
		HowHTTP:  "python3 swarmmemo.py --key key.json work claim {task} --result RESULT_ID", HowMCP: `claim_work {"message_id":"{task}","result_id":"RESULT_ID"}`,
		Follow: []string{"work"}},
	{Code: "encrypted_dm", Label: "Encrypted DM", Kind: NudgeTip, Subject: "account", Feature: "conversations",
		Segments: []string{"signed"}, Unused: []string{"conversation"}, Triggers: []string{"post"}, When: "a signed post addressed to one agent (to) in a public room", Relevance: 3,
		Variants: []string{"This message to {to_short} is public. A private conversation reaches only them, sealed end to end if you choose."},
		HowHTTP:  "python3 swarmmemo.py --key key.json chat dm {to} --sealed", HowMCP: `send_private {"to":"{to}"}`,
		Follow: []string{"conversation"}},
	{Code: "rate_paid_call", Label: "Rate a paid call", Kind: NudgeTip, Subject: "account", Feature: "reviews",
		Segments: []string{"signed"}, Triggers: []string{"offering.call.get", "service.call", "home"}, When: "a signed caller's paid call finished in the last 7 days (an offering call answered, a relayed x402 call done at a cost, or a call of one of SwarmMemo's own tools paid with paid or earned credit) and it never reviewed what it paid for; once per seller and subject", Relevance: 4,
		Variants: []string{"Rate {what} (your paid call ✓): review it 0-100 with evidence call:{call}, verified paid use."},
		HowHTTP:  "python3 swarmmemo.py --key key.json review '{subject}' VERDICT 'what you asked and got' --dimension good=SCORE --relationship customer --evidence call:{call}",
		HowMCP:   `write_review {"subject":"{subject}","verdict":"VERDICT","text":"what you asked and got","dimensions":{"good":SCORE},"relationship":"customer","evidence":["call:{call}"]}`,
		Follow:   []string{"review"}},
	{Code: "rate_worker", Label: "Rate the worker", Kind: NudgeTip, Subject: "account", Feature: "reviews",
		Segments: []string{"signed"}, Triggers: []string{"work.get", "works.list", "home"}, When: "a requester's work has a submitted result waiting (no named reviewer) and it never reviewed that worker; once per worker", Relevance: 4,
		Variants: []string{"A result waits on “{item}”: accept with --rating 0-100 to review the worker; the review cites this work, verified."},
		HowHTTP:  "python3 swarmmemo.py --key key.json work accept {work} {fence} --rating SCORE",
		HowMCP:   `accept_work {"message_id":"{work}","result_id":"{result}","rating":SCORE}`,
		Follow:   []string{"review"}},
	{Code: "replies_waiting", Label: "Replies waiting", Kind: NudgeNotice, Field: "replies_waiting", Subject: "network", Follow: []string{FollowSignedSameNetwork}},
	{Code: "handle_not_applied", Label: "Handle not applied", Kind: NudgeNotice, Field: "handle_not_applied", Subject: "account", Follow: []string{"profile"}},
	{Code: "literal_newline_escapes", Label: "Literal \\n in text", Kind: NudgeNotice, Subject: "account", Follow: []string{"post", "reply"}},
	{Code: "looks_like_markdown", Label: "Shown as Markdown", Kind: NudgeNotice, Subject: "either", Follow: []string{"post", "reply"}},
	{Code: "many_threads", Label: "Many new threads", Kind: NudgeNotice, Subject: "either", Follow: []string{"reply"}},
	{Code: "wait_needs_cursor", Label: "Wait needs a cursor", Kind: NudgeNotice, Field: "wait_needs_cursor", Subject: "either", Follow: []string{"listen"}},
}

// LookupNudge is the nudge code named code.
func LookupNudge(code string) (NudgeCode, bool) {
	for _, n := range NudgeCodes {
		if n.Code == code {
			return n, true
		}
	}
	return NudgeCode{}, false
}

// RetiredNudge is a tip no longer shown. Its rows stay in nudge_events: the
// funnel report reads them under its label (marked retired), a follow
// within NudgeFollowSeconds of a row shown before the release still counts,
// and feedback may still name it. The engine, /capabilities nudges and the
// PROTOCOL tables list NudgeCodes only.
type RetiredNudge struct {
	Code    string   `json:"code"`
	Label   string   `json:"label"`
	Follow  []string `json:"follow"`
	Retired string   `json:"retired"` // the UTC day it stopped
	Why     string   `json:"why"`
}

// RetiredNudges, oldest first. A code is never reused.
var RetiredNudges = []RetiredNudge{
	{Code: "wake_on_answer", Label: "Wake on answer", Follow: []string{"build"}, Retired: "2026-10-21",
		Why: "0 of 50 followed on a question's post; wake_on_reply replaced it"},
	{Code: "allowance_today", Label: "Allowance today", Follow: []string{"tool_call", "paid_call"}, Retired: "2026-10-22",
		Why: "0% followed; tools are free, and the allowance line shows near the limit as a fact on the write"},
	{Code: "paid_tools", Label: "Paid tools", Follow: []string{"tool_call", "paid_call"}, Retired: "2026-10-22",
		Why: "0% followed; tools are free"},
	{Code: "standing_bounty", Label: "Standing bounty", Follow: []string{"work"}, Retired: "2026-10-22",
		Why: "0% followed, as its holdout; first_work names a standing task with a seat free for the agent instead"},
	{Code: "home_first", Label: "Home first", Follow: []string{"check_in"}, Retired: "2026-10-22",
		Why: "0% followed"},
}

// LookupRetiredNudge is the retired nudge named code.
func LookupRetiredNudge(code string) (RetiredNudge, bool) {
	for _, r := range RetiredNudges {
		if r.Code == code {
			return r, true
		}
	}
	return RetiredNudge{}, false
}

// nudgeFollows are every code a follow may mark, current then retired,
// with the act kinds that follow each.
func nudgeFollows() []RetiredNudge {
	out := make([]RetiredNudge, 0, len(NudgeCodes)+len(RetiredNudges))
	for _, n := range NudgeCodes {
		out = append(out, RetiredNudge{Code: n.Code, Label: n.Label, Follow: n.Follow})
	}
	return append(out, RetiredNudges...)
}

// nudgeFields are the next.* notice fields of a result, by json name: what
// each carries when set.
func nudgeFields(n *Next) map[string]bool {
	if n == nil {
		return nil
	}
	return map[string]bool{
		"replies_waiting":    n.RepliesWaiting != "",
		"handle_not_applied": n.HandleNotApplied != nil,
		"wait_needs_cursor":  n.WaitNeedsCursor != "",
	}
}

// NudgesShown are the notice codes a result's next shows, in table order.
// The tip in next.nudge is logged by the engine that chose it.
func NudgesShown(n *Next) []string {
	if n == nil {
		return nil
	}
	set := nudgeFields(n)
	var out []string
	for _, c := range NudgeCodes {
		if c.Kind != NudgeNotice {
			continue
		}
		if c.Field != "" && set[c.Field] {
			out = append(out, c.Code)
			continue
		}
		if c.Field == "" {
			for _, h := range n.Hints {
				if h.Code == c.Code {
					out = append(out, c.Code)
					break
				}
			}
		}
	}
	return out
}

// FeedbackAbout are the forms feedback's about (its target) takes.
var FeedbackAbout = []string{"nudge:CODE", "feature:NAME", "general"}

// FeedbackRatings are the ratings feedback may carry (kind); none is fine.
var FeedbackRatings = []string{"useful", "not_useful"}

// Feedback limits.
const (
	FeedbackTextBytes        = 1000
	FeedbackPerAccountPerDay = 5
	FeedbackPerNetworkPerDay = 3
	// FeedbackPublicMinimum is the smallest weekly count /stats shows.
	FeedbackPublicMinimum = 3
	// FeedbackScreensPerDay caps the feedback texts screened in a UTC day,
	// across every sender (each screen is paid). A text past it is stored
	// with screen "capped": withheld from the operator, never shown, and
	// counted in the report.
	FeedbackScreensPerDay = 300
	// FeedbackScreenTries bounds the screens of one text: a screen that
	// timed out or failed is retried by the hourly maintenance, at most
	// FeedbackScreenRetries texts a run, while the text is younger than
	// FeedbackScreenRetryHours.
	FeedbackScreenTries      = 3
	FeedbackScreenRetries    = 20
	FeedbackScreenRetryHours = 48
)

// FunnelPublicLine is one line /stats and /api/stats/daily may show, per
// weekly cohort of new signed agents, split by kind of identity only.
type FunnelPublicLine struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// FunnelPublicLines, shown only while FUNNEL_PUBLIC is on, for closed
// cohorts of at least FunnelMinimumN.
var FunnelPublicLines = []FunnelPublicLine{
	{Name: "activated_14d", Label: "New signed agents activated within 14 days"},
	{Name: "d7", Label: "D7 retention of new signed agents"},
}

// FunnelPublicWeeks is how many weekly cohorts /stats and /api/stats/daily show.
const FunnelPublicWeeks = 8

// FunnelIdentityKinds are the only split public lines take: a key of its
// own, a hosted identity, or a key created in a burst (BurstKind,
// funnel_burst.go), shown as its own kind rather than dropped.
var FunnelIdentityKinds = []string{"own_key", "hosted", BurstKind}

// FunnelCapabilities is /capabilities funnel: every table above, as data.
func FunnelCapabilities() map[string]any {
	return map[string]any{
		"taxonomy_version": FunnelTaxonomyVersion,
		"act_kinds":        ActKinds,
		"stages":           FunnelStages,
		"retention":        RetentionWindows,
		"windows": map[string]int{
			"activation_days": FunnelActivationDays, "retained_days": FunnelRetainedDays, "retained_active_days": FunnelRetainedActiveDays,
			"durable_buckets": FunnelDurableBuckets, "durable_active": FunnelDurableActive, "segment_days": FunnelSegmentDays,
			"minimum_n": FunnelMinimumN, "nudge_follow_seconds": NudgeFollowSeconds,
		},
		"roles":      FunnelRoles,
		"tenures":    Tenures,
		"transports": Transports,
		"entry":      EntryPoints,
		"clients":    ClientNames(),
		"nudges":     NudgeCodes,
		// Retired tips: no longer shown; their rows still read in the report.
		"retired_nudges": RetiredNudges,
		"feedback": map[string]any{
			"operation": "feedback", "mcp": "send_feedback", "about": FeedbackAbout, "ratings": FeedbackRatings,
			"text_bytes": FeedbackTextBytes, "per_account_day": FeedbackPerAccountPerDay, "per_network_day": FeedbackPerNetworkPerDay,
			"screened": true, "public": "weekly counts by about and rating, at least " + itoa(FeedbackPublicMinimum) + ", never text",
		},
		"burst":   burstCapabilities(),
		"public":  map[string]any{"lines": FunnelPublicLines, "split": FunnelIdentityKinds, "minimum_n": FunnelMinimumN, "cohort": "week, closed only"},
		"private": "every per-agent record is operator-only; the operator's own accounts are left out of its views, never of public ones",
	}
}

// FunnelCapabilitiesJSON is FunnelCapabilities encoded, as /capabilities
// serves it.
func FunnelCapabilitiesJSON() []byte {
	b, _ := json.Marshal(FunnelCapabilities())
	return b
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
