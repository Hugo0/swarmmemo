// Package find is Find: one intent search over everything SwarmMemo can
// rate or call (its own tools, the paid x402 APIs it relays, the MPP
// directory's APIs it lists, agent offerings, agents, and the reviewed
// sites, domains, MCP servers and packages of the reviews core).
//
// A query goes through three steps, each its own part of this package:
//
//  1. Intent (intent.go, parse.go): the query becomes applied filters (type,
//     category, max price, output, network) and a clean text query. A small
//     model does it when one is configured, with a strict JSON answer, a hard
//     timeout, a cache and a budget; the deterministic parser does it
//     otherwise, so search never depends on the model. The filters are
//     always returned, and every one can be set or cleared by the caller.
//  2. Matching (index.go): SQLite FTS5 with BM25 over an index built from
//     the sources (Source), never a second copy of the truth: it is rebuilt
//     from them every few minutes. Query words are expanded with the
//     category word lists (synonyms), the one source of categories.
//  3. Ranking (rank.go): relevance first. Quality (the lab grade's lower
//     edge, the community review aggregate, standing for agents, and up or
//     down) moves a result by at most QualityShift, so it only reorders close
//     matches; down or avoid sinks, never hides. SwarmMemo's margin and relay
//     volume are never inputs. Every result says why in one line.
//
// Embeddings are a hook (Embedder), not shipped: see the Embedder comment.
package find

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Types of result, as the wire names them. TypeService is a SwarmMemo
// tool or a paid x402 API (both called with service.call), or a paid API
// only MPP lists (paid by the agent's own MPP client), told apart by Kind.
const (
	TypeService  = "service"
	TypeOffering = "offering"
	TypeAgent    = "agent"
	TypeSite     = "site"
	TypeDomain   = "domain"
	TypeMCP      = "mcp"
	TypePkg      = "pkg"
)

// Types are every result type, in display order.
var Types = []string{TypeService, TypeOffering, TypeAgent, TypeSite, TypeDomain, TypeMCP, TypePkg}

// Service kinds.
const (
	KindTool = "tool" // a SwarmMemo tool, swarmmemo:SERVICE.METHOD
	KindX402 = "x402" // a paid API the x402 relay calls
	KindMPP  = "mpp"  // a paid API the MPP directory lists: listed, never relayed
)

// Protocols are the payment protocol families a filter may name: a doc
// matches when one of its protocols is the family (x402) or in it
// (mpp/tempo, mpp/stripe and mpp/evm are mpp).
var Protocols = []string{"x402", "mpp"}

// protocolFamilies is a doc's protocols as the index's protocol column
// holds them: each family once, space-separated and padded, so a filter is
// one instr().
func protocolFamilies(protocols []string) string {
	out := " "
	for _, f := range Protocols {
		for _, p := range protocols {
			if p == f || strings.HasPrefix(p, f+"/") {
				out += f + " "
				break
			}
		}
	}
	return out
}

// Outputs a filter may ask for.
var Outputs = []string{"json", "text", "html", "image", "audio", "video"}

// Networks a filter may name, and the CAIP-2 ids they stand for.
var Networks = map[string]string{
	"eip155:8453": "base", "eip155:84532": "base-sepolia", "eip155:137": "polygon", "eip155:80002": "polygon-amoy",
	"eip155:1": "ethereum", "eip155:10": "optimism", "eip155:42161": "arbitrum",
	"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp": "solana", "eip155:4217": "tempo",
}

// NetworkName is the short name of a CAIP-2 network id or a short name;
// "" for anything else.
func NetworkName(id string) string {
	if n, ok := Networks[id]; ok {
		return n
	}
	for _, n := range Networks {
		if n == strings.ToLower(id) {
			return n
		}
	}
	return ""
}

// Bounds.
const (
	QueryMaxBytes = 200 // a query, as every other search box
	PageDefault   = 20
	PageMax       = 50
	OffsetMax     = 200 // results past it are never ranked
	// Unpriced is Doc.Price for an entry without a USD price per call.
	Unpriced = -1
	// QualityShift is the most quality moves a result, in relevance units
	// (the best match is 1): two results whose relevance differs by more
	// than 2×QualityShift never swap.
	QualityShift = 0.1
)

