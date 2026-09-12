package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"swarmmemo/internal/board"
)

type publicUpdater interface {
	PublicUpdates(context.Context, int64) ([]board.Message, int64, error)
}
type generationPublicUpdater interface {
	PublicUpdatesGeneration(context.Context, int64, string) ([]board.Message, int64, string, error)
}

var generationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "generation") || len(values) != 1 {
			writeError(w, bad("Use at most one after and one generation parameter."))
			return
		}
	}
	if q.Has("generation") && !generationPattern.MatchString(q.Get("generation")) {
		writeError(w, bad("Use the generation returned by this server."))
		return
	}
	after := int64(-1)
	if value := q.Get("after"); value != "" {
		var err error
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < -1 {
			writeError(w, bad("Invalid change watermark."))
			return
		}
	}
	updater, ok := s.service.(publicUpdater)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "updates_unavailable", Message: "Live corrections are temporarily unavailable."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var events []board.Message
	var next int64
	var generation string
	var err error
	if scoped, ok := s.service.(generationPublicUpdater); ok {
		events, next, generation, err = scoped.PublicUpdatesGeneration(ctx, after, q.Get("generation"))
	} else if q.Has("generation") {
		writeError(w, &board.Error{Status: 503, Code: "updates_unavailable", Message: "Generation-bound corrections are unavailable; do not advance durable correction state."})
		return
	} else {
		events, next, err = updater.PublicUpdates(ctx, after)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	result := map[string]any{"ok": true, "messages": events, "after": next}
	if generation != "" {
		result["generation"] = generation
		result["service_id"] = s.cfg.ServiceID
	}
	jsonResponse(w, 200, result)
}
