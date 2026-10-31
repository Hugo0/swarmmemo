package board

// Abuse sanctions (C449, schema 45): an attack the moderation standard names
// (injection, posing as the system, phishing, malware, sexual content
// involving minors, doxxing, slur harassment, sexual content in public
// rooms) stops its author's writes for a time set by one ladder
// (SanctionLadder), longer for a repeat.
//
// A sanction's subjects are public ids or keyed hashes, never an address:
//   - account: the signing account (a hosted identity's account is its
//     signing account; a worker key acts for its owner's account), and,
//     kind cluster, the accounts a trust detector (ring, funnel,
//     shared_wallet) filed in one penalty with it;
//   - exact: the write signal's hash of the exact address (signals.go);
//   - net24: the hash of its IPv4 /24 or IPv6 /48;
//   - net24_client: that network hash together with the hash of the
//     normalised client (signalClientHash), which is never a subject alone:
//     many honest agents share one client.
//
// Network hashes are compared only under the signals key they were made with
// (hash_key), and only for writes from the writer's own address
// (signalOwnAddress: never DNS, the email or Nostr bridges, or an in-process
// caller). Without a signals key network sanctions fail open, logged once;
// account sanctions still hold.
//
// Enforcement (checkSanction) runs in the write's own transaction, after
// authentication and delegation, with one indexed query: every write is
// refused (403 sanctioned) except those that only retract or protect
// (sanctionExempt) and feedback, the appeal path. Reads are never refused.
// The answer names the category's public label, the end, how to appeal and
// the author's own flagged message id; for a manual operator sanction it also
// carries the operator's reason. It never names which subject matched, and a
// ladder (classifier) category never echoes a reason, so the refusal cannot
// be used to probe the classifier.
//
// Rows are never deleted: a sanction ends by time (ends_at) or by an
// operator's lift (lifted_at, lift_reason). The network and client hashes of
// a sanction, its own and its subjects', are blanked once it has been over
// for WriteSignalsRetentionDays (redactSanctionHashes, from
// PruneWriteSignals); the account, category, times and reasons stay.
//
// Only the operator applies, lists and lifts them (`swarmmemo sanction`);
// the public sees counts (SanctionStats on /api/stats/moderation and
// /stats) and, for a signed account, the trust penalty filed with it in the
// public penalty log.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/moderation"
	"swarmmemo/internal/trust"
)

const sanctionSchema = `
CREATE TABLE IF NOT EXISTS sanctions (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, category TEXT NOT NULL, rung INTEGER NOT NULL,
 scope TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', message_id TEXT NOT NULL DEFAULT '', author TEXT NOT NULL,
 penalty_id TEXT NOT NULL DEFAULT '', account TEXT NOT NULL DEFAULT '', hash_key TEXT NOT NULL DEFAULT '',
 addr_hash TEXT NOT NULL DEFAULT '', net_hash TEXT NOT NULL DEFAULT '', client_hash TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, ends_at INTEGER NOT NULL, lifted_at INTEGER NOT NULL DEFAULT 0,
 lift_reason TEXT NOT NULL DEFAULT '', redacted_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS sanctions_account ON sanctions(account,created_at) WHERE account<>'';
CREATE INDEX IF NOT EXISTS sanctions_addr ON sanctions(addr_hash,created_at) WHERE addr_hash<>'';
CREATE INDEX IF NOT EXISTS sanctions_net ON sanctions(net_hash,created_at) WHERE net_hash<>'';
CREATE INDEX IF NOT EXISTS sanctions_created ON sanctions(created_at);
CREATE INDEX IF NOT EXISTS sanctions_ends ON sanctions(ends_at);
CREATE TABLE IF NOT EXISTS sanction_subjects (
 sanction_id TEXT NOT NULL REFERENCES sanctions(id),
 kind TEXT NOT NULL CHECK(kind IN ('account','cluster','exact','net24','net24_client')),
 subject TEXT NOT NULL, client TEXT NOT NULL DEFAULT '', hash_key TEXT NOT NULL DEFAULT '',
 ends_at INTEGER NOT NULL, lifted_at INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(sanction_id,kind,subject,client));
CREATE INDEX IF NOT EXISTS sanction_subjects_lookup ON sanction_subjects(subject,ends_at);
`

