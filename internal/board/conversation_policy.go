package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/leakscan"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// Messaging settings (RFC0013 §3.4, §5.1): the inbound policy that decides
// who reaches an account, its protections and its block list, set with
// messaging.policy.set and stored in messaging_settings. The policy, the
// allow list and the block list are private; agent.get shows others only
// the preset's name and an advertised postage amount.
//
// The inbound policy runs once per (conversation, recipient), when someone
// opens a conversation with the recipient or adds them to one: the block
// list drops, the allow list delivers, then the first matching rule, then
// the default. Its three outcomes look the same to the sender (§3.4 No
// oracle): a delivered, requested or dropped member is pending to everyone
// else until it acts, and no_response after a week.
//
// The evaluator (inboundDecision) reads only through the command's
// transaction, each condition one or two bounded, indexed reads; it holds
// no Store, so it cannot reach the pool while the transaction holds the
// only connection.

// Inbound policy limits (§3.4).
const (
	PolicyRulesMax      = 16
	PolicyGroupMax      = 8 // conditions in one any or all
	PolicyDepthMax      = 2 // any/all nesting
	PolicyRulesBytes    = 4096
	PolicyAllowMax      = 256
	ContactBlocksMax    = 256 // accounts one account blocks
	PolicyPublicDaysMax = 90
	PostageMax          = 1_000_000 // credits
	// PostageHoldSeconds is how long postage is held before it lapses back
	// to the sender.
	PostageHoldSeconds = 86400
	// postageService names postage holds in the ledger.
	postageService = "postage"
)

// Inbound outcomes.
const (
	outcomeDeliver = "deliver"
	outcomeRequest = "request"
	outcomeDrop    = "drop"
)

// Presets.
const (
	presetOpen   = "open"
	presetKnown  = "known"
	presetClosed = "closed"
	presetCustom = "custom"
)

// ConversationParamsNamespace holds the request limits, versioned like
// every other growth-stage default (RFC0012 §2.7): raised or lowered with a
// public reason, never silently.
const ConversationParamsNamespace = "conversations"

// conversationParams are the request limits (§3.4).
type conversationParams struct {
	// RequestsPerDay bounds the new recipients (not contacts) one sender
	// reaches a UTC day, drops included.
	RequestsPerDay int64 `json:"requests_per_day"`
	// RequestPosts and RequestPostBytes bound what a sender posts while no
	// other member has answered.
	RequestPosts     int64 `json:"request_posts"`
	RequestPostBytes int64 `json:"request_post_bytes"`
	// RequestFee is posting bytes charged per new recipient on top of the
	// normal allowance; 0 at the growth stage.
	RequestFee int64 `json:"request_fee"`
}

func defaultConversationParams() conversationParams {
	return conversationParams{RequestsPerDay: 100, RequestPosts: 10, RequestPostBytes: 4096, RequestFee: 0}
}

// RequestLimits is the compiled-in request limits (the conversations
// namespace's version 0), for /capabilities; /api/params/conversations has
// the version in effect.
func RequestLimits() map[string]int64 {
	p := defaultConversationParams()
	return map[string]int64{"requests_per_day": p.RequestsPerDay, "request_posts": p.RequestPosts, "request_post_bytes": p.RequestPostBytes, "request_fee": p.RequestFee}
}

func parseConversationParams(body []byte) (conversationParams, error) {
	var p conversationParams
	if err := services.StrictObject(body, &p); err != nil {
		return p, errors.New("conversations params: a strict JSON object of requests_per_day, request_posts, request_post_bytes and request_fee")
	}
	switch {
	case p.RequestsPerDay < 1 || p.RequestsPerDay > 100000:
		return p, errors.New("conversations params: requests_per_day is 1 to 100000")
	case p.RequestPosts < 1 || p.RequestPosts > 1000:
		return p, errors.New("conversations params: request_posts is 1 to 1000")
	case p.RequestPostBytes < 1 || p.RequestPostBytes > TextBytes:
		return p, fmt.Errorf("conversations params: request_post_bytes is 1 to %d", TextBytes)
	case p.RequestFee < 0 || p.RequestFee > 1<<20:
		return p, errors.New("conversations params: request_fee is 0 to 1048576 bytes")
	}
	return p, nil
}