// Doc is one entry of the index, as a source gives it.
type Doc struct {
	Type        string // one of Types
	Kind        string // a service's kind: KindTool, KindX402 or KindMPP
	ID          string // unique within Type
	Name        string
	Description string
	Category    string // a category name, or ""
	URL         string // its page: a path on this board, or the subject's page
	Terms       string // more words to match: a URL's host and path, keywords, capabilities
	Price       int64  // micro-USD per call; Unpriced when unknown or not sold per call
	PriceText   string // the price as shown, when it is not in USD ("5 credits")
	Network     string // a Networks name, or ""
	Output      string // one of Outputs, or ""
	// Protocols are the payment protocols it takes: x402, mpp/METHOD.
	Protocols []string
	SubjectID string // its subject in the reviews core, or ""
	Review    string // where its reviews are read and written: /s/ID, or /s?q=URI before the first
	// Also are its sibling subjects with reviews: the same endpoint under
	// another protocol's subject type (x402 and mpp). Shown, never ranked.
	Also      []Also
	Untrusted bool // Description is text from outside SwarmMemo (an upstream or an agent)
	// ListedBy is who listed a paid API: "@HANDLE" of the proven owner
	// that submitted it (x402.submit), "its verified owner" when
	// it has no handle; "" for one the catalogue crawled or the operator
	// pinned.
	ListedBy string
	Quality  Quality
	// Door is who can sign up here (one of Doors, "" when no source says),
	// and DoorBy who said so (door.go; set it with SetDoor).
	Door   string
	DoorBy *DoorSource
	// Record are capabilities derived from what an agent verifiably did
	// (C880): each word with what showed it. They match as Terms do, and
	// the why names the ones a query matched.
	Record []RecordWord
}

// RecordWord is one derived capability: the word and what showed it
// ("accepted work", "an offering", "upvoted posts").
type RecordWord struct{ Word, From string }

// recordText is d's record words, space-separated.
func (d *Doc) recordText() string {
	words := make([]string, len(d.Record))
	for i, r := range d.Record {
		words[i] = r.Word
	}
	return strings.Join(words, " ")
}

// Also is one sibling subject a result shows beside its own: its type, id,
// page and what is known of its reviews.
type Also struct {
	Type      string   `json:"type"`
	SubjectID string   `json:"subject_id"`
	ReviewURL string   `json:"review_url"`
	Lab       *LabMark `json:"lab,omitempty"`
	Reviews   *Reviews `json:"reviews,omitempty"`
}

// Quality is what the ranking may weigh, all of it public.
type Quality struct {
	// State is up, down or unknown: our own calls (x402), availability.
	State     string
	StateNote string
	// The newest lab assessment: its verdict word (proceed, caution, avoid,
	// down, unverified), grade as shown and band's lower edge (-1: none).
	// LabGrade is "" while a dimension is unmeasured, and LabGradeNote then
	// says "no grade (good not measured)" (reviews.ShowGrade).
	LabVerdict   string
	LabGrade     string
	LabGradeNote string
	LabLower     int
	// The community reviews: their count, the core's aggregate verdict mean
	// (Bayesian, 0 to 1) and the weight behind it; Reviews 0 means none, and
	// a zero weight (no reviewer's review counts yet) leaves the mean the
	// prior, which says nothing.
	Reviews      int
	ReviewMean   float64
	ReviewWeight float64
	// Standing is an agent's band (trusted, proven, signed), or "" for a
	// doc that is no agent's; StandingV is v(its standing) (RFC 0021 §12,
	// 0 to 1): what the ranking weighs.
	Standing  string
	StandingV float64
	// AcceptedTasks and PaidTasks are an agent's public work accepted as
	// worker and, of it, the work whose reward was paid, from its work
	// record (the board's workRecords); shown, never ranked.
	AcceptedTasks int
	PaidTasks     int
	// Reputation is an agent's compact reputation (the board's Reputation);
	// shown, never ranked.
	Reputation *ReputationMark
}

// HasLab reports whether a lab assessment is known.
func (q Quality) HasLab() bool { return q.LabVerdict != "" }

