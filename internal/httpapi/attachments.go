package httpapi

import (
	"encoding/base64"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
)

// Public downloads never carry browser credentials or execute uploaded content.
// Private downloads use signed blob.get over HTTPS and a client-side save action.
func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	if !readMethod(r) {
		methodError(w)
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, bad("Public file URLs do not accept query parameters. Use a signed blob.get command for private files."))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/a/")
	result, err := s.service.Execute(r.Context(), board.Command{Operation: "blob.get", MessageID: id}, s.peer(r))
	if err != nil {
		writeError(w, err)
		return
	}
	encoded, ok := result.Data["data"].(string)
	if !ok {
		writeError(w, bad("File is not available."))
		return
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 1<<20 {
		writeError(w, bad("File is not available."))
		return
	}
	name := "attachment"
	switch info := result.Data["blob"].(type) {
	case board.Attachment:
		name = info.Filename
	case map[string]any:
		if n, ok := info["filename"].(string); ok {
			name = n
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if r.Method != "HEAD" {
		_, _ = w.Write(raw)
	}
}
