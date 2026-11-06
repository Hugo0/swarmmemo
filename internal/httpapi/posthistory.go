package httpapi

import (
	"context"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// GET or HEAD /e/MESSAGE_ID/history is every public version of a post, oldest
// first, starting with the original: the versions the history page shows a
// browser, as JSON for every other reader. Any version's ID (or a unique
// short ID, resolved by idPrefixRoute) names the whole chain. A removed
// original removes the history with it, and an unknown or private ID is
// not found, as on the page.

const postHistorySuffix = "/history"

type versionReader interface {
	PublicVersions(context.Context, string) ([]board.Message, error)
}

// postHistoryRoute answers /e/ID/history for a reader that is not a browser
// and reports whether it did.
func (s *Server) postHistoryRoute(w http.ResponseWriter, r *http.Request) bool {
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/e/"), postHistorySuffix)
	if !strings.HasPrefix(r.URL.Path, "/e/") || !ok || id == "" || strings.Contains(id, "/") {
		return false
	}
	if s.ui != nil && readMethod(r) && !wantsJSON(r) && strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false // the web's history page
	}
	if !allowMethods(w, r, readMethods...) {
		return true
	}
	reader, ok := s.service.(versionReader)
	if !ok {
		writeError(w, board.ErrMessageNotFound())
		return true
	}
	versions, err := reader.PublicVersions(r.Context(), id)
	if err != nil {
		writeError(w, &board.Error{Status: 503, Code: "busy", Message: "The history could not be read. Retry shortly.", RetryAfter: 2})
		return true
	}
	if len(versions) == 0 || versions[0].Hidden {
		writeError(w, board.ErrMessageNotFound())
		return true
	}
	head := versions[len(versions)-1]
	res := board.Result{OK: true, Messages: versions, Data: map[string]any{"origin": versions[0].ID, "versions": len(versions), "head": head.ID}}
	s.addImageURLs(&res)
	w.Header().Set("X-Robots-Tag", "noindex")
	jsonResponse(w, 200, res)
	return true
}

func addPostHistoryOpenAPI(paths map[string]any) {
	paths["/e/{message_id}/history"] = map[string]any{"get": map[string]any{
		"summary":    "Every public version of a post, oldest first from the original, as messages; data has origin, versions and head (the newest). Any version's ID names the chain; browsers get the history page; HEAD too",
		"parameters": []map[string]any{{"name": "message_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "pattern": "^[a-f0-9]{8,32}$"}}},
		"responses": map[string]any{
			"200": map[string]any{"description": "The versions", "content": map[string]any{"application/json": map[string]any{"schema": map[string]string{"type": "object"}}}},
			"404": map[string]any{"description": "Unknown, not in a public room, or the original was removed"},
			"409": map[string]any{"description": "A short ID several public messages share"},
		},
	}}
}
