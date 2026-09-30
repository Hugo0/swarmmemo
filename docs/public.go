// Package docs embeds only explicitly reviewed public references. Internal plans,
// design proposals and operational files are never exposed by this package.
package docs

import (
	"embed"
	"strings"
)

//go:embed PROTOCOL.md DATASET.md CURATION.md SOURCE_SYNC.md OUTBOX.md INBOX.md legal/privacy.md legal/terms.md
var public embed.FS

// legalPages maps each legal page's site path to its one Markdown source. The
// site renders /privacy and /terms from these files and serves the files
// themselves at /privacy.md and /terms.md, so the two can never disagree.
var legalPages = map[string]string{"/privacy": "legal/privacy.md", "/terms": "legal/terms.md"}

// LegalPaths lists the legal pages' site paths in a fixed order.
func LegalPaths() []string { return []string{"/privacy", "/terms"} }

// Legal returns the Markdown source of the legal page at path ("/privacy" or
// "/terms").
func Legal(path string) ([]byte, bool) {
	name, ok := legalPages[path]
	if !ok {
		return nil, false
	}
	content, err := public.ReadFile(name)
	return content, err == nil
}

// ReadPath resolves exact documented aliases, never a filesystem directory.
func ReadPath(path string) ([]byte, bool) {
	if page, ok := strings.CutSuffix(path, ".md"); ok && legalPages[page] != "" {
		return Legal(page)
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
		case "PROTOCOL.md", "DATASET.md", "CURATION.md", "SOURCE_SYNC.md", "OUTBOX.md", "INBOX.md":
			name = candidate
		}
	}
	if name == "" {
		return nil, false
	}
	content, err := public.ReadFile(name)
	return content, err == nil
}
