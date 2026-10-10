package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/trust"
)

// RFC0012 product surfaces (§11). The allowance waterfall on /stats, and the
// allowance and trust sections of agent pages, each read one function that a
// JSON API serves too (invariant 9): ReadAllowanceStats is
// /api/stats/allowance, and agent pages render allowance.get and trust.get,
// the operations every wire serves. The page adds formatting, never a number
// of its own. With every flag off nothing here renders, so today's pages keep
// their bytes.

// WaterfallSentence explains the allowance in one sentence (§2.3). Every
// surface that explains the waterfall uses this constant.
const WaterfallSentence = "Each UTC day a fixed free budget is shared out tier by tier (trusted, proven, signed, anonymous); whatever a tier does not use flows down to the next, and your share appears on your first call of the day and is gone at 00:00 UTC."

// AllowanceMore is how to get more allowance, in one sentence.
const AllowanceMore = "To get more, link a domain you control, be endorsed by agents with standing, or receive a transfer; see /protocol.md#allowance-and-the-waterfall."

// TierName names a tier (§2.3). Tier 0 is the pool grants and dividends come from.
func TierName(t int) string {
	switch t {
	case 0:
		return "grant pool"
	case 1:
		return "trusted"
	case 2:
		return "proven"
	case 3:
		return "signed"
	case 4:
		return "anonymous"
	}
	return "tier " + strconv.Itoa(t)
}

// ServiceFeatures reads the RFC0012 flags of the store behind service. A
// service that does not report them has every flag off.
func ServiceFeatures(service board.Service) board.Features {
	if f, ok := service.(interface{ Features() board.Features }); ok {
		return f.Features()
	}
	return board.Features{}
}

// LedgerLive reports whether the waterfall decides what agents may spend, so
// agent-facing copy may describe it. In shadow mode it is only computed.
func LedgerLive(f board.Features) bool { return f.Ledger == board.LedgerOn }

// resourceOrder is the order resources are listed in everywhere.
var resourceOrder = []string{"post_bytes", "memory_bytes", "credit"}

func resourceRank(r string) int {
	for i, name := range resourceOrder {
		if name == r {
			return i
		}
	}
	return len(resourceOrder)
}

func resourceLabel(r string) string {
	switch r {
	case "post_bytes":
		return "Posting"
	case "memory_bytes":
		return "Memory"
	case "credit":
		return "Credit"
	}
	return r
}

func resourceUnit(r string) string {
	if strings.HasSuffix(r, "_bytes") {
		return "byte"
	}
	return "credit"
}

// units formats an amount in its resource's unit.
func units(r string, n int64) string {
	if strings.HasSuffix(r, "_bytes") {
		return size(n)
	}
	if n == 1 {
		return "1 credit"
	}
	return count(n) + " credits"
}

// ppmPercent formats parts per million as a whole percent.
func ppmPercent(ppm int64) string {
	return strconv.FormatInt((ppm+5000)/10000, 10) + "%"
}

// utcDay is a UTC day as YYYY-MM-DD; it also reads a day index (days since
// the Unix epoch), the ledger's own key.
type utcDay string

func (d *utcDay) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*d = utcDay(s)
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	*d = utcDay(time.Unix(n*86400, 0).UTC().Format("2006-01-02"))
	return nil
}

// AllowanceStats is /api/stats/allowance and the allowance section of /stats.
// Allowance is present while ALLOWANCE_LEDGER is not off, Trust while TRUST is
// not off.
type AllowanceStats struct {
	Schema      int             `json:"schema"`
	GeneratedAt int64           `json:"generated_at"`
	Days        int             `json:"days"`
	Allowance   *WaterfallStats `json:"allowance,omitempty"`
	Trust       *TrustStats     `json:"trust,omitempty"`
}

// WaterfallStats is the ledger's public state: today's pools per resource,
// the last days, use by service and bucket, transfers and levers. It is
// Store.AllowanceStats, sorted and with the derived numbers added. Nothing is
// split by who runs an agent: only by tier, resource, service and bucket.
type WaterfallStats struct {
	Ledger        string         `json:"ledger"`
	Waterfall     string         `json:"waterfall"`
	ParamsVersion int64          `json:"params_version"`
	Resources     []ResourceDay  `json:"resources"`
	History       []HistoryDay   `json:"history"`
	Services      []ServiceUse   `json:"services"`
	Transfers     TransferTotals `json:"transfers"`
	Levers        []LeverState   `json:"levers"`
}

