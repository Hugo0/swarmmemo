package httpapi

// Allowance and ledger routes (RFC0012 §8.2), owned by builder B. While
// ALLOWANCE_LEDGER is off every route declines, so the request falls through
// to today's handling, and the capability fragment is omitted.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
)

// paramsReader is the store side of /api/params.
type paramsReader interface {
	AllowanceParams(ctx context.Context, namespace string, version int64) (ledger.ParamsVersion, error)
	AllowanceParamsList(ctx context.Context, namespace string) ([]ledger.ParamsVersion, error)
}

// ledgerRoute serves GET /api/allowance[?agent=], /api/ledger?agent=&cursor=&limit=
// and /api/params/NAMESPACE[/VERSION]; false leaves the request to today's
// handling.
func (s *Server) ledgerRoute(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Features.Ledger == board.LedgerOff {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	p := r.URL.Path
	switch p {
	case "/api/allowance", "/api/ledger":
		c, err := queryCommand(r.URL.Query())
		if err != nil {
			writeError(w, err)
			return true
		}
		c.Operation = "allowance.get"
		if p == "/api/ledger" {
			c.Operation = "ledger.list"
		}
		// The generic path restates the "free today" note as next.allowance.
		s.execute(w, r, c)
		return true
	}
	s.paramsRoute(w, r, strings.TrimPrefix(p, "/api/params/"))
	return true
}

// paramsRoute serves /api/params/NAMESPACE (the version in effect and the list of
// versions) and /api/params/NAMESPACE/VERSION (one body).
func (s *Server) paramsRoute(w http.ResponseWriter, r *http.Request, rest string) {
	store, ok := s.service.(paramsReader)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "service_unavailable", Message: "Parameters are not available on this service."})
		return
	}
	namespace, version, hasVersion := strings.Cut(rest, "/")
	notFound := &board.Error{Status: 404, Code: "not_found", Message: "No such parameter namespace or version; /api/params/allowance lists the allowance versions."}
	if namespace == "" || strings.Contains(version, "/") || len(r.URL.Query()) > 0 && !(len(r.URL.Query()) == 1 && r.URL.Query().Has("format")) {
		writeError(w, bad("Expected /api/params/NAMESPACE or /api/params/NAMESPACE/VERSION."))
		return
	}
	lookup := func(err error) {
		var e *allowance.Err
		if errors.Is(err, ledger.ErrNotFound) || errors.As(err, &e) {
			writeError(w, notFound)
			return
		}
		writeError(w, err)
	}
	if hasVersion {
		v, err := strconv.ParseInt(version, 10, 64)
		if err != nil || v < 0 {
			writeError(w, bad("A parameter version is a whole number."))
			return
		}
		pv, err := store.AllowanceParams(r.Context(), namespace, v)
		if err != nil {
			lookup(err)
			return
		}
		jsonResponse(w, 200, map[string]any{"ok": true, "data": pv})
		return
	}
	current, err := store.AllowanceParams(r.Context(), namespace, -1)
	if err != nil {
		lookup(err)
		return
	}
	versions, err := store.AllowanceParamsList(r.Context(), namespace)
	if err != nil {
		lookup(err)
		return
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "data": map[string]any{"namespace": namespace, "current": current, "versions": versions}})
}

// allowanceCapabilities is the /capabilities "allowance" object; nil omits it.
func (s *Server) allowanceCapabilities() map[string]any {
	if s.cfg.Features.Ledger == board.LedgerOff {
		return nil
	}
	store, ok := s.service.(interface {
		AllowanceCapabilities(context.Context) map[string]any
	})
	if !ok {
		return nil
	}
	return store.AllowanceCapabilities(context.Background())
}