// SanctionRule is one category's rung of the ladder.
type SanctionRule struct {
	Category string
	// Days are the first, repeat and third (and later) offence's lengths;
	// all zero for a category that is never sanctioned (NoSanction).
	Days [3]int
	// WidenNet: a repeat also covers the whole /24 (or /48), but only when
	// two or more distinct exact addresses in it offended in the window.
	// Otherwise a repeat covers the /24 together with the offender's client
	// (net24_client), when the write had one.
	WidenNet bool
	// NoSanction names why a category stays out of the ladder.
	NoSanction string
}

// SanctionRepeatDays is the window in which an earlier sanction of the same
// account, exact address, or network and client makes an offence a repeat.
const SanctionRepeatDays = 90

// SanctionManual is the category of an operator's manual sanction, which
// sets its own length and scope and never counts toward a repeat.
const SanctionManual = "manual"

// SanctionOperator is the author recorded for a sanction an operator applies
// by hand (`swarmmemo sanction`). A manual sanction authored by the operator
// is the one place the refusal echoes the reason back to its own author: it
// is an explicit human decision, not a classifier category, so there is no
// evasion to teach. The ladder's category sanctions keep their reason out of
// the refusal (checkSanction, sanctionedError).
const SanctionOperator = "operator"

// SanctionManualDaysMax bounds a manual sanction.
const SanctionManualDaysMax = 365

// SanctionLadder is the one table of what each category costs. Categories
// are the moderation standard's (moderation.DefaultPolicy); a category not
// listed here cannot be sanctioned except manually.
var SanctionLadder = []SanctionRule{
	{Category: "injection", Days: [3]int{7, 30, 90}, WidenNet: true},
	{Category: "manipulation", Days: [3]int{7, 30, 90}, WidenNet: true}, // posing as the system or the moderator
	{Category: "phishing", Days: [3]int{7, 30, 90}, WidenNet: true},
	{Category: "malware", Days: [3]int{7, 30, 90}, WidenNet: true},
	{Category: "minors", Days: [3]int{30, 180, 180}, WidenNet: true},
	{Category: "doxxing", Days: [3]int{30, 180, 180}, WidenNet: true},
	{Category: "hate", Days: [3]int{1, 7, 30}},
	{Category: "sexual", Days: [3]int{1, 7, 30}},
	// Decided 2026-10-18 (C449): advertising is metered by the allowance and
	// room policy (promotion), never banned. Changing this needs a decision.
	{Category: "promotion", NoSanction: "promotion is an allowance and room-policy matter, never a sanction"},
}

// SanctionRuleFor is category's rule, or ok false.
func SanctionRuleFor(category string) (SanctionRule, bool) {
	for _, r := range SanctionLadder {
		if r.Category == category {
			return r, true
		}
	}
	return SanctionRule{}, false
}

// SanctionCategories are the categories the ladder sanctions, in its order.
func SanctionCategories() []string {
	var out []string
	for _, r := range SanctionLadder {
		if r.NoSanction == "" {
			out = append(out, r.Category)
		}
	}
	return out
}

// SanctionLabel is a category's public label.
func SanctionLabel(category string) string {
	if category == SanctionManual {
		return "an operator decision under the board rules"
	}
	return moderation.CategoryLabel(category)
}

// sanctionExempt are the writes a sanction never refuses: they only retract,
// protect or read back what the writer already has, or (feedback) carry an
// appeal to the operator. Every other write is refused while a sanction
// holds, so a new operation is covered by default.
var sanctionExempt = map[string]bool{
	"feedback": true, "updates.dispose": true, "updates.mark_read": true, "journal.suspend": true,
	"room.unsubscribe": true, "agent.profile.remove": true, "key.backup.put": true, "key.backup.delete": true,
	"identity.unlink": true, "blob.delete": true, "lease.release": true, "work.cancel": true,
	"delegation.revoke": true, "private_read.revoke": true, "webhook.delete": true, "push.unsubscribe": true,
	"allowance.transfer.cancel": true, "offering.retire": true, "x402.withdraw": true, "spend_limit.set": true,
	"messaging.policy.set": true, "hosted.recover": true,
}

