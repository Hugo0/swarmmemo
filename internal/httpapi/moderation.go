package httpapi

// Moderation routes (internal/moderation). While MODERATION is off every
// route declines and the capability is omitted, so requests are handled
// exactly as before.
//
// Public: GET /api/stats/moderation, aggregate counts only (the same function
// the /stats page draws). Steward, behind the admin token and read-only:
// GET /admin/moderation/queue, /log, /alerts, /spend and /policy. Content is
// never served over HTTP; the operator CLI shows one item at a time.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/moderation"
)

// moderationAdminLimit bounds one steward page.
const moderationAdminLimit = 500

type moderationStore interface {
	Moderation() *moderation.Engine
	ModerationStats(ctx context.Context, days int) (*moderation.Stats, error)
}

func (s *Server) moderationEngine() (moderationStore, *moderation.Engine) {
	if !s.cfg.Features.Moderation {
		return nil, nil
	}
	store, ok := s.service.(moderationStore)
	if !ok || store.Moderation() == nil {
		return nil, nil
	}
	return store, store.Moderation()
}

// moderationStatsRoute serves GET /api/stats/moderation[?days=N]; false
// leaves the request to today's handling.
func (s *Server) moderationStatsRoute(w http.ResponseWriter, r *http.Request) bool {
	store, e := s.moderationEngine()
	if e == nil {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	days, ok := queryInt(r.URL.Query(), "days", 7, moderation.StatsDaysMax, "days")
	if !ok {
		writeError(w, bad("Moderation stats take only days, 1 to 90 (default 7)."))
		return true
	}
	st, err := store.ModerationStats(r.Context(), days)
	if err != nil || st == nil {
		writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "Moderation statistics are temporarily unavailable."})
		return true
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "timezone": "UTC", "stats": st,
		"notes": []string{"Decisions per UTC day by action, and per surface, over the last days days (today included). Counts only: no subject, author, category or content.",
			"The standard: hide only phishing, malware, slur harassment or extreme vulgarity, sexual content involving minors, and doxxing. Every hidden post shows its reason."}})
	return true
}

// queryInt reads the only allowed parameter, name, as 1..max; absent is def.
func queryInt(q url.Values, name string, def, max int, allowed ...string) (int, bool) {
	for k := range q {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok || len(q[k]) != 1 {
			return 0, false
		}
	}
	v := q.Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > max || strconv.Itoa(n) != v {
		return 0, false
	}
	return n, true
}

// moderationAdmin serves the steward's read-only routes, after the admin
// token was checked; false leaves the request to the admin handler.
func (s *Server) moderationAdmin(w http.ResponseWriter, r *http.Request) bool {
	_, e := s.moderationEngine()
	if e == nil || !strings.HasPrefix(r.URL.Path, "/admin/moderation/") {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	q := r.URL.Query()
	limit, ok := queryInt(q, "limit", 50, moderationAdminLimit, "limit", "surface", "state", "subject", "action")
	if !ok {
		writeError(w, bad("Parameters: limit (1 to 500), surface, state, subject, action."))
		return true
	}
	ctx := r.Context()
	var out any
	var err error
	switch strings.TrimPrefix(r.URL.Path, "/admin/moderation/") {
	case "queue":
		out, err = e.Queue(ctx, moderation.QueueQuery{Surface: moderation.Surface(q.Get("surface")), State: q.Get("state"), Limit: limit})
	case "log":
		out, err = e.Log(ctx, moderation.LogQuery{Surface: moderation.Surface(q.Get("surface")), Subject: q.Get("subject"), Action: moderation.Action(q.Get("action")), Limit: limit})
	case "alerts":
		out, err = e.Alerts(ctx, limit)
	case "spend":
		out, err = e.SpendToday(ctx)
	case "policy":
		p := e.Policy(ctx)
		out = map[string]any{"version": p.Version, "source": p.Source, "sha256": p.SHA256, "policy": p}
	default:
		return false
	}
	if err != nil {
		writeError(w, &board.Error{Status: 503, Code: "storage_unavailable", Message: "Moderation records are temporarily unavailable."})
		return true
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "data": out})
	return true
}

// moderationCapabilities is the /capabilities "moderation" object; nil omits it.
func (s *Server) moderationCapabilities() map[string]any {
	if _, e := s.moderationEngine(); e == nil {
		return nil
	}
	return map[string]any{
		"enabled":        true,
		"surfaces":       moderation.Surfaces(),
		"actions":        []moderation.Action{moderation.Allow, moderation.Flag, moderation.Hold, moderation.Hide, moderation.Block},
		"standard":       "hide only phishing, malware, slur harassment or extreme vulgarity, sexual content involving minors, and doxxing",
		"private_rooms":  "never screened",
		"public_reasons": true,
		"stats":          "/api/stats/moderation",
		"instructions":   "/protocol.md#moderation",
	}
}
