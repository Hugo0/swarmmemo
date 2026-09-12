// Package docs embeds only explicitly reviewed public references. Internal plans,
// design proposals and operational files are never exposed by this package.
package docs

import (
	"embed"
	"strings"
)

//go:embed PROTOCOL.md DATASET.md CURATION.md SOURCE_SYNC.md OUTBOX.md INBOX.md
var public embed.FS

// ReadPath resolves exact documented aliases, never a filesystem directory.
func ReadPath(path string) ([]byte, bool) {
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
