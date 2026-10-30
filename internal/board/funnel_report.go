package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// The operator's funnel views (GET /admin/funnel, `swarmmemo funnel`), all
// from the records funnel_store.go keeps and the definitions in funnel.go,
// computed in Go over agent_funnel and agent_days. The operator's own
// accounts (the exclude list) are left out of every view here and counted
// in ExcludedFound; public lines (FunnelPublic) never use the list.

// FunnelSplits are the dimensions a report may split cohorts by.
var FunnelSplits = []string{"family", "client", "transport", "entry", "role", "tenure", "holdout", "hosted"}

// FunnelQuery selects a report: cohorts first seen in [From, To] (unix
// seconds), split by one of FunnelSplits or none, organic acts only.
type FunnelQuery struct {
	From, To int64
	Split    string
	Organic  bool
}

// Rate is k of n, with its percent; Low marks n below FunnelMinimumN.
type Rate struct {
	K   int     `json:"k"`
	N   int     `json:"n"`
	Pct float64 `json:"pct"`
	Low bool    `json:"low_n,omitempty"`
}

func rate(k, n int) Rate {
	r := Rate{K: k, N: n, Low: n < FunnelMinimumN}
	if n > 0 {
		r.Pct = math.Round(float64(k)*1000/float64(n)) / 10
	}
	return r
}

// FunnelReport is the whole operator view, grouped by question.
type FunnelReport struct {
	Generated       int64  `json:"generated"`
	From            int64  `json:"from"`
	To              int64  `json:"to"`
	Split           string `json:"split,omitempty"`
	Organic         bool   `json:"organic"`
	ExcludedFound   int    `json:"excluded_found"`
	ReadsSince      int64  `json:"reads_since"`
	TaxonomyVersion int    `json:"taxonomy_version"`
	ComputedDay     int64  `json:"computed_day"`

	Acquisition FunnelAcquisition `json:"acquisition"`
	Steps       []FunnelStep      `json:"steps"`
	Cohorts     []FunnelCohort    `json:"cohorts"`
	Weekly      []FunnelWeek      `json:"weekly"`
	Segments    []FunnelSegment   `json:"segments"`
	Convinces   FunnelConvinces   `json:"what_convinces"`
	Expansion   FunnelExpansion   `json:"expansion"`
	Lapsing     []FunnelLapsing   `json:"lapsing"`
	Nudges      []FunnelNudge     `json:"nudges"`
	Loop        FunnelLoop        `json:"loop"`
	Feedback    FunnelFeedback    `json:"feedback"`
	// Trust is the operator dashboard's trust numbers from the latest run
	// (RFC 0021 §13; trustDashboard): the share of the flow passed on that
	// the excluded (operator) accounts pass, the top-5 sources' share, the
	// penalties in force, the accounts at or above the vote floor. null
	// before any run.
	Trust map[string]any `json:"trust"`
	// Refusals is the allowance refusals per day of the range, by resource
	// and code (allowance_refusals, C320c): whether anyone hit a limit.
	Refusals []FunnelRefusal `json:"allowance_refusals"`
	// Burst is the burst clusters (funnel_burst.go) and the range's new
	// agents' rates with and without their members.
	Burst FunnelBurst `json:"burst"`
	// MCPTools is the hosted MCP tool calls of the last 7 and 30 days
	// ending the range (mcptoolstats.go): calls, errors and caller-days per
	// profile and tool.
	MCPTools []MCPToolWindow `json:"mcp_tools"`
	// RepeatPaid is the range's unprompted repeat paid use
	// (funnel_repeatpaid.go): paid calls by agents with no work as a
	// worker in the week before, and who came back on another day.
	RepeatPaid FunnelRepeatPaid `json:"repeat_paid"`
}

// FunnelBurst is the burst clusters the last recompute found (every
// account), the range's new agents among their members, and the rates over
// all the range's new agents against those excluding burst members.
type FunnelBurst struct {
	ComputedAt     int64          `json:"computed_at"`
	Clusters       int            `json:"clusters"`
	Members        int            `json:"members"`
	InRange        int            `json:"members_in_range"`
	Overall        FunnelRates    `json:"overall"`
	ExcludingBurst FunnelRates    `json:"excluding_burst"`
	List           []BurstCluster `json:"list"`
}

// FunnelRates are a group's rates, each over the members whose window is
// over: activated, retained, and each of RetentionWindows.
type FunnelRates struct {
	N         int             `json:"n"`
	Activated Rate            `json:"activated"`
	Retained  Rate            `json:"retained"`
	Retention map[string]Rate `json:"retention"`
}

// funnelRates is FunnelRates over rows.
func funnelRates(rows []FunnelRow, today int64, active func(string, int64, int64, uint64) bool, activatedAt, retainedAt func(FunnelRow) int64) FunnelRates {
	out := FunnelRates{N: len(rows), Retention: map[string]Rate{}}
	var an, ak, rn, rk int
	for _, r := range rows {
		d0 := r.FirstSeen / 86400
		if windowClosed(d0, FunnelActivationDays-1, today) {
			an++
			if activatedAt(r) > 0 {
				ak++
			}
		}
		if windowClosed(d0, FunnelRetainedDays, today) {
			rn++
			if retainedAt(r) > 0 {
				rk++
			}
		}
	}
	out.Activated, out.Retained = rate(ak, an), rate(rk, rn)
	for _, w := range RetentionWindows {
		n, k := 0, 0
		for _, r := range rows {
			d0 := r.FirstSeen / 86400
			if !windowClosed(d0, w.To, today) {
				continue
			}
			n++
			if active(r.Account, d0+int64(w.From), d0+int64(w.To), VerifiableActs) {
				k++
			}
		}
		out.Retention[w.Name] = rate(k, n)
	}
	return out
}

// FunnelRefusal is one day's refusals of one resource for one code.
type FunnelRefusal struct {
	Day      string `json:"day"`
	Resource string `json:"resource"`
	Code     string `json:"code"`
	N        int64  `json:"n"`
}

// funnelRefusals reads allowance_refusals for the days of [from, to].
func funnelRefusals(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, from, to int64) ([]FunnelRefusal, error) {
	rows, err := q.QueryContext(ctx, "SELECT day,resource,code,n FROM allowance_refusals WHERE day>=? AND day<=? ORDER BY day,resource,code LIMIT 3000", from/86400, to/86400)
	if err != nil {
		return nil, err
	}
	out := []FunnelRefusal{}
	for rows.Next() {
		var r FunnelRefusal
		var day int64
		if err = rows.Scan(&day, &r.Resource, &r.Code, &r.N); err != nil {
			rows.Close()
			return nil, err
		}
		r.Day = dayString(day)
		out = append(out, r)
	}
	return out, closeRows(rows)
}

