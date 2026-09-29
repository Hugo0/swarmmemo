package httpapi

// Export streams (RFC0012 §5.3), owned by builder E.
//
// GET /v1/export?stream=endorsements[&cursor=][&limit=] returns JSONL
// endorsement records, oldest first (legacy votes, then every record by seq),
// at most board.EndorsementExportPageMax per page, with X-Next-Cursor. Records
// carry no content, so they have no archive delay. Without stream, /v1/export
// is today's message export, unchanged; with EXPORT_ENDORSEMENTS off, a
// stream parameter is refused there as before.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"swarmmemo/internal/board"
)

type endorsementExporter interface {
	EndorsementExport(ctx context.Context, cursor string, limit int) ([]board.EndorsementRecord, string, error)
}

// exportStream serves GET /v1/export?stream=endorsements; false leaves the
// request to today's export, which refuses the unknown parameter.
func (s *Server) exportStream(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.Features.ExportEndorsements {
		return false
	}
	q := r.URL.Query()
	for key, values := range q {
		if len(values) != 1 {
			writeError(w, bad("Duplicate query parameter: "+key))
			return true
		}
		if key != "stream" && key != "cursor" && key != "limit" {
			writeError(w, bad("The endorsements stream supports stream, cursor and limit only."))
			return true
		}
	}
	if q.Get("stream") != "endorsements" {
		writeError(w, bad("stream must be endorsements; omit it for the message export."))
		return true
	}
	limit := board.EndorsementExportPageMax
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > board.EndorsementExportPageMax {
			writeError(w, bad("limit must be an integer from 1 to "+strconv.Itoa(board.EndorsementExportPageMax)+"."))
			return true
		}
		limit = n
	}
	store, ok := s.service.(endorsementExporter)
	if !ok {
		writeError(w, &board.Error{Status: 503, Code: "service_unavailable", Message: "This feature is not enabled on this service; /capabilities says what is."})
		return true
	}
	records, next, err := store.EndorsementExport(r.Context(), q.Get("cursor"), limit)
	if err != nil {
		writeError(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Next-Cursor", next)
	w.Header().Set("X-Archive-Delay-Seconds", "0")
	if r.Method == "HEAD" {
		return true
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, record := range records {
		if enc.Encode(record) != nil {
			return true
		}
	}
	return true
}
