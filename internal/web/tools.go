package web

import (
	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// The tool pages (/tools and /tools/NAME): Markdown in docs, rendered like
// the guides with their FAQ as JSON-LD; /tools/fetch and /tools/receive add a
// small form a person can use (assets/tools.js). Each is served, listed in
// the sitemap and linked from /llms.txt only while its service is enabled
// (a Core page always, the top-up page while top-ups are on); the index,
// while any service's page is.

// ToolServed reports whether path is a tool page this deployment serves.
func ToolServed(f board.Features, path string) bool {
	service, ok := publicdocs.ToolPages[path]
	switch {
	case !ok:
		return false
	case service == publicdocs.Core:
		return true
	case service == publicdocs.Topup:
		return f.Topup
	case service == publicdocs.AllTools:
		return services.ToolsEnabled(f.Services)
	case service != "":
		return f.ServiceEnabled(service)
	}
	for _, s := range publicdocs.ToolPages {
		if s != "" && s != publicdocs.Core && (f.ServiceEnabled(s) || s == publicdocs.Topup && f.Topup) {
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

// ToolPageFor is the first tool page this deployment serves for service (a
// catalogue id, or publicdocs.Core); "" when there is none.
func ToolPageFor(f board.Features, service string) string {
	for _, path := range publicdocs.ToolPaths() {
		if publicdocs.ToolPages[path] == service && ToolServed(f, path) {
			return path
		}
	}
	return ""
}

// servedTool is path while this deployment serves that tool page, else "".
func servedTool(f board.Features, path string) string {
	if ToolServed(f, path) {
		return path
	}
	return ""
}

// isToolPath reports whether path is a tool page, served or not.
func isToolPath(path string) bool {
	_, ok := publicdocs.ToolPages[path]
	return ok
}

// JobServed reports whether path is a job page (docs/jobs.go) this
// deployment serves: one is served while the page that does its job is.
func JobServed(f board.Features, path string) bool {
	j, ok := publicdocs.JobFor(path)
	if !ok || j.Tool == "" {
		return false
	}
	return !isToolPath(j.Tool) || ToolServed(f, j.Tool)
}

// JobPaths are the job pages this deployment serves.
func JobPaths(f board.Features) []string {
	var out []string
	for _, path := range publicdocs.JobPaths() {
		if JobServed(f, path) {
			out = append(out, path)
		}
	}
	return out
}

// isJobPath reports whether path is a job page, served or not.
func isJobPath(path string) bool {
	j, ok := publicdocs.JobFor(path)
	return ok && j.Tool != ""
}