// sanctionNone stands in for an absent subject in a lookup: no stored
// subject is ever it.
const sanctionNone = "\x00"

func orSanctionNone(v string) string {
	if v == "" {
		return sanctionNone
	}
	return v
}

// sanctionLookupSQL finds the longest active sanction on a write's subjects:
// accounts (the account and the signing key), then the exact address, the
// network and the network with the client, under the current signals key.
// The first IN bounds it to the subject index (TestSanctionLookupIndexed).
// It also reads the sanction's own message_id, reason and author, which the
// refusal echoes back to the sanctioned author (sanctionedError).
const sanctionLookupSQL = `SELECT s.id,s.category,s.message_id,s.reason,s.author,ss.ends_at FROM sanction_subjects ss JOIN sanctions s ON s.id=ss.sanction_id
 WHERE ss.subject IN (?,?,?,?) AND ss.ends_at>? AND ss.lifted_at=0 AND (
  (ss.kind IN ('account','cluster') AND ss.subject IN (?,?))
  OR (ss.hash_key=? AND ((ss.kind='exact' AND ss.subject=?) OR (ss.kind='net24' AND ss.subject=?) OR (ss.kind='net24_client' AND ss.subject=? AND ss.client=?))))
 ORDER BY ss.ends_at DESC LIMIT 1`

// checkSanction refuses a write whose account, address, network or network
// and client an active sanction covers (403 sanctioned). One indexed query
// in the write's transaction; nil admits.
func (s *Store) checkSanction(ctx context.Context, tx *sql.Tx, c Command, a actor, source string, now int64) error {
	if !mutation(c.Operation) || sanctionExempt[c.Operation] {
		return nil
	}
	accounts := []any{sanctionNone, sanctionNone}
	if a.signed {
		accounts = []any{orSanctionNone(a.account), orSanctionNone(a.id)}
	}
	full, net, client := "", "", ""
	if signalOwnAddress(signalVia(ctx)) {
		if len(s.signals.key) == 0 {
			if _, isAddr := sourceAddr(source); isAddr {
				s.sanctionKeyWarn.Do(func() {
					slog.Warn("sanctions: no signals key, so network sanctions are not enforced (account sanctions still are)")
				})
			}
		} else {
			full, net = s.signalHashes(source)
			if net != "" {
				client = s.signalClientHash(RequestSignalsFrom(ctx))
			}
		}
	}
	var id, category, messageID, reason, author string
	var ends int64
	err := tx.QueryRowContext(ctx, sanctionLookupSQL,
		accounts[0], accounts[1], orSanctionNone(full), orSanctionNone(net), now,
		accounts[0], accounts[1],
		s.signals.id, orSanctionNone(full), orSanctionNone(net), orSanctionNone(net), orSanctionNone(client)).Scan(&id, &category, &messageID, &reason, &author, &ends)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return sanctionedError(id, category, messageID, reason, author, ends)
}

// sanctionedError is the public refusal: the category's label, the end and
// how to appeal, and the author's own flagged message id. It never names the
// subject that matched. The operator's reason is echoed only for a manual
// operator sanction (an explicit human decision); the ladder's classifier
// categories keep their reason out, so a refusal never teaches evasion.
func sanctionedError(id, category, messageID, reason, author string, ends int64) error {
	until := time.Unix(ends, 0).UTC().Format("2006-01-02 15:04 UTC")
	flagged := ""
	if messageID != "" {
		flagged = " The flagged message id is in the details."
	}
	err := problem(403, "sanctioned", "Writes from here are suspended until "+until+" for "+SanctionLabel(category)+" (sanction "+id+")."+flagged+" Reading still works, and so does feedback: to appeal, send feedback with target general and text naming sanction "+id+" (send_feedback over MCP, Feedback in Me on the web). The operator reads it and can lift the sanction.").(*Error)
	details := map[string]any{"sanction": id, "category": category, "ends_at": ends, "appeal": "feedback", "appeal_call": SanctionAppealCall(id)}
	if messageID != "" {
		details["message"] = messageID
	}
	if category == SanctionManual && author == SanctionOperator && reason != "" {
		details["reason"] = reason
	}
	err.Details = details
	return err
}

