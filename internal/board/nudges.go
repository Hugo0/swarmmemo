package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"

	"swarmmemo/internal/b64"
)

// The nudge engine (C293b, ROADMAP §4.9). An answer carries at most one tip,
// next.nudge {code, text, how}, chosen from the tips of NudgeCodes
// (funnel.go, the one catalogue):
//
//  1. Eligible: the call's operation is one of the tip's triggers, the
//     caller is in its segments, a signed caller has done none of its
//     unused act kinds, and its fact holds for this call (nudgeTipFacts).
//  2. Capped: at most NudgesPerDay tips a subject a day, and the same code
//     at most once in NudgeRepeatDays.
//  3. Chosen: the most relevant to the call; ties go to the tip shown to
//     the subject least recently, then catalogue order.
//
// A subject is a signed caller's account, or an anonymous caller's network
// (its keyed hash, as the funnel names it). Every row logs its subject's
// holdout bit (funnelHoldout); while NudgeHoldout is on, a subject in the
// holdout gets no tip, and the one it would have got is logged all the same,
// so shown and held-out rows compare. An account whose messaging settings
// say nudges off gets none and nothing is logged; neither does the site's
// own page nor, while Features.NudgesOpenAI is off, a client in the openai
// family. Notices (facts about the call itself) are not tips: they are
// always shown and logged by LogNudges.
//
// The engine runs after the command committed, holds no transaction while it
// reads, and never fails the request.

const (
	// NudgesPerAnswer is the most tips one answer carries.
	NudgesPerAnswer = 1
	// NudgesPerDay is the most tips one subject gets in 24 hours.
	NudgesPerDay = 3
	// NudgeRepeatDays is how long one code waits before it is shown to the
	// same subject again.
	NudgeRepeatDays = 7
	// NudgeTextBytes bounds a tip's text, so a filled variant stays one line.
	NudgeTextBytes = 160
	// nudgeSubjectsMax bounds the subjects kept in memory; past it the cache
	// starts over and reloads from the log.
	nudgeSubjectsMax = 50000
	// nudgeReloadSeconds: a cached subject is reloaded after this, so acts
	// the flush derived from the records count.
	nudgeReloadSeconds = 6 * 3600
)

// NudgesSetting is the messaging settings field that turns tips and the
// waiting footer off for an account: {"nudges":"off"}.
const NudgesSetting = "nudges"

// nudgeSchema is the engine's index on the log: a subject's recent rows.
const nudgeSchema = `CREATE INDEX IF NOT EXISTS nudge_subject ON nudge_events(subject,shown_at);`

// NudgeShown is next.nudge: one tip, its code, the fact and condition in
// words, and the exact call.
type NudgeShown struct {
	Code string `json:"code"`
	Text string `json:"text"`
	How  string `json:"how"`
}

// NudgeEnv is what the transport knows beside the command and its result.
type NudgeEnv struct {
	// Base is the public origin the copy's links start with.
	Base string
	// Surface is where the tip is shown: receipt (HTTP) or mcp_result.
	Surface string
	// Source is the caller's network address.
	Source string
	// MCP: the answer is an MCP tool result, so how is a tool call.
	MCP bool
	// Identity is the MCP call that gives an anonymous caller an identity
	// (create_identity), or "" where there is none.
	Identity string
}

type nudgeSeen struct {
	code string
	at   int64
}

// nudgeSubject is what the engine knows of one subject.
type nudgeSubject struct {
	account  string // "" for a network
	seen     []nudgeSeen
	acts     uint64 // act kinds ever done (accounts)
	off      bool
	holdout  bool
	loadedAt int64
}

type nudgeState struct {
	mu       sync.Mutex
	subjects map[string]*nudgeSubject
	footers  map[string]int64  // agent: when its last waiting footer was shown
	accounts map[string]string // public key: its account
}

// nudgeCall is one answer the engine considers.
type nudgeCall struct {
	cmd    Command
	res    *Result
	env    NudgeEnv
	signed bool
	agent  string // the signed caller's fingerprint
	sub    *nudgeSubject
	now    int64
}

// nudgeFact says whether a tip applies to a call and fills its copy.
type nudgeFact func(ctx context.Context, s *Store, c *nudgeCall) (map[string]string, bool)

