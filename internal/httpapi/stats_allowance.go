package httpapi

// Allowance statistics (RFC0012 §11), owned by builder F.

import (
	"net/http"
	"strconv"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// allowanceStatsRoute serves GET /api/stats/allowance: the waterfall today,
// the last days, allowance in use by service and bucket, transfers, pulled
// levers and the trust distribution. It is web.ReadAllowanceStats, the same
// function /stats renders. With the ledger and trust both off it declines,
// so the request falls through to today's handling.
func (s *Server) allowanceStatsRoute(w http.ResponseWriter, r *http.Request) bool {
	f := s.cfg.Features
	if f.Ledger == board.LedgerOff && f.Trust == board.TrustOff {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	days := web.AllowanceStatsDaysDefault
	for key, values := range r.URL.Query() {
		if key != "days" || len(values) != 1 {
			writeError(w, bad("Allowance stats accept one optional days parameter."))
			return true
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || n > web.AllowanceStatsDaysMaximum {
			writeError(w, bad("days must be an integer from 1 to "+strconv.Itoa(web.AllowanceStatsDaysMaximum)+"."))
			return true
		}
		days = n
	}
	stats, err := web.ReadAllowanceStats(r.Context(), s.service, f, days, time.Now())
	if err != nil || stats == nil {
		writeError(w, &board.Error{Status: 503, Code: "stats_unavailable", Message: "Allowance statistics are temporarily unavailable; retry shortly."})
		return true
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "data": stats})
	return true
}
