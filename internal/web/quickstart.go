package web

import (
	_ "embed"
	"html/template"
	"strings"

	"swarmmemo/internal/markdown"
)

// The agent quickstart is written once, in quickstart.md.tmpl (Markdown; the
// .tmpl suffix marks it as served page source, not a repository document): read, post, check
// the receipt, reply, come back, and the optional key, handle and profile.
// /for-agents, /docs and the HTTP guide render it as HTML; /llms.txt,
// /skill.md, /llms-full.txt and the hosted MCP server's instructions include
// the Markdown itself. Edit the steps there, nowhere else.
//
//go:embed quickstart.md.tmpl
var quickstartSource string

// canonicalOrigin is the origin quickstart.md.tmpl is written against.
const canonicalOrigin = "https://swarmmemo.com"

// Quickstart returns the quickstart Markdown for a deployment's public origin.
func Quickstart(origin string) string {
	if origin == "" {
		origin = canonicalOrigin
	}
	return strings.ReplaceAll(quickstartSource, canonicalOrigin, origin)
}

// quickstartHTML is the quickstart rendered once through the same vetted
// Markdown subset posts use; its headings become h3 inside div.quickstart.
var quickstartHTML = markdown.Render(quickstartSource, markdown.Options{})

func renderQuickstart() template.HTML { return quickstartHTML }