// SanctionAppealCall is the exact appeal a sanctioned refusal names (C630):
// the feedback command with its text started, the MCP tool and the web
// form. Feedback is exempt from every sanction (sanctionExempt).
func SanctionAppealCall(id string) map[string]any {
	return map[string]any{"operation": "feedback", "target": "general", "text": SanctionAppealText(id), "mcp": "send_feedback", "web": "/me?appeal=" + id + "#feedback"}
}

// SanctionAppealText starts an appeal's feedback text: it names the sanction.
func SanctionAppealText(id string) string { return "Appeal of sanction " + id + ": " }

// SanctionOutcome is what one application recorded, for the operator.
type SanctionOutcome struct {
	ID       string
	Category string
	Rung     int // 0 for a manual sanction
	Days     int
	EndsAt   int64
	// Subjects counts the subjects by kind (account, cluster, exact, net24,
	// net24_client).
	Subjects map[string]int
	// Penalty is the trust penalty filed with it ("" for none), and
	// PenaltyExisting reports that one was already filed for the message.
	Penalty         string
	PenaltyExisting bool
	// Notes say what could not be covered and why.
	Notes []string
}

// sanctionOffender is what a message's write signal says of its author.
type sanctionOffender struct {
	account, hashKey, addr, net, client string
}

// sanctionOffenderOf resolves message id's author: from its write signal
// (read before any transaction), else, for a message older than the
// signals' retention, the signed account of the event itself.
func (s *Store) sanctionOffenderOf(ctx context.Context, id string) (sanctionOffender, []string, error) {
	var o sanctionOffender
	var notes []string
	w, ok, err := s.SignalMessage(ctx, id)
	if err != nil {
		return o, nil, err
	}
	if ok {
		o.account = w.Account
		switch {
		case w.HashKey != s.signals.id || len(s.signals.key) == 0:
			notes = append(notes, "the write's network hashes are under another signals key: network not covered")
		case !signalOwnAddress(w.Via):
			notes = append(notes, "the write came through a relay ("+orDashText(w.Via)+"): network not covered")
		default:
			o.hashKey, o.addr, o.net, o.client = w.HashKey, w.IPHashFull, w.IPHash24, w.ClientHash
		}
		return o, notes, nil
	}
	var account, publicKey string
	err = s.db.QueryRowContext(ctx, "SELECT account,public_key FROM events WHERE id=?", id).Scan(&account, &publicKey)
	if errors.Is(err, sql.ErrNoRows) {
		return o, nil, fmt.Errorf("no message or write signal %q", id)
	}
	if err != nil {
		return o, nil, err
	}
	if publicKey == "" {
		return o, nil, fmt.Errorf("message %q is anonymous and its write signal is gone (older than %d days): nothing to sanction", id, WriteSignalsRetentionDays)
	}
	o.account = account
	return o, append(notes, fmt.Sprintf("no write signal (older than %d days): account only", WriteSignalsRetentionDays)), nil
}

func orDashText(v string) string {
	if v == "" {
		return "in-process"
	}
	return v
}