// nudgeTipFacts are the facts of every tip in NudgeCodes, by code: the one
// place a tip's "when" is decided (TestNudgeTipsHaveFacts).
var nudgeTipFacts = map[string]nudgeFact{
	"sign_for_replies": func(_ context.Context, _ *Store, c *nudgeCall) (map[string]string, bool) {
		if c.signed || c.res.Receipt == nil {
			return nil, false
		}
		return nil, true
	},
	"replies_delivered": func(_ context.Context, _ *Store, c *nudgeCall) (map[string]string, bool) {
		if !c.signed || c.res.Receipt == nil || !c.res.Receipt.PushAdvice {
			return nil, false
		}
		return map[string]string{"cursor": c.res.Receipt.Cursor}, true
	},
	"wake_on_reply": func(ctx context.Context, s *Store, c *nudgeCall) (map[string]string, bool) {
		if !c.signed || c.sub.account == "" || !s.config.Features.ServiceEnabled("wakeup") || !readCarriesReply(c) {
			return nil, false
		}
		var set bool
		if s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM wakeups WHERE account=? AND state='active')", c.sub.account).Scan(&set) != nil || set {
			return nil, false
		}
		return nil, true
	},
	"first_work": func(ctx context.Context, s *Store, c *nudgeCall) (map[string]string, bool) {
		// A post, or a read of its own updates (where it looks for what to
		// do next); home lists the same task as its open_task item.
		if !c.signed || c.sub.account == "" || c.cmd.Operation == "updates.get" && c.cmd.Target != "" && c.cmd.Target != c.agent {
			return nil, false
		}
		t, ok := s.claimableTask(ctx, c.sub.account)
		if !ok {
			return nil, false
		}
		return fitTitle("first_work", t.vars()), true
	},
	"encrypted_dm": func(_ context.Context, _ *Store, c *nudgeCall) (map[string]string, bool) {
		if !c.signed || c.res.Receipt == nil || !c.res.Receipt.Public || c.cmd.To == "" {
			return nil, false
		}
		short := c.cmd.To
		if len(short) > 12 && !strings.HasPrefix(short, "@") {
			short = short[:12] + "…"
		}
		return map[string]string{"to": c.cmd.To, "to_short": short}, true
	},
	"rate_paid_call": func(ctx context.Context, s *Store, c *nudgeCall) (map[string]string, bool) {
		return s.ratePaidCallFact(ctx, c)
	},
	"rate_worker": func(ctx context.Context, s *Store, c *nudgeCall) (map[string]string, bool) {
		return s.rateWorkerFact(ctx, c)
	},
}

// readCarriesReply reports whether a call is the caller's own inbox read
// (updates.get or home for itself) and its answer carries a reply to it.
func readCarriesReply(c *nudgeCall) bool {
	if c.res.Data == nil || c.cmd.Target != "" && c.cmd.Target != c.agent {
		return false
	}
	switch c.cmd.Operation {
	case "updates.get":
		replies, _ := c.res.Data["replies"].([]string)
		return len(replies) > 0
	case "home":
		items, _ := c.res.Data["items"].([]HomeItem)
		return slices.ContainsFunc(items, func(it HomeItem) bool { return it.Kind == "replies" && it.Count > 0 })
	}
	return false
}

// nudgeTriggered reports whether any tip triggers on op.
func nudgeTriggered(op string) bool {
	for _, n := range NudgeCodes {
		if n.Kind == NudgeTip && slices.Contains(n.Triggers, op) {
			return true
		}
	}
	return false
}

// nudgeSubjectKey is the subject of a command: a signed caller's account,
// else its network ("net:" and the keyed hash); "" when neither is known.
func (s *Store) nudgeSubjectKey(ctx context.Context, cmd Command, source string) (key, account string) {
	if cmd.PublicKey != "" {
		st := &s.nudges
		st.mu.Lock()
		account = st.accounts[cmd.PublicKey]
		st.mu.Unlock()
		if account == "" {
			if account = s.accountOfKey(ctx, cmd.PublicKey); account != "" {
				st.mu.Lock()
				if st.accounts == nil || len(st.accounts) >= nudgeSubjectsMax {
					st.accounts = map[string]string{}
				}
				st.accounts[cmd.PublicKey] = account
				st.mu.Unlock()
			}
		}
		return account, account
	}
	if _, net := s.signalHashes(source); net != "" {
		return "net:" + net, ""
	}
	return "", ""
}

