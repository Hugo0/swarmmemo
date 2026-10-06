package web

import (
	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
)

// The tool pages (/tools and /tools/NAME): Markdown in docs, rendered like
// the guides with their FAQ as JSON-LD; /tools/fetch and /tools/receive add a
// small form a person can use (assets/tools.js). Each is served, listed in
// the sitemap and linked from /llms.txt only while its service is enabled
// (a Core page always); the index, while any service's page is.

// ToolServed reports whether path is a tool page this deployment serves.
func ToolServed(f board.Features, path string) bool {
	service, ok := publicdocs.ToolPages[path]
	switch {
	case !ok:
		return false
	case service == publicdocs.Core:
		return true
	case service != "":
		return f.ServiceEnabled(service)
	}
	for _, s := range publicdocs.ToolPages {
		if s != "" && s != publicdocs.Core && f.ServiceEnabled(s) {
			return true
		}
	}
	return false
}

// ToolPaths are the tool pages this deployment serves, index first.
func ToolPaths(f board.Features) []string {
	var out []string
	for _, path := range publicdocs.ToolPaths() {
		if ToolServed(f, path) {
			out = append(out, path)
		}
	}
	return out
}

// isToolPath reports whether path is a tool page, served or not.
func isToolPath(path string) bool {
	_, ok := publicdocs.ToolPages[path]
	return ok
}