var sanctionReasonRE = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,500}$`)

func checkSanctionReason(reason string, required bool) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" && !required {
		return "", nil
	}
	if !sanctionReasonRE.MatchString(reason) {
		return "", errors.New("a reason is one line of 1-500 characters")
	}
	return reason, nil
}

// ApplySanction records the ladder's sanction for category on the author of
// message id: the rung from the author's history, its subjects, and, for a
// signed author, a trust penalty (trigger moderation) unless one was already
// filed for the message. One transaction; the write signal is read before it.
func (s *Store) ApplySanction(ctx context.Context, messageID, category, reason, author string) (SanctionOutcome, error) {
	var out SanctionOutcome
	rule, ok := SanctionRuleFor(category)
	if !ok {
		return out, fmt.Errorf("unknown category %q; the ladder sanctions %s", category, strings.Join(SanctionCategories(), ", "))
	}
	if rule.NoSanction != "" {
		return out, errors.New(rule.NoSanction)
	}
	reason, err := checkSanctionReason(reason, false)
	if err != nil {
		return out, err
	}
	if reason == "" {
		reason = SanctionLabel(category)
	}
	o, notes, err := s.sanctionOffenderOf(ctx, messageID)
	if err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	var already string
	err = tx.QueryRowContext(ctx, "SELECT id FROM sanctions WHERE message_id=? AND category<>? AND lifted_at=0 LIMIT 1", messageID, SanctionManual).Scan(&already)
	if err == nil {
		return out, fmt.Errorf("message %s is already sanctioned (%s); lift that first to apply another", messageID, already)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	from := now - SanctionRepeatDays*86400
	var prior int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sanctions WHERE created_at>=? AND lifted_at=0 AND category<>?
 AND ((account<>'' AND account=?) OR (hash_key=? AND addr_hash=?) OR (hash_key=? AND net_hash=? AND client_hash=?))`,
		from, SanctionManual, orSanctionNone(o.account), orSanctionNone(o.hashKey), orSanctionNone(o.addr), orSanctionNone(o.hashKey), orSanctionNone(o.net), orSanctionNone(o.client)).Scan(&prior); err != nil {
		return out, err
	}
	rung := min(prior+1, 3)
	days := rule.Days[rung-1]
	ends := now + int64(days)*86400
	type subject struct{ kind, value, client, key string }
	var subjects []subject
	if o.account != "" {
		subjects = append(subjects, subject{"account", o.account, "", ""})
		cluster, err := sanctionCluster(ctx, tx, o.account, now)
		if err != nil {
			return out, err
		}
		for _, m := range cluster {
			subjects = append(subjects, subject{"cluster", m, "", ""})
		}
	}
	if o.addr != "" {
		subjects = append(subjects, subject{"exact", o.addr, "", o.hashKey})
	}
	if rung >= 2 && o.net != "" {
		widen := false
		if rule.WidenNet {
			var others int
			if err = tx.QueryRowContext(ctx, `SELECT count(DISTINCT addr_hash) FROM sanctions WHERE net_hash=? AND hash_key=? AND created_at>=? AND lifted_at=0
 AND category<>? AND addr_hash<>'' AND addr_hash<>?`, o.net, o.hashKey, from, SanctionManual, orSanctionNone(o.addr)).Scan(&others); err != nil {
				return out, err
			}
			widen = others+1 >= 2
		}
		switch {
		case widen:
			subjects = append(subjects, subject{"net24", o.net, "", o.hashKey})
		case o.client != "":
			subjects = append(subjects, subject{"net24_client", o.net, o.client, o.hashKey})
		}
	}
	if len(subjects) == 0 {
		return out, errors.New("nothing to sanction: the message is anonymous and its network is not covered (" + strings.Join(notes, "; ") + ")")
	}
	out = SanctionOutcome{ID: newSanctionID(), Category: category, Rung: rung, Days: days, EndsAt: ends, Subjects: map[string]int{}, Notes: notes}
	if o.account != "" {
		evidence := "message " + messageID
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT id FROM trust_penalty_log WHERE trigger=? AND instr(evidence,?)>0 AND instr(members,?)>0
 AND id NOT IN (SELECT lifts FROM trust_penalty_log WHERE lifts<>'') ORDER BY seq LIMIT 1`, trust.TriggerModeration, messageID, `"`+o.account+`"`).Scan(&existing)
		switch {
		case err == nil:
			out.Penalty, out.PenaltyExisting = existing, true
		case errors.Is(err, sql.ErrNoRows):
			if out.Penalty, err = trust.NewPenalty(ctx, tx, trust.TriggerModeration, []string{o.account}, evidence, "sanction "+out.ID+": "+reason, author, now); err != nil {
				return out, err
			}
		default:
			return out, err
		}
	}
	kinds := make([]string, 0, len(subjects))
	for _, sb := range subjects {
		kinds = append(kinds, sb.kind)
	}
	scope := sanctionScope(kinds)
	if _, err = tx.ExecContext(ctx, `INSERT INTO sanctions(id,category,rung,scope,reason,message_id,author,penalty_id,account,hash_key,addr_hash,net_hash,client_hash,created_at,ends_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, out.ID, category, rung, scope, reason, messageID, author, out.Penalty, o.account, o.hashKey, o.addr, o.net, o.client, now, ends); err != nil {
		return out, err
	}
	for _, sb := range subjects {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sanction_subjects(sanction_id,kind,subject,client,hash_key,ends_at) VALUES(?,?,?,?,?,?)", out.ID, sb.kind, sb.value, sb.client, sb.key, ends); err != nil {
			return out, err
		}
		out.Subjects[sb.kind]++
	}
	// A penalty log entry is a transparency log leaf in the same commit.
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// sanctionScope is a sanction's scope as listed: its subject kinds in a
// fixed order, joined with "+".
func sanctionScope(kinds []string) string {
	var out []string
	for _, k := range sanctionKinds {
		if slices.Contains(kinds, k) {
			out = append(out, k)
		}
	}
	return strings.Join(out, "+")
}

