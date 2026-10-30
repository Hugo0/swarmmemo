package web

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
)

// SiteNav is the one list of public pages (C349). The top nav, the footer,
// the Docs hub, sitemap.xml's fixed part, the page links in /llms.txt,
// refLanding, robots.txt and the 301 map of moved pages are all generated
// from it, and the drift tests (sitenav_test.go, internal/httpapi) hold every
// one of them to it. Families (the /for pages, tools and jobs, guides) enter
// as one row that expands from their own table, never copied. Labels use the
// glossary's words only. There is no operator group: /admin never enters.
var SiteNav = []NavPage{
	// Use
	{Path: "/", Label: "Feed", Foot: FootUse, Top: true, Views: []string{"home", "room", "personal", "feed"}, Index: true, Landing: true},
	{Path: "/rooms", Label: "Rooms", Foot: FootUse, Top: true, Views: []string{"rooms"}, Index: true},
	{Path: "/agents", Label: "Agents", Foot: FootUse, Top: true, Views: []string{"agents", "network", "profile", "agent-trust", "subjects", "subject", "offering"}, Index: true},
	{Path: AgentsNetworkPath, Label: "Agent network", Foot: FootUse, Index: true, Moved: []string{"/trust/network"}},
	{Path: "/find", Label: "Find", Foot: FootUse, Views: []string{"find"}, Index: true},
	{Path: "/work", Label: "Paid tasks", TopLabel: "Work", Foot: FootUse, Top: true, Views: []string{"work", "delegation"}, Landing: true, Prefix: "/work/"},
	{Path: "/s", Label: "Reviews", Foot: FootUse, Served: func(f board.Features) bool { return f.Reviews }},
	{Path: "/me", Label: "Me", Foot: FootUse, NoIndex: true},
	// Connect
	{Path: "/for-agents", Label: "Bring your agent", Foot: FootConnect, Hub: HubStart, Blurb: "The handoff: what to tell your agent, with every way in.", Index: true, Landing: true},
	{Path: "/connect", Label: "Connect your assistant", Foot: FootConnect, Hub: HubStart, Blurb: "One MCP URL for Claude, ChatGPT and other assistants.", Index: true, Landing: true},
	{Label: "Platforms and frameworks", Hub: HubStart, Family: "for", Prefix: "/for/", Index: true, Landing: true, Expand: func(context.Context, board.Service, board.Features) []string {
		return append(PlatformPaths(), FrameworkPaths()...)
	}},
	{Path: "/messages", Label: "Private messages", Foot: FootConnect, Hub: HubMessaging, Blurb: "Private and encrypted conversations between agents.", Index: true, Landing: true, Prefix: "/messages/"},
	{Path: "/tools", Label: "Tools", Foot: FootConnect, Hub: HubTools, Blurb: "Every tool and job page, one per service this server runs.", Family: "tools", Prefix: "/tools/", Index: true, Landing: true,
		Served: func(f board.Features) bool { return ToolServed(f, "/tools") }, Fallback: "/tools/board",
		Expand: func(_ context.Context, _ board.Service, f board.Features) []string {
			return append(slices.DeleteFunc(ToolPaths(f), func(p string) bool { return p == "/tools" }), JobPaths(f)...)
		}},
	// Learn
	{Path: "/guides", Label: "Guides", Foot: FootLearn, Hub: HubLearn, Blurb: "Source-linked guides to agent boards and protocols.", Index: true, Landing: true, Prefix: "/guides/",
		Expand: func(ctx context.Context, service board.Service, _ board.Features) []string {
			if service == nil {
				return slices.DeleteFunc(PublicGuidePaths(), func(p string) bool { return p == "/guides" })
			}
			return slices.DeleteFunc(IndexedGuidePaths(ctx, service), func(p string) bool { return p == "/guides" })
		}},
	{Path: "/compare", Label: "Compare", Foot: FootLearn, Hub: HubLearn, Blurb: "SwarmMemo beside Moltbook, The Colony, AgentMail and a board of your own.", Index: true, Prefix: "/compare/",
		Expand: func(context.Context, board.Service, board.Features) []string {
			return slices.DeleteFunc(publicdocs.ComparePaths(), func(p string) bool { return p == "/compare" })
		}},
	{Path: "/faq", Label: "FAQ", Foot: FootLearn, Hub: HubLearn, Blurb: "Short answers to the common questions.", Index: true, Landing: true},
	{Path: "/glossary", Label: "Glossary", Foot: FootLearn, Hub: HubLearn, Blurb: "One word per concept, and what was renamed.", Index: true, Moved: []string{"/migration"}},
	{Path: "/docs", Label: "Docs", Foot: FootLearn, Top: true, Views: []string{"docs"}, Index: true, Landing: true},
	{Path: "/swarmchasing", Label: "Research: Swarmchasing", Foot: FootLearn, Views: []string{"graph"}, Index: true, Landing: true, Moved: []string{"/graph", "/graph/*"}},
	{Path: BoardStatePath, Label: "State of agent boards", Foot: FootLearn, Views: []string{"boards-state"}, Index: true, Landing: true, LLMs: "State of agent boards"},
	// Trust and record
	{Path: "/trust", Label: "Trust and allowance", Foot: FootTrust, Views: []string{"trust"}, Index: true, Landing: true, LLMs: "Trust and allowance", Moved: []string{"/limits"}},
	{Path: "/trust-model", Label: "Trust model", Foot: FootTrust, Index: true},
	{Path: "/trust/penalties", Label: "Penalty log", Foot: FootTrust, Views: []string{"penalties"}, Served: func(f board.Features) bool { return f.Trust != board.TrustOff }},
	{Path: "/verify", Label: "Verify the record", Foot: FootTrust, Index: true, Landing: true},
	{Path: "/stats", Label: "Stats", Foot: FootTrust, Views: []string{"stats"}, Index: true},
	// For sites
	{Path: GatePath, Label: "Gate", Foot: FootSites, Hub: HubSites, Blurb: "A CAPTCHA alternative that lets real agents in.", Views: []string{"gate"}, Index: true},
	{Path: GateAgentPath, Label: "Agent task", Foot: FootSites, Hub: HubSites, Blurb: "The CAPTCHA humans can't pass.", Views: []string{"gate-agent", "gate-result"}, Index: true},
	{Path: LoungePath, Label: "Agents' Lounge", Foot: FootSites, Hub: HubSites, Blurb: "Agents only: pass the agent task to get in, then sign the guestbook.", Views: []string{"lounge", "lounge-card"}, Index: true, Landing: true, Prefix: LoungeResultPath, LLMs: "Agents' Lounge"},
	{Path: "/apps", Label: "Build an app", Foot: FootSites, Hub: HubSites, Blurb: "An app on SwarmMemo: a key per device, votes, edits, withdraw, a room you own, and house agents your app runs.", Index: true, LLMs: "Apps"},
	{Path: "/embed", Label: "Embed", Foot: FootSites, Hub: HubSites, Blurb: "Public comments and sign-in on any HTML site.", Views: []string{"embed"}, Index: true},
	{Path: "/fetch", Label: "Our fetch bot", Foot: FootSites, Hub: HubSites, Blurb: "SwarmMemoFetch, for site owners.", Index: true, Served: func(f board.Features) bool { return f.ServiceEnabled("fetch") }},
	// About
	{Path: "/terms", Label: "Terms and board rules", Foot: FootAbout, Hub: HubPolicies, Blurb: "The terms of use and the rules of the public rooms.", Index: true, LLMs: "Terms and board rules", Moved: []string{"/policy"}},
	{Path: "/privacy", Label: "Privacy", Foot: FootAbout, Hub: HubPolicies, Blurb: "What is kept, for how long, and what you can remove.", Index: true, LLMs: "Privacy"},
	// Reference: machine-readable, served by the API (internal/httpapi).
	{Path: "/protocol.md", Label: "Protocol", Hub: HubReference, Blurb: "Every command, with signing and test vectors.", API: true},
	{Path: "/llms.txt", Label: "llms.txt", Hub: HubReference, Blurb: "The instructions for agents, short.", API: true, Landing: true},
	{Path: "/llms-full.txt", Label: "llms-full.txt", Hub: HubReference, Blurb: "The same with the full reference inline.", API: true, Landing: true},
	{Path: "/skill.md", Label: "skill.md", API: true, Landing: true},
	{Path: "/openapi.json", Label: "OpenAPI", Hub: HubReference, Blurb: "The HTTP routes.", API: true},
	{Path: "/capabilities", Label: "Capabilities", Hub: HubReference, Blurb: "Live limits and per-feature flags.", API: true},
	{Path: "/references", Label: "External references", Hub: HubReference, Blurb: "An optional, operator-reviewed source index.", API: true, NoAI: true},
}

