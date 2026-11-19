package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"swarmmemo/internal/allowance"
)

// Tool history (C1003): an agent's own past free tool calls, read back from
// the rows the call record already keeps (service_calls) and, for the
// notary, from its receipts (notary_receipts). Nothing new is stored: each
// entry is derived at read time, and it carries a call's target, hash and
// size, never the page text, the values or the screened text. service.read
// method status with the call id still reads a stored result in full.

const (
	// HistoryPageDefault and HistoryPageMax bound one service.calls page.
	HistoryPageDefault = 20
	HistoryPageMax     = 50
	// HistoryStaleDays: an entry older than this answers stale true (the
	// page or the values may have changed since). Nothing is deleted.
	HistoryStaleDays = 90
	// HistoryTargetBytes bounds an entry's target.
	HistoryTargetBytes = 512
	// HistoryNotaryPrefix starts the call id of a notary entry: notary:SEQ.
	HistoryNotaryPrefix = "notary:"
)

// HistoryStaleNote is what a stale entry's cited page says.
var HistoryStaleNote = "Older than " + itoa(HistoryStaleDays) + " days: the page or the values may have changed since."

// HistoryEntry is what a provider tells about one of its stored calls: the
// thing it was about (target), its hash (digest), its size in bytes and a
// few more named facts (Extra: a status, a verdict, a licence), never the
// content itself.
type HistoryEntry struct {
	Target string
	Digest string
	Bytes  int64
	Extra  map[string]any
}

// Historian is a provider whose calls are listed in tool history. Each
// provider owns its entry: the one place a body becomes an entry.
type Historian interface {
	HistoryEntry(method string, body, public json.RawMessage) HistoryEntry
}

// HistoryQuery is one service.calls page.
type HistoryQuery struct {
	Account string
	// Tool narrows the list: a service id (fetch) or service.method
	// (fetch.page); "" for every tool.
	Tool   string
	Since  int64
	Cursor string
	Limit  int
	Now    int64
}

// HistoryRow is one entry with its owner, for the board to decorate.
type HistoryRow struct {
	Entry   map[string]any
	ID      string
	Account string
	Notary  bool
	Hash    string // a notary entry's stamped hash
}

