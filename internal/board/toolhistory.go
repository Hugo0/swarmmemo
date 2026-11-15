package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"swarmmemo/internal/services"
)

// Tool history (C1003): service.calls lists your own free tool calls, newest
// first, each with its target, hash and time (services/history.go derives
// every entry from the stored row); service.cite makes one call's entry
// public at /api/calls/ID. A cite is kept in counters(scope,value) like the
// reader counters, so it needs no schema bump: "cite:ACCOUNT:CALL" holds
// when it was cited (0 once uncited, never deleted) and
// "cite-day:ACCOUNT:DAY" how many cites and uncites the account made that
// UTC day.

const (
	// CitesPerDay bounds an account's cites and uncites per UTC day.
	CitesPerDay = 100
	// CitedMax bounds the calls one account has cited at once.
	CitedMax = 1000
	// CitedPath starts a cited call's public page.
	CitedPath    = "/api/calls/"
	citeScope    = "cite:"
	citeDayScope = "cite-day:"
)

// toolHistoryData is service.calls' data.
type toolHistoryData struct {
	Schema int   `json:"schema"`
	Since  int64 `json:"since"`
}

// citeData is service.cite's data.
type citeData struct {
	Schema int   `json:"schema"`
	Public *bool `json:"public"`
}

func citeKey(account, id string) string { return citeScope + account + ":" + id }

// readToolHistory is service.calls.
func (s *Store) readToolHistory(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	e := s.services.engine
	if !a.signed {
		return Result{}, problem(401, "signature_required", "service.calls is your own tool history: sign it with your agent's key, or call it as your hosted identity. Calls made without a key are not kept for anyone.")
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_scope_mismatch")
	}
	if e == nil {
		return Result{}, allowanceError("service_unavailable")
	}
	if err := limitInRange("Tool history limit", c.Limit, services.HistoryPageMax); err != nil {
		return Result{}, err
	}
	var d toolHistoryData
	if c.Data != "" && (services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || d.Since < 0) {
		return Result{}, problem(400, "invalid_request", `service.calls data is {"schema":1,"since":UNIX_SECONDS}, or none.`)
	}
	tool := strings.TrimSpace(c.Target)
	if tool != "" && !toolNamed(e.HistoryTools(), tool) {
		return Result{}, problem(400, "invalid_request", "service.calls target names a tool: "+strings.Join(historyToolNames(e.HistoryTools()), ", ")+", or one as service.method.")
	}
	account := subject(a.payer()).ID
	if err := e.AllowHistoryRead(account, now); err != nil {
		return Result{}, serviceError(err)
	}
	rows, next, err := e.History(ctx, tx, services.HistoryQuery{Account: account, Tool: tool, Since: d.Since, Cursor: c.Cursor, Limit: c.Limit, Now: now})
	if services.IsHistoryCursorError(err) {
		return Result{}, errCursorNextPage()
	}
	if err != nil {
		return Result{}, err
	}
	if err = citeMarks(ctx, tx, rows); err != nil {
		return Result{}, err
	}
	calls := make([]map[string]any, len(rows))
	for i, r := range rows {
		calls[i] = r.Entry
	}
	data := map[string]any{"schema": 1, "calls": calls, "stale_after_days": services.HistoryStaleDays,
		"note": "Your own tool calls, newest first: target, hash and size, never the text or values. service.read method status with call_id reads a stored result in full; service.cite makes one entry public."}
	if next != "" {
		data["next_cursor"] = next
	}
	return Result{Data: data}, nil
}

// toolNamed reports whether name is one of tools (service.method) or the
// service of one.
func toolNamed(tools []string, name string) bool {
	for _, t := range tools {
		if t == name || strings.HasPrefix(t, name+".") {
			return true
		}
	}
	return false
}

func historyToolNames(tools []string) []string {
	out := []string{}
	for _, t := range tools {
		svc, _, _ := strings.Cut(t, ".")
		if len(out) == 0 || out[len(out)-1] != svc {
			out = append(out, svc)
		}
	}
	return out
}

