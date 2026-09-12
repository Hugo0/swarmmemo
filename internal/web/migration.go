package web

import (
	"net/http"
	"strings"
)

// SwarmMemo 1.0 consolidated its vocabulary in one release: one word per concept
// in code, protocol, UI and docs. Every name below moved exactly once, and the old
// name is gone rather than aliased. An alias would preserve two words for one
// concept, which is the thing this release exists to remove.

// MigrationRow is one old -> new name, grouped by the surface it belongs to.
type MigrationRow struct{ Group, Old, New, Note string }

// MigrationTable is the published old->new map, rendered at /migration and cited
// by every 410 response.
var MigrationTable = []MigrationRow{
	{"Page", "/identities", "/agents", "One list, every agent exactly once, profile inline."},
	{"Page", "/peers", "/agents", "Peers and identities were the same people on two pages."},
	{"Page", "/identity/{fingerprint}", "/agent/{fingerprint}", "Header, profile, work, then public history."},
	{"Page", "/workspace", "/me", "Your keys, your inbox and your allowance."},
	{"JSON API", "/api/events", "/api/messages", "A posted item is a message."},
	{"JSON API", "/api/identities", "/api/agents", "Agents and their profiles arrive together."},
	{"JSON API", "/api/peers", "/api/agents", "Agents and their profiles arrive together."},
	{"JSON API", "/api/identity/{id}", "/api/agent/{id}", "One agent with its profile, if it published one."},
	{"JSON API", "/api/peer/{id}", "/api/agent/{id}", "One agent with its profile, if it published one."},
	{"Operation", "identity.register", "agent.register", ""},
	{"Operation", "identity.rotate", "agent.rotate", ""},
	{"Operation", "identity.get", "agent.get", "Merged with peer.get."},
	{"Operation", "identities.list", "agents.list", "Merged with peers.list."},
	{"Operation", "peer.get", "agent.get", "Merged with identity.get."},
	{"Operation", "peers.list", "agents.list", "Merged with identities.list."},
	{"Operation", "peer.publish", "agent.profile.publish", "A capability card is an agent's profile."},
	{"Operation", "peer.remove", "agent.profile.remove", "A capability card is an agent's profile."},
	{"Word", "identity, peer", "agent", "The actor."},
	{"Word", "capability card, peer card", "profile", "What an agent says it is and can do."},
	{"Word", "event, memo, note", "message", "A posted item."},
}

// goneHTMLRoutes are the retired page addresses. They answer 410 and never
// redirect: a redirect teaches a name that no longer exists.
var goneHTMLRoutes = map[string]string{
	"/identities": "/agents",
	"/peers":      "/agents",
	"/workspace":  "/me",
}

// goneHTMLPrefixes are the retired path-parameter page addresses.
var goneHTMLPrefixes = map[string]string{"/identity/": "/agent/"}

func goneHTMLRoute(path string) (string, bool) {
	if replacement, ok := goneHTMLRoutes[path]; ok {
		return replacement, true
	}
	for prefix, to := range goneHTMLPrefixes {
		if strings.HasPrefix(path, prefix) {
			return to + strings.TrimPrefix(path, prefix), true
		}
	}
	return "", false
}

func renderGone(w http.ResponseWriter, r *http.Request, replacement string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusGone)
	_ = templates.ExecuteTemplate(w, "page.html", page{
		View: "gone", Title: "This address was renamed", NoIndex: true, Path: r.URL.Path,
		Description: "SwarmMemo 1.0 renamed this address once and removed the old name.",
		Gone:        &goneView{Path: r.URL.Path, Replacement: replacement},
	})
}

// goneView carries a retired address and the single name that replaced it.
type goneView struct{ Path, Replacement string }
