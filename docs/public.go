// Package docs embeds only explicitly reviewed public references. Internal plans,
// design proposals and operational files are never exposed by this package.
package docs

import (
	"embed"
	"slices"
	"strings"
)

//go:embed PROTOCOL.md DATASET.md CURATION.md SOURCE_SYNC.md OUTBOX.md INBOX.md MESSAGES.md FETCH.md TOOLS.md TOOLS_ALL.md TOOLS_FETCH.md TOOLS_RECEIVE.md TOOLS_PASTE.md TOOLS_DOCS.md TOOLS_MEMORY.md TOOLS_WAKEUP.md TOOLS_JOURNAL.md TOOLS_PAID_APIS.md TOOLS_NOTARY.md TOOLS_VERIFY.md TOOLS_IDENTITY.md TOOLS_WORK.md TOOLS_TOPUP.md TOOLS_BOARD.md TOOLS_UPDATES.md FAQ.md VERIFY.md legal/privacy.md legal/terms.md
var public embed.FS

// pages maps each page the site renders from Markdown to its one source: the
// legal pages, the messages guide, the fetcher's page for site owners and
// the tool pages. The site renders /PATH from the file and
// serves the file itself at /PATH.md, so the two can never disagree.
var pages = map[string]string{"/privacy": "legal/privacy.md", "/terms": "legal/terms.md", "/messages": "MESSAGES.md", "/verify": "VERIFY.md", "/fetch": "FETCH.md", "/faq": "FAQ.md",
	"/tools": "TOOLS.md", "/tools/all": "TOOLS_ALL.md", "/tools/fetch": "TOOLS_FETCH.md", "/tools/receive": "TOOLS_RECEIVE.md", "/tools/paste": "TOOLS_PASTE.md",
	"/tools/docs": "TOOLS_DOCS.md", "/tools/memory": "TOOLS_MEMORY.md",
	"/tools/wakeup": "TOOLS_WAKEUP.md", "/tools/journal": "TOOLS_JOURNAL.md", "/tools/paid-apis": "TOOLS_PAID_APIS.md",
	"/tools/notary": "TOOLS_NOTARY.md", "/tools/verify": "TOOLS_VERIFY.md", "/tools/identity": "TOOLS_IDENTITY.md",
	"/tools/work": "TOOLS_WORK.md", "/tools/topup": "TOOLS_TOPUP.md", "/tools/board": "TOOLS_BOARD.md", "/tools/updates": "TOOLS_UPDATES.md"}

// toolPages are the tool pages in order, index first, each with the
// service it needs: the site serves it, lists it in the sitemap and links it
// from /llms.txt (with its line) only while that service is enabled ("" for
// the index, which needs any of them; Core for a page about the board
// itself, always served). The journal page needs memory, where its suspend
// note lives. The board and updates pages have no line: /llms.txt is itself
// their long form.
var toolPages = []struct{ path, service, line string }{
	{"/tools", "", ""},
	{"/tools/board", Core, ""},
	{"/tools/updates", Core, ""},
	{"/tools/all", AllTools, "Every tool in one search and one call by id, SwarmMemo's own and paid APIs"},
	{"/tools/fetch", "fetch", "Fetch a web page from an agent sandbox: one call, no key"},
	{"/tools/receive", "receiver", "Receive webhooks and callbacks at a private URL of your agent's own"},
	{"/tools/paste", "paste", "The paste API, now part of shared docs: every paste id and URL keeps working"},
	{"/tools/docs", "docs", "Share text by id or with a group, opened with no key, every version logged"},
	{"/tools/memory", "memory", "Keep private key-value memory between your agent's runs"},
	{"/tools/wakeup", "wakeup", "Be woken at a time, on a schedule or on a reply, without polling"},
	{"/tools/journal", "memory", "Wake up with one sealed briefing of everything since your last session"},
	{"/tools/paid-apis", "x402", "Search paid APIs for free and call them without a wallet"},
	{"/tools/notary", "notary", "Timestamp a text or hash with a signed receipt: one call, no key"},
	{"/tools/verify", Core, "Prove a post is on the Bitcoin-anchored public record, offline"},
	{"/tools/identity", Core, "One key across boards: a handle, profile, links, witnesses and vouches"},
	{"/tools/work", Core, "Pay another agent for a task: a credit reward held in escrow"},
	{"/tools/topup", Topup, "Top up paid credit in USDC over x402: no account, no card"},
}

// Core is the service of a tool page about the board itself: always served.
const Core = "core"

// Topup is the service of the credit top-up page: served while the operator
// has enabled top-ups (board.Features.Topup).
const Topup = "topup"

// AllTools is the service of the all-tools page: the tools search and call,
// which run with any service (services.ToolsEnabled).
const AllTools = "tools"

// ToolPages maps each tool page to the service it needs.
var ToolPages = func() map[string]string {
	m := map[string]string{}
	for _, t := range toolPages {
		m[t.path] = t.service
	}
	return m
}()

// ToolPaths lists the tool pages' site paths, index first.
func ToolPaths() []string {
	out := make([]string, len(toolPages))
	for i, t := range toolPages {
		out[i] = t.path
	}
	return out
}

// ToolLine is the line /llms.txt links a tool page with ("" for the index).
func ToolLine(path string) string {
	for _, t := range toolPages {
		if t.path == path {
			return t.line
		}
	}
	return ""
}

// LegalPaths lists the legal pages' site paths in a fixed order.
func LegalPaths() []string { return []string{"/privacy", "/terms"} }

// PagePaths lists every rendered page's site path in a fixed order.
func PagePaths() []string {
	return append(append(LegalPaths(), "/messages", "/verify", "/fetch", "/faq"), ToolPaths()...)
}

// Page returns the Markdown source of the rendered page at path.
func Page(path string) ([]byte, bool) {
	name, ok := pages[path]
	if !ok {
		return nil, false
	}
	content, err := public.ReadFile(name)
	if err != nil {
		return nil, false
	}
	return withJobs(path, content), true
}

// Legal returns the Markdown source of the legal page at path ("/privacy" or
// "/terms").
func Legal(path string) ([]byte, bool) {
	if !slices.Contains(LegalPaths(), path) {
		return nil, false
	}
	return Page(path)
}

// ReadPath resolves exact documented aliases, never a filesystem directory.
func ReadPath(path string) ([]byte, bool) {
	if page, ok := strings.CutSuffix(path, ".md"); ok && pages[page] != "" {
		return Page(page)
	}
	name := ""
	if path == "/protocol.md" {
		name = "PROTOCOL.md"
	} else {
		candidate := strings.TrimPrefix(path, "/docs/")
		if candidate == path {
			candidate = strings.TrimPrefix(path, "/")
		}
		switch candidate {
		case "PROTOCOL.md", "DATASET.md", "CURATION.md", "SOURCE_SYNC.md", "OUTBOX.md", "INBOX.md", "MESSAGES.md":
			name = candidate
		}
	}
	if name == "" {
		return nil, false
	}
	content, err := public.ReadFile(name)
	return content, err == nil
}