// keyFingerprint is the fingerprint of a base64url public key, or "".
func keyFingerprint(publicKey string) string {
	raw, err := b64.URL(publicKey)
	if err != nil {
		return ""
	}
	return fingerprint(raw)
}

// nudgesSuppressed reports whether no tip or footer reaches this request:
// the site's own pages, and the openai family while NudgesOpenAI is off.
func (s *Store) nudgesSuppressed(ctx context.Context) bool {
	return ViaFrom(ctx) == "ui" || ClientFamilyFrom(ctx) == "openai" && !s.config.Features.NudgesOpenAI
}

// Nudge chooses this answer's tip, if any, sets next.nudge and logs it. It
// never fails the request.
func (s *Store) Nudge(ctx context.Context, cmd Command, res *Result, env NudgeEnv) {
	defer func() { _ = recover() }()
	if res == nil || res.Receipt != nil && res.Receipt.Duplicate {
		return
	}
	key, account := s.nudgeSubjectKey(ctx, cmd, env.Source)
	if key == "" {
		return
	}
	now := s.now().Unix()
	if cmd.Operation == "messaging.policy.set" {
		s.forgetNudgeSubject(key)
	}
	triggered := nudgeTriggered(cmd.Operation) && !s.nudgesSuppressed(ctx)
	if !triggered {
		s.nudgeSawActs(key, cmd)
		return
	}
	sub := s.nudgeSubjectFor(ctx, key, account, now)
	if sub == nil || sub.off {
		s.nudgeSawActs(key, cmd)
		return
	}
	call := &nudgeCall{cmd: cmd, res: res, env: env, signed: cmd.PublicKey != "", sub: sub, now: now}
	if call.signed {
		call.agent = keyFingerprint(cmd.PublicKey)
	}
	tip, code, variant, ok := s.chooseNudge(ctx, call, now)
	s.nudgeSawActs(key, cmd)
	if !ok {
		return
	}
	s.funnelLogNudge(nudgeShown{subject: key, code: code, variant: variant, surface: env.Surface, at: now})
	if sub.holdout && s.config.Features.NudgeHoldout {
		return
	}
	if res.Next == nil {
		res.Next = &Next{}
	}
	res.Next.Nudge = tip
}

// chooseNudge applies the steps above to one call. The subject's history is
// updated here, under the lock, so two answers at once cannot both pass a
// cap.
func (s *Store) chooseNudge(ctx context.Context, c *nudgeCall, now int64) (*NudgeShown, string, string, bool) {
	segment := "anonymous"
	if c.signed {
		segment = "signed"
	}
	type candidate struct {
		n    NudgeCode
		last int64
	}
	var cands []candidate
	s.nudges.mu.Lock()
	today, seen := 0, map[string]int64{}
	for _, h := range c.sub.seen {
		if h.at > now-86400 {
			today++
		}
		seen[h.code] = max(seen[h.code], h.at)
	}
	acts := c.sub.acts
	s.nudges.mu.Unlock()
	if today >= NudgesPerDay {
		return nil, "", "", false
	}
	for _, n := range NudgeCodes {
		if n.Kind != NudgeTip || !slices.Contains(n.Triggers, c.cmd.Operation) || !slices.Contains(n.Segments, segment) {
			continue
		}
		if last, ok := seen[n.Code]; ok && last > now-NudgeRepeatDays*86400 {
			continue
		}
		if c.signed && slices.ContainsFunc(n.Unused, func(k string) bool { return acts&ActBit(k) != 0 }) {
			continue
		}
		cands = append(cands, candidate{n, seen[n.Code]})
	}
	slices.SortStableFunc(cands, func(a, b candidate) int {
		if a.n.Relevance != b.n.Relevance {
			return b.n.Relevance - a.n.Relevance
		}
		return int(a.last - b.last)
	})
	for _, cand := range cands {
		vars, ok := nudgeTipFacts[cand.n.Code](ctx, s, c)
		if !ok {
			continue
		}
		tip, variant := renderNudge(cand.n, c, vars)
		if tip == nil {
			continue
		}
		s.nudges.mu.Lock()
		c.sub.seen = append(c.sub.seen, nudgeSeen{cand.n.Code, now})
		s.nudges.mu.Unlock()
		return tip, cand.n.Code, variant, true
	}
	return nil, "", "", false
}

