package httpapi

import (
	"context"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// GET /api/work/ID/listing (C716): a paid task as one paste-ready block,
// web.WorkListingFor, the same block /work/ID shows under "Share this task".
// JSON by default ({ok, data: {listing}}); Markdown with Accept:
// text/markdown or ?format=md. A valid ?ref=VENUE goes into the block's URL.
// It is its own route: work.get keeps its fields, as strict clients refuse
// unknown ones.

const workListingSuffix = "/listing"

const workListingHint = "Expected GET /api/work/MESSAGE_ID/listing, with optional format=md (or json) and ref=VENUE."

// workListingRefKey carries the read's ?ref= past withoutRef, which drops it
// from every request before routing.
type workListingRefKey struct{}

func workListingPath(p string) bool {
	return strings.HasPrefix(p, "/api/work/") && strings.HasSuffix(p, workListingSuffix)
}

// withWorkListingRef keeps a listing read's ref value in its context.
func withWorkListingRef(r *http.Request) *http.Request {
	if !workListingPath(r.URL.Path) || !strings.Contains(r.URL.RawQuery, "ref=") {
		return r
	}
	refs := r.URL.Query()["ref"]
	if len(refs) != 1 || !board.ValidReferrerRef(refs[0]) {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), workListingRefKey{}, refs[0]))
}

// workListingRoute serves GET /api/work/ID/listing and reports whether it did.
func (s *Server) workListingRoute(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if !workListingPath(p) {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/work/"), workListingSuffix)
	if !allowMethods(w, r, readMethods...) {
		return true
	}
	query := r.URL.Query()
	for key, values := range query {
		if key != "format" || len(values) != 1 {
			writeError(w, bad(workListingHint))
			return true
		}
	}
	if id == "" || strings.Contains(id, "/") {
		writeError(w, bad(workListingHint))
		return true
	}
	markdown := false
	switch query.Get("format") {
	case "md", "markdown":
		markdown = true
	case "json":
	case "":
		markdown = strings.Contains(r.Header.Get("Accept"), "text/markdown")
	default:
		writeError(w, bad("format is md (Markdown) or json, the default."))
		return true
	}
	// An unsigned read: a private room's work is 404, as on /api/work/ID.
	res, err := s.service.Execute(r.Context(), board.Command{Operation: "work.get", MessageID: id}, s.peer(r))
	if err != nil {
		writeError(w, err)
		return true
	}
	item, ok := res.Data["work"].(board.Work)
	if !ok {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No such work, or it is in a room you cannot read."})
		return true
	}
	if web.WorkListingClosed(item.State) {
		writeError(w, &board.Error{Status: 409, Code: "work_state_conflict", Message: "This task is " + item.State + ": it takes no more results, so it has no listing. Open tasks: /api/works."})
		return true
	}
	ref, _ := r.Context().Value(workListingRefKey{}).(string)
	listing := web.WorkListingFor(item, ref)
	if item.ResolvedFrom != "" {
		w.Header().Set("Content-Location", "/api/work/"+item.ID+workListingSuffix)
	}
	if markdown {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(listing.Markdown))
		}
		return true
	}
	jsonResponse(w, 200, map[string]any{"ok": true, "data": map[string]any{"listing": listing}})
	return true
}
