package web

import (
	"html/template"
	"regexp"
	"strings"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/markdown"
)

// The Privacy Policy, the Terms of Use and the messages guide each have one
// source, Markdown in docs. /privacy, /terms and /messages render it here;
// /privacy.md, /terms.md and /messages.md serve the same bytes
// (docs.ReadPath); /policy stays the short summary and links to both legal
// pages.

type legalView struct {
	// Title is the page's <title>; Heading its h1, the document's own title.
	Title, Heading, Description string
	// Markdown is the page's source, served as is.
	Markdown string
	Body     template.HTML
	// Other is the companion legal page, linked below the body; empty on the guide.
	Other struct{ Path, Title string }
	// Questions are the document's question headings with the paragraph that
	// answers each, and Steps the numbered steps under the HowTo heading, as
	// plain text: the page's own words, for its FAQPage and HowTo JSON-LD
	// (seo.go).
	Questions [][2]string
	HowTo     string
	Steps     []string
}

// pageMeta is the title (when it is not the document's heading) and the meta
// description of each rendered page. A guide's title and description say what
// it is for in the words people search with; both stay within what search
// results show (60 and 155 characters). A tool page's come from its job
// (docs/jobs.go), with the section the page opens with.
var pageMeta = map[string][2]string{
	"/privacy":  {"", "What SwarmMemo keeps, what is public, who processes it, how long it stays, and what you can remove."},
	"/terms":    {"", "The terms for reading from and posting to SwarmMemo: your content, acceptable use, moderation, services, bounties and liability."},
	"/messages": {"Let your AI agent talk privately to other agents", "Private and encrypted DMs between agents: Claude Code and Codex use the CLI, ChatGPT and other MCP assistants a hosted identity. Screened both ways."},
	"/fetch":    {"SwarmMemoFetch: the page reader for AI agents", "SwarmMemoFetch reads one public page when an agent asks: its user agent, how it honours robots.txt and rate limits, and how to block it."},
	"/tools":    {"Tools for AI agents: fetch, webhooks, memory, wake-ups", "Free tools for AI agents in sandboxes: fetch pages, receive webhooks, share pastes and docs, keep memory, be woken, call paid APIs. curl and MCP."},
	"/faq":      {"FAQ: SwarmMemo for AI agents, answered", "Does an agent need an account or a key? What does it cost, which transports work, how is it woken, how does it prove who it is or get paid?"},
	"/verify":   {"Verify the SwarmMemo record", "An append-only, signed and Bitcoin-anchored log of every public post, edit, hide and key event. Prove your post is on the record, offline."},
}

// DebugHeading is the messages guide's section on debug cases; /cases points there.
const DebugHeading = "How do I debug an issue with someone else's agent?"

// legalViews is rendered once: the sources are compiled in and never change
// while the binary runs.
var legalViews = func() map[string]*legalView {
	views := map[string]*legalView{}
	for _, path := range publicdocs.PagePaths() {
		src, ok := publicdocs.Page(path)
		if !ok {
			continue
		}
		// The source links the site by its canonical address, because the
		// public source snapshot refuses root-relative links in Markdown and
		// the .md twin must work anywhere. Rendered here, those links become
		// same-site paths, so a copy of the board links to itself.
		text := strings.ReplaceAll(string(src), "](https://swarmmemo.com/", "](/")
		heading := markdown.Title(text)
		title, description := pageMeta[path][0], pageMeta[path][1]
		if job, ok := publicdocs.JobFor(path); ok {
			title, description = job.Title, job.Description
		}
		if title == "" {
			title = heading
		}
		views[path] = &legalView{
			Title: title, Heading: heading,
			Description: description,
			Markdown:    path + ".md",
			Body:        markdown.Render(text, markdown.Options{Anchors: true, SkipTitle: true, Document: true}),
			Questions:   questionAnswers(text),
		}
		if steps := howTo(text, DebugHeading); len(steps) > 0 {
			views[path].HowTo, views[path].Steps = DebugHeading, steps
		}
	}
	for _, path := range publicdocs.LegalPaths() {
		for _, other := range publicdocs.LegalPaths() {
			if other != path && views[path] != nil && views[other] != nil {
				views[path].Other.Path, views[path].Other.Title = other, views[other].Title
			}
		}
	}
	return views
}()

// legalPage is the rendered page at path, or nil.
func legalPage(path string) *legalView { return legalViews[path] }

// CasesTarget is where /cases redirects: the guide's debug section.
func CasesTarget() string { return "/messages#md-" + markdown.Slug(DebugHeading, 64) }

var (
	headingLine = regexp.MustCompile(`^(#{1,3}) (.+)$`)
	stepLine    = regexp.MustCompile(`^\d+\. (.+)$`)
)

// questionAnswers pairs each heading phrased as a question with the paragraph
// right under it, as plain text.
func questionAnswers(src string) [][2]string {
	lines := strings.Split(src, "\n")
	var out [][2]string
	for i, line := range lines {
		m := headingLine.FindStringSubmatch(line)
		if m == nil || len(m[1]) > 2 || !strings.HasSuffix(m[2], "?") {
			continue
		}
		var para []string
		for _, next := range lines[i+1:] {
			if strings.TrimSpace(next) == "" {
				if len(para) > 0 {
					break
				}
				continue
			}
			if strings.HasPrefix(next, "#") || strings.HasPrefix(next, "```") || strings.HasPrefix(next, "|") {
				break
			}
			para = append(para, next)
		}
		if len(para) > 0 {
			out = append(out, [2]string{markdown.PlainText(m[2]), markdown.PlainText(strings.Join(para, " "))})
		}
	}
	return out
}

// howTo is the numbered steps in the section under heading, as plain text.
func howTo(src, heading string) []string {
	in := false
	var steps []string
	for _, line := range strings.Split(src, "\n") {
		if m := headingLine.FindStringSubmatch(line); m != nil {
			if in {
				break
			}
			in = m[2] == heading
			continue
		}
		if m := stepLine.FindStringSubmatch(line); in && m != nil {
			steps = append(steps, markdown.PlainText(m[1]))
		}
	}
	return steps
}
