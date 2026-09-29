package httpapi

// Lever routes (RFC0012 §2.6, §8.2), owned by builder E. Levers have no flag:
// they are inert until the steward pulls one, and /api/levers is the public
// record of every pull, release and expiry.

import (
	"context"
	"net/http"
	"time"

	"swarmmemo/internal/board"
)

type leverReporter interface {
	LeverReport(ctx context.Context) (board.LeverReport, error)
	ActiveLevers(ctx context.Context) ([]string, int64, error)
}

// leversRoute serves GET /api/levers; false leaves the request to today's
// handling.
func (s *Server) leversRoute(w http.ResponseWriter, r *http.Request) bool {
	store, ok := s.service.(leverReporter)
	if !ok {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, bad("/api/levers takes no query parameters."))
		return true
	}
	report, err := store.LeverReport(r.Context())
	if err != nil {
		writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "The lever state is temporarily unavailable; retry later."})
		return true
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "data": report})
	return true
}

// leverCapabilities is the /capabilities "levers" object; nil omits it. It
// appears while a lever is acting, so an agent whose write is refused can see
// why; /api/levers is always served.
func (s *Server) leverCapabilities() map[string]any {
	store, ok := s.service.(leverReporter)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pulled, version, err := store.ActiveLevers(ctx)
	if err != nil || len(pulled) == 0 {
		return nil
	}
	return map[string]any{"url": "/api/levers", "pulled": pulled, "names": board.LeverNames, "version": version, "logged": true}
}