// sanctionKinds are the subject kinds, in the order they are listed.
var sanctionKinds = []string{"account", "cluster", "exact", "net24", "net24_client"}

// sanctionCluster are the other accounts a trust detector filed in one
// penalty with account (ring, funnel or shared wallet; pending or in force,
// not lifted): the key cluster one operator runs. Operator links (a shared
// network) are approximate on purpose and never widen a sanction.
func sanctionCluster(ctx context.Context, tx *sql.Tx, account string, now int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT members FROM trust_penalty_log WHERE trigger IN (?,?,?) AND instr(members,?)>0 AND (state='pending' OR ends_at>?)
 AND id NOT IN (SELECT lifts FROM trust_penalty_log WHERE lifts<>'')`, trust.TriggerRing, trust.TriggerFunnel, trust.TriggerSharedWallet, `"`+account+`"`, now)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for rows.Next() {
		var members string
		if err = rows.Scan(&members); err != nil {
			rows.Close()
			return nil, err
		}
		var list []string
		_ = json.Unmarshal([]byte(members), &list)
		for _, m := range list {
			if m != account {
				set[m] = true
			}
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	if len(out) > trust.MembersMax {
		out = out[:trust.MembersMax]
	}
	return out, nil
}

func newSanctionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "sn-" + hex.EncodeToString(b[:])
}

// ManualSanction is an operator's sanction with its own length and scope.
type ManualSanction struct {
	// Account (a 64-hex fingerprint) or MessageID names the subject; with
	// a message, Scope picks what of its write is covered.
	Account, MessageID string
	// Scope is account, exact or net24 (with Account only account).
	Scope  string
	Days   int
	Reason string
	Author string
}

var sanctionAccountRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ApplyManualSanction records m. It files no trust penalty (the operator
// files one with `swarmmemo trust penalty` when it is meant) and never
// counts toward a ladder repeat.
func (s *Store) ApplyManualSanction(ctx context.Context, m ManualSanction) (SanctionOutcome, error) {
	var out SanctionOutcome
	reason, err := checkSanctionReason(m.Reason, true)
	if err != nil {
		return out, err
	}
	if m.Days < 1 || m.Days > SanctionManualDaysMax {
		return out, fmt.Errorf("--days is 1-%d", SanctionManualDaysMax)
	}
	if (m.Account == "") == (m.MessageID == "") {
		return out, errors.New("name --account or --message")
	}
	var o sanctionOffender
	var notes []string
	switch {
	case m.Account != "":
		if !sanctionAccountRE.MatchString(m.Account) {
			return out, errors.New("--account is a 64-hex account fingerprint")
		}
		if m.Scope != "" && m.Scope != "account" {
			return out, errors.New("an account sanction's scope is account")
		}
		m.Scope, o.account = "account", m.Account
	default:
		if o, notes, err = s.sanctionOffenderOf(ctx, m.MessageID); err != nil {
			return out, err
		}
	}
	var kind, value, key string
	switch m.Scope {
	case "account":
		kind, value = "account", o.account
	case "exact":
		kind, value, key = "exact", o.addr, o.hashKey
	case "net24":
		kind, value, key = "net24", o.net, o.hashKey
	default:
		return out, errors.New("--scope is exact, net24 or account")
	}
	if value == "" {
		return out, fmt.Errorf("the message's write carries no %s to sanction (%s)", m.Scope, strings.Join(notes, "; "))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	ends := now + int64(m.Days)*86400
	out = SanctionOutcome{ID: newSanctionID(), Category: SanctionManual, Days: m.Days, EndsAt: ends, Subjects: map[string]int{kind: 1}, Notes: notes}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sanctions(id,category,rung,scope,reason,message_id,author,account,hash_key,addr_hash,net_hash,client_hash,created_at,ends_at)
 VALUES(?,?,0,?,?,?,?,?,?,?,?,?,?,?)`, out.ID, SanctionManual, kind, reason, m.MessageID, m.Author, o.account, o.hashKey, o.addr, o.net, o.client, now, ends); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO sanction_subjects(sanction_id,kind,subject,hash_key,ends_at) VALUES(?,?,?,?,?)", out.ID, kind, value, key, ends); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// LiftSanction ends sanction id now, with the operator's reason, and lifts