// FunnelAcquisition is where agents come from: new agents per week by each
// entry dimension, and the keyless readers per day by entry point.
type FunnelAcquisition struct {
	Weeks   []FunnelNewWeek  `json:"weeks"`
	Readers []FunnelReadDay  `json:"readers"`
	Totals  map[string]Count `json:"totals"`
}

// Count is a tally by value.
type Count map[string]int

// FunnelNewWeek is one week's new agents.
type FunnelNewWeek struct {
	Week      string `json:"week"`
	New       int    `json:"new"`
	Hosted    int    `json:"hosted"`
	Entry     Count  `json:"entry"`
	Family    Count  `json:"family"`
	Client    Count  `json:"client"`
	Transport Count  `json:"transport"`
}

// FunnelReadDay is one day's distinct readers (an estimate, crawlers out).
type FunnelReadDay struct {
	Day        string           `json:"day"`
	Visitors   int64            `json:"visitors"`
	EntryPoint map[string]int64 `json:"entry_points"`
}

// FunnelStep is one stage over the whole range.
type FunnelStep struct {
	Stage string `json:"stage"`
	Label string `json:"label"`
	Count int64  `json:"count"`
	// FromPrevious is this stage's share of the previous one (not for
	// visitors, an aggregate, nor for identities, whose previous is it).
	FromPrevious *Rate `json:"from_previous,omitempty"`
}

// FunnelCohort is one weekly cohort (and split value).
type FunnelCohort struct {
	Week      string          `json:"week"`
	Value     string          `json:"value,omitempty"`
	N         int             `json:"n"`
	FirstAct  Rate            `json:"first_act"`
	Activated Rate            `json:"activated"`
	Retained  Rate            `json:"retained"`
	Durable   Rate            `json:"durable"`
	Retention map[string]Rate `json:"retention"`
}

// FunnelWeek is one calendar week (Monday first, UTC) of the range, every
// week present even when nothing happened in it: the new agents first seen
// in it, the outside agents with a verifiable act in it, the durable agents
// at its last day, and the activation of its new agents whose activation
// window has closed (Activated.N below New: some are still in theirs).
// Days counts its days inside the range, today included; a week cut by the
// range or not over yet is Partial, so a reader never takes it for a drop.
type FunnelWeek struct {
	Week      string `json:"week"`
	Days      int    `json:"days"`
	Partial   bool   `json:"partial,omitempty"`
	New       int    `json:"new"`
	Active    int    `json:"active"`
	Durable   int    `json:"durable"`
	Activated Rate   `json:"activated_closed"`
}

// FunnelSegmentDims are the dimensions FunnelSegment splits the range's new
// agents by: their primary role now, their entry point (ref:VENUE counted
// as ref), their first transport and their first client family.
var FunnelSegmentDims = []string{"role", "entry", "transport", "family"}

// FunnelSegment is one value of one of FunnelSegmentDims over the range's
// new agents: Activated over those whose activation window closed, D7 over
// those whose D7 window closed. A rate's Low flag says n is too small to
// compare.
type FunnelSegment struct {
	Dim       string `json:"dim"`
	Value     string `json:"value"`
	N         int    `json:"n"`
	Activated Rate   `json:"activated"`
	D7        Rate   `json:"d7"`
}

// RateOf is k of n as a Rate, Low below FunnelMinimumN: the one rule every
// view (the operator's dashboard included) applies.
func RateOf(k, n int) Rate { return rate(k, n) }

// windowClosed reports whether a window ending lastDay days after day d0
// is over on day today.
func windowClosed(d0 int64, lastDay int, today int64) bool { return d0+int64(lastDay) < today }

// FunnelCandidate is one act kind as an activation candidate: D14
// retention of the accounts that did it in their first 7 days against
// those that did not.
type FunnelCandidate struct {
	Act      string `json:"act"`
	Label    string `json:"label"`
	Doers    Rate   `json:"doers_d14"`
	NonDoers Rate   `json:"non_doers_d14"`
	Lift     string `json:"lift"`
}

// FunnelConvinces is what convinces them.
type FunnelConvinces struct {
	Candidates []FunnelCandidate `json:"candidates"`
	// ActivationActs counts the act that activated each account;
	// FirstActs the first verifiable act of each activated account.
	ActivationActs Count `json:"activation_acts"`
	FirstActs      Count `json:"first_acts"`
	// DaysToActivation counts activated accounts by days from first_seen.
	DaysToActivation []int `json:"days_to_activation"`
}

// FunnelExpansion is how agents take on more roles and features.
type FunnelExpansion struct {
	// Roles: for each role, the accounts that took it on, and how many
	// added another within 14 and 30 days; Next[role][other] counts the
	// roles added within 30 days.
	Roles []FunnelRoleGrowth `json:"roles"`
	// Breadth counts retained accounts by the number of verifiable act
	// kinds they ever used.
	Breadth []int `json:"breadth"`
	// AdoptNext counts, for retained accounts, each pair "first kind ->
	// next new kind".
	AdoptNext Count `json:"adopt_next"`
}

// FunnelRoleGrowth is one role's expansion.
type FunnelRoleGrowth struct {
	Role    string `json:"role"`
	Label   string `json:"label"`
	Took    int    `json:"took"`
	Added14 Rate   `json:"added_14d"`
	Added30 Rate   `json:"added_30d"`
	Next    Count  `json:"next"`
}

// FunnelLapsing is one lapsing account (operator-only).
type FunnelLapsing struct {
	Account       string `json:"account"`
	Handle        string `json:"handle,omitempty"`
	LastActiveDay string `json:"last_active_day"`
	Role          string `json:"role"`
	Family        string `json:"family,omitempty"`
	Client        string `json:"client,omitempty"`
}