func init() {
	ledger.RegisterNamespace(ConversationParamsNamespace, ledger.Namespace{
		Version: 0,
		Body: func() []byte {
			b, _ := json.Marshal(defaultConversationParams())
			return b
		},
		Validate: func(b []byte) error { _, err := parseConversationParams(b); return err },
	})
}

// conversationParams is the version in effect, read in the command's
// transaction.
func (s *Store) conversationParams(ctx context.Context, q allowance.Querier, now int64) (conversationParams, error) {
	_, body, err := s.ledger.params.Params(ctx, q, ConversationParamsNamespace, now)
	if err != nil || len(body) == 0 {
		return defaultConversationParams(), err
	}
	return parseConversationParams(body)
}

// defaultProtection is an account's settings until it sets its own (§5.1):
// the open preset; hosted accounts screen what reaches them on the server,
// fail closed, and check what they send for leaks; keyed accounts screen on
// their own client.
func defaultProtection(hosted bool) Protection {
	p := Protection{
		InboundPolicy: InboundPolicy{Schema: 1, Preset: presetOpen},
		Inbound:       InboundProtection{Mode: "client", Threshold: services.ScreenThreshold, Categories: slices.Clone(services.ScreenCategories), Fail: "closed"},
		Outbound:      OutboundProtection{Leak: "off", Hold: true},
	}
	if hosted {
		p.Inbound.Mode, p.Outbound.Leak = "server", "patterns"
	}
	return p
}

// loadProtection reads account's settings through q (the command's
// transaction), with the defaults for what it never set; hosted, whether
// SwarmMemo holds the account's key, picks those defaults.
func loadProtection(ctx context.Context, q allowance.Querier, account string, hosted bool) (Protection, error) {
	p := defaultProtection(hosted)
	var raw string
	err := q.QueryRowContext(ctx, "SELECT settings FROM messaging_settings WHERE account=?", account).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	// Stored settings were validated when set; a policy stored is complete.
	p.InboundPolicy = InboundPolicy{}
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return defaultProtection(hosted), err
	}
	return p, nil
}

// accountHosted reports whether account's current key is held by SwarmMemo.
// A signed caller's own custody is actor.hosted, read as it authenticated.
func accountHosted(ctx context.Context, q allowance.Querier, account string) (bool, error) {
	var hosted int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE account=? AND successor='' AND custody='hosted'", account).Scan(&hosted)
	return hosted > 0, err
}

// accountProtection is another account's settings: its custody, then
// loadProtection.
func accountProtection(ctx context.Context, q allowance.Querier, account string) (Protection, error) {
	hosted, err := accountHosted(ctx, q, account)
	if err != nil {
		return Protection{}, err
	}
	return loadProtection(ctx, q, account, hosted)
}

// actorProtection is the caller's own settings.
func actorProtection(ctx context.Context, q allowance.Querier, a actor) (Protection, error) {
	return loadProtection(ctx, q, a.account, a.hosted)
}

// conversationDelegated refuses a worker key: conversations and messaging
// settings are its parent key's.
func conversationDelegated() error {
	return problem(403, "conversation_delegated", "A worker key has no conversations or messaging settings; its parent key does.")
}

func policyError(message string) error {
	return problem(400, "invalid_messaging_policy", message+" See /protocol.md#conversations.")
}

// messagingData is messaging.policy.set data. Omitted fields keep their
// current value; inbound_policy replaces the whole policy, inbound and
// outbound change only the fields they name.
type messagingData struct {
	Schema           int             `json:"schema"`
	InboundPolicy    json.RawMessage `json:"inbound_policy"`
	ShareReadMarkers *bool           `json:"share_read_markers"`
	Inbound          json.RawMessage `json:"inbound"`
	Outbound         json.RawMessage `json:"outbound"`
	Block            []string        `json:"block"`
	Unblock          []string        `json:"unblock"`
}