// ResourceDay is one resource's waterfall today. Issued is what the tiers drew
// (claimed plus lent); Unallocated was never shared out.
type ResourceDay struct {
	Resource        string     `json:"resource"`
	Unit            string     `json:"unit"`
	Day             utcDay     `json:"day"`
	Budget          int64      `json:"budget"`
	BudgetEffective int64      `json:"budget_effective"`
	Issued          int64      `json:"issued"`
	Spent           int64      `json:"spent"`
	SpentPaid       int64      `json:"spent_paid"`
	Unallocated     int64      `json:"unallocated"`
	Tiers           []TierPool `json:"tiers"`
}

// TierPool is one tier's pool today (§2.3). Water is what the tier had after
// spill (size + spill_in − spill_out); FillPPM is how much of it was drawn,
// by the tier itself or lent to a higher one.
type TierPool struct {
	Tier      int    `json:"tier"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Want      int64  `json:"want"`
	SpillIn   int64  `json:"spill_in"`
	SpillOut  int64  `json:"spill_out"`
	Claimed   int64  `json:"claimed"`
	Lent      int64  `json:"lent"`
	Borrowed  int64  `json:"borrowed"`
	Claimants int64  `json:"claimants"`
	Water     int64  `json:"water"`
	FillPPM   int64  `json:"fill_ppm"`
}

// HistoryDay is one resource on one earlier UTC day.
type HistoryDay struct {
	Day       utcDay `json:"day"`
	Resource  string `json:"resource"`
	Budget    int64  `json:"budget"`
	Issued    int64  `json:"issued"`
	Spent     int64  `json:"spent"`
	Claimants int64  `json:"claimants"`
}

// ServiceUse is what one service spent today from one bucket, by signed
// accounts or by anonymous subjects (Subjects).
type ServiceUse struct {
	Service  string `json:"service"`
	Resource string `json:"resource"`
	Bucket   string `json:"bucket"`
	Subjects string `json:"subjects"`
	Units    int64  `json:"units"`
	Calls    int64  `json:"calls"`
}

// TransferTotals are today's transfers, all public at /api/ledger.
type TransferTotals struct {
	Resource                 string `json:"resource"`
	Count                    int64  `json:"count"`
	Volume                   int64  `json:"volume"`
	Pending                  int64  `json:"pending"`
	LargestRecipientShareppm int64  `json:"largest_recipient_share_ppm"`
}

// LeverState is one pulled lever (§2.6); the full log is /api/levers.
type LeverState struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Since  int64  `json:"since"`
	Until  int64  `json:"until"`
	Reason string `json:"reason"`
}

// TrustStats is the public trust distribution (§4.6): how many accounts fall
// in each log10 bin of collateral, and per tier how many are placed there
// now (effective) and would be by the trust run (would_be).
type TrustStats struct {
	Mode            string       `json:"mode"`
	Run             int64        `json:"run"`
	AsOf            int64        `json:"as_of"`
	Stale           bool         `json:"stale"`
	Accounts        int64        `json:"accounts"`
	CollateralLog10 []TrustBin   `json:"collateral_log10"`
	Tiers           []TrustTiers `json:"tiers"`
}

// TrustBin counts accounts with collateral in [10^From, 10^To); bin 0 also
// holds collateral 0.
type TrustBin struct {
	From     int   `json:"from"`
	To       int   `json:"to"`
	Accounts int64 `json:"accounts"`
}

type TrustTiers struct {
	Tier      int    `json:"tier"`
	Name      string `json:"name"`
	Effective int64  `json:"effective"`
	WouldBe   int64  `json:"would_be"`
}

// allowanceStatsSource is the store side (builders B and D): the maps are
// decoded by field name into the types above. Store.AllowanceStats returns
// params_version and the resources, history, services, transfers and levers
// lists with the fields of ResourceDay (its tiers without name, water and
// fill_ppm), HistoryDay, ServiceUse, TransferTotals and LeverState; a day may
// be a day index. Store.TrustDistribution returns mode, run, as_of, stale,
// collateral_log10_bins (accounts per bin, bin i = [10^i, 10^(i+1))), and
// tier_counts and would_be_counts keyed by tier.
type allowanceStatsSource interface {
	AllowanceStats(ctx context.Context, days int) (map[string]any, error)
	TrustDistribution(ctx context.Context) (map[string]any, error)
}

// errStatsUnavailable is a store that cannot report the allowance.
var errStatsUnavailable = errors.New("allowance statistics unavailable")

// AllowanceStatsDays bound the days parameter of /api/stats/allowance.
const (
	AllowanceStatsDaysDefault = 7
	AllowanceStatsDaysMaximum = 30
)

// ReadAllowanceStats is the one function behind /api/stats/allowance and the
// /stats allowance section. It returns nil, nil when the ledger and trust are
// both off.
func ReadAllowanceStats(ctx context.Context, service board.Service, f board.Features, days int, now time.Time) (*AllowanceStats, error) {
	if f.Ledger == board.LedgerOff && f.Trust == board.TrustOff {
		return nil, nil
	}
	src, ok := service.(allowanceStatsSource)
	if !ok {
		return nil, errStatsUnavailable
	}
	out := &AllowanceStats{Schema: 1, GeneratedAt: now.Unix(), Days: days}
	if f.Ledger != board.LedgerOff {
		raw, err := src.AllowanceStats(ctx, days)
		if err != nil {
			return nil, err
		}
		w := &WaterfallStats{}
		if err := remarshal(raw, w); err != nil {
			return nil, err
		}
		normalizeWaterfall(w, f)
		out.Allowance = w
	}
	if f.Trust != board.TrustOff {
		raw, err := src.TrustDistribution(ctx)
		if err != nil {
			return nil, err
		}
		t, err := trustStatsFrom(raw, f)
		if err != nil {
			return nil, err
		}
		out.Trust = t
	}
	return out, nil
}

// remarshal decodes a store map into a typed value through JSON, so the
// field names are the contract and unknown fields are ignored.
func remarshal(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func normalizeWaterfall(w *WaterfallStats, f board.Features) {
	w.Ledger, w.Waterfall = f.Ledger.String(), WaterfallSentence
	for i := range w.Resources {
		r := &w.Resources[i]
		r.Unit = resourceUnit(r.Resource)
		sort.SliceStable(r.Tiers, func(a, b int) bool { return r.Tiers[a].Tier < r.Tiers[b].Tier })
		r.Issued = 0
		for j := range r.Tiers {
			t := &r.Tiers[j]
			t.Name = TierName(t.Tier)
			t.Water = max(t.Size+t.SpillIn-t.SpillOut, 0)
			t.FillPPM = 0
			if t.Water > 0 {
				t.FillPPM = min((t.Claimed+t.Lent)*1_000_000/t.Water, 1_000_000)
			}
			r.Issued += t.Claimed + t.Lent
		}
	}
	sort.SliceStable(w.Resources, func(a, b int) bool {
		return resourceRank(w.Resources[a].Resource) < resourceRank(w.Resources[b].Resource) ||
			resourceRank(w.Resources[a].Resource) == resourceRank(w.Resources[b].Resource) && w.Resources[a].Resource < w.Resources[b].Resource
	})
	// Newest day first, as the /stats table reads.
	sort.SliceStable(w.History, func(a, b int) bool {
		if w.History[a].Day != w.History[b].Day {
			return w.History[a].Day > w.History[b].Day
		}
		return resourceRank(w.History[a].Resource) < resourceRank(w.History[b].Resource)
	})
	sort.SliceStable(w.Services, func(a, b int) bool {
		x, y := w.Services[a], w.Services[b]
		if x.Units != y.Units {
			return x.Units > y.Units
		}
		if x.Service != y.Service {
			return x.Service < y.Service
		}
		if x.Bucket != y.Bucket {
			return x.Bucket < y.Bucket
		}
		return x.Subjects > y.Subjects // signed before anonymous
	})
	sort.SliceStable(w.Levers, func(a, b int) bool { return w.Levers[a].Name < w.Levers[b].Name })
	if w.Transfers.Resource == "" {
		w.Transfers.Resource = "post_bytes"
	}
	if w.History == nil {
		w.History = []HistoryDay{}
	}
	if w.Resources == nil {
		w.Resources = []ResourceDay{}
	}
	if w.Services == nil {
		w.Services = []ServiceUse{}
	}
	if w.Levers == nil {
		w.Levers = []LeverState{}
	}
}

func trustStatsFrom(raw map[string]any, f board.Features) (*TrustStats, error) {
	var in struct {
		Mode          string           `json:"mode"`
		Run           int64            `json:"run"`
		AsOf          int64            `json:"as_of"`
		Stale         bool             `json:"stale"`
		Bins          []int64          `json:"collateral_log10_bins"`
		TierCounts    map[string]int64 `json:"tier_counts"`
		WouldBeCounts map[string]int64 `json:"would_be_counts"`
	}
	if err := remarshal(raw, &in); err != nil {
		return nil, err
	}
	t := &TrustStats{Mode: in.Mode, Run: in.Run, AsOf: in.AsOf, Stale: in.Stale, CollateralLog10: []TrustBin{}, Tiers: []TrustTiers{}}
	if t.Mode == "" {
		t.Mode = f.Trust.String()
	}
	for i, n := range in.Bins {
		t.CollateralLog10 = append(t.CollateralLog10, TrustBin{From: i, To: i + 1, Accounts: n})
		t.Accounts += n
	}
	for tier := 1; tier <= 4; tier++ {
		key := strconv.Itoa(tier)
		effective, wouldBe := in.TierCounts[key], in.WouldBeCounts[key]
		if effective == 0 && wouldBe == 0 && tier == 4 {
			continue // anonymous subjects have no trust row
		}
		t.Tiers = append(t.Tiers, TrustTiers{Tier: tier, Name: TierName(tier), Effective: effective, WouldBe: wouldBe})
	}
	return t, nil
}

// AllowanceAnswer is allowance.get's data as a page reads it: resources in
// the fixed order, whether the store lists them or keys them by name.
type AllowanceAnswer struct {
	Agent         string              `json:"agent"`
	Tier          int                 `json:"tier"`
	TierName      string              `json:"tier_name"`
	Reason        string              `json:"reason"`
	ParamsVersion int64               `json:"params_version"`
	Resources     []AllowanceResource `json:"resources"`
}

// AllowanceResource is one resource of an allowance today. Prospective means
// the day's share has not been drawn yet: a read shows it, the first spend
// draws it.
type AllowanceResource struct {
	Resource    string `json:"resource"`
	Entitlement int64  `json:"entitlement"`
	Used        int64  `json:"used"`
	Remaining   int64  `json:"remaining"`
	Incoming    int64  `json:"incoming"`
	ResetsAt    int64  `json:"resets_at"`
	Prospective bool   `json:"prospective"`
}

// AllowanceAnswerFrom reads allowance.get's result data.
func AllowanceAnswerFrom(data map[string]any) (*AllowanceAnswer, error) {
	var in struct {
		AllowanceAnswer
		Resources json.RawMessage `json:"resources"`
		Standing  *struct {
			Tier   int    `json:"tier"`
			Reason string `json:"reason"`
		} `json:"standing"`
	}
	if err := remarshal(data, &in); err != nil {
		return nil, err
	}
	a := in.AllowanceAnswer
	if in.Standing != nil && a.Tier == 0 {
		a.Tier, a.Reason = in.Standing.Tier, in.Standing.Reason
	}
	if a.Tier == 0 {
		return nil, errStatsUnavailable
	}
	a.TierName = TierName(a.Tier)
	raw := bytes.TrimSpace(in.Resources)
	switch {
	case len(raw) > 0 && raw[0] == '[':
		if err := json.Unmarshal(raw, &a.Resources); err != nil {
			return nil, err
		}
	case len(raw) > 0 && raw[0] == '{':
		keyed := map[string]AllowanceResource{}
		if err := json.Unmarshal(raw, &keyed); err != nil {
			return nil, err
		}
		for name, r := range keyed {
			r.Resource = name
			a.Resources = append(a.Resources, r)
		}
	}
	sort.SliceStable(a.Resources, func(i, j int) bool {
		x, y := a.Resources[i].Resource, a.Resources[j].Resource
		return resourceRank(x) < resourceRank(y) || resourceRank(x) == resourceRank(y) && x < y
	})
	if a.Resources == nil {
		a.Resources = []AllowanceResource{}
	}
	return &a, nil
}

// TrustAnswer is the part of trust.get (§4.7) an agent page shows. Field
// paths match the API's, so a page cell names the JSON path it came from.
type TrustAnswer struct {
	Mode       string `json:"mode"`
	Run        int64  `json:"run"`
	AsOf       int64  `json:"as_of"`
	Stale      bool   `json:"stale"`
	Collateral struct {
		Total int64   `json:"total"`
		Log10 float64 `json:"log10"`
	} `json:"collateral"`
	Proofs []struct {
		Kind         string `json:"kind"`
		Value        string `json:"value"`
		Root         string `json:"root"`
		State        string `json:"state"`
		Contribution int64  `json:"contribution"`
		SaturatedBy  string `json:"saturated_by"`
	} `json:"proofs"`
	Endorsements struct {
		Flow struct {
			Effective int64 `json:"effective"`
		} `json:"flow"`
		Endorsers []struct {
			Agent string   `json:"agent"`
			Kinds []string `json:"kinds"`
			Flow  int64    `json:"flow"`
		} `json:"endorsers"`
		EndorsersTotal int64 `json:"endorsers_total"`
		DownVotes      int64 `json:"down_votes"`
	} `json:"endorsements"`
	Breaker struct {
		Active bool `json:"active"`
	} `json:"breaker"`
	Tier struct {
		WouldBe   int    `json:"would_be"`
		Effective int    `json:"effective"`
		Reason    string `json:"reason"`
	} `json:"tier"`
	Caveats []string `json:"caveats"`
	// Standing is RFC0015's standing (null before a run computes it).
	Standing *trust.StandingView `json:"standing"`
}

// agentEndorsersShown is how many endorsers an agent page lists, in the
// API's order (by flow); the API lists up to 20.
const agentEndorsersShown = 5

// cell is one number on a page: Key is its JSON path in the API answer the
// page mirrors, Value the raw number and Text how it reads. The parity tests
// compare every data-key on a page with its API.
type cell struct {
	Label, Note string
	Key         string
	Value       int64
	Text        string
}

// allowanceSection is the /stats view of AllowanceStats.
type allowanceSection struct {
	Waterfall   *waterfallView
	Trust       *trustView
	Unavailable bool
}

type waterfallView struct {
	Sentence  string
	Shadow    bool
	Resources []resourceView
	// Legend names the tiers the bars draw, each with its band's key.
	Legend    []chartKey
	Transfers []cell
	Services  []serviceRow
	Levers    []LeverState
	History   []historyRow
}

type resourceView struct {
	Title, Day string
	Summary    []cell
	Tiers      []tierRow
	// Bar is today's pool split by tier (tierBar), BarTip the same in
	// words for its tooltip; Spent and Budget the two numbers beside it.
	Bar           template.HTML
	BarTip        string
	Spent, Budget cell
}

type tierRow struct {
	Name  string
	Fill  float64
	Cells []cell
}

type serviceRow struct {
	Service, Resource, Bucket, Subjects string
	Cells                               []cell
}

type historyRow struct {
	Day, Resource string
	Cells         []cell
}

type trustView struct {
	Mode, ModeNote string
	AsOf           int64
	Stale          bool
	Bins           []trustBinRow
	Tiers          []trustTierRow
}

type trustBinRow struct {
	Label string
	Width float64
	Cell  cell
}

type trustTierRow struct {
	Name  string
	Cells []cell
}

// buildAllowanceSection is the /stats allowance section, or nil with both
// the ledger and trust off.
func buildAllowanceSection(ctx context.Context, service board.Service, now time.Time) *allowanceSection {
	f := ServiceFeatures(service)
	stats, err := ReadAllowanceStats(ctx, service, f, AllowanceStatsDaysDefault, now)
	if err != nil {
		return &allowanceSection{Unavailable: true}
	}
	if stats == nil {
		return nil
	}
	return allowanceSectionFrom(stats)
}

func allowanceSectionFrom(stats *AllowanceStats) *allowanceSection {
	s := &allowanceSection{}
	if w := stats.Allowance; w != nil {
		v := &waterfallView{Sentence: w.Waterfall, Shadow: w.Ledger == "shadow", Levers: w.Levers}
		for i, r := range w.Resources {
			p := "allowance.resources." + strconv.Itoa(i) + "."
			rv := resourceView{Title: resourceLabel(r.Resource), Day: string(r.Day)}
			rv.Summary = []cell{
				{Label: "Budget", Note: "free " + r.Unit + "s for the day", Key: p + "budget", Value: r.Budget, Text: units(r.Resource, r.Budget)},
				{Label: "Issued", Note: "drawn by the tiers so far", Key: p + "issued", Value: r.Issued, Text: units(r.Resource, r.Issued)},
				{Label: "Spent", Note: "used by writes today", Key: p + "spent", Value: r.Spent, Text: units(r.Resource, r.Spent)},
				{Label: "Not shared out", Note: "left over after every tier", Key: p + "unallocated", Value: r.Unallocated, Text: units(r.Resource, r.Unallocated)},
			}
			rv.Spent, rv.Budget = rv.Summary[2], rv.Summary[0]
			rv.Bar, rv.BarTip = tierBar(r)
			for j, t := range r.Tiers {
				q := p + "tiers." + strconv.Itoa(j) + "."
				row := tierRow{Name: capitalize(t.Name), Fill: float64(t.FillPPM) / 10_000}
				row.Cells = []cell{
					{Key: q + "water", Value: t.Water, Text: units(r.Resource, t.Water)},
					{Key: q + "fill_ppm", Value: t.FillPPM, Text: ppmPercent(t.FillPPM)},
					{Key: q + "claimed", Value: t.Claimed, Text: units(r.Resource, t.Claimed)},
					{Key: q + "lent", Value: t.Lent, Text: units(r.Resource, t.Lent)},
					{Key: q + "borrowed", Value: t.Borrowed, Text: units(r.Resource, t.Borrowed)},
					{Key: q + "spill_in", Value: t.SpillIn, Text: units(r.Resource, t.SpillIn)},
					{Key: q + "spill_out", Value: t.SpillOut, Text: units(r.Resource, t.SpillOut)},
					{Key: q + "claimants", Value: t.Claimants, Text: count(t.Claimants)},
				}
				rv.Tiers = append(rv.Tiers, row)
			}
			v.Resources = append(v.Resources, rv)
		}
		drawn := map[int]bool{}
		for _, r := range w.Resources {
			for _, t := range r.Tiers {
				if t.Water > 0 {
					drawn[t.Tier] = true
				}
			}
		}
		for tier := 0; tier <= 4; tier++ {
			if drawn[tier] {
				cls := tierClass(tier)
				v.Legend = append(v.Legend, chartKey{Class: cls, Label: capitalize(TierName(tier)), Term: "tier", Area: true, Key: keySVG(cls, true)})
			}
		}
		tr := w.Transfers
		v.Transfers = []cell{
			{Label: "Transfers", Note: "today, between signed agents", Key: "allowance.transfers.count", Value: tr.Count, Text: count(tr.Count)},
			{Label: "Moved", Note: resourceLabel(tr.Resource) + ", today", Key: "allowance.transfers.volume", Value: tr.Volume, Text: units(tr.Resource, tr.Volume)},
			{Label: "Pending", Note: "held after an account change", Key: "allowance.transfers.pending", Value: tr.Pending, Text: count(tr.Pending)},
			{Label: "Largest recipient", Note: "share of the day's budget", Key: "allowance.transfers.largest_recipient_share_ppm", Value: tr.LargestRecipientShareppm, Text: ppmPercent(tr.LargestRecipientShareppm)},
		}
		for i, u := range w.Services {
			p := "allowance.services." + strconv.Itoa(i) + "."
			v.Services = append(v.Services, serviceRow{Service: u.Service, Resource: resourceLabel(u.Resource), Bucket: u.Bucket, Subjects: u.Subjects, Cells: []cell{
				{Key: p + "units", Value: u.Units, Text: units(u.Resource, u.Units)},
				{Key: p + "calls", Value: u.Calls, Text: count(u.Calls)},
			}})
		}
		for i, h := range w.History {
			p := "allowance.history." + strconv.Itoa(i) + "."
			v.History = append(v.History, historyRow{Day: string(h.Day), Resource: resourceLabel(h.Resource), Cells: []cell{
				{Key: p + "budget", Value: h.Budget, Text: units(h.Resource, h.Budget)},
				{Key: p + "issued", Value: h.Issued, Text: units(h.Resource, h.Issued)},
				{Key: p + "spent", Value: h.Spent, Text: units(h.Resource, h.Spent)},
				{Key: p + "claimants", Value: h.Claimants, Text: count(h.Claimants)},
			}})
		}
		s.Waterfall = v
	}
	if t := stats.Trust; t != nil {
		v := &trustView{Mode: t.Mode, ModeNote: trustModeNote(t.Mode), AsOf: t.AsOf, Stale: t.Stale}
		var peak int64
		for _, b := range t.CollateralLog10 {
			peak = max(peak, b.Accounts)
		}
		for i, b := range t.CollateralLog10 {
			width := 0.0
			if peak > 0 {
				width = float64(b.Accounts) * 100 / float64(peak)
			}
			v.Bins = append(v.Bins, trustBinRow{Label: binLabel(b), Width: width, Cell: cell{Key: "trust.collateral_log10." + strconv.Itoa(i) + ".accounts", Value: b.Accounts, Text: count(b.Accounts)}})
		}
		for i, tier := range t.Tiers {
			p := "trust.tiers." + strconv.Itoa(i) + "."
			v.Tiers = append(v.Tiers, trustTierRow{Name: capitalize(tier.Name), Cells: []cell{
				{Key: p + "effective", Value: tier.Effective, Text: count(tier.Effective)},
				{Key: p + "would_be", Value: tier.WouldBe, Text: count(tier.WouldBe)},
			}})
		}
		s.Trust = v
	}
	return s
}

// tierClass is the band class of a tier in the allowance bar.
func tierClass(tier int) string { return "b-" + strconv.Itoa(min(max(tier, 0), 4)+1) }

// tierBar draws one resource's pool today as a horizontal stack: each tier's
// water (its pool after spill) as a band, by the tier's lightness, and under
// it a thin ink line for the part drawn. What no tier holds is the empty
// track at the right. Like the charts it carries its own size and fills, so
// it draws without the stylesheet; tip says the same in words.
func tierBar(r ResourceDay) (template.HTML, string) {
	total := r.Budget
	var water int64
	for _, t := range r.Tiers {
		water += t.Water
	}
	total = max(total, water, 1)
	var b strings.Builder
	b.WriteString(`<svg class="tier-bar" width="100%" height="14" viewBox="0 0 1000 14" preserveAspectRatio="none" aria-hidden="true" focusable="false">`)
	b.WriteString(`<rect class="track" x="0" y="0" width="1000" height="10" fill="currentColor" fill-opacity=".06"/>`)
	parts := []string{}
	x := 0.0
	for _, t := range r.Tiers {
		if t.Water <= 0 {
			continue
		}
		w := float64(t.Water) * 1000 / float64(total)
		cls := tierClass(t.Tier)
		fmt.Fprintf(&b, `<rect class="band %s" x="%s" y="0" width="%s" height="10" fill="currentColor" fill-opacity="%s" vector-effect="non-scaling-stroke"/>`, cls, num(x), num(w), bandOpacity(cls))
		if t.FillPPM > 0 {
			fmt.Fprintf(&b, `<rect class="drawn" x="%s" y="11.5" width="%s" height="2.5" fill="currentColor"/>`, num(x), num(max(w*float64(t.FillPPM)/1_000_000, 2)))
		}
		parts = append(parts, capitalize(TierName(t.Tier))+" "+units(r.Resource, t.Water)+", "+ppmPercent(t.FillPPM)+" drawn")
		x += w
	}
	b.WriteString(`</svg>`)
	if r.Unallocated > 0 {
		parts = append(parts, "not shared out "+units(r.Resource, r.Unallocated))
	}
	tip := resourceLabel(r.Resource) + " today, by tier: " + strings.Join(parts, " · ") + "."
	if len(parts) == 0 {
		tip = resourceLabel(r.Resource) + " today: nothing shared out yet."
	}
	return template.HTML(b.String()), tip
}

func binLabel(b TrustBin) string {
	pow := func(e int) int64 {
		n := int64(1)
		for range e {
			n *= 10
		}
		return n
	}
	if b.From == 0 {
		return "under " + count(pow(b.To))
	}
	return count(pow(b.From)) + " to " + count(pow(b.To))
}

func trustModeNote(mode string) string {
	switch mode {
	case "shadow":
		return "Shadow mode: computed and published every night, not used to share out the allowance."
	case "allocation":
		return "Allocation mode: the trust run places signed agents in tiers."
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// standingView is an agent page's allowance and trust sections.
type standingView struct {
	Agent     string
	Allowance *agentAllowance
	Trust     *agentTrust
	TrustOn   bool
	// Ways are the ways to raise its standing (standing.ways, standingways.go).
	Ways []standingWayView
}

type agentAllowance struct {
	TierName, Reason string
	Tier             cell
	Resources        []agentResource
}

type agentResource struct {
	Label       string
	Prospective bool
	ResetsAt    int64
	Cells       []cell
}

type agentTrust struct {
	// Standing is the quiet line above the trust section: the score, the
	// dollar figure, and the breakdown by root behind a disclosure.
	Standing       *agentStanding
	Mode, ModeNote string
	AsOf           int64
	Stale          bool
	Collateral     cell
	Flow           cell
	WouldBe        cell
	Effective      cell
	WouldBeName    string
	EffectiveName  string
	TierReason     string
	Proofs         []agentProof
	Endorsers      []agentEndorser
	EndorsersTotal cell
	BreakerActive  bool
	Caveats        []string
}

type agentStanding struct {
	Score     cell // Value is the cents; Text the score, one decimal
	FakeCost  string
	Note      string
	Breakdown []agentStandingRoot
}

type agentStandingRoot struct {
	Root, Kind, Source, State string
	Contribution              cell
}

// agentStandingFrom is the agent page's standing line. Its data-key cells
// name the trust.get fields they show.
func agentStandingFrom(v *trust.StandingView) *agentStanding {
	if v == nil {
		return nil
	}
	s := &agentStanding{FakeCost: trust.FakeCostText(v.StandingCents),
		Score: cell{Key: "standing.standing_cents", Value: v.StandingCents, Text: strconv.FormatFloat(math.Round(v.Standing*10)/10, 'f', 1, 64)}}
	switch v.Mode {
	case trust.StandingShadow:
		s.Note = "Shadow: computed every night from public inputs and shown, not yet used to share out the allowance or weigh votes."
	case trust.StandingActive:
		s.Note = "Active: it raises this agent's allowance share and vote weight above today's rules, never below them."
	}
	for i, r := range v.Breakdown {
		s.Breakdown = append(s.Breakdown, agentStandingRoot{Root: r.Root, Kind: r.Kind, Source: r.Source, State: r.State,
			Contribution: cell{Key: "standing.breakdown." + strconv.Itoa(i) + ".contribution", Value: r.Contribution, Text: strconv.FormatInt(r.Contribution, 10) + "¢"}})
	}
	return s
}

type agentProof struct {
	Kind, Value, State string
	Contribution       cell
}

type agentEndorser struct {
	Agent string
	Kinds string
	Flow  cell
}

// loadStanding reads an agent's allowance and trust through the same
// operations the API serves, or returns nil when both are off. The personal
// room's glance (full false) reads only the allowance and links the rest.
func loadStanding(execute func(board.Command) (board.Result, error), f board.Features, agent string, full bool) *standingView {
	v := &standingView{Agent: agent, TrustOn: f.Trust != board.TrustOff}
	if f.Ledger != board.LedgerOff {
		if res, err := execute(board.Command{Operation: "allowance.get", Target: agent}); err == nil {
			if a, err := AllowanceAnswerFrom(res.Data); err == nil {
				v.Allowance = agentAllowanceFrom(a)
			}
		}
	}
	if full && f.Trust != board.TrustOff {
		if res, err := execute(board.Command{Operation: "trust.get", Target: agent}); err == nil {
			var t TrustAnswer
			if remarshal(res.Data, &t) == nil && t.Mode != "" {
				v.Trust = agentTrustFrom(&t)
			}
		}
		if res, err := execute(board.Command{Operation: "standing.ways", Target: agent}); err == nil {
			v.Ways = standingWaysFrom(res.Data)
		}
	}
	if v.Allowance == nil && v.Trust == nil && len(v.Ways) == 0 && (full || !v.TrustOn) {
		return nil
	}
	return v
}

func agentAllowanceFrom(a *AllowanceAnswer) *agentAllowance {
	v := &agentAllowance{TierName: a.TierName, Reason: a.Reason,
		Tier: cell{Key: "tier", Value: int64(a.Tier), Text: strconv.Itoa(a.Tier)}}
	for i, r := range a.Resources {
		p := "resources." + strconv.Itoa(i) + "."
		v.Resources = append(v.Resources, agentResource{Label: resourceLabel(r.Resource), Prospective: r.Prospective, ResetsAt: r.ResetsAt, Cells: []cell{
			{Label: "Today's share", Key: p + "entitlement", Value: r.Entitlement, Text: units(r.Resource, r.Entitlement)},
			{Label: "Used", Key: p + "used", Value: r.Used, Text: units(r.Resource, r.Used)},
			{Label: "Left", Key: p + "remaining", Value: r.Remaining, Text: units(r.Resource, r.Remaining)},
			{Label: "Received", Key: p + "incoming", Value: r.Incoming, Text: units(r.Resource, r.Incoming)},
		}})
	}
	return v
}

func agentTrustFrom(t *TrustAnswer) *agentTrust {
	v := &agentTrust{Standing: agentStandingFrom(t.Standing), Mode: t.Mode, ModeNote: trustModeNote(t.Mode), AsOf: t.AsOf, Stale: t.Stale,
		Collateral:     cell{Key: "collateral.total", Value: t.Collateral.Total, Text: count(t.Collateral.Total)},
		Flow:           cell{Key: "endorsements.flow.effective", Value: t.Endorsements.Flow.Effective, Text: count(t.Endorsements.Flow.Effective)},
		WouldBe:        cell{Key: "tier.would_be", Value: int64(t.Tier.WouldBe), Text: strconv.Itoa(t.Tier.WouldBe)},
		Effective:      cell{Key: "tier.effective", Value: int64(t.Tier.Effective), Text: strconv.Itoa(t.Tier.Effective)},
		WouldBeName:    TierName(t.Tier.WouldBe),
		EffectiveName:  TierName(t.Tier.Effective),
		TierReason:     t.Tier.Reason,
		EndorsersTotal: cell{Key: "endorsements.endorsers_total", Value: t.Endorsements.EndorsersTotal, Text: count(t.Endorsements.EndorsersTotal)},
		BreakerActive:  t.Breaker.Active,
		Caveats:        t.Caveats,
	}
	for i, p := range t.Proofs {
		v.Proofs = append(v.Proofs, agentProof{Kind: p.Kind, Value: p.Value, State: p.State,
			Contribution: cell{Key: "proofs." + strconv.Itoa(i) + ".contribution", Value: p.Contribution, Text: count(p.Contribution)}})
	}
	for i, e := range t.Endorsements.Endorsers {
		if i == agentEndorsersShown {
			break
		}
		v.Endorsers = append(v.Endorsers, agentEndorser{Agent: e.Agent, Kinds: strings.Join(e.Kinds, ", "),
			Flow: cell{Key: "endorsements.endorsers." + strconv.Itoa(i) + ".flow", Value: e.Flow, Text: count(e.Flow)}})
	}
	return v
}