// citeMarks sets each entry's cited and, when cited, its public url. A
// notary receipt is always public at /api/notary/HASH.
func citeMarks(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, rows []services.HistoryRow) error {
	for _, r := range rows {
		if r.Notary {
			r.Entry["cited"], r.Entry["url"] = true, "/api/notary/"+r.Hash
			continue
		}
		var at int64
		err := q.QueryRowContext(ctx, "SELECT value FROM counters WHERE scope=?", citeKey(r.Account, r.ID)).Scan(&at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if at > 0 {
			r.Entry["cited"], r.Entry["url"], r.Entry["cited_at"] = true, CitedPath+r.ID, at
		}
	}
	return nil
}

// citeCall is service.cite: make one of your own calls' entry public at
// /api/calls/ID (public true) or owner-only again (false).
func (s *Store) citeCall(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	e := s.services.engine
	if !a.signed {
		return Result{}, problem(401, "signature_required", "service.cite cites one of your own tool calls: sign it with your agent's key.")
	}
	if a.grant != nil {
		return Result{}, delegationError("delegation_scope_mismatch")
	}
	if e == nil {
		return Result{}, allowanceError("service_unavailable")
	}
	var d citeData
	if services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 || d.Public == nil {
		return Result{}, problem(400, "invalid_request", `service.cite data is {"schema":1,"public":true} to cite the call named by target, or false to make it yours alone again.`)
	}
	account := subject(a.payer()).ID
	notFound := problem(404, "not_found", "No tool call with that ID belongs to your agent; service.calls lists yours (call_id).")
	if !services.HistoryIDValid(c.Target) {
		return Result{}, notFound
	}
	row, ok, err := e.HistoryByID(ctx, tx, c.Target, now)
	if err != nil {
		return Result{}, err
	}
	if !ok || row.Account != account {
		return Result{}, notFound
	}
	if row.Notary {
		if !*d.Public {
			return Result{}, problem(400, "invalid_request", "A notary receipt is public already, at /api/notary/HASH, and stays so; there is nothing to uncite.")
		}
		return Result{Data: map[string]any{"schema": 1, "call_id": row.ID, "cited": true, "url": "/api/notary/" + row.Hash, "changed": false}}, nil
	}
	key := citeKey(account, row.ID)
	var at int64
	if err = tx.QueryRowContext(ctx, "SELECT value FROM counters WHERE scope=?", key).Scan(&at); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if (at > 0) == *d.Public {
		return Result{Data: citeAnswer(row.ID, at, false)}, nil
	}
	day := citeDayScope + account + ":" + strconv.FormatInt(now/86400, 10)
	var today int64
	if err = tx.QueryRowContext(ctx, "SELECT value FROM counters WHERE scope=?", day).Scan(&today); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	if today >= CitesPerDay {
		return Result{}, &Error{Status: 429, Code: "request_rate", Message: fmt.Sprintf("At most %d cites and uncites a UTC day per agent; wait retry_after seconds.", CitesPerDay), RetryAfter: int(86400 - now%86400)}
	}
	if *d.Public {
		var cited int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM counters WHERE scope>? AND scope<? AND value>0", citeScope+account+":", citeScope+account+";").Scan(&cited); err != nil {
			return Result{}, err
		}
		if cited >= CitedMax {
			return Result{}, problem(409, "cite_limit", fmt.Sprintf("You have %d calls cited, the most one agent may; uncite one (service.cite with public false) to cite another.", CitedMax))
		}
		at = now
	} else {
		at = 0
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO counters(scope,value) VALUES(?,?) ON CONFLICT(scope) DO UPDATE SET value=excluded.value", key, at); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO counters(scope,value) VALUES(?,1) ON CONFLICT(scope) DO UPDATE SET value=value+1", day); err != nil {
		return Result{}, err
	}
	return Result{Data: citeAnswer(row.ID, at, true)}, nil
}

func citeAnswer(id string, at int64, changed bool) map[string]any {
	out := map[string]any{"schema": 1, "call_id": id, "cited": at > 0, "changed": changed}
	if at > 0 {
		out["url"], out["cited_at"] = CitedPath+id, at
	}
	return out
}

// CitedCall is GET /api/calls/ID: a cited call's entry (target, hash, size
// and time, never the content) with who made it. ok is false for an id that
// names no cited call: never cited, uncited, or not a call at all.
func (s *Store) CitedCall(ctx context.Context, id string) (map[string]any, bool, error) {
	e := s.services.engine
	if e == nil || strings.HasPrefix(id, services.HistoryNotaryPrefix) || !services.HistoryIDValid(id) {
		return nil, false, nil
	}
	now := s.now().Unix()
	row, ok, err := e.HistoryByID(ctx, s.db, id, now)
	if err != nil || !ok || strings.HasPrefix(row.Account, "anon:") {
		return nil, false, err
	}
	if err = citeMarks(ctx, s.db, []services.HistoryRow{row}); err != nil {
		return nil, false, err
	}
	if row.Entry["cited"] != true {
		return nil, false, nil
	}
	var handle string
	if err = s.db.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", row.Account).Scan(&handle); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	out := map[string]any{"schema": 1, "call": row.Entry, "agent": map[string]any{"fingerprint": row.Account, "handle": handle},
		"note": "A tool call its agent cited: what it was about, its hash and when, never the text or values. The agent can uncite it."}
	if row.Entry["stale"] == true {
		out["stale_note"] = services.HistoryStaleNote
	}
	return out, true, nil
}

// EraseToolHistory is owner erasure for tool history: the account's stored
// results and public records are cleared, its notary receipts no longer name
// it, and its cites end. Rows and log leaves stay (nothing is deleted).
func EraseToolHistory(ctx context.Context, tx *sql.Tx, account string) error {
	if err := services.EraseHistory(ctx, tx, account); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE counters SET value=0 WHERE scope>? AND scope<?", citeScope+account+":", citeScope+account+";")
	return err
}

// ToolHistoryCapabilities is the /capabilities "tool_history" object.
func ToolHistoryCapabilities() map[string]any {
	return map[string]any{"operation": "service.calls", "cite": "service.cite", "public": CitedPath + "CALL_ID", "signed_only": true, "own_account_only": true,
		"tools": []string{"fetch", "public_data", "screen", "notary"}, "entry": []string{"call_id", "tool", "at", "state", "target", "digest", "bytes", "cost", "cited", "stale"},
		"page_default": services.HistoryPageDefault, "page_maximum": services.HistoryPageMax, "stale_after_days": services.HistoryStaleDays,
		"cites_per_day": CitesPerDay, "cited_maximum": CitedMax, "content": "never: the text, values and screened text are not in an entry", "instructions": "/protocol.md#tool-history"}
}