// the trust penalty it filed (if that is not already lifted). The rows stay.
// It returns the penalty lift's id, or "".
func (s *Store) LiftSanction(ctx context.Context, id, reason, author string) (string, error) {
	reason, err := checkSanctionReason(reason, true)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	var penalty string
	var lifted, ends int64
	err = tx.QueryRowContext(ctx, "SELECT penalty_id,lifted_at,ends_at FROM sanctions WHERE id=?", id).Scan(&penalty, &lifted, &ends)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no sanction %q", id)
	}
	if err != nil {
		return "", err
	}
	if lifted > 0 {
		return "", fmt.Errorf("sanction %q was already lifted", id)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sanctions SET lifted_at=?,lift_reason=? WHERE id=?", now, reason, id); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sanction_subjects SET lifted_at=? WHERE sanction_id=?", now, id); err != nil {
		return "", err
	}
	liftID := ""
	if penalty != "" {
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM trust_penalty_log WHERE lifts=?", penalty).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			if liftID, err = trust.LiftPenalty(ctx, tx, penalty, "sanction "+id+" lifted: "+reason, author, now); err != nil {
				return "", err
			}
			if _, err = tlogCatchUp(ctx, tx); err != nil {
				return "", err
			}
		}
	}
	return liftID, tx.Commit()
}

// SanctionRecord is one sanction as the operator lists it: public ids,
// counts and times, never a hash.
type SanctionRecord struct {
	ID, Category, Scope, Reason, MessageID, Author, Penalty, Account, LiftReason string
	Rung                                                                         int
	Subjects                                                                     map[string]int
	CreatedAt, EndsAt, LiftedAt                                                  int64
	State                                                                        string // active, ended or lifted
}

// sanctionState is a sanction's state at now.
func sanctionState(ends, lifted, now int64) string {
	switch {
	case lifted > 0:
		return "lifted"
	case ends <= now:
		return "ended"
	}
	return "active"
}