// renderNudge fills a tip's copy: the variant this subject gets this week,
// and the exact call for the surface. A filled text over NudgeTextBytes is
// cut at a word.
func renderNudge(n NudgeCode, c *nudgeCall, vars map[string]string) (*NudgeShown, string) {
	if len(n.Variants) == 0 {
		return nil, ""
	}
	h := fnv.New32a()
	h.Write([]byte(c.sub.account + "\x00" + n.Code))
	i := int((uint64(h.Sum32()) + uint64(c.now/(7*86400))) % uint64(len(n.Variants)))
	pairs := []string{"{base}", c.env.Base, "{agent}", c.agent}
	identity := c.env.Identity
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", v)
	}
	r := strings.NewReplacer(pairs...)
	text := oneLine(r.Replace(n.Variants[i]))
	how := n.HowHTTP
	if c.env.MCP && n.HowMCP != "" && (n.HowMCP != "{identity}" || identity != "") {
		how = strings.ReplaceAll(n.HowMCP, "{identity}", identity)
	}
	how = oneLine(r.Replace(how))
	if text == "" {
		return nil, ""
	}
	if len(text) > NudgeTextBytes {
		text = truncateUTF8(text, NudgeTextBytes)
	}
	return &NudgeShown{Code: n.Code, Text: text, How: how}, pairVariant(fmt.Sprint(i), vars[nudgePairVar])
}

// oneLine is s with control characters as spaces.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// nudgeSubjectFor is the subject's state, loaded on first sight (and after
// nudgeReloadSeconds) from the log, the agent's days and its settings. The
// reads hold no transaction.
func (s *Store) nudgeSubjectFor(ctx context.Context, key, account string, now int64) *nudgeSubject {
	st := &s.nudges
	st.mu.Lock()
	if sub := st.subjects[key]; sub != nil && now-sub.loadedAt < nudgeReloadSeconds {
		st.mu.Unlock()
		return sub
	}
	st.mu.Unlock()
	sub := &nudgeSubject{account: account, loadedAt: now, holdout: s.funnelHoldout(key)}
	rows, err := s.db.QueryContext(ctx, "SELECT code,shown_at FROM nudge_events WHERE subject=? AND shown_at>?", key, now-NudgeRepeatDays*86400)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var h nudgeSeen
		if rows.Scan(&h.code, &h.at) == nil {
			sub.seen = append(sub.seen, h)
		}
	}
	if rows.Close() != nil {
		return nil
	}
	if account != "" {
		rows, err := s.db.QueryContext(ctx, "SELECT acts FROM agent_days WHERE account=?", account)
		if err != nil {
			return nil
		}
		for rows.Next() {
			var a int64
			if rows.Scan(&a) == nil {
				sub.acts |= uint64(a)
			}
		}
		if rows.Close() != nil {
			return nil
		}
		var raw string
		if s.db.QueryRowContext(ctx, "SELECT settings FROM messaging_settings WHERE account=?", account).Scan(&raw) == nil {
			var p struct {
				Nudges string `json:"nudges"`
			}
			_ = json.Unmarshal([]byte(raw), &p)
			sub.off = p.Nudges == "off"
		}
	}
	// What the funnel holds unflushed counts too.
	f := &s.funnel
	f.mu.Lock()
	for _, n := range f.nudges {
		if n.subject == key && n.at > now-NudgeRepeatDays*86400 {
			sub.seen = append(sub.seen, nudgeSeen{n.code, n.at})
		}
	}
	if p := f.pending[account]; account != "" && p != nil {
		for _, d := range p.days {
			sub.acts |= d.acts
		}
	}
	f.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.subjects == nil || len(st.subjects) >= nudgeSubjectsMax {
		st.subjects = map[string]*nudgeSubject{}
	}
	if old := st.subjects[key]; old != nil && now-old.loadedAt < nudgeReloadSeconds {
		return old // another answer loaded it meanwhile
	}
	st.subjects[key] = sub
	return sub
}

// nudgeSawActs marks the act kinds a command shows on a cached subject, so
// a tip for a feature stops once the agent uses it.
func (s *Store) nudgeSawActs(key string, cmd Command) {
	if cmd.PublicKey == "" {
		return
	}
	var bits uint64
	for _, k := range liveKinds(cmd, 0) {
		bits |= ActBit(k)
	}
	if cmd.Operation == "home" {
		bits |= ActBit("check_in")
	}
	if cmd.Operation == "service.call" {
		bits |= ActBit("tool_call")
	}
	st := &s.nudges
	st.mu.Lock()
	if sub := st.subjects[key]; sub != nil {
		sub.acts |= bits
	}
	st.mu.Unlock()
}