// FunnelNudge is one nudge code's record over the range. Shown, Followed
// and the holdout and D7 columns are account-attributed: a nudge shown to an
// account and followed by that account. A nudge shown to an anonymous
// caller is attributed to its network (the /24 or /48's keyed hash) and
// followed by any new key's first write from that network: a weaker,
// network-attributed signal, kept apart in the Network columns and never
// mixed into the account ones. The holdout bit is recorded per subject, but
// slice 1 still shows a holdout subject its nudges (FunnelHoldoutPer256).
type FunnelNudge struct {
	Code  string `json:"code"`
	Label string `json:"label"`
	// Retired: a RetiredNudges code, listed only when the range has its rows.
	Retired     bool `json:"retired,omitempty"`
	Shown       int  `json:"shown"`
	Followed    Rate `json:"followed"`
	HoldoutRows int  `json:"holdout_rows"`
	HoldoutFol  Rate `json:"holdout_followed"`
	// Network-attributed: shown to anonymous callers, followed from their
	// network.
	NetworkShown       int  `json:"network_attributed_shown"`
	NetworkFollowed    Rate `json:"network_attributed_followed"`
	NetworkHoldoutRows int  `json:"network_attributed_holdout_rows"`
	NetworkHoldoutFol  Rate `json:"network_attributed_holdout_followed"`
	// D7 of the account subjects shown it against the holdout's.
	D7        Rate `json:"d7_shown"`
	D7Holdout Rate `json:"d7_holdout"`
	Useful    int  `json:"rated_useful"`
	NotUseful int  `json:"rated_not_useful"`
}

// FunnelFeedback is feedback by about and rating, by screen state (capped:
// past FeedbackScreensPerDay, withheld unscreened), and the newest items
// (text only when screened pass).
type FunnelFeedback struct {
	ByAbout  map[string]Count `json:"by_about"`
	ByScreen Count            `json:"by_screen"`
	Recent   []FeedbackItem   `json:"recent"`
}

func dayString(day int64) string { return time.Unix(day*86400, 0).UTC().Format("2006-01-02") }

// weekOf is the Monday on or before day (unix day 0 was a Thursday).
func weekOf(day int64) int64 { return day - (day+3)%7 }