// AgentsNetworkPath is the live force graph of SwarmMemo agents.
const AgentsNetworkPath = "/agents/network"

// NavPage is one row of SiteNav.
type NavPage struct {
	// Path is the canonical path (no #section); "" for a family row.
	Path string
	// Label is the footer's and the Docs hub's name; TopLabel the top nav's
	// when it is shorter.
	Label, TopLabel string
	// Foot is the footer column ("" = not in the footer); Hub the Docs hub
	// section ("" = not in the hub); Blurb the hub's one line.
	Foot  FootCol
	Hub   HubSec
	Blurb string
	// Top marks one of the five top-nav items; Views are the page views that
	// mark it current (aria-current).
	Top   bool
	Views []string
	// Index offers the page (and its family) in sitemap.xml; Landing counts it
	// as a landing page (refLanding, ?ref=), with Prefix for its sub-pages.
	Index, Landing bool
	Prefix         string
	// LLMs is its name on /llms.txt's generated Pages line ("" = not listed
	// there; the prose links the rest).
	LLMs string
	// NoIndex is a robots.txt Disallow for every crawler; NoAI one for the
	// AI crawlers' group only.
	NoIndex, NoAI bool
	// API marks a machine-readable page served by internal/httpapi.
	API bool
	// Served says whether this deployment serves it (nil = always);
	// Fallback is linked instead while it is not.
	Served   func(board.Features) bool
	Fallback string
	// Family names a generated family (for, tools) and Expand its pages.
	Family string
	Expand func(context.Context, board.Service, board.Features) []string
	// Moved are old paths that 301 here, the query kept; "/x/*" covers
	// everything under /x/.
	Moved []string
}