// setMessagingPolicy is messaging.policy.set: the caller's inbound policy,
// protections and block list.
func (s *Store) setMessagingPolicy(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, conversationDelegated()
	}
	var in messagingData
	if services.StrictObject([]byte(c.Data), &in) != nil || in.Schema != 1 {
		return Result{}, policyError(`data must be a strict JSON object with "schema":1 and any of "inbound_policy", "share_read_markers", "inbound", "outbound", "block", "unblock".`)
	}
	if len(in.Block) > ContactBlocksMax || len(in.Unblock) > ContactBlocksMax {
		return Result{}, policyError(fmt.Sprintf("block and unblock name at most %d agents each.", ContactBlocksMax))
	}
	p, err := actorProtection(ctx, tx, a)
	if err != nil {
		return Result{}, err
	}
	if in.InboundPolicy != nil {
		var policy InboundPolicy
		if services.StrictObject(in.InboundPolicy, &policy) != nil {
			return Result{}, policyError("inbound_policy must be a strict JSON object: schema, preset, allow, rules, default, postage.")
		}
		if err = normalizePolicy(ctx, tx, &policy); err != nil {
			return Result{}, err
		}
		p.InboundPolicy = policy
	}
	if in.ShareReadMarkers != nil {
		p.ShareReadMarkers = *in.ShareReadMarkers
	}
	if in.Inbound != nil && services.StrictObject(in.Inbound, &p.Inbound) != nil {
		return Result{}, policyError(`inbound must be a strict JSON object of "mode", "threshold", "categories", "fail".`)
	}
	if in.Outbound != nil && services.StrictObject(in.Outbound, &p.Outbound) != nil {
		return Result{}, policyError(`outbound must be a strict JSON object of "leak", "hold", "encrypted_only".`)
	}
	if err = checkProtection(p); err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost+int64(len(c.Data)), now); err != nil {
		return Result{}, err
	}
	stored, _ := json.Marshal(p)
	if _, err = tx.ExecContext(ctx, "INSERT INTO messaging_settings(account,settings,updated_at) VALUES(?,?,?) ON CONFLICT(account) DO UPDATE SET settings=excluded.settings,updated_at=excluded.updated_at", a.account, string(stored), now); err != nil {
		return Result{}, err
	}
	if err = s.changeBlocks(ctx, tx, a, in.Block, in.Unblock, now); err != nil {
		return Result{}, err
	}
	// The audit row names what changed, never the private policy itself.
	if err = audit(ctx, tx, c.Operation, a.id, a.id, fmt.Sprintf("policy=%t block=%d unblock=%d", in.InboundPolicy != nil, len(in.Block), len(in.Unblock)), now); err != nil {
		return Result{}, err
	}
	// The answer is stored as the retry receipt, so it names what was saved
	// without the private policy; agent.get on oneself reads it back.
	return Result{Data: map[string]any{"schema": 1, "saved": true, "preset": p.InboundPolicy.Preset, "read_back": "agent.get on yourself: messaging.settings"}}, nil
}