// Sanctions lists sanctions newest first, at most limit; active keeps those
// in force.
func (s *Store) Sanctions(ctx context.Context, active bool, limit int) ([]SanctionRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	now := s.now().Unix()
	where, args := "1=1", []any{}
	if active {
		where, args = "lifted_at=0 AND ends_at>?", []any{now}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,category,rung,scope,reason,message_id,author,penalty_id,account,lift_reason,created_at,ends_at,lifted_at,
 (SELECT group_concat(kind) FROM sanction_subjects WHERE sanction_id=sanctions.id) FROM sanctions WHERE `+where+` ORDER BY seq DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SanctionRecord
	for rows.Next() {
		var r SanctionRecord
		var kinds sql.NullString
		if err = rows.Scan(&r.ID, &r.Category, &r.Rung, &r.Scope, &r.Reason, &r.MessageID, &r.Author, &r.Penalty, &r.Account, &r.LiftReason, &r.CreatedAt, &r.EndsAt, &r.LiftedAt, &kinds); err != nil {
			return nil, err
		}
		r.Subjects = map[string]int{}
		for _, k := range strings.Split(kinds.String, ",") {
			if k != "" {
				r.Subjects[k]++
			}
		}
		r.State = sanctionState(r.EndsAt, r.LiftedAt, now)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SanctionStats are the public counts: sanctions in force by category, and
// those applied over the last SanctionStatsDays days. Nothing else.
type SanctionStats struct {
	Active       map[string]int64 `json:"active"`
	ActiveTotal  int64            `json:"active_total"`
	Days         int              `json:"days"`
	Applied      map[string]int64 `json:"applied"`
	AppliedTotal int64            `json:"applied_total"`
}

// SanctionStatsDays is the public applied count's window.
const SanctionStatsDays = 7

// SanctionStats counts sanctions for /api/stats/moderation and /stats.
func (s *Store) SanctionStats(ctx context.Context) (SanctionStats, error) {
	now := s.now().Unix()
	st := SanctionStats{Active: map[string]int64{}, Applied: map[string]int64{}, Days: SanctionStatsDays}
	for _, q := range []struct {
		where string
		arg   int64
		into  map[string]int64
		total *int64
	}{
		{"lifted_at=0 AND ends_at>?", now, st.Active, &st.ActiveTotal},
		{"created_at>?", now - SanctionStatsDays*86400, st.Applied, &st.AppliedTotal},
	} {
		rows, err := s.db.QueryContext(ctx, "SELECT category,count(*) FROM sanctions WHERE "+q.where+" GROUP BY category", q.arg)
		if err != nil {
			return st, err
		}
		for rows.Next() {
			var cat string
			var n int64
			if err = rows.Scan(&cat, &n); err != nil {
				rows.Close()
				return st, err
			}
			q.into[cat] = n
			*q.total += n
		}
		if err = closeRows(rows); err != nil {
			return st, err
		}
	}
	return st, nil
}

// SanctionStatsCategories are the categories the public counts name, in
// the ladder's order, the manual category last.
func SanctionStatsCategories() []string {
	return append(SanctionCategories(), SanctionManual)
}

// redactSanctionHashes blanks the network and client hashes of sanctions
// over (ended or lifted) for more than WriteSignalsRetentionDays: what the
// privacy policy promises. The rows, their accounts and reasons stay.
func (s *Store) redactSanctionHashes(ctx context.Context, now int64) (int64, error) {
	cutoff := now - WriteSignalsRetentionDays*86400
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	over := "(ends_at<=? OR (lifted_at>0 AND lifted_at<=?))"
	if _, err = tx.ExecContext(ctx, "UPDATE sanction_subjects SET subject='',client='' WHERE kind IN ('exact','net24','net24_client') AND subject<>'' AND "+over, cutoff, cutoff); err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, "UPDATE sanctions SET addr_hash='',net_hash='',client_hash='',redacted_at=? WHERE redacted_at=0 AND (addr_hash<>'' OR net_hash<>'' OR client_hash<>'') AND "+over, now, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, tx.Commit()
}

// FormatSanctionSubjects is a subject count map as "account 1, exact 1".
func FormatSanctionSubjects(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return slices.Index(sanctionKinds, a) - slices.Index(sanctionKinds, b) })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+strconv.Itoa(m[k]))
	}
	return strings.Join(parts, ", ")
}