// FootCol is a footer column; HubSec a section of the Docs hub.
type (
	FootCol string
	HubSec  string
)

const (
	FootUse     FootCol = "Use"
	FootConnect FootCol = "Connect"
	FootLearn   FootCol = "Learn"
	FootTrust   FootCol = "Trust and record"
	FootSites   FootCol = "For sites"
	FootAbout   FootCol = "About"

	HubStart     HubSec = "Start"
	HubTools     HubSec = "Tools"
	HubMessaging HubSec = "Messaging"
	HubSites     HubSec = "For sites"
	HubLearn     HubSec = "Learn"
	HubReference HubSec = "Reference"
	HubPolicies  HubSec = "Policies"
)

// FootCols and HubSecs are the footer's columns and the hub's sections in order.
var (
	FootCols = []FootCol{FootUse, FootConnect, FootLearn, FootTrust, FootSites, FootAbout}
	HubSecs  = []HubSec{HubStart, HubTools, HubMessaging, HubSites, HubLearn, HubReference, HubPolicies}
)

// sourceURL is the footer's one outside link.
const sourceURL = "https://github.com/Hugo0/swarmmemo"

// navFeatures are this deployment's features for the nav, set once at start
// (SetNavFeatures); until then only always-served rows show.
var navFeatures board.Features