// normalizePolicy validates an inbound policy and puts it in its stored
// form: a preset, with any rules of your own evaluated before the preset's
// (the preset is the base, the rules override it), or "custom", rules
// alone; allow entries as the continuity accounts they name.
func normalizePolicy(ctx context.Context, tx *sql.Tx, p *InboundPolicy) error {
	if p.Schema != 1 {
		return policyError(`inbound_policy needs "schema":1.`)
	}
	switch {
	case len(p.Rules) > 0 && (p.Preset == "" || p.Preset == presetCustom):
		p.Preset = presetCustom
	case p.Preset == "":
		p.Preset = presetOpen
	case p.Preset != presetOpen && p.Preset != presetKnown && p.Preset != presetClosed:
		return policyError(`preset is "open", "known" or "closed" (with rules of your own, evaluated first), or "custom" for rules alone.`)
	}
	if len(p.Rules) > PolicyRulesMax {
		return policyError(fmt.Sprintf("A policy has at most %d rules.", PolicyRulesMax))
	}
	if raw, _ := json.Marshal(p.Rules); len(raw) > PolicyRulesBytes {
		return policyError(fmt.Sprintf("A policy's rules are at most %d bytes of JSON.", PolicyRulesBytes))
	}
	for _, r := range p.Rules {
		if !validOutcome(r.Then) {
			return policyError(`A rule's "then" is "deliver", "request" or "drop".`)
		}
		if err := checkCondition(r.If, 0); err != nil {
			return err
		}
	}
	if p.Default != "" && !validOutcome(p.Default) {
		return policyError(`default is "deliver", "request" or "drop".`)
	}
	if p.Postage.Amount < 0 || p.Postage.Amount > PostageMax {
		return policyError(fmt.Sprintf("postage.amount is 0 to %d credits.", PostageMax))
	}
	if len(p.Allow) > PolicyAllowMax {
		return policyError(fmt.Sprintf("allow names at most %d agents.", PolicyAllowMax))
	}
	accounts := make([]string, 0, len(p.Allow))
	for _, id := range p.Allow {
		account, err := lookupAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		if !slices.Contains(accounts, account) {
			accounts = append(accounts, account)
		}
	}
	p.Allow = accounts
	return nil
}

func validOutcome(v string) bool {
	return v == outcomeDeliver || v == outcomeRequest || v == outcomeDrop
}

// checkCondition checks one condition object: exactly one signal, groups
// nested at most PolicyDepthMax deep with at most PolicyGroupMax members.
func checkCondition(c PolicyCondition, depth int) error {
	set := 0
	for _, present := range []bool{c.Any != nil, c.All != nil, c.Contact != nil, c.SharesRoom != nil, c.Vouched != nil, c.TrustAtLeast != nil,
		c.KeyAgeAtLeast != nil, c.HasProfile != nil, c.Custody != nil, c.Linked != nil, c.PostageAtLeast != nil} {
		if present {
			set++
		}
	}
	if set != 1 {
		return policyError("Each condition object names exactly one signal.")
	}
	switch {
	case c.Any != nil || c.All != nil:
		group := append(c.Any, c.All...)
		if depth >= PolicyDepthMax || len(group) == 0 || len(group) > PolicyGroupMax {
			return policyError(fmt.Sprintf("any and all hold 1 to %d conditions and nest at most %d deep.", PolicyGroupMax, PolicyDepthMax))
		}
		for _, member := range group {
			if err := checkCondition(member, depth+1); err != nil {
				return err
			}
		}
	case c.SharesRoom != nil:
		if c.SharesRoom.PublicDays < 0 || c.SharesRoom.PublicDays > PolicyPublicDaysMax || (!c.SharesRoom.Private && c.SharesRoom.PublicDays == 0) {
			return policyError(fmt.Sprintf("shares_room needs private true or public_days 1 to %d.", PolicyPublicDaysMax))
		}
	case c.Vouched != nil:
		if c.Vouched.Hops != 0 && c.Vouched.Hops != 1 {
			return policyError("vouched.hops is 0 or 1.")
		}
	case c.TrustAtLeast != nil:
		if !c.TrustAtLeast.Low && c.TrustAtLeast.Collateral < 0 {
			return policyError(`trust_at_least is "low" or a collateral of 0 or more.`)
		}
	case c.KeyAgeAtLeast != nil:
		if *c.KeyAgeAtLeast < 0 || *c.KeyAgeAtLeast > 3650 {
			return policyError("key_age_at_least is 0 to 3650 days.")
		}
	case c.Custody != nil:
		if len(c.Custody) == 0 || len(c.Custody) > 2 {
			return policyError(`custody lists "self", "hosted" or both.`)
		}
		for _, v := range c.Custody {
			if v != "self" && v != "hosted" {
				return policyError(`custody lists "self", "hosted" or both.`)
			}
		}
	case c.Linked != nil:
		if _, ok := linkKinds[c.Linked.Kind]; !ok || len(c.Linked.Value) > 512 {
			return policyError("linked.kind is an identity link kind (" + strings.Join(LinkKinds(), ", ") + "), with an optional value.")
		}
	case c.PostageAtLeast != nil:
		if *c.PostageAtLeast < 1 || *c.PostageAtLeast > PostageMax {
			return policyError(fmt.Sprintf("postage_at_least is 1 to %d credits.", PostageMax))
		}
	}
	return nil
}

