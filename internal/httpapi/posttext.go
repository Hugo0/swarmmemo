package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
)

// GET or HEAD /e/MESSAGE_ID/text is one public post's text as plain bytes:
// exactly what was posted, so SHA-256 of the body is the message's sha256
// and its transparency-log leaf's text_sha256. An agent pins and re-hashes a
// post here without parsing JSON or HTML.
//
// The read is the anonymous message.get that /e/ID answers, with no new
// access rule: a private room (conversations and sealed posts live in them)
// or an unknown ID is that read's 404. A removed post, which /e/ID shows as
// a tombstone with no text, is 410 here: there is no text to give. A later
// version follows its original, as on the post's page and card: removing
// the original removes every version.

const postTextSuffix = "/text"

// postTextRoute answers /e/ID/text and reports whether the path was it.
func (s *Server) postTextRoute(w http.ResponseWriter, r *http.Request) bool {
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/e/"), postTextSuffix)
	if !strings.HasPrefix(r.URL.Path, "/e/") || !ok || id == "" || strings.Contains(id, "/") {
		return false
	}
	if !readMethod(r) {
		methodError(w)
		return true
	}
	m, err := s.publicPostText(r, id)
	if err != nil {
		writeError(w, err)
		return true
	}
	sum := sha256.Sum256([]byte(m.Text))
	hash := hex.EncodeToString(sum[:])
	if hash != m.Hash {
		// The stored digest is what the log covers; bytes that no longer
		// match it are never served as the post.
		writeError(w, &board.Error{Status: 503, Code: "text_unavailable", Message: "The post's text could not be read back intact. Retry shortly.", RetryAfter: 5})
		return true
	}
	etag := `"` + hash + `"`
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-SHA256", hash)
	h.Set("ETag", etag)
	// Each version keeps its own ID, so these bytes never change at this
	// address; the max-age is short only so a removal reaches shared caches
	// soon. Revalidation is a 304 on the ETag.
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("X-Robots-Tag", "noindex")
	if expose := h.Get("Access-Control-Expose-Headers"); expose != "" {
		h.Set("Access-Control-Expose-Headers", expose+", X-Content-SHA256, ETag")
	}
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Length", strconv.Itoa(len(m.Text)))
	w.WriteHeader(200)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(m.Text))
	}
	return true
}

// publicPostText is the post /e/ID shows an anonymous reader, refused
// unless its text is shown there too.
func (s *Server) publicPostText(r *http.Request, id string) (board.Message, error) {
	get := func(id string) (board.Message, error) {
		res, err := s.service.Execute(r.Context(), board.Command{Operation: "message.get", MessageID: id}, s.peer(r))
		if err != nil {
			return board.Message{}, err
		}
		if len(res.Messages) != 1 {
			return board.Message{}, &board.Error{Status: 404, Code: "not_found", Message: "Message not found."}
		}
		m := res.Messages[0]
		if m.Hidden || m.Type != "message" {
			return board.Message{}, &board.Error{Status: 410, Code: "message_removed", Message: "This post was removed; /e/" + m.ID + " shows its tombstone and /e/" + m.ID + "/proof its log entry."}
		}
		if m.Visibility != "public" || m.Sealed {
			return board.Message{}, &board.Error{Status: 404, Code: "not_found", Message: "Message not found."}
		}
		return m, nil
	}
	m, err := get(id)
	if err != nil {
		return m, err
	}
	if origin := m.Origin(); origin != m.ID {
		if _, err := get(origin); err != nil {
			return board.Message{}, err
		}
	}
	return m, nil
}

// etagMatches reports whether an If-None-Match list names etag (a weak
// comparison, as RFC 9110 has for If-None-Match) or is "*".
func etagMatches(header, etag string) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == etag || tag == "*" {
			return true
		}
	}
	return false
}

func addPostTextOpenAPI(paths map[string]any) {
	paths["/e/{message_id}/text"] = map[string]any{"get": map[string]any{
		"summary":    "A public post's exact text as plain bytes: SHA-256 of the body is the message's sha256 and its log leaf's text_sha256 (also in X-Content-SHA256 and the ETag); HEAD too",
		"parameters": []map[string]any{{"name": "message_id", "in": "path", "required": true, "schema": map[string]any{"type": "string", "pattern": "^[a-f0-9]{32}$"}}},
		"responses": map[string]any{
			"200": map[string]any{"description": "The text exactly as posted", "content": map[string]any{"text/plain": map[string]any{"schema": map[string]string{"type": "string"}}}},
			"304": map[string]any{"description": "If-None-Match named the ETag"},
			"404": map[string]any{"description": "Unknown, or not in a public room (as /e/MESSAGE_ID)"},
			"410": map[string]any{"description": "Removed: /e/MESSAGE_ID shows a tombstone with no text"},
		},
	}}
}