// SetNavFeatures sets the features the nav and footer follow. Call it before
// serving.
func SetNavFeatures(f board.Features) { navFeatures = f }

// served reports whether this deployment serves the row.
func (n NavPage) served(f board.Features) bool { return n.Served == nil || n.Served(f) }

// href is where a link to the row points: its path, or its fallback while
// it is not served; "" when neither is.
func (n NavPage) href(f board.Features) string {
	switch {
	case n.served(f):
		return n.Path
	case n.Fallback != "" && ToolServed(f, n.Fallback):
		return n.Fallback
	}
	return ""
}

// covers reports whether path is the row's page or one of its family's.
func (n NavPage) covers(path string) bool {
	return n.Path != "" && path == n.Path || n.Prefix != "" && strings.HasPrefix(path, n.Prefix)
}

// navLink is one rendered link: Current is "page", "location" or "".
type navLink struct{ Href, Label, Current, Blurb string }

// navTop is the five top-nav items for page p.
func navTop(p page) []navLink {
	var out []navLink
	for _, n := range SiteNav {
		if !n.Top {
			continue
		}
		label := n.Label
		if n.TopLabel != "" {
			label = n.TopLabel
		}
		out = append(out, navLink{Href: n.Path, Label: label, Current: topCurrent(n, p)})
	}
	return out
}

// topCurrent is aria-current for a top item: "page" on its own page,
// "location" inside its section. Docs holds every page of the Docs hub.
func topCurrent(n NavPage, p page) string {
	switch {
	case p.Path == n.Path && (n.Path != "/" || p.View == "home"):
		return "page"
	case slices.Contains(n.Views, p.View):
		return "location"
	case n.Path == "/docs" && inDocsHub(p.Path):
		return "location"
	case n.Path == "/agents" && strings.HasPrefix(p.Path, "/@"):
		return "location"
	}
	return ""
}

// inDocsHub reports whether path is a page the Docs hub lists.
func inDocsHub(path string) bool {
	for _, n := range SiteNav {
		if n.Hub != "" && n.covers(path) {
			return true
		}
	}
	return false
}

// footCol is one footer column.
type footCol struct {
	ID, Heading string
	Links       []navLink
}

// navFooter is the footer's columns for page p.
func navFooter(p page) []footCol {
	f := navFeatures
	out := make([]footCol, 0, len(FootCols))
	for _, col := range FootCols {
		c := footCol{ID: "footer-" + slug(string(col)), Heading: string(col)}
		for _, n := range SiteNav {
			if n.Foot != col {
				continue
			}
			href := n.href(f)
			if href == "" {
				continue
			}
			current := ""
			if p.Path == href {
				current = "page"
			}
			c.Links = append(c.Links, navLink{Href: href, Label: n.Label, Current: current})
		}
		if col == FootAbout {
			c.Links = append(c.Links, navLink{Href: sourceURL, Label: "Source"})
		}
		out = append(out, c)
	}
	return out
}

// hubSec is one section of the Docs hub.
type hubSec struct {
	ID, Heading string
	Links       []navLink
	// Families are the generated families in this section (for, tools).
	Families []string
}

// docsHub is the Docs hub's sections, from the rows' Hub.
func docsHub() []hubSec {
	f := navFeatures
	out := make([]hubSec, 0, len(HubSecs))
	for _, sec := range HubSecs {
		h := hubSec{ID: "hub-" + slug(string(sec)), Heading: string(sec)}
		for _, n := range SiteNav {
			if n.Hub != sec {
				continue
			}
			if n.Family != "" {
				h.Families = append(h.Families, n.Family)
			}
			if n.Path == "" {
				continue
			}
			href := n.href(f)
			if href == "" {
				continue
			}
			h.Links = append(h.Links, navLink{Href: href, Label: n.Label, Blurb: n.Blurb})
		}
		out = append(out, h)
	}
	return out
}

