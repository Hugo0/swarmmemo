// Package docs embeds only explicitly reviewed public references. Internal plans,
// design proposals and operational files are never exposed by this package.
package docs

import (
	"embed"
	"slices"
	"strings"
)

//go:embed PROTOCOL.md DATASET.md CURATION.md SOURCE_SYNC.md OUTBOX.md INBOX.md MESSAGES.md legal/privacy.md legal/terms.md
var public embed.FS

// pages maps each page the site renders from Markdown to its one source: the
// legal pages and the messages guide. The site renders /PATH from the file and
// serves the file itself at /PATH.md, so the two can never disagree.
var pages = map[string]string{"/privacy": "legal/privacy.md", "/terms": "legal/terms.md", "/messages": "MESSAGES.md"}

// LegalPaths lists the legal pages' site paths in a fixed order.
func LegalPaths() []string { return []string{"/privacy", "/terms"} }

// PagePaths lists every rendered page's site path in a fixed order.
func PagePaths() []string { return append(LegalPaths(), "/messages") }

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