// historyID reports whether id is a call id (32 hex) or notary:SEQ.
func historyID(id string) bool {
	if seq, ok := strings.CutPrefix(id, HistoryNotaryPrefix); ok {
		n, err := strconv.ParseInt(seq, 10, 64)
		return err == nil && n > 0 && strconv.FormatInt(n, 10) == seq
	}
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// HistoryIDValid reports whether id names a tool history entry's shape.
func HistoryIDValid(id string) bool { return historyID(id) }

// historyServices is the enabled services whose calls are listed, the
// notary aside (its entries come from its receipts).
func (e *Engine) historyServices() (calls []string, hasNotary bool) {
	for _, id := range e.cfg.Registry.Enabled() {
		p := e.cfg.Registry.providers[id]
		if _, ok := p.(*notary); ok {
			hasNotary = true
			continue
		}
		if _, ok := p.(Historian); ok {
			calls = append(calls, id)
		}
	}
	return calls, hasNotary
}

// HistoryTools is every tool tool history lists, as service.method, for
// docs and /capabilities.
func (e *Engine) HistoryTools() []string {
	calls, hasNotary := e.historyServices()
	out := []string{}
	for _, id := range calls {
		for _, m := range e.cfg.Registry.providers[id].Describe().Methods {
			if m.Write {
				out = append(out, id+"."+m.Name)
			}
		}
	}
	if hasNotary {
		out = append(out, e.notaryID()+".stamp")
	}
	sort.Strings(out)
	return out
}

func (e *Engine) notaryID() string {
	for _, id := range e.cfg.Registry.Enabled() {
		if _, ok := e.cfg.Registry.providers[id].(*notary); ok {
			return id
		}
	}
	return "notary"
}

func historyCursor(at int64, id string) string { return strconv.FormatInt(at, 10) + ":" + id }

func parseHistoryCursor(c string) (int64, string, bool) {
	at, id, ok := strings.Cut(c, ":")
	n, err := strconv.ParseInt(at, 10, 64)
	return n, id, ok && err == nil && n > 0 && historyID(id)
}

// errHistoryCursor is a cursor no page gave.
var errHistoryCursor = errors.New("invalid tool history cursor")

// IsHistoryCursorError reports a malformed service.calls cursor.
func IsHistoryCursorError(err error) bool { return errors.Is(err, errHistoryCursor) }

// History is one page of an account's tool history, newest first, and the
// cursor of the next page ("" at the end). It reads in the command's
// transaction and is bounded: two indexed queries of at most Limit+1 rows.
func (e *Engine) History(ctx context.Context, q allowance.Querier, h HistoryQuery) ([]HistoryRow, string, error) {
	if h.Limit <= 0 {
		h.Limit = HistoryPageDefault
	}
	h.Limit = min(h.Limit, HistoryPageMax)
	beforeAt, beforeID := int64(1)<<62, "~"
	if h.Cursor != "" {
		var ok bool
		if beforeAt, beforeID, ok = parseHistoryCursor(h.Cursor); !ok {
			return nil, "", errHistoryCursor
		}
	}
	calls, withNotary := e.historyServices()
	service, method, _ := strings.Cut(h.Tool, ".")
	notaryID := e.notaryID()
	var rows []HistoryRow
	if h.Tool != "" && service != notaryID {
		withNotary = false
		if !containsString(calls, service) {
			calls = nil
		} else {
			calls = []string{service}
		}
	} else if h.Tool != "" {
		calls = nil
		if method != "" && method != "stamp" {
			withNotary = false
		}
	}
	if len(calls) > 0 {
		args := []any{h.Account, h.Since, beforeAt, beforeAt, beforeID}
		in := "?" + strings.Repeat(",?", len(calls)-1)
		for _, c := range calls {
			args = append(args, c)
		}
		cond := ""
		if method != "" {
			cond = " AND method=?"
			args = append(args, method)
		}
		args = append(args, h.Limit+1)
		r, err := q.QueryContext(ctx, `SELECT id,service,method,state,resource,cost,created_at,body,public FROM service_calls
 WHERE account=? AND created_at>=? AND (created_at<? OR created_at=? AND id<?) AND service IN (`+in+`)`+cond+`
 ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
		if err != nil {
			return nil, "", err
		}
		for r.Next() {
			var id, svc, m, state, resource, body, public string
			var cost, at int64
			if err = r.Scan(&id, &svc, &m, &state, &resource, &cost, &at, &body, &public); err != nil {
				r.Close()
				return nil, "", err
			}
			rows = append(rows, HistoryRow{ID: id, Account: h.Account, Entry: e.callEntry(id, svc, m, state, resource, cost, at, body, public, h.Now)})
		}
		if err = r.Close(); err != nil {
			return nil, "", err
		}
		if err = r.Err(); err != nil {
			return nil, "", err
		}
	}
	if withNotary {
		r, err := q.QueryContext(ctx, `SELECT seq,hash,time,coalesce((SELECT c.cost FROM service_calls c WHERE c.account=n.account AND c.service=? AND c.method='stamp' AND c.created_at=n.time LIMIT 1),0)
 FROM notary_receipts n WHERE account=? AND time>=? AND (time<? OR time=? AND ('notary:'||seq)<?)
 ORDER BY time DESC, ('notary:'||seq) DESC LIMIT ?`, notaryID, h.Account, h.Since, beforeAt, beforeAt, beforeID, h.Limit+1)
		if err != nil {
			return nil, "", err
		}
		for r.Next() {
			var seq, at, cost int64
			var hash string
			if err = r.Scan(&seq, &hash, &at, &cost); err != nil {
				r.Close()
				return nil, "", err
			}
			id := HistoryNotaryPrefix + strconv.FormatInt(seq, 10)
			rows = append(rows, HistoryRow{ID: id, Account: h.Account, Notary: true, Hash: hash, Entry: notaryEntry(id, notaryID, hash, at, cost, h.Now)})
		}
		if err = r.Close(); err != nil {
			return nil, "", err
		}
		if err = r.Err(); err != nil {
			return nil, "", err
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ai, aj := rows[i].Entry["at"].(int64), rows[j].Entry["at"].(int64)
		if ai != aj {
			return ai > aj
		}
		return rows[i].ID > rows[j].ID
	})
	next := ""
	if len(rows) > h.Limit {
		rows = rows[:h.Limit]
		last := rows[len(rows)-1]
		next = historyCursor(last.Entry["at"].(int64), last.ID)
	}
	return rows, next, nil
}

// HistoryByID is one entry by its id, with its owner: a call of a listed
// tool, or a notary receipt. found is false for any other id.
func (e *Engine) HistoryByID(ctx context.Context, q allowance.Querier, id string, now int64) (HistoryRow, bool, error) {
	if !historyID(id) {
		return HistoryRow{}, false, nil
	}
	calls, hasNotary := e.historyServices()
	if seq, ok := strings.CutPrefix(id, HistoryNotaryPrefix); ok {
		if !hasNotary {
			return HistoryRow{}, false, nil
		}
		var hash, account string
		var at, cost int64
		err := q.QueryRowContext(ctx, `SELECT hash,time,account,coalesce((SELECT c.cost FROM service_calls c WHERE c.account=n.account AND c.service=? AND c.method='stamp' AND c.created_at=n.time LIMIT 1),0)
 FROM notary_receipts n WHERE seq=?`, e.notaryID(), seq).Scan(&hash, &at, &account, &cost)
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryRow{}, false, nil
		}
		if err != nil {
			return HistoryRow{}, false, err
		}
		return HistoryRow{ID: id, Account: account, Notary: true, Hash: hash, Entry: notaryEntry(id, e.notaryID(), hash, at, cost, now)}, true, nil
	}
	var svc, m, state, resource, body, public, account string
	var cost, at int64
	err := q.QueryRowContext(ctx, "SELECT service,method,state,resource,cost,created_at,body,public,account FROM service_calls WHERE id=?", id).
		Scan(&svc, &m, &state, &resource, &cost, &at, &body, &public, &account)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !containsString(calls, svc) {
		return HistoryRow{}, false, nil
	}
	if err != nil {
		return HistoryRow{}, false, err
	}
	return HistoryRow{ID: id, Account: account, Entry: e.callEntry(id, svc, m, state, resource, cost, at, body, public, now)}, true, nil
}

// EraseHistory is the owner-erasure step for tool history: the account's
// stored results and public records are cleared and its notary receipts
// no longer name it. The rows and the log's leaves (hashes) stay.
func EraseHistory(ctx context.Context, tx *sql.Tx, account string) error {
	if account == "" {
		return errors.New("erase: no account")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE service_calls SET body='', public='{}' WHERE account=?", account); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE notary_receipts SET account='' WHERE account=?", account)
	return err
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// historyBase is the entry every tool shares; cited is the board's. cost is
// what the call used, in resource (credit for every listed tool), and
// cost_note reads it in plain words (C1107).
func historyBase(id, tool, state, resource string, at, cost, now int64) map[string]any {
	return map[string]any{"call_id": id, "tool": tool, "at": at, "state": state, "target": "", "digest": "", "bytes": int64(0),
		"cost": cost, "resource": resource, "cost_note": historyCostNote(cost, resource), "cited": false, "stale": at < now-HistoryStaleDays*86400}
}

// historyCostNote is an entry's cost_note (C1107). A call's cost is counted
// against the free daily allowance first and spends the balance only past
// the daily limit; the stored row keeps no split between the two (the call's
// own answer has it: credits_spent, paid_from), so the note says the order.
func historyCostNote(cost int64, resource string) string {
	if cost <= 0 {
		return "Free: no cost."
	}
	return costUnits(cost, resource) + ", counted against the free daily allowance first; the agent's balance pays only past the daily limit."
}

// notaryEntry is a notary receipt's entry; notary.stamp is priced in credit.
func notaryEntry(id, service, hash string, at, cost, now int64) map[string]any {
	e := historyBase(id, service+".stamp", "done", string(allowance.Credit), at, cost, now)
	e["target"], e["digest"] = hash, hash
	return e
}

func (e *Engine) callEntry(id, service, method, state, resource string, cost, at int64, body, public string, now int64) map[string]any {
	out := historyBase(id, service+"."+method, state, resource, at, cost, now)
	p, ok := e.cfg.Registry.providers[service].(Historian)
	if !ok || state != "done" {
		return out
	}
	h := p.HistoryEntry(method, json.RawMessage(body), json.RawMessage(public))
	out["target"], out["digest"], out["bytes"] = clipTarget(h.Target), h.Digest, h.Bytes
	for k, v := range h.Extra {
		if _, taken := out[k]; !taken {
			out[k] = v
		}
	}
	return out
}

// clipTarget bounds a target at HistoryTargetBytes, on a rune boundary.
func clipTarget(s string) string {
	if len(s) <= HistoryTargetBytes {
		return s
	}
	cut := HistoryTargetBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// historyFields decodes the named top-level fields of a stored body; a body
// that is not an object gives none.
func historyFields(body json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return map[string]json.RawMessage{}
	}
	return m
}

func historyString(m map[string]json.RawMessage, key string) string {
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return s
}

func historyInt(m map[string]json.RawMessage, key string) int64 {
	var n int64
	if json.Unmarshal(m[key], &n) != nil {
		return 0
	}
	return n
}

// historyVerdict is a screening verdict's word: a string, or an object's
// verdict field.
func historyVerdict(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"verdict", "Verdict"} {
		if json.Unmarshal(m[k], &s) == nil {
			return s
		}
	}
	return ""
}

// HistoryEntry for fetch: the page's final URL, the SHA-256 of the body as
// received, the text's size, its HTTP status and screening verdict.
func (*fetch) HistoryEntry(_ string, body, _ json.RawMessage) HistoryEntry {
	m := historyFields(body)
	target := historyString(m, "final_url")
	if target == "" {
		target = historyString(m, "url")
	}
	extra := map[string]any{"status": historyInt(m, "status")}
	if v := historyVerdict(m["verdict"]); v != "" {
		extra["verdict"] = v
	}
	return HistoryEntry{Target: target, Digest: historyString(m, "raw_sha256"), Bytes: historyInt(m, "text_bytes"), Extra: extra}
}

// HistoryEntry for public data: the dataset and its parameters, the SHA-256
// of the values, their source and licence; a bulk call names its datasets.
func (*publicData) HistoryEntry(_ string, body, _ json.RawMessage) HistoryEntry {
	m := historyFields(body)
	var results []json.RawMessage
	if json.Unmarshal(m["results"], &results) == nil && len(results) > 0 {
		if len(results) == 1 {
			m = historyFields(results[0])
		} else {
			names := []string{}
			for _, r := range results {
				if d := historyString(historyFields(r), "dataset"); d != "" && !containsString(names, d) {
					names = append(names, d)
				}
			}
			return HistoryEntry{Target: strings.Join(names, ", "), Bytes: int64(len(body)), Extra: map[string]any{"requests": len(results)}}
		}
	}
	target := historyString(m, "dataset")
	var params map[string]any
	if json.Unmarshal(m["params"], &params) == nil && len(params) > 0 {
		target += " " + string(canonicalJSON(params))
	}
	extra := map[string]any{}
	if v := historyString(m, "source_url"); v != "" {
		extra["source_url"] = v
	}
	if v := historyString(m, "licence"); v != "" {
		extra["licence"] = v
	}
	return HistoryEntry{Target: target, Digest: historyString(m, "data_sha256"), Bytes: int64(len(m["data"])), Extra: extra}
}

// HistoryEntry for screen: where the text came from, its SHA-256 and size,
// and the verdict.
func (*screen) HistoryEntry(_ string, body, _ json.RawMessage) HistoryEntry {
	m := historyFields(body)
	target := historyString(m, "source")
	if target == "" || target == "unknown" {
		target = "text"
	}
	extra := map[string]any{}
	if v := historyVerdict(m["verdict"]); v != "" {
		extra["verdict"] = v
	}
	return HistoryEntry{Target: target, Digest: historyString(m, "text_sha256"), Bytes: historyInt(m, "text_bytes"), Extra: extra}
}

// AllowHistoryRead spends one of subject's signed service reads this minute
// (the same limit as service.read).
func (e *Engine) AllowHistoryRead(subject string, now int64) error { return e.allowRead(subject, now) }
