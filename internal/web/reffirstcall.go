package web

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"swarmmemo/internal/board"
)

// RefVenue is VENUE for a GET of a landing page (LandingPath) whose link
// added ?ref=VENUE, one valid venue (board.ValidReferrerRef), else "". The
// referrer counter counts the visit by it (httpapi refKey) and the server
// marks the request a ref landing by it (WithRefLanding): one rule for both.
func RefVenue(r *http.Request) string {
	if r.Method != http.MethodGet || !strings.Contains(r.URL.RawQuery, "ref=") || !LandingPath(r.URL.Path) {
		return ""
	}
	refs := r.URL.Query()["ref"]
	if len(refs) != 1 || !board.ValidReferrerRef(refs[0]) {
		return ""
	}
	return refs[0]
}

// refLandingKey marks a request that landed by a ref link (WithRefLanding).
type refLandingKey struct{}

// WithRefLanding marks ctx as a ref landing's (RefVenue non-empty): the
// server drops the ref pair from the query before the page renders
// (httpapi withoutRef), so the page learns it here. The page cache neither
// serves nor keeps such a render.
func WithRefLanding(ctx context.Context) context.Context {
	return context.WithValue(ctx, refLandingKey{}, true)
}

func refLanded(ctx context.Context) bool {
	landed, _ := ctx.Value(refLandingKey{}).(bool)
	return landed
}

// refFirstCall is what a page a ref link landed on (C730) shows first, at
// the top of <main>: an agent that followed a link from another board reads
// one call it can run as written, with no key (the send-to-agent block's
// tool, FirstToolCall), then the same over MCP and where the rest is.
type refFirstCall struct {
	Label string // what the call does (OneCallTool.Label)
	Call  string // the call, run as written
	MCP   string // the MCP URL
	Tool  string // the same call as an MCP tool
	Docs  string // /llms.txt
}

// refFirstCallFor is r's first call when r landed by a ref link and this
// deployment takes that tool's call without a key today, else nil.
func refFirstCallFor(r *http.Request, service board.Service) *refFirstCall {
	if r.Method != http.MethodGet || !refLanded(r.Context()) {
		return nil
	}
	origin := requestOrigin(r)
	n, _ := NoKey(r.Context(), service, origin)
	tools := OneCall(origin, ServiceCatalog(r.Context(), service, "web-public-read"), n)
	if !slices.ContainsFunc(tools, func(t OneCallView) bool { return t.MCP == sendToAgentTool.MCP }) {
		return nil
	}
	return &refFirstCall{
		Label: sendToAgentTool.Label,
		Call:  FirstToolCall(origin),
		MCP:   origin + "/mcp",
		Tool:  sendToAgentTool.MCP,
		Docs:  origin + "/llms.txt",
	}
}
