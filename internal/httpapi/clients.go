package httpapi

import (
	"net/http"
	"strings"

	"swarmmemo/internal/board"
)

// Arrivals by client (board/clientstats.go): each request is classified into
// one of board.ClientFamilies from what the server already sees, the MCP
// initialize clientInfo.name and the User-Agent, which are then discarded.
// clientTokens is the one table to extend for a new platform: a token
// matches a whole word of the lowercased User-Agent or client name ("claude"
// matches "claude-code/1.0" and "Claude-User", not "ClaudeBot"), first match
// wins. uaFamilyClients maps the named User-Agent families the referrer
// counters already recognize (userAgentFamily); any other named family is a
// crawler.

var clientTokens = []struct{ token, family string }{
	{"cursor", "cursor-grok"}, {"grok", "cursor-grok"}, {"xai", "cursor-grok"},
	{"chatgpt", "openai"}, {"openai", "openai"}, {"codex", "openai"}, {"dots", "openai"},
	{"muse", "meta-muse"}, {"meta-ai", "meta-muse"}, {"metaai", "meta-muse"},
	{"claude", "claude"},
	{"gemini", "gemini"}, {"geminicli", "gemini"},
	{"perplexity", "perplexity"},
}

var uaFamilyClients = map[string]string{
	"ChatGPT-User": "openai", "Claude-User": "claude", "Perplexity-User": "perplexity", "meta-externalfetcher": "meta-muse",
	"curl": "scripts", "Wget": "scripts", "python-requests": "scripts", "python-httpx": "scripts", "Python-urllib": "scripts",
	"aiohttp": "scripts", "Go-http-client": "scripts", "node": "scripts", "axios": "scripts",
}

// assistantFamilies are the families named for an assistant platform.
var assistantFamilies = map[string]bool{"cursor-grok": true, "openai": true, "meta-muse": true, "claude": true, "gemini": true, "perplexity": true}

// hasToken reports whether token occurs in s as a whole word: not preceded
// or followed by a letter or digit.
func hasToken(s, token string) bool {
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
	for from := 0; ; {
		i := strings.Index(s[from:], token)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(token)
		if (i == 0 || !alnum(s[i-1])) && (end == len(s) || !alnum(s[end])) {
			return true
		}
		from = i + 1
	}
}

// tokenFamily is the assistant family whose token s names, or "".
func tokenFamily(s string) string {
	s = strings.ToLower(s)
	for _, t := range clientTokens {
		if hasToken(s, t.token) {
			return t.family
		}
	}
	return ""
}

// userAgentClient is the family of a User-Agent: a named crawler or agent
// family first (so GPTBot, whose User-Agent mentions openai.com, is a
// crawler), then an assistant token, then any other self-declared crawler,
// a browser, or other.
func userAgentClient(ua string) string {
	if len(ua) > userAgentBytes {
		ua = ua[:userAgentBytes]
	}
	named := userAgentFamily(ua)
	if family, ok := uaFamilyClients[named]; ok {
		return family
	}
	if named != "" && named != "other-bot" {
		return "crawlers"
	}
	if family := tokenFamily(ua); family != "" {
		return family
	}
	switch {
	case named == "other-bot":
		return "crawlers"
	case strings.HasPrefix(strings.ToLower(ua), "mozilla/"):
		return "browsers"
	}
	return "other"
}

// requestClient is the family of a request. At /mcp every caller is an MCP
// client, so one no assistant token names is other-mcp.
func requestClient(r *http.Request) string {
	family := userAgentClient(r.UserAgent())
	if profile, _, _ := mcpPath(r.URL.Path); profile == "/mcp" && !assistantFamilies[family] {
		return "other-mcp"
	}
	return family
}

// mcpClient is the family of an MCP initialize from its clientInfo.name
// (its first board.ClientNameMaxBytes only), else the request's assistant
// family, else other-mcp; unknown is the reduced name to count when no
// family matched it.
func mcpClient(r *http.Request, name string) (family, unknown string) {
	if len(name) > board.ClientNameMaxBytes {
		name = name[:board.ClientNameMaxBytes]
	}
	if family = tokenFamily(name); family != "" {
		return family, ""
	}
	if family = requestClient(r); !assistantFamilies[family] {
		family = "other-mcp"
	}
	return family, board.ClientName(name)
}

type clientCounter interface {
	CountClient(family, metric, name string)
}

// countClient counts one request to a discovery surface by the request's
// family. It never touches storage and never fails the request.
func (s *Server) countClient(r *http.Request, metric string) {
	if c, ok := s.service.(clientCounter); ok {
		c.CountClient(board.ClientFamilyFrom(r.Context()), metric, "")
	}
}

// countClientInitialize counts one MCP initialize and, when no family
// matched it, its client name (counted, never published).
func (s *Server) countClientInitialize(r *http.Request, name string) {
	if c, ok := s.service.(clientCounter); ok {
		family, unknown := mcpClient(r, name)
		c.CountClient(family, "mcp_initialize", unknown)
	}
}

// clientsJSON is one day's arrivals by client as /api/stats/daily serves
// them: each family's published metrics (absent is zero or not published,
// board.ReadClientStats) and services, and the count of unknown MCP client
// names.
func clientsJSON(d board.ClientDay) map[string]any {
	families := map[string]any{}
	for name, f := range d.Families {
		counts := map[string]any{}
		for metric, n := range f.Counts {
			counts[metric] = n
		}
		if len(f.Services) > 0 {
			counts["services"] = f.Services
		}
		families[name] = counts
	}
	return map[string]any{"families": families, "unknown_mcp_clients": d.UnknownMCPClients}
}