// FunnelReport builds the operator view. ExcludedFound counts the excluded
// accounts in the data; a caller that prints refuses at 0, since a view
// that does not leave the operator's own accounts out reads as growth.
func (s *Store) FunnelReport(ctx context.Context, q FunnelQuery) (*FunnelReport, error) {
	if q.Split != "" && !slices.Contains(FunnelSplits, q.Split) {
		return nil, fmt.Errorf("split is one of %s", strings.Join(FunnelSplits, ", "))
	}
	now := s.now().Unix()
	today := now / 86400
	if q.To == 0 || q.To > now {
		q.To = now
	}
	if q.From == 0 {
		q.From = q.To - FunnelBackfillDays*86400
	}
	rep := &FunnelReport{Generated: now, From: q.From, To: q.To, Split: q.Split, Organic: q.Organic, TaxonomyVersion: FunnelTaxonomyVersion}
	var raw string
	if s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", metaFunnelReads).Scan(&raw) == nil {
		fmt.Sscan(raw, &rep.ReadsSince)
	}
	if s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", metaFunnelComputed).Scan(&raw) == nil {
		fmt.Sscan(raw, &rep.ComputedDay)
	}
	rows, err := s.FunnelRows(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.loadFunnelAccounts(ctx)
	if err != nil {
		return nil, err
	}
	days := map[string][]agentDay{}
	for _, a := range accounts {
		days[a.account] = a.days
	}
	var all, cohort []FunnelRow
	for _, r := range rows {
		if r.Excluded {
			rep.ExcludedFound++
			continue
		}
		all = append(all, r)
		if r.FirstSeen >= q.From && r.FirstSeen <= q.To {
			cohort = append(cohort, r)
		}
	}
	mask := func(d agentDay) uint64 {
		if q.Organic {
			return d.organic
		}
		return d.acts
	}
	// active reports whether account had an act in m on a day in [from, to].
	active := func(account string, from, to int64, m uint64) bool {
		for _, d := range days[account] {
			if d.day >= from && d.day <= to && mask(d)&m != 0 {
				return true
			}
		}
		return false
	}
	split := func(r FunnelRow) string {
		switch q.Split {
		case "family":
			return r.Family
		case "client":
			return r.Client
		case "transport":
			return r.Transport
		case "entry":
			return r.Entry
		case "role":
			return r.Role
		case "tenure":
			return r.Tenure
		case "holdout":
			return fmt.Sprint(r.Holdout)
		case "hosted":
			return fmt.Sprint(r.Hosted)
		}
		return ""
	}
	activatedAt := func(r FunnelRow) int64 {
		if q.Organic {
			return r.ActivatedOrganicAt
		}
		return r.ActivatedAt
	}
	retainedAt := func(r FunnelRow) int64 {
		if q.Organic {
			return r.RetainedOrganicAt
		}
		return r.RetainedAt
	}

	// 1. Acquisition.
	weeks := map[int64]*FunnelNewWeek{}
	rep.Acquisition.Totals = map[string]Count{"entry": {}, "family": {}, "client": {}, "transport": {}}
	for _, r := range cohort {
		w := weekOf(r.FirstSeen / 86400)
		nw := weeks[w]
		if nw == nil {
			nw = &FunnelNewWeek{Week: dayString(w), Entry: Count{}, Family: Count{}, Client: Count{}, Transport: Count{}}
			weeks[w] = nw
		}
		nw.New++
		if r.Hosted {
			nw.Hosted++
		}
		entry := strings.SplitN(r.Entry, ":", 2)[0]
		if r.Entry != entry {
			entry = r.Entry // ref:VENUE kept whole
		}
		for _, kv := range [][2]string{{"entry", entry}, {"family", orNone(r.FirstFamily)}, {"client", orNone(r.FirstClient)}, {"transport", orNone(TransportOf(r.FirstVia, r.Hosted))}} {
			rep.Acquisition.Totals[kv[0]][kv[1]]++
		}
		nw.Entry[entry]++
		nw.Family[orNone(r.FirstFamily)]++
		nw.Client[orNone(r.FirstClient)]++
		nw.Transport[orNone(TransportOf(r.FirstVia, r.Hosted))]++
	}
	for _, w := range sortedInt64(weeks) {
		rep.Acquisition.Weeks = append(rep.Acquisition.Weeks, *weeks[w])
	}
	span := int((q.To-q.From)/86400) + 1
	if span > 366 {
		span = 366
	}
	var visitors int64
	if stats, err := s.ReadDailyStats(ctx, time.Unix(q.To, 0), span); err == nil {
		for _, d := range stats {
			rd := FunnelReadDay{Day: d.Day, Visitors: d.Distinct[ReaderMetricKey(ReaderAnyMetric, "other")], EntryPoint: map[string]int64{}}
			for _, m := range ReaderMetrics {
				if n := d.Distinct[ReaderMetricKey(m, "other")]; n > 0 {
					rd.EntryPoint[m] = n
				}
			}
			visitors += rd.Visitors
			rep.Acquisition.Readers = append(rep.Acquisition.Readers, rd)
		}
	}

	// 2. Funnel steps and cohorts.
	var nFirst, nAct, nRet, nDur int
	for _, r := range cohort {
		if r.FirstActAt > 0 {
			nFirst++
		}
		if activatedAt(r) > 0 {
			nAct++
		}
		if retainedAt(r) > 0 {
			nRet++
		}
		if r.Durable {
			nDur++
		}
	}
	counts := []int64{visitors, int64(len(cohort)), int64(nFirst), int64(nAct), int64(nRet), int64(nDur)}
	for i, st := range FunnelStages {
		step := FunnelStep{Stage: st.Name, Label: st.Label, Count: counts[i]}
		if i >= 2 {
			r := rate(int(counts[i]), int(counts[i-1]))
			step.FromPrevious = &r
		}
		rep.Steps = append(rep.Steps, step)
	}
	type ckey struct {
		week  int64
		value string
	}
	groups := map[ckey][]FunnelRow{}
	for _, r := range cohort {
		k := ckey{weekOf(r.FirstSeen / 86400), split(r)}
		groups[k] = append(groups[k], r)
	}
	keys := make([]ckey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].week < keys[j].week || keys[i].week == keys[j].week && keys[i].value < keys[j].value
	})
	for _, k := range keys {
		g := groups[k]
		c := FunnelCohort{Week: dayString(k.week), Value: k.value, N: len(g), Retention: map[string]Rate{}}
		var fa, ac, re, du int
		for _, r := range g {
			if r.FirstActAt > 0 {
				fa++
			}
			if activatedAt(r) > 0 {
				ac++
			}
			if retainedAt(r) > 0 {
				re++
			}
			if r.Durable {
				du++
			}
		}
		c.FirstAct, c.Activated, c.Retained, c.Durable = rate(fa, len(g)), rate(ac, len(g)), rate(re, len(g)), rate(du, len(g))
		for _, w := range RetentionWindows {
			n, kk := 0, 0
			for _, r := range g {
				d0 := r.FirstSeen / 86400
				if !windowClosed(d0, w.To, today) {
					continue
				}
				n++
				if active(r.Account, d0+int64(w.From), d0+int64(w.To), VerifiableActs) {
					kk++
				}
			}
			c.Retention[w.Name] = rate(kk, n)
		}
		rep.Cohorts = append(rep.Cohorts, c)
	}

	// The weeks of the range, and the segments of its new agents.
	rep.Weekly = funnelWeeks(q, today, cohort, all, days, mask, activatedAt)
	rep.Segments = funnelSegments(cohort, today, active, activatedAt)

	// Burst clusters: the same rates with and without their members.
	bursts, err := s.FunnelBursts(ctx)
	if err != nil {
		return nil, err
	}
	rep.Burst = FunnelBurst{ComputedAt: bursts.ComputedAt, Clusters: len(bursts.Clusters), List: bursts.Clusters}
	for i := range rep.Burst.List {
		for j := range rep.Burst.List[i].Members {
			m := &rep.Burst.List[i].Members[j]
			m.Handle = s.signalHandle(ctx, m.Account)
			rep.Burst.Members++
		}
	}
	var unburst []FunnelRow
	for _, r := range cohort {
		if r.Burst != "" {
			rep.Burst.InRange++
			continue
		}
		unburst = append(unburst, r)
	}
	rep.Burst.Overall = funnelRates(cohort, today, active, activatedAt, retainedAt)
	rep.Burst.ExcludingBurst = funnelRates(unburst, today, active, activatedAt, retainedAt)

	// 3. What convinces them.
	d14 := RetentionWindows[2]
	for _, k := range ActKinds {
		if !k.Verifiable && k.Name != "vote" && k.Name != "read" {
			continue
		}
		var dn, dk, nn, nk int
		for _, r := range cohort {
			d0 := r.FirstSeen / 86400
			if d0+int64(d14.To) >= today {
				continue
			}
			did := active(r.Account, d0, d0+6, 1<<k.Bit)
			kept := active(r.Account, d0+int64(d14.From), d0+int64(d14.To), VerifiableActs)
			if did {
				dn++
				if kept {
					dk++
				}
			} else {
				nn++
				if kept {
					nk++
				}
			}
		}
		c := FunnelCandidate{Act: k.Name, Label: k.Label, Doers: rate(dk, dn), NonDoers: rate(nk, nn)}
		if c.NonDoers.Pct > 0 {
			c.Lift = fmt.Sprintf("%.1fx", c.Doers.Pct/c.NonDoers.Pct)
		}
		rep.Convinces.Candidates = append(rep.Convinces.Candidates, c)
	}
	rep.Convinces.ActivationActs, rep.Convinces.FirstActs = Count{}, Count{}
	rep.Convinces.DaysToActivation = make([]int, FunnelActivationDays)
	for _, r := range cohort {
		at := activatedAt(r)
		if at == 0 {
			continue
		}
		rep.Convinces.ActivationActs[r.ActivationAct]++
		for _, d := range days[r.Account] {
			if m := mask(d) & VerifiableActs; m != 0 {
				rep.Convinces.FirstActs[ActNames(m)[0]]++
				break
			}
		}
		if n := at/86400 - r.FirstSeen/86400; n >= 0 && n < FunnelActivationDays {
			rep.Convinces.DaysToActivation[n]++
		}
	}

	// 4. Expansion: the first day each role was met, acts cumulated.
	growth := map[string]*FunnelRoleGrowth{}
	for _, role := range FunnelRoles {
		if role.Name != "dormant" {
			growth[role.Name] = &FunnelRoleGrowth{Role: role.Name, Label: role.Label, Next: Count{}}
		}
	}
	rep.Expansion.Breadth = make([]int, len(ActKinds)+1)
	rep.Expansion.AdoptNext = Count{}
	n14, k14, n30, k30 := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, r := range all {
		var acts, organic, bounty uint64
		first := map[string]int64{}
		var kindOrder []string
		seenKind := map[string]bool{}
		for _, d := range days[r.Account] {
			acts, organic, bounty = acts|d.acts, organic|d.organic, bounty|d.bounty
			_, m := RolesOf(acts, organic, bounty)
			for _, role := range FunnelRoles {
				if m&(1<<role.Bit) != 0 {
					if _, ok := first[role.Name]; !ok {
						first[role.Name] = d.day
					}
				}
			}
			for _, name := range ActNames(d.acts & VerifiableActs) {
				if !seenKind[name] {
					seenKind[name] = true
					kindOrder = append(kindOrder, name)
				}
			}
		}
		for role, f := range first {
			g := growth[role]
			if g == nil {
				continue
			}
			g.Took++
			added := func(window int64) bool {
				for other, f2 := range first {
					if other != role && other != "lurker" && f2 > f && f2 <= f+window {
						return true
					}
				}
				return false
			}
			if f+14 < today {
				n14[role]++
				if added(14) {
					k14[role]++
				}
			}
			if f+30 < today {
				n30[role]++
				if added(30) {
					k30[role]++
				}
			}
			for other, f2 := range first {
				if other != role && f2 > f && f2 <= f+30 {
					g.Next[other]++
				}
			}
		}
		if r.RetainedAt > 0 {
			rep.Expansion.Breadth[len(kindOrder)]++
			if len(kindOrder) >= 2 {
				rep.Expansion.AdoptNext[kindOrder[0]+" -> "+kindOrder[1]]++
			}
		}
	}
	for _, role := range FunnelRoles {
		if g := growth[role.Name]; g != nil {
			g.Added14, g.Added30 = rate(k14[role.Name], n14[role.Name]), rate(k30[role.Name], n30[role.Name])
			rep.Expansion.Roles = append(rep.Expansion.Roles, *g)
		}
	}

	// 5. Retention: the cohorts above carry Dn; the lapsing list.
	for _, r := range all {
		if r.Tenure != "lapsing" {
			continue
		}
		rep.Lapsing = append(rep.Lapsing, FunnelLapsing{Account: r.Account, Handle: s.signalHandle(ctx, r.Account), LastActiveDay: dayString(r.LastActiveDay), Role: r.Role, Family: r.Family, Client: r.Client})
	}
	sort.Slice(rep.Lapsing, func(i, j int) bool { return rep.Lapsing[i].LastActiveDay > rep.Lapsing[j].LastActiveDay })

	// 6. Nudges.
	if rep.Nudges, err = s.funnelNudges(ctx, q, today, all, active); err != nil {
		return nil, err
	}
	if rep.Loop, err = s.funnelLoop(ctx, q, now, rep.Nudges); err != nil {
		return nil, err
	}
	if rep.RepeatPaid, err = s.funnelRepeatPaidExcluding(ctx, q.From, q.To); err != nil {
		return nil, err
	}

	// 7. Feedback.
	rep.Feedback.ByAbout = map[string]Count{}
	fr, err := s.db.QueryContext(ctx, "SELECT about,rating,count(*) FROM feedback WHERE created_at BETWEEN ? AND ? AND (account='' OR account NOT IN (SELECT account FROM agent_funnel WHERE excluded=1)) GROUP BY 1,2", q.From, q.To)
	if err != nil {
		return nil, err
	}
	for fr.Next() {
		var about, rating string
		var n int
		if err = fr.Scan(&about, &rating, &n); err != nil {
			fr.Close()
			return nil, err
		}
		cat := strings.SplitN(about, ":", 2)[0]
		if rep.Feedback.ByAbout[about] == nil {
			rep.Feedback.ByAbout[about] = Count{}
		}
		if rep.Feedback.ByAbout["all:"+cat] == nil {
			rep.Feedback.ByAbout["all:"+cat] = Count{}
		}
		rep.Feedback.ByAbout[about][orNone(rating)] += n
		rep.Feedback.ByAbout["all:"+cat][orNone(rating)] += n
	}
	if err = fr.Close(); err != nil {
		return nil, err
	}
	rep.Feedback.ByScreen = Count{}
	sr, err := s.db.QueryContext(ctx, "SELECT screen,count(*) FROM feedback WHERE created_at BETWEEN ? AND ? AND text<>'' GROUP BY 1", q.From, q.To)
	if err != nil {
		return nil, err
	}
	for sr.Next() {
		var screen string
		var n int
		if err = sr.Scan(&screen, &n); err != nil {
			sr.Close()
			return nil, err
		}
		rep.Feedback.ByScreen[screen] = n
	}
	if err = sr.Close(); err != nil {
		return nil, err
	}
	if rep.Feedback.Recent, err = s.FeedbackList(ctx, 25); err != nil {
		return nil, err
	}
	if rep.Refusals, err = funnelRefusals(ctx, s.db, q.From, q.To); err != nil {
		return nil, err
	}
	if rep.MCPTools, err = s.MCPToolReport(ctx, time.Unix(q.To, 0)); err != nil {
		return nil, err
	}
	if s.config.Features.Trust != TrustOff {
		ops, err := operatorAccountSet(ctx, s.db)
		if err != nil {
			return nil, err
		}
		if rep.Trust, err = s.trustDashboard(ctx, s.db, ops, now); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// funnelWeeks is FunnelReport.Weekly.
func funnelWeeks(q FunnelQuery, today int64, cohort, all []FunnelRow, days map[string][]agentDay, mask func(agentDay) uint64, activatedAt func(FunnelRow) int64) []FunnelWeek {
	fromDay, toDay := q.From/86400, min(q.To/86400, today)
	var out []FunnelWeek
	for ws := weekOf(fromDay); ws <= toDay; ws += 7 {
		first, last := max(ws, fromDay), min(ws+6, toDay)
		w := FunnelWeek{Week: dayString(ws), Days: int(last - first + 1)}
		w.Partial = ws < fromDay || ws+6 > toDay || ws+6 >= today
		var closed, act int
		for _, r := range cohort {
			d0 := r.FirstSeen / 86400
			if d0 < first || d0 > last {
				continue
			}
			w.New++
			if windowClosed(d0, FunnelActivationDays-1, today) {
				closed++
				if activatedAt(r) > 0 {
					act++
				}
			}
		}
		w.Activated = rate(act, closed)
		for _, r := range all {
			ds := days[r.Account]
			for _, d := range ds {
				if d.day >= first && d.day <= last && mask(d)&VerifiableActs != 0 {
					w.Active++
					break
				}
			}
			if durableOn(ds, last) {
				w.Durable++
			}
		}
		out = append(out, w)
	}
	return out
}

// funnelSegments is FunnelReport.Segments, by FunnelSegmentDims, each
// dimension's values by name.
func funnelSegments(cohort []FunnelRow, today int64, active func(string, int64, int64, uint64) bool, activatedAt func(FunnelRow) int64) []FunnelSegment {
	d7 := RetentionWindows[1]
	value := func(dim string, r FunnelRow) string {
		switch dim {
		case "role":
			return orNone(r.Role)
		case "entry":
			return orNone(strings.SplitN(r.Entry, ":", 2)[0])
		case "transport":
			return orNone(TransportOf(r.FirstVia, r.Hosted))
		}
		return orNone(r.FirstFamily)
	}
	var out []FunnelSegment
	for _, dim := range FunnelSegmentDims {
		type tally struct{ n, an, ak, dn, dk int }
		groups := map[string]*tally{}
		for _, r := range cohort {
			v := value(dim, r)
			t := groups[v]
			if t == nil {
				t = &tally{}
				groups[v] = t
			}
			t.n++
			d0 := r.FirstSeen / 86400
			if windowClosed(d0, FunnelActivationDays-1, today) {
				t.an++
				if activatedAt(r) > 0 {
					t.ak++
				}
			}
			if windowClosed(d0, d7.To, today) {
				t.dn++
				if active(r.Account, d0+int64(d7.From), d0+int64(d7.To), VerifiableActs) {
					t.dk++
				}
			}
		}
		names := make([]string, 0, len(groups))
		for v := range groups {
			names = append(names, v)
		}
		sort.Strings(names)
		for _, v := range names {
			t := groups[v]
			out = append(out, FunnelSegment{Dim: dim, Value: v, N: t.n, Activated: rate(t.ak, t.an), D7: rate(t.dk, t.dn)})
		}
	}
	return out
}

func (s *Store) funnelNudges(ctx context.Context, q FunnelQuery, today int64, all []FunnelRow, active func(string, int64, int64, uint64) bool) ([]FunnelNudge, error) {
	known := map[string]bool{}
	for _, r := range all {
		known[r.Account] = true
	}
	type row struct {
		subject         string
		holdout         bool
		shown, followed int64
		rated           string
	}
	rows, err := s.db.QueryContext(ctx, "SELECT subject,code,holdout,shown_at,followed_at,rated FROM nudge_events WHERE shown_at BETWEEN ? AND ? ORDER BY seq", q.From, q.To)
	if err != nil {
		return nil, err
	}
	byCode := map[string][]row{}
	for rows.Next() {
		var r row
		var code string
		if err = rows.Scan(&r.subject, &code, &r.holdout, &r.shown, &r.followed, &r.rated); err != nil {
			rows.Close()
			return nil, err
		}
		if !strings.HasPrefix(r.subject, "net:") && !known[r.subject] {
			continue // an excluded or unknown account
		}
		byCode[code] = append(byCode[code], r)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var out []FunnelNudge
	for i, c := range nudgeFollows() {
		// A retired code's rows stay readable: listed while the range has any.
		retired := i >= len(NudgeCodes)
		if retired && len(byCode[c.Code]) == 0 {
			continue
		}
		n := FunnelNudge{Code: c.Code, Label: c.Label, Retired: retired}
		var fol, hn, hf, dn, dk, hdn, hdk, nfol, nhf int
		for _, r := range byCode[c.Code] {
			switch r.rated {
			case "useful":
				n.Useful++
			case "not_useful":
				n.NotUseful++
			}
			if strings.HasPrefix(r.subject, "net:") {
				switch {
				case r.holdout:
					n.NetworkHoldoutRows++
					if r.followed > 0 {
						nhf++
					}
				default:
					n.NetworkShown++
					if r.followed > 0 {
						nfol++
					}
				}
				continue
			}
			d := r.shown / 86400
			closed := d+13 < today
			kept := closed && active(r.subject, d+7, d+13, VerifiableActs)
			if r.holdout {
				hn++
				if r.followed > 0 {
					hf++
				}
				if closed {
					hdn++
					if kept {
						hdk++
					}
				}
				continue
			}
			n.Shown++
			if r.followed > 0 {
				fol++
			}
			if closed {
				dn++
				if kept {
					dk++
				}
			}
		}
		n.Followed, n.HoldoutRows, n.HoldoutFol = rate(fol, n.Shown), hn, rate(hf, hn)
		n.D7, n.D7Holdout = rate(dk, dn), rate(hdk, hdn)
		n.NetworkFollowed, n.NetworkHoldoutFol = rate(nfol, n.NetworkShown), rate(nhf, n.NetworkHoldoutRows)
		out = append(out, n)
	}
	return out, nil
}

// FunnelLoop is the judgement loop (C306, RFC 0020 §9: a score needs more
// judges per subject): organic judgements per week, the share of paid
// calls and accepted works their party rated within FunnelLoopRateDays,
// and the rating prompts (NudgeCodes with Feature reviews) against the
// holdout. The exclude list's accounts are left out throughout.
type FunnelLoop struct {
	Weeks []FunnelLoopWeek `json:"weeks"`
	// PaidCalls: offering calls answered and relayed x402 calls done at a
	// cost, whose caller reviewed them citing call:ID within the window.
	PaidCalls Rate `json:"paid_calls_rated"`
	// Works: work.accept verdicts whose requester reviewed the worker
	// citing work:ID (a rated accept, or a review later) within the window.
	Works    Rate          `json:"accepted_works_rated"`
	RateDays int           `json:"rate_days"`
	Nudges   []FunnelNudge `json:"nudges"`
}

// FunnelLoopWeek is one week's organic judgements: reviews (not by an
// importer or the lab), rated accepts (counted apart from reviews) and
// vouches.
type FunnelLoopWeek struct {
	Week         string `json:"week"`
	Reviews      int    `json:"reviews"`
	RatedAccepts int    `json:"rated_accepts"`
	Vouches      int    `json:"vouches"`
	Total        int    `json:"total"`
}

// funnelLoopSQL are the loop's reads; ?1 from, ?2 to, ?3 the exclude list
// as JSON, ?4 the rate window in seconds.
const (
	funnelLoopExcluded = `(SELECT value FROM json_each(?3))`
	// A rated accept's review is posted in the accept's transaction, at its
	// time, by the requester, citing the work.
	funnelLoopReviewsSQL = `SELECT r.created_at, EXISTS(SELECT 1 FROM review_evidence v JOIN works w ON v.evidence='work:'||w.id
 JOIN work_transitions t ON t.work_id=w.id AND t.operation='work.accept' AND t.accepted_at=r.created_at
 WHERE v.event_id=r.event_id AND w.requester=r.account)
 FROM reviews r WHERE r.created_at BETWEEN ?1 AND ?2 AND r.relationship NOT IN ('outside_source','lab') AND r.account NOT IN ` + funnelLoopExcluded
	funnelLoopVouchesSQL = `SELECT created_at FROM endorsement_records WHERE kind='vouch' AND value=1 AND created_at BETWEEN ?1 AND ?2 AND voter_account NOT IN ` + funnelLoopExcluded
	funnelLoopRatedSQL   = `EXISTS(SELECT 1 FROM review_evidence v JOIN reviews r ON r.event_id=v.event_id WHERE v.evidence=%s AND r.account=%s AND r.created_at<=%s+?4)`
	funnelLoopCallsSQL   = `SELECT x.rated FROM (
 SELECT ` + `%[1]s` + ` AS rated, i.account AS who FROM offering_calls c JOIN identities i ON i.id=c.caller
  WHERE c.caller<>'' AND c.answered_at>0 AND c.answered_at BETWEEN ?1 AND ?2
 UNION ALL
 SELECT ` + `%[2]s` + `, s.account FROM service_calls s
  WHERE s.service='x402' AND s.state='done' AND s.cost>0 AND coalesce(json_extract(s.public,'$.http_status'),0)<400 AND s.created_at BETWEEN ?1 AND ?2
 ) x WHERE x.who NOT IN ` + funnelLoopExcluded
	funnelLoopWorksSQL = `SELECT ` + `%s` + ` FROM work_transitions t JOIN works w ON w.id=t.work_id
 WHERE t.operation='work.accept' AND t.accepted_at BETWEEN ?1 AND ?2 AND w.worker<>'' AND w.worker<>w.requester AND w.requester NOT IN ` + funnelLoopExcluded
)

// funnelLoop is FunnelReport.Loop: judgements per week over the range,
// rated shares over the transactions in the range whose window is over,
// and the rating prompts' rows of nudges.
func (s *Store) funnelLoop(ctx context.Context, q FunnelQuery, now int64, nudges []FunnelNudge) (FunnelLoop, error) {
	loop := FunnelLoop{RateDays: FunnelLoopRateDays, Weeks: []FunnelLoopWeek{}}
	excluded, err := s.funnelExcluded(ctx)
	if err != nil {
		return loop, err
	}
	excl, _ := json.Marshal(funnelSortedKeys(excluded))
	window := int64(FunnelLoopRateDays) * 86400
	weeks := map[int64]*FunnelLoopWeek{}
	week := func(at int64) *FunnelLoopWeek {
		w := weekOf(at / 86400)
		if weeks[w] == nil {
			weeks[w] = &FunnelLoopWeek{Week: dayString(w)}
		}
		return weeks[w]
	}
	for d := weekOf(q.From / 86400); d <= q.To/86400; d += 7 {
		week(d * 86400)
	}
	scan := func(query string, to int64, each func(*sql.Rows) error) error {
		rows, err := s.db.QueryContext(ctx, query, q.From, to, string(excl), window)
		if err != nil {
			return fmt.Errorf("funnel loop: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err = each(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	var at int64
	var flag bool
	if err = scan(funnelLoopReviewsSQL, q.To, func(r *sql.Rows) error {
		if err := r.Scan(&at, &flag); err != nil {
			return err
		}
		if flag {
			week(at).RatedAccepts++
		} else {
			week(at).Reviews++
		}
		return nil
	}); err != nil {
		return loop, err
	}
	if err = scan(funnelLoopVouchesSQL, q.To, func(r *sql.Rows) error {
		if err := r.Scan(&at); err != nil {
			return err
		}
		week(at).Vouches++
		return nil
	}); err != nil {
		return loop, err
	}
	for _, d := range sortedInt64(weeks) {
		w := weeks[d]
		w.Total = w.Reviews + w.RatedAccepts + w.Vouches
		loop.Weeks = append(loop.Weeks, *w)
	}
	// The rated shares: only transactions whose window is over.
	closed := min(q.To, now-window)
	tally := func(query string) (Rate, error) {
		k, n := 0, 0
		if closed < q.From {
			return rate(0, 0), nil
		}
		err := scan(query, closed, func(r *sql.Rows) error {
			if err := r.Scan(&flag); err != nil {
				return err
			}
			n++
			if flag {
				k++
			}
			return nil
		})
		return rate(k, n), err
	}
	calls := fmt.Sprintf(funnelLoopCallsSQL,
		fmt.Sprintf(funnelLoopRatedSQL, "'call:'||c.id", "i.account", "c.answered_at"),
		fmt.Sprintf(funnelLoopRatedSQL, "'call:'||s.id", "s.account", "coalesce(nullif(s.finished_at,0),s.created_at)"))
	if loop.PaidCalls, err = tally(calls); err != nil {
		return loop, err
	}
	if loop.Works, err = tally(fmt.Sprintf(funnelLoopWorksSQL, fmt.Sprintf(funnelLoopRatedSQL, "'work:'||w.id", "w.requester", "t.accepted_at"))); err != nil {
		return loop, err
	}
	codes := rateCodes()
	for _, n := range nudges {
		if slices.Contains(codes, n.Code) {
			loop.Nudges = append(loop.Nudges, n)
		}
	}
	return loop, nil
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

func sortedInt64[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// FunnelKPI is the north-star KPI kpi.py computed: accounts outside the
// exclude list with a verifiable act in the last days, by day and by act,
// and those active on 3 or more days (organic: bounty acts left out).
type FunnelKPI struct {
	WindowDays    int            `json:"window_days"`
	Active        int            `json:"weekly_active_verifiable_agents"`
	Repeat        int            `json:"repeat_agents_3plus_days"`
	RepeatOrganic int            `json:"repeat_agents_3plus_days_organic"`
	DailyActive   map[string]int `json:"daily_active"`
	ByAction      map[string]int `json:"by_action"`
	Agents        [][2]any       `json:"agents"`
	ExcludedFound int            `json:"excluded_found"`
	// RepeatPaid is the window's unprompted repeat paid use
	// (funnel_repeatpaid.go), aggregates only.
	RepeatPaid FunnelRepeatPaid `json:"repeat_paid"`
}

// FunnelKPIFor computes FunnelKPI over the days ending today.
func (s *Store) FunnelKPIFor(ctx context.Context, days int) (*FunnelKPI, error) {
	if days < 1 || days > 366 {
		return nil, errors.New("days is 1 to 366")
	}
	today := s.now().Unix() / 86400
	excluded, err := s.funnelExcluded(ctx)
	if err != nil {
		return nil, err
	}
	operators, err := operatorAccountSet(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := &FunnelKPI{WindowDays: days, DailyActive: map[string]int{}, ByAction: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, "SELECT account,day,acts,organic FROM agent_days WHERE day>? AND day<=? AND acts&?<>0 ORDER BY account,day", today-int64(days), today, int64(VerifiableActs))
	if err != nil {
		return nil, err
	}
	type acc struct {
		kinds         map[string]bool
		days, organic int
	}
	per := map[string]*acc{}
	seenExcluded := map[string]bool{}
	for rows.Next() {
		var account string
		var day, acts, organic int64
		if err = rows.Scan(&account, &day, &acts, &organic); err != nil {
			rows.Close()
			return nil, err
		}
		if excluded[account] {
			if operators[account] {
				seenExcluded[account] = true // house agents are not the operator's
			}
			continue
		}
		a := per[account]
		if a == nil {
			a = &acc{kinds: map[string]bool{}}
			per[account] = a
		}
		a.days++
		if uint64(organic)&VerifiableActs != 0 {
			a.organic++
		}
		out.DailyActive[dayString(day)]++
		for _, n := range ActNames(uint64(acts) & VerifiableActs) {
			a.kinds[n] = true
		}
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	out.ExcludedFound = len(seenExcluded)
	if out.ExcludedFound == 0 {
		// The operator's own accounts act every day; finding none means
		// the list is missing, and the numbers would count them.
		var n int
		_ = s.db.QueryRowContext(ctx, "SELECT count(*) FROM agent_funnel WHERE excluded=1").Scan(&n)
		out.ExcludedFound = n
	}
	accounts := funnelSortedKeys(func() map[string]bool {
		m := map[string]bool{}
		for a := range per {
			m[a] = true
		}
		return m
	}())
	for _, account := range accounts {
		a := per[account]
		out.Active++
		if a.days >= 3 {
			out.Repeat++
		}
		if a.organic >= 3 {
			out.RepeatOrganic++
		}
		kinds := funnelSortedKeys(a.kinds)
		for _, k := range kinds {
			out.ByAction[k]++
		}
		name := s.signalHandle(ctx, account)
		if name == "" {
			name = account[:min(12, len(account))]
		}
		out.Agents = append(out.Agents, [2]any{name, kinds})
	}
	if out.RepeatPaid, err = s.funnelRepeatPaid(ctx, (today-int64(days)+1)*86400, s.now().Unix(), excluded); err != nil {
		return nil, err
	}
	return out, nil
}

// FunnelPublicCohort is one public line's weekly cohort.
type FunnelPublicCohort struct {
	Week string  `json:"week"`
	Kind string  `json:"kind"`
	Line string  `json:"line"`
	N    int     `json:"n"`
	Pct  float64 `json:"pct"`
}

// FunnelPublicWeek is one week's public feedback counts by about category
// and rating, only those of at least FeedbackPublicMinimum.
type FunnelPublicWeek struct {
	Week   string         `json:"week"`
	Counts map[string]int `json:"counts"`
}

// FunnelPublic is what /stats may show (FUNNEL_PUBLIC): FunnelPublicLines
// per closed weekly cohort of new signed agents with at least
// FunnelMinimumN, split by kind of identity only, over every account (no
// exclude list: public pages never split operator from community); and
// weekly feedback counts. Never text, never an account.
func (s *Store) FunnelPublic(ctx context.Context, weeks int) ([]FunnelPublicCohort, []FunnelPublicWeek, error) {
	today := s.now().Unix() / 86400
	from := (weekOf(today) - int64(weeks)*7) * 86400
	bursts, err := s.FunnelBursts(ctx)
	if err != nil {
		return nil, nil, err
	}
	burst := bursts.Members()
	rows, err := s.db.QueryContext(ctx, "SELECT first_seen,hosted,activated_at,account FROM agent_funnel WHERE first_seen>=? ORDER BY first_seen", from)
	if err != nil {
		return nil, nil, err
	}
	type key struct {
		week int64
		kind string
	}
	type tally struct{ n, act, d7n, d7 int }
	groups := map[key]*tally{}
	var members []struct {
		k       key
		account string
		d0      int64
	}
	for rows.Next() {
		var first, activated int64
		var hosted bool
		var account string
		if err = rows.Scan(&first, &hosted, &activated, &account); err != nil {
			rows.Close()
			return nil, nil, err
		}
		kind := "own_key"
		switch {
		case burst[account] != nil:
			kind = BurstKind
		case hosted:
			kind = "hosted"
		}
		k := key{weekOf(first / 86400), kind}
		t := groups[k]
		if t == nil {
			t = &tally{}
			groups[k] = t
		}
		t.n++
		if activated > 0 {
			t.act++
		}
		members = append(members, struct {
			k       key
			account string
			d0      int64
		}{k, account, first / 86400})
	}
	if err = rows.Close(); err != nil {
		return nil, nil, err
	}
	d7 := RetentionWindows[1]
	for _, m := range members {
		if m.d0+int64(d7.To) >= today {
			continue
		}
		groups[m.k].d7n++
		var n int
		if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM agent_days WHERE account=? AND day BETWEEN ? AND ? AND acts&?<>0", m.account, m.d0+int64(d7.From), m.d0+int64(d7.To), int64(VerifiableActs)).Scan(&n); err != nil {
			return nil, nil, err
		}
		if n > 0 {
			groups[m.k].d7++
		}
	}
	var out []FunnelPublicCohort
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].week < keys[j].week || keys[i].week == keys[j].week && keys[i].kind < keys[j].kind
	})
	for _, k := range keys {
		t := groups[k]
		// Closed: every member's activation window ended (and, for D7,
		// every member's D7 window).
		if k.week+6+FunnelActivationDays < today && t.n >= FunnelMinimumN {
			out = append(out, FunnelPublicCohort{Week: dayString(k.week), Kind: k.kind, Line: FunnelPublicLines[0].Name, N: t.n, Pct: rate(t.act, t.n).Pct})
		}
		if k.week+6+int64(d7.To) < today && t.d7n >= FunnelMinimumN {
			out = append(out, FunnelPublicCohort{Week: dayString(k.week), Kind: k.kind, Line: FunnelPublicLines[1].Name, N: t.d7n, Pct: rate(t.d7, t.d7n).Pct})
		}
	}
	fr, err := s.db.QueryContext(ctx, "SELECT created_at,about,rating FROM feedback WHERE created_at>=?", from)
	if err != nil {
		return nil, nil, err
	}
	fw := map[int64]map[string]int{}
	for fr.Next() {
		var at int64
		var about, rating string
		if err = fr.Scan(&at, &about, &rating); err != nil {
			fr.Close()
			return nil, nil, err
		}
		w := weekOf(at / 86400)
		if fw[w] == nil {
			fw[w] = map[string]int{}
		}
		fw[w][strings.SplitN(about, ":", 2)[0]+":"+orNone(rating)]++
	}
	if err = fr.Close(); err != nil {
		return nil, nil, err
	}
	var weeksOut []FunnelPublicWeek
	for _, w := range sortedInt64(fw) {
		if w+6 >= today {
			continue // the week is still open
		}
		c := map[string]int{}
		for k, n := range fw[w] {
			if n >= FeedbackPublicMinimum {
				c[k] = n
			}
		}
		if len(c) > 0 {
			weeksOut = append(weeksOut, FunnelPublicWeek{Week: dayString(w), Counts: c})
		}
	}
	return out, weeksOut, nil
}
