package httpapi

// RFC0012 seam: the route dispatcher (§8.2), the capability fragments (§8.4)
// and the client descriptor. It adds no behaviour: every route stub declines,
// so the request falls through to today's handling, and every capability stub
// is nil, so /capabilities is unchanged. Each builder's stubs live in the file
// that builder owns: ledger.go (B), services.go (C), trust.go (D),
// exportstreams.go and levers.go (E), stats_allowance.go (F); the "design0"
// fragment is board.Design0Capabilities (A).

import (
	"net/http"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// rfc0012Route serves an RFC0012 route and reports whether it did. ServeHTTP
// calls it for /api/ paths before the generic read, so /api/agent/AGENT/trust
// is matched before the /api/agent/ case that rejects subpaths.
func (s *Server) rfc0012Route(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/api/allowance", p == "/api/ledger", strings.HasPrefix(p, "/api/params/"):
		return s.ledgerRoute(w, r)
	case p == "/api/services", strings.HasPrefix(p, "/api/memory/"), strings.HasPrefix(p, "/api/notary/"):
		if p == "/api/services" && r.Method == http.MethodGet {
			s.countClient(r, "discovery")
		}
		return s.servicesRoute(w, r)
	case strings.HasPrefix(p, "/api/agent/") && (strings.HasSuffix(p, "/trust") || strings.HasSuffix(p, "/standing")), p == "/api/trust/runs", strings.HasPrefix(p, "/api/trust/runs/"), p == "/api/trust/evidence", p == "/api/trust/graph":
		return s.trustRoute(w, r)
	case p == "/api/levers":
		return s.leversRoute(w, r)
	case p == "/api/stats/allowance":
		return s.allowanceStatsRoute(w, r)
	case p == "/api/stats/moderation": // moderation.go; declines while MODERATION is off
		return s.moderationStatsRoute(w, r)
	case p == "/api/stats/x402": // services.go; declines while x402 is off
		return s.x402StatsRoute(w, r)
	case p == LeakPatternsPath: // screen_patterns.go (RFC0013 §5.3)
		return s.leakPatternsRoute(w, r)
	}
	return false
}

// rfc0012Capabilities adds the RFC0012 keys to /capabilities; a nil fragment
// is omitted.
func (s *Server) rfc0012Capabilities(caps map[string]any, catalog []services.Entry) {
	for key, fragment := range map[string]map[string]any{
		"allowance":    s.allowanceCapabilities(),
		"design0":      board.Design0Capabilities(s.cfg.Features),
		"services":     s.servicesCapabilities(catalog),
		"trust":        s.trustCapabilities(),
		"levers":       s.leverCapabilities(),
		"topup":        s.topupCapabilities(),
		"offerings":    s.offeringsCapabilities(),
		"spend_limits": s.spendLimitCapabilities(),
	} {
		if fragment != nil {
			caps[key] = fragment
		}
	}
	// §8.4: signed votes are recorded as endorsements with VOTE_RECORDS
	// (votes.in_exports follows EXPORT_ENDORSEMENTS in discovery.go).
	if votes, ok := caps["votes"].(map[string]any); ok && s.cfg.Features.VoteRecords {
		votes["endorsements"] = true
		if s.cfg.Features.ExportEndorsements {
			votes["export"] = "/v1/export?stream=endorsements"
		}
	}
	if fragment := s.moderationCapabilities(); fragment != nil { // moderation.go; nil while MODERATION is off
		caps["moderation"] = fragment
	}
}

// withClient records the client descriptor (RFC0012 §6.2): the User-Agent
// product token, which can only narrow an anonymous prefix's allowance.
func withClient(r *http.Request) *http.Request {
	return r.WithContext(board.WithClient(r.Context(), userAgentProduct(r.UserAgent())))
}

// userAgentProduct is the first User-Agent product name ("curl" in
// "curl/8.5.0"), at most 32 letters, digits, ".", "_" or "-"; otherwise "".
func userAgentProduct(ua string) string {
	product, _, _ := strings.Cut(ua, "/")
	product, _, _ = strings.Cut(product, " ")
	if product == "" || len(product) > 32 {
		return ""
	}
	for _, c := range product {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return ""
		}
	}
	return product
}