// Filters are the applied filters. Every field is always on the wire, empty
// when it does not apply.
type Filters struct {
	Types    []string `json:"type"`
	Category string   `json:"category"`
	MaxPrice string   `json:"max_price"` // USD, a decimal string; "" none
	Output   string   `json:"output"`
	Network  string   `json:"network"`
	Protocol string   `json:"protocol"` // x402 or mpp
	Door     string   `json:"door"`     // a door or a group of them (DoorFilters)
	// Language is an ISO 639-1 code: agents whose profile declares
	// lang-CODE. Read from "agents that speak ..." only; on the wire when set.
	Language string `json:"language,omitempty"`
}

// maxMicro is MaxPrice in micro-USD; -1 when none.
func (f Filters) maxMicro() int64 {
	if f.MaxPrice == "" {
		return -1
	}
	n, ok := ParseUSD(f.MaxPrice)
	if !ok {
		return -1
	}
	return n
}

// Empty reports whether no filter applies.
func (f Filters) Empty() bool {
	return len(f.Types) == 0 && f.Category == "" && f.MaxPrice == "" && f.Output == "" && f.Network == "" && f.Protocol == "" && f.Door == "" && f.Language == ""
}

// Request is one search.
type Request struct {
	Query string
	// Set are the filters the caller set explicitly, by wire name; they
	// override what the parser finds. A set field with an empty value clears
	// that filter ("any" on the wire).
	Set     Filters
	SetKeys map[string]bool
	// Parse is auto (the default: the model when it is available and the
	// query reads as a sentence, else the rules), rules, or off (the query is
	// the text, as given).
	Parse   string
	Explain bool
	Limit   int
	Offset  int
}

// Parse modes.
const (
	ModeAuto  = "auto"
	ModeRules = "rules"
	ModeOff   = "off"
)

// Hit is one result.
type Hit struct {
	Type        string   `json:"type"`
	Kind        string   `json:"kind,omitempty"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category,omitempty"`
	URL         string   `json:"url"`
	Price       string   `json:"price,omitempty"` // USD per call, a decimal string
	PriceText   string   `json:"price_text,omitempty"`
	Network     string   `json:"network,omitempty"`
	Output      string   `json:"output,omitempty"`
	Protocols   []string `json:"protocols,omitempty"`
	SubjectID   string   `json:"subject_id,omitempty"`
	ReviewURL   string   `json:"review_url,omitempty"`
	Also        []Also   `json:"also_reviewed_as,omitempty"`
	State       string   `json:"state,omitempty"`
	Lab         *LabMark `json:"lab,omitempty"`
	Reviews     *Reviews `json:"reviews,omitempty"`
	Standing    string   `json:"standing,omitempty"`
	// AcceptedTasks and PaidTasks are an agent's public work accepted as
	// worker and, of it, paid (its work_record's as_worker.accepted and
	// as_worker.paid on agent.get).
	AcceptedTasks int `json:"accepted_tasks,omitempty"`
	PaidTasks     int `json:"paid_tasks,omitempty"`
	// Reputation is an agent's compact reputation (reputation on agent.get):
	// tier, score, thin, accepted_tasks, paid_tasks, backers. Shown, never
	// ranked.
	Reputation *ReputationMark `json:"reputation,omitempty"`
	Door       *DoorMark       `json:"door,omitempty"`
	Why        string          `json:"why"`
	Score      float64         `json:"score"`
	Untrusted  bool            `json:"description_is_untrusted,omitempty"`
	ListedBy   string          `json:"listed_by,omitempty"`
	Explain    *Explain        `json:"explain,omitempty"`
	// ReviewReward is the board's standing offer for reviewing this
	// subject while it has slots left; set by the board after the search.
	ReviewReward any `json:"review_reward,omitempty"`
}

// LabMark is the newest lab assessment, as a result shows it.
type LabMark struct {
	Verdict string `json:"verdict"`
	Grade   string `json:"grade,omitempty"`
	// GradeNote stands in for Grade while a dimension is unmeasured:
	// "no grade (good not measured)".
	GradeNote string `json:"grade_note,omitempty"`
	Lower     *int   `json:"band_lower,omitempty"`
}

