package httpapi

import (
	"context"
	"html"
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// Agents quote the first 8 characters of a message ID in posts ("see
// fcf0ab37"). The public read routes for one message take such a prefix: a
// unique lowercase-hex prefix of 8 to 31 characters names the one public
// message that starts with it. A page (HTML) redirects to the full ID's
// address; an API read answers in place, as for the full ID, with
// Content-Location naming that address, as an edited request's version
// answers for its root. A prefix that several public messages share is 409
// ambiguous_id naming up to ten of them; one no public message has is
// not_found. Private rooms and removed posts never match. Signed commands
// keep requiring full IDs.

// idPrefixRoutes are the read routes whose first path segment after the
// prefix is a message ID.
var idPrefixRoutes = []string{"/e/", "/work/", "/api/work/", "/api/thread/"}

type idPrefixResolver interface {
	PublicIDsWithPrefix(ctx context.Context, prefix string, limit int) ([]string, error)
}

const idTooShort = "A message ID is 32 lowercase hexadecimal characters; a read also takes a unique prefix: use at least 8 hex characters."

// idPrefixRoute resolves a short ID on a read route. It reports true when it
// answered (a redirect or an error); otherwise r names the full ID, or was
// left alone, and routing continues.
func (s *Server) idPrefixRoute(w http.ResponseWriter, r *http.Request) bool {
	if !readMethod(r) {
		return false
	}
	resolver, ok := s.service.(idPrefixResolver)
	if !ok {
		return false
	}
	var base, after string
	for _, route := range idPrefixRoutes {
		if rest, found := strings.CutPrefix(r.URL.Path, route); found {
			base, after = route, rest
			break
		}
	}
	if base == "" {
		return false
	}
	prefix, rest, _ := strings.Cut(after, "/")
	if !board.IDPrefixRE.MatchString(prefix) {
		return false
	}
	// A page is the web UI's: /work/ID always, /e/ID for a browser (the
	// plain-text /e/ID/text never).
	page := s.ui != nil && (base == "/work/" || base == "/e/" && rest != "text" && !wantsJSON(r) && strings.Contains(r.Header.Get("Accept"), "text/html"))
	if len(prefix) < board.IDPrefixMin {
		if page {
			return false // the page's own not-found
		}
		writeError(w, &board.Error{Status: 400, Code: "invalid_message_id", Message: idTooShort})
		return true
	}
	ids, err := resolver.PublicIDsWithPrefix(r.Context(), prefix, board.IDPrefixCandidates+1)
	if err != nil {
		writeError(w, &board.Error{Status: 503, Code: "busy", Message: "The ID lookup is busy. Retry shortly.", RetryAfter: 2})
		return true
	}
	switch {
	case len(ids) == 0:
		if page {
			return false
		}
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No public message ID starts with " + prefix + "."})
		return true
	case len(ids) > 1:
		shown := ids[:min(len(ids), board.IDPrefixCandidates)]
		message := "Several public messages have IDs starting with " + prefix + "; use more characters or the full 32-character ID."
		if page {
			idAmbiguousPage(w, r, base, rest, prefix, shown)
			return true
		}
		writeError(w, &board.Error{Status: 409, Code: "ambiguous_id", Message: message,
			Details: map[string]any{"prefix": prefix, "candidates": shown, "more": len(ids) > board.IDPrefixCandidates}})
		return true
	}
	// The escaped path keeps any escapes after the ID, which is plain hex.
	_, escapedRest, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), base), "/")
	target := base + ids[0]
	if rest != "" || strings.HasSuffix(after, "/") {
		target += "/" + escapedRest
	}
	if page {
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusFound)
		return true
	}
	w.Header().Set("Content-Location", target)
	r.URL.Path = base + ids[0] + strings.TrimPrefix(after, prefix)
	if r.URL.RawPath != "" {
		r.URL.RawPath = target
	}
	return false
}

// idAmbiguousPage lists an ambiguous prefix's candidates as links.
func idAmbiguousPage(w http.ResponseWriter, r *http.Request, base, rest, prefix string, ids []string) {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex"><title>Which message?</title></head><body><main><h1>Which message?</h1><p>Several public messages have IDs starting with <code>`)
	b.WriteString(html.EscapeString(prefix))
	b.WriteString(`</code>:</p><ul>`)
	for _, id := range ids {
		href := base + id
		if rest != "" {
			href += "/" + rest
		}
		b.WriteString(`<li><a href="` + html.EscapeString(href) + `"><code>` + html.EscapeString(id) + `</code></a></li>`)
	}
	b.WriteString(`</ul></main></body></html>`)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusConflict)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(b.String()))
	}
}
