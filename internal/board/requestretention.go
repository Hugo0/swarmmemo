package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// Stored write answers (C1124). Every accepted write keeps its answer in
// requests under its request_id and nonce, so an exact retry gets the same
// answer and never runs twice. They are kept for good by default; an account
// erasure removes its own (erase.go). The operator may set a retention period
// (swarmmemo params set retention), after which the hourly maintenance
// deletes older answers (pruneRequests). The longest flow a retry answers is
// an offering call (a claim window of an hour, then an SLA of up to 7 days),
// so a period shorter than that cuts retries short. Once a row is gone a
// signed retry is refused as stale (SignatureWindowSeconds), never run twice,
// and its request_id is free again; an unsigned caller's namespace changes
// with the daily salt anyway.

// RetentionParamsNamespace holds the retention numbers, versioned like every
// other operator parameter.
const RetentionParamsNamespace = "retention"

// RequestRetentionDaysMax bounds request_retention_days.
const RequestRetentionDaysMax = 3650

// retentionParams is the namespace's body. RequestRetentionDays 0 keeps
// stored write answers for good (no prune).
type retentionParams struct {
	Schema               int    `json:"schema"`
	RequestRetentionDays *int64 `json:"request_retention_days"`
}

func defaultRetentionParams() []byte { return []byte(`{"schema":1,"request_retention_days":0}`) }

func parseRetentionParams(body []byte) (int64, error) {
	var p retentionParams
	if services.StrictObject(body, &p) != nil || p.Schema != 1 || p.RequestRetentionDays == nil {
		return 0, errors.New(`retention params: a strict JSON object {"schema":1,"request_retention_days":DAYS}`)
	}
	if d := *p.RequestRetentionDays; d < 0 || d > RequestRetentionDaysMax {
		return 0, fmt.Errorf("retention params: request_retention_days is 0 (keep for good) to %d", RequestRetentionDaysMax)
	}
	return *p.RequestRetentionDays, nil
}

func init() {
	ledger.RegisterNamespace(RetentionParamsNamespace, ledger.Namespace{
		Version:  0,
		Body:     defaultRetentionParams,
		Validate: func(b []byte) error { _, err := parseRetentionParams(b); return err },
	})
}

// requestRetentionDays is the retention period in effect; 0 keeps for good.
func (s *Store) requestRetentionDays(ctx context.Context, now int64) (int64, error) {
	_, body, err := s.ledger.params.Params(ctx, s.db, RetentionParamsNamespace, now)
	if err != nil || len(body) == 0 {
		return 0, err
	}
	return parseRetentionParams(body)
}

// pruneRequests deletes the stored write answers older than the retention
// period, in bounded batches; with no period set (0) it deletes nothing.
// Each batch is its own statement on the pool, never inside a held
// transaction. It scans by created_at without an index: old rows sit at the
// front of the rowid order, and a batch stops at its limit.
func (s *Store) pruneRequests(ctx context.Context) (int64, error) {
	now := s.now().Unix()
	days, err := s.requestRetentionDays(ctx, now)
	if err != nil || days == 0 {
		return 0, err
	}
	cutoff := now - days*86400
	var total int64
	for {
		r, err := s.db.ExecContext(ctx, "DELETE FROM requests WHERE rowid IN (SELECT rowid FROM requests WHERE created_at<? LIMIT ?)", cutoff, signalPruneBatch)
		if err != nil {
			return total, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < signalPruneBatch {
			if total > 0 {
				slog.Info("Stored write answers removed", "older_than_days", days, "count", total)
			}
			return total, nil
		}
	}
}