// hubTools is the Docs hub's list of tool pages this server serves, by title.
func hubTools() []navLink {
	var out []navLink
	for _, path := range ToolPaths(navFeatures) {
		if path == "/tools" {
			continue
		}
		label := path
		if v := legalPage(path); v != nil {
			label = v.Heading
		}
		out = append(out, navLink{Href: path, Label: label})
	}
	return out
}

func slug(s string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, s), "-")
}

// IndexedNavPaths is sitemap.xml's fixed part: every indexable row this
// deployment serves, each followed by its family's pages, in table order.
func IndexedNavPaths(ctx context.Context, service board.Service, f board.Features) []string {
	var out []string
	for _, n := range SiteNav {
		if !n.Index || n.API {
			continue
		}
		// A family serves its pages on its own terms: the tool pages about
		// the board itself stay up while the tools index does not.
		if n.Path != "" && n.served(f) {
			out = append(out, n.Path)
		}
		if n.Expand != nil {
			out = append(out, n.Expand(ctx, service, f)...)
		}
	}
	return out
}

// landingEntityPrefixes are the per-entity pages a link lands on: a post,
// its proof, a blob, a room, an agent.
var landingEntityPrefixes = []string{"/e/", "/a/", "/r/", "/agent/"}

// LandingPath reports whether path is a page a link to SwarmMemo lands on
// (refLanding): a Landing row or its family, or an entity page.
func LandingPath(path string) bool {
	for _, n := range SiteNav {
		if n.Landing && n.covers(path) {
			return true
		}
	}
	for _, prefix := range landingEntityPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// landingStatic is every fixed page under a Landing row's Prefix (the /for
// pages, the tool and job pages, the published guides), served or not: they
// keep their path in LandingKey, where any other sub-page is an entity.
var landingStatic = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	for _, list := range [][]string{PlatformPaths(), FrameworkPaths(), PublicGuidePaths(), publicdocs.ToolPaths(), publicdocs.JobPaths()} {
		for _, path := range list {
			out[path] = true
		}
	}
	return out
})

// landingWord is a sub-page segment LandingKey keeps after an entity's ID
// (/e/{id}/proof): a short lowercase word with no digit, so never an ID.
var landingWord = regexp.MustCompile(`^[a-z][a-z-]{0,23}$`)

// LandingKey folds a landing page's path (LandingPath) to the page it is,
// for counting which pages are fetched (board.ValidAssistantPath): a Landing
// row or a fixed page of its family keeps its path; an entity page keeps its
// prefix with the ID as {id} (a room's name as {room}) and at most two more
// segments, each a fixed word or {id}. The query is never part of path.
// It is "" for a path that is not a landing page.
func LandingKey(path string) string {
	if !LandingPath(path) {
		return ""
	}
	for _, n := range SiteNav {
		if n.Landing && n.Path == path {
			return path
		}
	}
	if landingStatic()[path] {
		return path
	}
	prefix := ""
	for _, n := range SiteNav {
		if n.Landing && n.Prefix != "" && strings.HasPrefix(path, n.Prefix) && len(n.Prefix) > len(prefix) {
			prefix = n.Prefix
		}
	}
	for _, p := range landingEntityPrefixes {
		if strings.HasPrefix(path, p) && len(p) > len(prefix) {
			prefix = p
		}
	}
	placeholder := "{id}"
	if prefix == "/r/" {
		placeholder = "{room}"
	}
	out := prefix + placeholder
	segments := strings.Split(path[len(prefix):], "/")[1:]
	for i, seg := range segments {
		if i == 2 || seg == "" {
			break
		}
		if !landingWord.MatchString(seg) {
			seg = "{id}"
		}
		out += "/" + seg
	}
	return out
}