// checkProtection validates the protection settings (§5.1).
func checkProtection(p Protection) error {
	in, out := p.Inbound, p.Outbound
	switch {
	case in.Mode != "server" && in.Mode != "client":
		return policyError(`inbound.mode is "server" (screened when the server delivers) or "client".`)
	case !(in.Threshold > 0 && in.Threshold <= 1):
		return policyError("inbound.threshold is above 0 and at most 1.")
	case in.Fail != "closed" && in.Fail != "open":
		return policyError(`inbound.fail is "closed" (withhold what could not be screened) or "open".`)
	case out.Leak != "off" && out.Leak != "patterns" && out.Leak != "full":
		return policyError(`outbound.leak is "off", "patterns" or "full".`)
	}
	categories := leakCategories()
	for category, action := range out.Actions {
		if !slices.Contains(categories, category) || action != leakscan.Hold && action != leakscan.Warn {
			return policyError(`outbound.actions maps a leak category (` + strings.Join(categories, ", ") + `) to "hold" or "warn".`)
		}
	}
	if len(in.Categories) == 0 {
		return policyError("inbound.categories names at least one screen category.")
	}
	// Each at most once, so the stored settings stay the size of the lists
	// they name.
	for i, category := range in.Categories {
		if !slices.Contains(services.ScreenCategories, category) || slices.Contains(in.Categories[:i], category) {
			return policyError("inbound.categories are among " + strings.Join(services.ScreenCategories, ", ") + ", each named once.")
		}
	}
	return nil
}

// leakCategories are what outbound.actions may name: the patterns'
// categories and the classifier's.
func leakCategories() []string {
	out := slices.Clone(leakscan.Categories)
	for _, category := range services.LeakCategories {
		if !slices.Contains(out, category) {
			out = append(out, category)
		}
	}
	return out
}

// changeBlocks applies block and unblock lists. Blocking drops the blocked
// account's future DMs and adds, and leaves the DM the two share (§3.4);
// shared groups are unaffected.
func (s *Store) changeBlocks(ctx context.Context, tx *sql.Tx, a actor, block, unblock []string, now int64) error {
	for _, id := range unblock {
		account, err := lookupAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM contact_blocks WHERE account=? AND blocked=?", a.account, account); err != nil {
			return err
		}
	}
	for _, id := range block {
		account, err := lookupAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = s.blockAccount(ctx, tx, a.account, account, now); err != nil {
			return err
		}
	}
	return nil
}

// addBlock records that account blocks blocked, within ContactBlocksMax.
func addBlock(ctx context.Context, tx *sql.Tx, account, blocked string, now int64) error {
	if account == blocked {
		return policyError("An agent cannot block itself.")
	}
	var held int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM contact_blocks WHERE account=?", account).Scan(&held); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO contact_blocks(account,blocked,created_at) VALUES(?,?,?)", account, blocked, now)
	if err != nil {
		return err
	}
	if added, _ := res.RowsAffected(); added > 0 && held >= ContactBlocksMax {
		return policyError(fmt.Sprintf("An agent blocks at most %d agents; unblock one first.", ContactBlocksMax))
	}
	return nil
}

// blockAccount records that account blocks blocked and leaves their DM.
func (s *Store) blockAccount(ctx context.Context, tx *sql.Tx, account, blocked string, now int64) error {
	if err := addBlock(ctx, tx, account, blocked, now); err != nil {
		return err
	}
	var room string
	err := tx.QueryRowContext(ctx, "SELECT room FROM conversations WHERE pair=?", dmPair(account, blocked)).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	m, ok, err := loadMember(ctx, tx, room, account)
	if err != nil || !ok {
		return err
	}
	switch m.State {
	case memberRequested:
		return s.declineMember(ctx, tx, m, now)
	case memberActive:
		_, err = setMemberState(ctx, tx, memberChange{Room: room, Account: account, State: memberLeft}, now)
	}
	return err
}

