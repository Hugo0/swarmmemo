package httpapi

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// DocRawPathPrefix is the path-only URL of a doc's or paste's current text:
// GET /d/ID.txt, for agents whose fetch tool refuses a query string.
const DocRawPathPrefix = "/d/"

var docRawRE = regexp.MustCompile(`^/d/([0-9a-f]{32})\.txt$`)

// docRaw serves GET /d/ID.txt: exactly the bytes docs.open with format=text
// answers, as one unsigned docs.open (the same per-network limits, price,
// screening and stats, refused from another site's page), inline with the
// bytes' SHA-256 as its ETag. Who may open it is docs.open's: an unlisted doc
// or paste by id; a private one is doc_not_found, as an unknown id is.
func (s *Server) docRaw(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if !s.cfg.Features.ServiceEnabled(services.DocsID) {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "No docs service runs here; /capabilities says what does."})
		return
	}
	// GET only: every answer is an open, which spends a credit.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		methodError(w)
		return
	}
	m := docRawRE.FindStringSubmatch(r.URL.Path)
	if m == nil || r.URL.RawQuery != "" {
		writeError(w, &board.Error{Status: 404, Code: "not_found", Message: "A doc's text is at /d/DOC_ID.txt (32 hex digits, no query); /call/docs/open?id=DOC_ID takes the other arguments."})
		return
	}
	if err := s.crossSiteCall(r); err != nil {
		w.Header().Del("Access-Control-Allow-Origin")
		w.Header().Del("Access-Control-Expose-Headers")
		writeError(w, err)
		return
	}
	e, method, found := services.LookupMethod(s.staticCatalog(), services.DocsID, "open")
	if !found {
		writeError(w, board.ServiceRefusal(services.UnknownTarget(s.staticCatalog(), services.DocsID, "open")))
		return
	}
	data, _, err := services.CallData(method, url.Values{"id": {m[1]}})
	if err != nil {
		writeError(w, board.ServiceRefusal(services.CallArgsRefusal(err)))
		return
	}
	c := board.Command{Operation: method.Operation, Target: e.ID, Data: data}
	r.Header.Set("Accept", "application/json")
	s.textDownload(w, withVia(r, strings.ToLower(r.Method)), c, services.DocsID, true)
}
