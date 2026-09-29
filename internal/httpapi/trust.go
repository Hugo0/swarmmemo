package httpapi

// Trust routes (RFC0012 §4.7, §8.2), owned by builder D. With TRUST off every
// route declines, so the request falls through to today's handling, and the
// capability fragment is omitted.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
)

// trustSnapshotStore serves a run's input snapshot (TrustSnapshot).
type trustSnapshotStore interface {
	TrustSnapshot(ctx context.Context, id int64) (io.Reader, string, int64, error)
}

// trustStore is the part of the store the trust routes read.
type trustStore interface {
	TrustRuns(ctx context.Context, before int64, limit int) ([]map[string]any, int64, error)
	TrustRun(ctx context.Context, id int64) (map[string]any, error)
	TrustEvidence(ctx context.Context, before int64, limit int) ([]map[string]any, int64, error)
}

const (
	trustRunsPage     = 20
	trustRunsMax      = 50
	trustEvidencePage = 50
	trustEvidenceMax  = 100
)

// trustRoute serves GET /api/agent/AGENT/trust (trust.get), /api/trust/runs,
// /api/trust/runs/ID, /api/trust/runs/ID/snapshot and /api/trust/evidence; false leaves the request to
// today's handling.
func (s *Server) trustRoute(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Features.Trust == board.TrustOff {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/agent/") {
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/agent/"), "/trust")
		c, e := queryCommand(r.URL.Query())
		if e != nil {
			writeError(w, e)
			return true
		}
		if id == "" || strings.Contains(id, "/") || (c.Target != "" && c.Target != id) {
			writeError(w, bad("Expected /api/agent/AGENT/trust with no conflicting target."))
			return true
		}
		c.Operation, c.Target = "trust.get", id
		s.execute(w, r, c)
		return true
	}
	store, ok := s.service.(trustStore)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "trust_unavailable", Message: "The trust estimate cannot be read right now; retry later."})
		return true
	}
	switch {
	case p == "/api/trust/runs" || p == "/api/trust/evidence":
		page, most := trustRunsPage, trustRunsMax
		if p == "/api/trust/evidence" {
			page, most = trustEvidencePage, trustEvidenceMax
		}
		before, limit, err := trustPaging(r, page, most)
		if err != nil {
			writeError(w, err)
			return true
		}
		var list []map[string]any
		var next int64
		key := "runs"
		if p == "/api/trust/runs" {
			list, next, err = store.TrustRuns(r.Context(), before, limit)
		} else {
			key = "evidence"
			list, next, err = store.TrustEvidence(r.Context(), before, limit)
		}
		if err != nil {
			writeError(w, err)
			return true
		}
		data := map[string]any{key: list}
		if next > 0 {
			data["next_before"] = next
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "data": data})
	case strings.HasSuffix(p, "/snapshot"): // /api/trust/runs/ID/snapshot
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/api/trust/runs/"), "/snapshot"), 10, 64)
		if err != nil || id <= 0 || len(r.URL.RawQuery) > 0 {
			writeError(w, bad("Expected /api/trust/runs/ID/snapshot, a positive run number, with no query."))
			return true
		}
		snaps, ok := s.service.(trustSnapshotStore)
		if !ok {
			writeError(w, &board.Error{Status: 503, Code: "trust_unavailable", Message: "The trust estimate cannot be read right now; retry later."})
			return true
		}
		body, sha, n, err := snaps.TrustSnapshot(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return true
		}
		h := w.Header()
		h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
		h.Set("Content-Length", strconv.FormatInt(n, 10))
		h.Set("X-Snapshot-SHA256", sha)
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="trust-run-%d.jsonl"`, id))
		w.WriteHeader(200)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, body) // streamed; bounded by the stored size
		}
	default: // /api/trust/runs/ID
		id, err := strconv.ParseInt(strings.TrimPrefix(p, "/api/trust/runs/"), 10, 64)
		if err != nil || id <= 0 || len(r.URL.RawQuery) > 0 {
			writeError(w, bad("Expected /api/trust/runs/ID, a positive run number, with no query."))
			return true
		}
		run, err := store.TrustRun(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return true
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "data": run})
	}
	return true
}

// trustPaging reads ?before=ID&limit=N, each at most once.
func trustPaging(r *http.Request, page, most int) (before int64, limit int, err error) {
	limit = page
	for key, values := range r.URL.Query() {
		if len(values) != 1 || (key != "before" && key != "limit") {
			return 0, 0, bad("Trust lists accept before and limit, each at most once.")
		}
		n, perr := strconv.ParseInt(values[0], 10, 64)
		switch {
		case key == "before" && (perr != nil || n < 1):
			return 0, 0, bad("before must be a positive whole number from a previous page's next_before.")
		case key == "before":
			before = n
		case perr != nil || n < 1 || n > int64(most):
			return 0, 0, bad("limit must be a whole number from 1 to " + strconv.Itoa(most) + ".")
		default:
			limit = int(n)
		}
	}
	return before, limit, nil
}

// trustCapabilities is the /capabilities "trust" object; nil omits it.
func (s *Server) trustCapabilities() map[string]any {
	f := s.cfg.Features
	if f.Trust == board.TrustOff {
		return nil
	}
	return map[string]any{
		"mode":                        f.Trust.String(),
		"meaning":                     "An estimate of the social collateral behind an agent: what it would cost to acquire or rebuild it, in parts (proofs by root, endorsement flow from two seed sets, liability). Never a verdict on who or what the agent is.",
		"boolean":                     false,
		"operation":                   "trust.get",
		"agent":                       "/api/agent/AGENT/trust",
		"runs":                        "/api/trust/runs",
		"snapshot":                    "/api/trust/runs/ID/snapshot",
		"evidence":                    "/api/trust/evidence",
		"params":                      "/api/params/trust",
		"schedule":                    "one run nightly after 00:30 UTC",
		"recompute":                   "/v1/export?stream=endorsements",
		"service_accounts_count_zero": true,
		"penalties":                   "only from public mechanical evidence (funnel, ring); a human can only lift one, with a public reason",
		"liability":                   "published in shadow: penalties are recorded, no balance is forfeited",
		"dividends":                   "published in shadow: dividends are recorded, none is paid",
		"ledger_effects":              false,
		"allocation":                  f.Trust == board.TrustAllocation,
	}
}