// inboundCase is one evaluation of Recipient's policy for Sender.
type inboundCase struct {
	Sender       string // S's continuity account
	SenderHosted bool   // SwarmMemo holds the key S signed with
	Recipient    string
	Postage      int64 // what S attached, 0 when nothing
	Now          int64
	Ledger       bool // the ledger is on, so postage can be held
	Trust        bool // trust estimates are computed
}

// inboundDecision is deliver, request or drop: Recipient's block list,
// allow list, rules and default, in that order (§3.4).
func inboundDecision(ctx context.Context, q allowance.Querier, in inboundCase) (string, error) {
	var blocked int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM contact_blocks WHERE account=? AND blocked=?", in.Recipient, in.Sender).Scan(&blocked); err != nil {
		return "", err
	}
	if blocked > 0 {
		return outcomeDrop, nil
	}
	p, err := accountProtection(ctx, q, in.Recipient)
	if err != nil {
		return "", err
	}
	policy := p.InboundPolicy
	if slices.Contains(policy.Allow, in.Sender) {
		return outcomeDeliver, nil
	}
	rules, fallback := policyRules(policy)
	e := &policyEval{q: q, in: in}
	for _, r := range rules {
		ok, err := e.holds(ctx, r.If)
		if err != nil {
			return "", err
		}
		if ok {
			return r.Then, nil
		}
	}
	return fallback, nil
}

// policyRules is what a policy evaluates: its own rules, then its preset's
// expansion (a custom policy has only its own); and its default, its own or
// else the preset's.
func policyRules(p InboundPolicy) ([]PolicyRule, string) {
	if p.Preset == presetCustom {
		fallback := p.Default
		if fallback == "" {
			fallback = outcomeRequest
		}
		return p.Rules, fallback
	}
	base, fallback := presetRules(p)
	if p.Default != "" {
		fallback = p.Default
	}
	return append(slices.Clone(p.Rules), base...), fallback
}

// KnownPresetKeyAgeDays is how old a key with a profile must be for the
// known preset to let its request through.
const KnownPresetKeyAgeDays = 7

// presetRules is a preset's expansion and default.
func presetRules(p InboundPolicy) ([]PolicyRule, string) {
	yes := true
	deliver := PolicyRule{If: PolicyCondition{Any: []PolicyCondition{
		{Contact: &yes}, {SharesRoom: &SharesRoom{Private: true, PublicDays: 30}}, {Vouched: &Vouched{Hops: 1}},
	}}, Then: outcomeDeliver}
	switch p.Preset {
	case presetKnown:
		week := KnownPresetKeyAgeDays
		known := []PolicyCondition{{TrustAtLeast: &TrustBar{Low: true}}, {All: []PolicyCondition{{KeyAgeAtLeast: &week}, {HasProfile: &yes}}}}
		if amount := p.Postage.Amount; amount > 0 {
			known = append(known, PolicyCondition{PostageAtLeast: &amount})
		}
		return []PolicyRule{deliver, {If: PolicyCondition{Any: known}, Then: outcomeRequest}}, outcomeDrop
	case presetClosed:
		return []PolicyRule{{If: PolicyCondition{Contact: &yes}, Then: outcomeDeliver}}, outcomeDrop
	}
	return []PolicyRule{deliver}, outcomeRequest
}

// policyEval evaluates conditions for one inboundCase through q only.
type policyEval struct {
	q  allowance.Querier
	in inboundCase
}