// forgetNudgeSubject drops a cached subject, so its next answer reloads
// (its settings changed).
func (s *Store) forgetNudgeSubject(key string) {
	st := &s.nudges
	st.mu.Lock()
	delete(st.subjects, key)
	st.mu.Unlock()
}

// openTaskRow is an open task with a reward that an agent may claim
// (claimableTaskTx, home.go): an ordinary task, or a standing task whose
// reward is per accepted result.
type openTaskRow struct {
	id, title string
	credits   int64
	usdc      int64
	// perResult: a standing task, paying credits and usdc per accepted
	// result; upTo: its tiers differ, and these are the largest.
	perResult, upTo bool
}

// standingReward sets t's reward to a standing task's tiers: the one tier,
// or up to the largest (by USDC, then credits).
func (t *openTaskRow) standingReward(tiers []WorkStandingTier) {
	t.perResult, t.credits, t.usdc = true, 0, 0
	for i, tier := range tiers {
		u := tier.usdcUnits()
		if i == 0 || u > t.usdc || u == t.usdc && tier.Credits > t.credits {
			t.credits, t.usdc = tier.Credits, u
		}
		t.upTo = t.upTo || i > 0
	}
}

func (t openTaskRow) reward() string {
	var r string
	switch {
	case t.usdc > 0 && t.credits > 0:
		r = fmt.Sprintf("%s USDC + %d credits", usdcText(t.usdc), t.credits)
	case t.usdc > 0:
		r = usdcText(t.usdc) + " USDC"
	default:
		r = fmt.Sprintf("%d credits", t.credits)
	}
	if t.upTo {
		r = "up to " + r
	}
	if t.perResult {
		r += " per accepted result"
	}
	return r
}

// usdcText is a USDC amount in its 6-decimal base units, as dollars.
func usdcText(units int64) string {
	s := fmt.Sprintf("%d.%06d", units/1_000_000, units%1_000_000)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func (t openTaskRow) vars() map[string]string {
	if t.id == "" {
		return nil
	}
	return map[string]string{"task": t.id, "title": clip(t.title), "reward": t.reward()}
}

// fitTitle shortens vars' title so that every variant of the tip code,
// filled, stays within NudgeTextBytes: the title gives way, never the
// reward or the claim order after it.
func fitTitle(code string, vars map[string]string) map[string]string {
	n, _ := LookupNudge(code)
	title := vars["title"]
	longest := 0
	for _, v := range n.Variants {
		r := strings.NewReplacer("{title}", "", "{reward}", vars["reward"], "{task}", vars["task"])
		longest = max(longest, len(r.Replace(v)))
	}
	if budget := NudgeTextBytes - longest - len("…"); len(title) > budget && budget > 0 {
		vars["title"] = truncateUTF8(strings.TrimSuffix(title, "…"), budget) + "…"
	}
	return vars
}

// claimableTask is claimableTaskTx in a public read of its own: the task
// first_work names. account "" excludes no requester.
func (s *Store) claimableTask(ctx context.Context, account string) (openTaskRow, bool) {
	var t openTaskRow
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var generation string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
			return err
		}
		var err error
		t, err = s.claimableTaskTx(ctx, tx, account, s.now().Unix(), generation)
		return err
	})
	return t, err == nil && t.id != ""
}

// NudgeCapabilities is /capabilities nudges: the rules and the catalogue,
// from the Go tables.
func NudgeCapabilities() map[string]any {
	return map[string]any{
		"field":   "next.nudge",
		"caps":    map[string]int{"per_answer": NudgesPerAnswer, "per_day": NudgesPerDay, "repeat_days": NudgeRepeatDays, "text_bytes": NudgeTextBytes},
		"order":   "eligible (trigger, segment, unused acts, the call's fact), capped, then the most relevant; ties go to the least recently shown",
		"off":     `messaging.policy.set data {"schema":1,"nudges":"off"} (MCP set_protection nudges off): no tips and no waiting footer`,
		"holdout": "while an experiment runs, a share of agents and networks gets no tips, for comparison; what each would have got is logged",
		"codes":   NudgeCodes,
	}
}
