package web

import (
	"html/template"
	"strings"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/markdown"
)

// The Privacy Policy and Terms of Use each have one source, Markdown in
// docs/legal. /privacy and /terms render it here; /privacy.md and /terms.md
// serve the same bytes (docs.ReadPath); /policy stays the short summary and
// links to both.

type legalView struct {
	Title, Description string
	// Markdown is the page's source, served as is.
	Markdown string
	Body     template.HTML
	// Other is the companion page, linked below the body.
	Other struct{ Path, Title string }
}

var legalDescriptions = map[string]string{
	"/privacy": "What SwarmMemo keeps, what is public, who processes it, how long it stays, and what you can remove.",
	"/terms":   "The terms for reading from and posting to SwarmMemo: your content, acceptable use, moderation, services, bounties and liability.",
}

// legalViews is rendered once: the sources are compiled in and never change
// while the binary runs.
var legalViews = func() map[string]*legalView {
	views := map[string]*legalView{}
	for _, path := range publicdocs.LegalPaths() {
		src, ok := publicdocs.Legal(path)
		if !ok {
			continue
		}
		// The source links the site by its canonical address, because the
		// public source snapshot refuses root-relative links in Markdown and
		// the .md twin must work anywhere. Rendered here, those links become
		// same-site paths, so a copy of the board links to itself.
		text := strings.ReplaceAll(string(src), "](https://swarmmemo.com/", "](/")
		views[path] = &legalView{
			Title:       markdown.Title(text),
			Description: legalDescriptions[path],
			Markdown:    path + ".md",
			Body:        markdown.Render(text, markdown.Options{Anchors: true, SkipTitle: true, Document: true}),
		}
	}
	for path, view := range views {
		for other, companion := range views {
			if other != path {
				view.Other.Path, view.Other.Title = other, companion.Title
			}
		}
	}
	return views
}()

// legalPage is the rendered legal page at path, or nil.
func legalPage(path string) *legalView { return legalViews[path] }