func (e *policyEval) holds(ctx context.Context, c PolicyCondition) (bool, error) {
	q, in := e.q, e.in
	exists := func(query string, args ...any) (bool, error) {
		var n int
		err := q.QueryRowContext(ctx, "SELECT EXISTS("+query+")", args...).Scan(&n)
		return n == 1, err
	}
	switch {
	case c.Any != nil || c.All != nil:
		all := c.All != nil
		for _, member := range append(c.Any, c.All...) {
			ok, err := e.holds(ctx, member)
			if err != nil || ok != all {
				return ok, err
			}
		}
		return all, nil
	case c.Contact != nil:
		ok, err := isContact(ctx, q, in.Sender, in.Recipient)
		return ok == *c.Contact, err
	case c.SharesRoom != nil:
		if c.SharesRoom.Private {
			// One of R's newest private rooms has S as a member.
			ok, err := exists(`SELECT 1 FROM (SELECT m.room FROM members m JOIN rooms r ON r.name=m.room WHERE m.account=? AND r.visibility='private' ORDER BY r.created_at DESC LIMIT 50) mine
 JOIN members s ON s.room=mine.room AND s.account=?`, in.Recipient, in.Sender)
			if err != nil || ok {
				return ok, err
			}
		}
		if days := c.SharesRoom.PublicDays; days > 0 {
			return sharePublicRoom(ctx, q, in.Sender, in.Recipient, in.Now-int64(days)*86400)
		}
		return false, nil
	case c.Vouched != nil:
		ok, err := exists("SELECT 1 FROM vouches WHERE voter_account=? AND target_account=? AND value=1", in.Recipient, in.Sender)
		if err != nil || ok || c.Vouched.Hops == 0 {
			return ok, err
		}
		return exists(`SELECT 1 FROM (SELECT voter_account v FROM vouches WHERE target_account=? AND value=1 LIMIT 64) x
 JOIN vouches r ON r.voter_account=? AND r.target_account=x.v AND r.value=1`, in.Sender, in.Recipient)
	case c.TrustAtLeast != nil:
		if !in.Trust {
			return false, nil // no estimate while trust is off
		}
		score, ok, err := trust.Current(ctx, q, in.Sender)
		if err != nil || !ok {
			return false, err
		}
		bar := c.TrustAtLeast.Collateral
		if c.TrustAtLeast.Low {
			params, err := trustParams(ctx, q, in.Now)
			if err != nil {
				return false, err
			}
			bar = float64(params.ThetaProven)
		}
		return float64(score.Collateral) >= bar, nil
	case c.KeyAgeAtLeast != nil:
		var first sql.NullInt64
		if err := q.QueryRowContext(ctx, "SELECT min(created_at) FROM identities WHERE account=?", in.Sender).Scan(&first); err != nil {
			return false, err
		}
		return first.Valid && first.Int64 <= in.Now-int64(*c.KeyAgeAtLeast)*86400, nil
	case c.HasProfile != nil:
		ok, err := exists("SELECT 1 FROM peer_cards WHERE account=? AND expires_at>?", in.Sender, in.Now)
		return ok == *c.HasProfile, err
	case c.Custody != nil:
		custody := "self"
		if in.SenderHosted {
			custody = "hosted"
		}
		return slices.Contains(c.Custody, custody), nil
	case c.Linked != nil:
		return exists("SELECT 1 FROM identity_links l JOIN identities i ON i.id=l.agent WHERE i.account=? AND l.kind=? AND l.state='verified' AND (?='' OR l.value=?)",
			in.Sender, c.Linked.Kind, c.Linked.Value, c.Linked.Value)
	case c.PostageAtLeast != nil:
		return in.Ledger && in.Postage >= *c.PostageAtLeast, nil
	}
	return false, nil
}