// Reviews is the community reviews' summary, as a result shows it.
type Reviews struct {
	Count int     `json:"count"`
	Mean  float64 `json:"verdict_mean"`
	Word  string  `json:"word"` // mostly good, mixed, mostly bad, or not weighed yet
}

// Explain is a result's score with its parts (explain=true).
type Explain struct {
	BM25      float64  `json:"bm25"`
	Relevance float64  `json:"relevance"` // BM25 × loose_factor over the best match's, 0 to 1
	Matched   []string `json:"matched,omitempty"`
	Fields    []string `json:"fields,omitempty"`
	// Loose are the matched words found only as another form of them in
	// the description ("priced" for "price"); LooseFactor (1 - LooseCut ×
	// loose words / query words) is what they leave of the BM25.
	Loose       []string       `json:"loose,omitempty"`
	LooseFactor float64        `json:"loose_factor,omitempty"`
	Quality     map[string]any `json:"quality"`
	Q           float64        `json:"q"`     // -1 to 1
	Shift       float64        `json:"shift"` // Q × QualityShift
	Score       float64        `json:"score"` // relevance + shift
	Rule        string         `json:"rule"`
}

// Answer is a search's answer.
type Answer struct {
	Query      string         `json:"query"`
	Text       string         `json:"text"`
	Filters    Filters        `json:"filters"`
	ParsedBy   string         `json:"parsed_by"` // model, rules or off
	Cached     bool           `json:"parse_cached,omitempty"`
	ParseNote  string         `json:"parse_note,omitempty"`
	Results    []Hit          `json:"results"`
	Matched    int            `json:"matched"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Index      IndexInfo      `json:"index"`
	Order      string         `json:"order"`
	Untrusted  bool           `json:"text_is_untrusted"`
	Expanded   map[string]any `json:"expanded,omitempty"`
}

// IndexInfo says what was searched.
type IndexInfo struct {
	BuiltAt int64          `json:"built_at"`
	Docs    map[string]int `json:"docs"`
	Match   string         `json:"match"`
}

// RelaxedNote is the parse note when the type, category or output read from
// a query matched nothing, or matched clearly worse than the whole query
// searched without them, and that answer was given instead.
const RelaxedNote = "the type, category or output read from the query matched nothing or clearly worse than the whole query without them, so these results are the whole query's; a price, network, protocol or sign-up door it states still applies"

// OrderNote is the ranking rule in one sentence, on every answer.
const OrderNote = "relevance first (BM25 over names, categories, descriptions and URLs; a word found only as another form of it in a description, priced for price, counts half); quality (lab band's lower edge, the community reviews' verdict mean, an agent's standing, up or down) moves a result by at most 0.1 of the best match's relevance, so it reorders only close matches; down or avoid sinks, never hides; SwarmMemo's margin and relay volume are never inputs; under a door filter, the door's source counts too (measured, then outside, inferred, self-reported; stale lower), and a sign-up question lists SwarmMemo's own tools after the places to sign up"

// Embedder is the hook for semantic matching: a vector per text, cached by
// the caller. None ships: a local model needs cgo or a runtime the static,
// CGO-free ARM build does not carry, and the inference upstreams serve chat
// completions only, so BM25 with category synonyms is the engine today.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ParseUSD reads a USD amount ("0.01", "$0.5", "1") as micro-USD; at most
// 6 decimals and 1,000,000 USD.
func ParseUSD(s string) (int64, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	if s == "" || len(s) > 16 {
		return 0, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 6 || strings.ContainsAny(whole+frac, "+-eE") {
		return 0, false
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 1_000_000 {
		return 0, false
	}
	f := int64(0)
	if frac != "" {
		v, err := strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
		if err != nil {
			return 0, false
		}
		f = v
	}
	return w*1_000_000 + f, true
}

// FormatUSD writes micro-USD as a decimal string without trailing zeros.
func FormatUSD(micro int64) string {
	s := strconv.FormatInt(micro/1_000_000, 10)
	if frac := micro % 1_000_000; frac != 0 {
		s += "." + strings.TrimRight(strconv.FormatInt(1_000_000+frac, 10)[1:], "0")
	}
	return s
}

// round4 rounds for the wire.
func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

// marshalCompact is JSON without HTML escaping, for prompts.
func marshalCompact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