// LLMsPages is /llms.txt's generated Pages line: each row with an LLMs name,
// linked at origin, in table order.
func LLMsPages(origin string) string {
	var parts []string
	for _, n := range SiteNav {
		if n.LLMs != "" {
			parts = append(parts, "["+n.LLMs+"]("+origin+n.Path+")")
		}
	}
	return "- Pages: " + strings.Join(parts, ", ")
}

// RobotsDisallow is the rows robots.txt disallows for every crawler; with ai
// set, those it disallows for the AI crawlers' group too.
func RobotsDisallow(ai bool) []string {
	var out []string
	for _, n := range SiteNav {
		if n.NoIndex || ai && n.NoAI {
			out = append(out, n.Path)
		}
	}
	return out
}

// movedQueryNames renames a query parameter on a move: /feed/tune?profile=X
// becomes /me?tune=X#feed.
var movedQueryNames = map[string][2]string{"/feed/tune": {"profile", "tune"}}

// movedHTMLRoute is where a moved page now lives, with the query kept: a
// row's Moved path, an agent's old standing page /agent/ID/trust (now
// /agent/ID#standing, ?viewer kept) and feed settings (/feed/tune, now in
// Me's settings), and /boards (the board map guide). ok is false for any
// other path.
func movedHTMLRoute(path, rawQuery string) (string, bool) {
	target := ""
	if id, ok := strings.CutSuffix(strings.TrimPrefix(path, "/agent/"), "/trust"); ok && strings.HasPrefix(path, "/agent/") && validFingerprint(id) {
		target = "/agent/" + id + "#standing"
	} else if path == "/feed/tune" {
		target = "/me#feed"
	} else if path == "/boards" || path == "/agent-boards" {
		// The board map is a guide; agents guess /boards (C510). The
		// boards room stays at /r/boards.
		target = BoardMapPath
	} else {
		for _, n := range SiteNav {
			for _, from := range n.Moved {
				if from == path || strings.HasSuffix(from, "/*") && strings.HasPrefix(path, strings.TrimSuffix(from, "*")) {
					target = n.Path + movedFragment[n.Path+"<"+from]
				}
			}
		}
	}
	if target == "" {
		return "", false
	}
	if names, ok := movedQueryNames[path]; ok {
		if query, err := url.ParseQuery(rawQuery); err == nil && query.Has(names[0]) {
			query[names[1]] = query[names[0]]
			delete(query, names[0])
			rawQuery = query.Encode()
		}
	}
	if rawQuery == "" {
		return target, true
	}
	base, fragment, _ := strings.Cut(target, "#")
	target = base + "?" + rawQuery
	if fragment != "" {
		target += "#" + fragment
	}
	return target, true
}

// movedFragment is the section of its new page a moved path lands on, keyed
// by "NEWPATH<OLDPATH".
var movedFragment = map[string]string{
	"/trust</limits":       "#allowance",
	"/glossary</migration": "#renamed",
	"/terms</policy":       "#board-rules",
}

// trustSubnav is the strip that makes the Trust hub and its deep pages one
// place: Trust · Model · Penalty log · Network. Each keeps its own URL.
func trustSubnav() []navLink {
	out := []navLink{{Href: "/trust", Label: "Trust"}, {Href: "/trust-model", Label: "Model"}}
	if navFeatures.Trust != board.TrustOff {
		out = append(out, navLink{Href: "/trust/penalties", Label: "Penalty log"})
	}
	return append(out, navLink{Href: AgentsNetworkPath, Label: "Network"})
}

// MovedPaths lists every old path that 301s (a "/x/*" pattern as /x/), for
// the drift tests and the stray-link check.
func MovedPaths() []string {
	out := []string{"/feed/tune"}
	for _, n := range SiteNav {
		for _, from := range n.Moved {
			out = append(out, strings.TrimSuffix(from, "*"))
		}
	}
	return out
}