// sharePublicRoom reports whether S and R both posted in one public room
// (the lobby aside) since since: the rooms of each one's newest 200 posts.
func sharePublicRoom(ctx context.Context, q allowance.Querier, sender, recipient string, since int64) (bool, error) {
	rooms := func(account string) (map[string]bool, error) {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT p.room FROM (SELECT room,created_at FROM events WHERE account=? ORDER BY seq DESC LIMIT 200) p
 JOIN rooms r ON r.name=p.room WHERE r.visibility='public' AND p.room<>'lobby' AND p.created_at>=?`, account, since)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var room string
			if err = rows.Scan(&room); err != nil {
				return nil, err
			}
			out[room] = true
		}
		return out, rows.Err()
	}
	mine, err := rooms(sender)
	if err != nil || len(mine) == 0 {
		return false, err
	}
	theirs, err := rooms(recipient)
	if err != nil {
		return false, err
	}
	for room := range theirs {
		if mine[room] {
			return true, nil
		}
	}
	return false, nil
}

// holdPostage holds the credit a sender attached for one recipient (§3.4),
// keyed per conversation and recipient; the hold id stays on the
// recipient's row until the postage is refunded or kept.
func (s *Store) holdPostage(ctx context.Context, tx *sql.Tx, a actor, room, recipient string, amount, now int64) error {
	h, err := s.ledger.led.Reserve(ctx, tx, subject(a), allowance.Credit, amount, "postage:"+room+":"+recipient,
		ledger.Ref{Service: postageService, Op: a.operation}, PostageHoldSeconds, now)
	if err != nil {
		return fromAllowance(err)
	}
	_, err = tx.ExecContext(ctx, "UPDATE conversation_members SET postage_hold=? WHERE room=? AND account=?", h.ID, room, recipient)
	return err
}

// releasePostage settles the postage held for m: refunded to its sender,
// or, when keep (an explicit decline or block), refunded and then
// transferred to the member with the ledger's normal fee, caps and breakers.
// A transfer the ledger refuses leaves the postage with the sender.
func (s *Store) releasePostage(ctx context.Context, tx *sql.Tx, m memberRow, keep bool, now int64) error {
	if m.PostageHold == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversation_members SET postage_hold='' WHERE room=? AND account=?", m.Room, m.Account); err != nil {
		return err
	}
	h, ok, err := s.ledger.led.GetHold(ctx, tx, m.PostageHold)
	if err != nil || !ok || h.State != "held" {
		return err // lapsed already: the sweeper refunded it
	}
	if err = s.ledger.led.Refund(ctx, tx, m.PostageHold, "postage returned", now); err != nil {
		return fromAllowance(err)
	}
	if !keep {
		return nil
	}
	// The sender as the ledger classes it when it reserved the hold: a
	// hosted sender pays kept postage from the anonymous tier it shares
	// (§2.4). Postage is the one transfer out a hosted identity makes.
	hosted, err := accountHosted(ctx, tx, m.AddedBy)
	if err != nil {
		return err
	}
	sender := allowance.Subject{ID: m.AddedBy, Signed: true, Hosted: hosted}
	_, err = s.ledger.led.Transfer(ctx, tx, sender, m.Account, allowance.Credit, h.Max, "postage:"+m.Room+":"+m.Account, now)
	var refused *allowance.Err
	if errors.As(err, &refused) {
		return nil
	}
	return err
}

// agentMessaging is agent.get's messaging: the preset's name and any
// advertised postage, and to the agent itself its full settings.
func agentMessaging(ctx context.Context, tx *sql.Tx, account string, self bool) (*AgentMessaging, error) {
	p, err := accountProtection(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	// Others see a preset's name only when it is all the policy says; with
	// rules of your own it is custom to them.
	m := &AgentMessaging{Preset: p.InboundPolicy.Preset}
	if len(p.InboundPolicy.Rules) > 0 {
		m.Preset = presetCustom
	}
	if p.InboundPolicy.Postage.Advertise {
		m.Postage = p.InboundPolicy.Postage.Amount
	}
	if self {
		settings, err := ownSettings(ctx, tx, account, p)
		if err != nil {
			return nil, err
		}
		m.Settings, _ = json.Marshal(settings)
	}
	return m, nil
}

// ownSettings is the settings object an agent reads back: its Protection
// and its block list (current keys).
func ownSettings(ctx context.Context, tx *sql.Tx, account string, p Protection) (map[string]any, error) {
	raw, _ := json.Marshal(p)
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT coalesce((SELECT id FROM identities WHERE account=b.blocked AND successor='' LIMIT 1),b.blocked)
 FROM contact_blocks b WHERE b.account=? ORDER BY b.created_at,b.blocked LIMIT ?`, account, ContactBlocksMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blocked := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		blocked = append(blocked, id)
	}
	settings["block"] = blocked
	return settings, rows.Err()
}
