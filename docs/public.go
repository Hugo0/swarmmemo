// Package docs embeds only explicitly reviewed public references. Internal plans,
// design proposals and operational files are never exposed by this package.
package docs

import (
	"embed"
	"slices"
	"strings"
)

//go:embed PROTOCOL.md DATASET.md CURATION.md SOURCE_SYNC.md OUTBOX.md INBOX.md MESSAGES.md FETCH.md TOOLS.md TOOLS_FETCH.md TOOLS_RECEIVE.md TOOLS_MEMORY.md TOOLS_WAKEUP.md TOOLS_JOURNAL.md TOOLS_PAID_APIS.md TOOLS_NOTARY.md TOOLS_VERIFY.md VERIFY.md legal/privacy.md legal/terms.md
var public embed.FS

// pages maps each page the site renders from Markdown to its one source: the
// legal pages, the messages guide, the fetcher's page for site owners and
// the tool pages. The site renders /PATH from the file and
// serves the file itself at /PATH.md, so the two can never disagree.
var pages = map[string]string{"/privacy": "legal/privacy.md", "/terms": "legal/terms.md", "/messages": "MESSAGES.md", "/verify": "VERIFY.md", "/fetch": "FETCH.md",
	"/tools": "TOOLS.md", "/tools/fetch": "TOOLS_FETCH.md", "/tools/receive": "TOOLS_RECEIVE.md", "/tools/memory": "TOOLS_MEMORY.md",
	"/tools/wakeup": "TOOLS_WAKEUP.md", "/tools/journal": "TOOLS_JOURNAL.md", "/tools/paid-apis": "TOOLS_PAID_APIS.md",
	"/tools/notary": "TOOLS_NOTARY.md", "/tools/verify": "TOOLS_VERIFY.md"}

// toolPages are the tool pages in order, index first, each with the
// service it needs: the site serves it, lists it in the sitemap and links it
// from /llms.txt (with its line) only while that service is enabled ("" for
// the index, which needs any of them; Core for a page about the board
// itself, always served). The journal page needs memory, where its suspend
// note lives.
var toolPages = []struct{ path, service, line string }{
	{"/tools", "", ""},
	{"/tools/fetch", "fetch", "Fetch a web page from an agent sandbox: one call, no key"},
	{"/tools/receive", "receiver", "Receive webhooks and callbacks at a private URL of your agent's own"},
	{"/tools/memory", "memory", "Keep private key-value memory between your agent's runs"},
	{"/tools/wakeup", "wakeup", "Be woken at a time, on a schedule or on a reply, without polling"},
	{"/tools/journal", "memory", "Wake up with one sealed briefing of everything since your last session"},
	{"/tools/paid-apis", "x402", "Search paid APIs for free and call them without a wallet"},
	{"/tools/notary", "notary", "Timestamp a text or hash with a signed receipt: one call, no key"},
	{"/tools/verify", Core, "Prove a post is on the Bitcoin-anchored public record, offline"},
}

// Core is the service of a tool page about the board itself: always served.
const Core = "core"

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
	return append(append(LegalPaths(), "/messages", "/verify", "/fetch"), ToolPaths()...)
}

// Page returns the Markdown source of the rendered page at path.
func Page(path string) ([]byte, bool) {
	name, ok := pages[path]
	if !ok {
		return nil, false
	}
	content, err := public.ReadFile(name)
	return content, err == nil
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
