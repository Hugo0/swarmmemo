// Package servicestest is a stand-in ledger and price source for tests of the
// service middleware, in this package and in the board and HTTP layers. The
// fake keeps its journal in a table of the caller's own database, written
// through the caller's transaction, so a rolled-back command spends nothing,
// exactly as with the real ledger.
package servicestest

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

const table = `CREATE TABLE IF NOT EXISTS fake_ledger (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, hold_id TEXT NOT NULL DEFAULT '',
 account TEXT NOT NULL, resource TEXT NOT NULL, units INTEGER NOT NULL, max_units INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT '', request_key TEXT NOT NULL DEFAULT '', service TEXT NOT NULL DEFAULT '',
 method TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL DEFAULT 0)`

// Meter is a fake services.Meter: a flat daily budget per resource and
// account, spends and holds journalled in fake_ledger.
type Meter struct {
	mu     sync.Mutex
	Budget map[allowance.Resource]int64
	next   int
}

// NewMeter gives every account budget units of each resource.
func NewMeter(budget int64) *Meter {
	return &Meter{Budget: map[allowance.Resource]int64{allowance.MemoryBytes: budget, allowance.Credit: budget, allowance.PostBytes: budget}}
}

// Entry is one fake journal line.
type Entry struct {
	Kind, HoldID, Account, Resource, State, RequestKey, Service, Method string
	Units, MaxUnits, ExpiresAt                                          int64
}

func (m *Meter) used(ctx context.Context, q allowance.Querier, account string, r allowance.Resource) (int64, error) {
	if _, err := q.ExecContext(ctx, table); err != nil {
		return 0, err
	}
	var used int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(sum(CASE WHEN kind='spend' THEN units WHEN state IN ('held','expired') THEN max_units WHEN state='committed' THEN units ELSE 0 END),0)
 FROM fake_ledger WHERE account=? AND resource=?`, account, string(r)).Scan(&used)
	return used, err
}

func (m *Meter) Spend(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, units int64, ref ledger.Ref, now int64) (ledger.Receipt, error) {
	if units <= 0 {
		return ledger.Receipt{}, &allowance.Err{Code: "invalid_amount"}
	}
	used, err := m.used(ctx, q, s.ID, r)
	if err != nil {
		return ledger.Receipt{}, err
	}
	if used+units > m.Budget[r] {
		return ledger.Receipt{}, &allowance.Err{Code: "quota_exhausted", RetryAfter: int(86400 - now%86400)}
	}
	_, err = q.ExecContext(ctx, "INSERT INTO fake_ledger(kind,account,resource,units,service,method) VALUES('spend',?,?,?,?,?)", s.ID, string(r), units, ref.Service, ref.Method)
	return ledger.Receipt{Resource: r, Used: units}, err
}

func (m *Meter) Reserve(ctx context.Context, q allowance.Querier, s allowance.Subject, r allowance.Resource, max int64, requestKey string, ref ledger.Ref, ttl int64, now int64) (ledger.Hold, error) {
	used, err := m.used(ctx, q, s.ID, r)
	if err != nil {
		return ledger.Hold{}, err
	}
	var open int
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM fake_ledger WHERE kind='hold' AND account=? AND request_key=?", s.ID, requestKey).Scan(&open); err != nil {
		return ledger.Hold{}, err
	}
	if open > 0 {
		return ledger.Hold{}, &allowance.Err{Code: "request_in_flight", RetryAfter: 1}
	}
	if used+max > m.Budget[r] {
		return ledger.Hold{}, &allowance.Err{Code: "quota_exhausted", RetryAfter: int(86400 - now%86400)}
	}
	m.mu.Lock()
	m.next++
	id := fmt.Sprintf("hold-%d-%s", m.next, requestKey)
	m.mu.Unlock()
	h := ledger.Hold{ID: id, Account: s.ID, Resource: r, Max: max, ExpiresAt: now + ttl}
	_, err = q.ExecContext(ctx, "INSERT INTO fake_ledger(kind,hold_id,account,resource,units,max_units,state,request_key,service,method,expires_at) VALUES('hold',?,?,?,0,?,'held',?,?,?,?)",
		id, s.ID, string(r), max, requestKey, ref.Service, ref.Method, h.ExpiresAt)
	return h, err
}

func (m *Meter) hold(ctx context.Context, q allowance.Querier, id string) (state string, resource string, max int64, err error) {
	if _, err = q.ExecContext(ctx, table); err != nil {
		return
	}
	err = q.QueryRowContext(ctx, "SELECT state,resource,max_units FROM fake_ledger WHERE kind='hold' AND hold_id=?", id).Scan(&state, &resource, &max)
	return
}

func (m *Meter) Commit(ctx context.Context, q allowance.Querier, holdID string, used int64, now int64) (ledger.Receipt, error) {
	state, resource, max, err := m.hold(ctx, q, holdID)
	if err != nil {
		return ledger.Receipt{}, err
	}
	if state != "held" || used < 0 || used > max {
		return ledger.Receipt{}, &allowance.Err{Code: "hold_not_held"}
	}
	_, err = q.ExecContext(ctx, "UPDATE fake_ledger SET state='committed', units=? WHERE kind='hold' AND hold_id=?", used, holdID)
	return ledger.Receipt{HoldID: holdID, Resource: allowance.Resource(resource), Used: used, Refunded: max - used}, err
}

func (m *Meter) Refund(ctx context.Context, q allowance.Querier, holdID string, reason string, now int64) error {
	state, _, _, err := m.hold(ctx, q, holdID)
	if err != nil {
		return err
	}
	if state != "held" {
		return &allowance.Err{Code: "hold_not_held"}
	}
	_, err = q.ExecContext(ctx, "UPDATE fake_ledger SET state='refunded' WHERE kind='hold' AND hold_id=?", holdID)
	return err
}

// Sweep settles every held hold that expired by now at its maximum, as the
// real ledger's sweeper does (§2.5 crashes).
func (m *Meter) Sweep(ctx context.Context, db *sql.DB, now int64) (int64, error) {
	if _, err := db.ExecContext(ctx, table); err != nil {
		return 0, err
	}
	r, err := db.ExecContext(ctx, "UPDATE fake_ledger SET state='expired', units=max_units WHERE kind='hold' AND state='held' AND expires_at<=?", now)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Entries is the fake journal, oldest first.
func (m *Meter) Entries(ctx context.Context, db *sql.DB) ([]Entry, error) {
	if _, err := db.ExecContext(ctx, table); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT kind,hold_id,account,resource,state,request_key,service,method,units,max_units,expires_at FROM fake_ledger ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err = rows.Scan(&e.Kind, &e.HoldID, &e.Account, &e.Resource, &e.State, &e.RequestKey, &e.Service, &e.Method, &e.Units, &e.MaxUnits, &e.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Params is a fake price source: Version and Body are what it returns.
type Params struct {
	mu      sync.Mutex
	Version int64
	Body    []byte
}

func (p *Params) Set(version int64, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Version, p.Body = version, body
}

func (p *Params) Params(ctx context.Context, q allowance.Querier, namespace string, now int64) (int64, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if namespace != "services" {
		return 0, nil, allowance.Unavailable()
	}
	return p.Version, p.Body, nil
}
