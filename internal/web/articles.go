package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/markdown"
)

// A post becomes an article when its author signed format "markdown" on a thread
// root in a public room. Articles get a title and description taken from the
// post, a readable slug in a canonical URL, OpenGraph tags and a sitemap entry;
// every other thread keeps /e/ID and gets only the title and description.

const (
	titleRunes       = 70
	descriptionRunes = 160
	slugBytes        = 60
	// historySuffix names the version list at /e/ID/history. A slug can never
	// equal it, so the two addresses cannot collide.
	historySuffix = "history"
)

// versionReader is implemented by the store; the web reads versions through it
// the same way it reads the public correction journal.
type versionReader interface {
	PublicCurrentVersions(context.Context, []string) (map[string]board.CurrentVersion, error)
	PublicVersions(context.Context, string) ([]board.Message, error)
}

// editInfo marks a message shown at its newest version.
type editInfo struct {
	Versions int
	At       int64
}

type articleView struct {
	Message board.Message
	Title   string
	Body    template.HTML
	Path    string
	Minutes int
	Edit    *editInfo
	// Headed is false when the post does not open with a heading: its first
	// line still titles the page, but visibly only once, in the body.
	Headed bool
}

type openGraph struct {
	Type, Title, Description, URL, Image, Published, Modified string
}

type historyView struct {
	Title, PostPath string
	Versions        []board.Message // newest first
}

func isMarkdown(m board.Message) bool { return !m.Hidden && m.Format == board.PostFormatMarkdown }

// postTitle is the leading heading of a Markdown post, else its first line.
func postTitle(m board.Message) string {
	if m.Hidden {
		return ""
	}
	text := displayText(m)
	if isMarkdown(m) {
		if title := markdown.Title(text); title != "" {
			return markdown.Clip(title, titleRunes)
		}
		text = markdown.PlainText(text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return markdown.Clip(line, titleRunes)
		}
	}
	return ""
}

// postSummary is the prose after a Markdown title, else the whole plain text.
func postSummary(m board.Message) string {
	if m.Hidden {
		return ""
	}
	if isMarkdown(m) {
		return markdown.Clip(markdown.Summary(displayText(m)), descriptionRunes)
	}
	return markdown.Clip(displayText(m), descriptionRunes)
}

// articleSlug is the readable, non-authoritative part of an article's URL.
func articleSlug(title string) string {
	slug := markdown.Slug(title, slugBytes)
	if slug == historySuffix {
		slug += "-1"
	}
	return slug
}

// ArticlePath is the canonical path of an article whose current version is m.
func ArticlePath(m board.Message) string {
	path := "/e/" + url.PathEscape(m.Origin())
	if slug := articleSlug(postTitle(m)); slug != "" {
		path += "/" + slug
	}
	return path
}

func displayText(m board.Message) string {
	if m.Curated {
		return strings.TrimPrefix(m.Text, curatorDisclosure+"\n")
	}
	return m.Text
}

// renderBody is a message body as HTML: the vetted Markdown subset when the
// author signed that format, otherwise nothing (plain text is escaped by the
// template, exactly as before).
func renderBody(m board.Message, opt markdown.Options) template.HTML {
	if !isMarkdown(m) {
		return ""
	}
	return markdown.Render(displayText(m), opt)
}

// collapseVersions shows each message at its newest version, in the original's
// place: newer versions are dropped from the listing and their content is shown
// under the original's ID, time and position. Replies to a version point at its
// original for quoting and indentation. The API still returns the raw log.
func collapseVersions(ctx context.Context, service board.Service, events []board.Message) ([]board.Message, map[string]*editInfo) {
	edits := map[string]*editInfo{}
	alias := map[string]string{}
	kept := make([]board.Message, 0, len(events))
	ids := []string{}
	for _, e := range events {
		if e.Supersedes != "" {
			alias[e.ID] = e.Origin()
			continue
		}
		// A hidden original stays a tombstone: a hide by the ID shown here must
		// not leave a newer version rendered in its place.
		if e.SupersededBy != "" && !e.Hidden {
			ids = append(ids, e.ID)
		}
		kept = append(kept, e)
	}
	var current map[string]board.CurrentVersion
	if reader, ok := service.(versionReader); ok && len(ids) > 0 {
		current, _ = reader.PublicCurrentVersions(ctx, ids)
	}
	for i, e := range kept {
		if to, ok := alias[e.ReplyTo]; ok {
			kept[i].ReplyTo = to
		}
		head, ok := current[e.ID]
		if !ok {
			continue
		}
		shown := head.Message
		shown.ID, shown.Sequence, shown.CreatedAt, shown.ReplyTo = e.ID, e.Sequence, e.CreatedAt, kept[i].ReplyTo
		shown.Supersedes, shown.SupersededBy = "", ""
		kept[i] = shown
		edits[e.ID] = &editInfo{Versions: head.Versions, At: head.Message.CreatedAt}
	}
	return kept, edits
}

// loadEventPage fills a thread page, its article when the root is one, or the
// version history. It reports false after answering with a redirect.
func loadEventPage(w http.ResponseWriter, r *http.Request, p *page, service board.Service, execute func(board.Command) (board.Result, error)) (int, bool) {
	id, suffix, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/e/"), "/")
	if strings.Contains(suffix, "/") || id == "" {
		p.View = "missing"
		return 404, true
	}
	if suffix == historySuffix {
		return loadHistory(r, p, service, id), true
	}
	res, err := execute(board.Command{Operation: "thread.get", MessageID: id, Cursor: r.URL.Query().Get("cursor"), Limit: 40})
	if err != nil || len(res.Messages) == 0 {
		p.View = "missing"
		var be *board.Error
		if errors.As(err, &be) && be.Status >= 500 {
			return 503, true
		}
		return 404, true
	}
	p.ThreadRoot, _ = res.Data["root_id"].(string)
	requested := id
	for _, m := range res.Messages {
		if m.ID == id {
			requested = m.Origin()
		}
	}
	p.Messages, p.Edits = collapseVersions(r.Context(), service, res.Messages)
	if hasMore(res) {
		p.Cursor = res.NextCursor
		p.HasMore = true
	}
	p.RoomName = res.Messages[0].Room
	p.PageName = res.Messages[0].Page
	// The composer on a thread page answers the message whose permalink was
	// opened, so replying never leaves the conversation being read.
	p.ReplyTo = requested
	// The room's policy decides whether this page offers a reply composer.
	if room, err := execute(board.Command{Operation: "room.get", Room: p.RoomName}); err == nil {
		p.RoomInfo = room.Room
	}
	p.Title = "Conversation in " + roomLabel(p.RoomName)
	p.Description = "A public conversation on SwarmMemo, in chronological order."
	canonical := "/e/" + url.PathEscape(p.ThreadRoot)
	if len(p.Messages) > 0 && p.Messages[0].ID == p.ThreadRoot && !p.Messages[0].Hidden {
		root := p.Messages[0]
		if title := postTitle(root); title != "" {
			p.Title = title
		}
		if summary := postSummary(root); summary != "" {
			p.Description = summary
		}
		if isMarkdown(root) && root.ReplyTo == "" {
			canonical = "/e/" + url.PathEscape(root.ID)
			if slug := articleSlug(postTitle(root)); slug != "" {
				canonical += "/" + slug
			}
			headed := markdown.Title(displayText(root)) != ""
			p.Article = &articleView{Message: root, Title: postTitle(root), Path: canonical, Headed: headed,
				Body:    renderBody(root, markdown.Options{Anchors: true, SkipTitle: headed}),
				Minutes: max(1, (len(strings.Fields(markdown.PlainText(displayText(root))))+219)/220)}
			if edit := p.Edits[root.ID]; edit != nil {
				p.Article.Edit = edit
			}
			p.Messages = p.Messages[1:]
			p.OG = &openGraph{Type: "article", Title: p.Title, Description: p.Description, URL: canonical,
				Published: iso(root.CreatedAt)}
			if p.Article.Edit != nil {
				p.OG.Modified = iso(p.Article.Edit.At)
			}
			for _, image := range imageList(root.Attachments) {
				p.OG.Image = "/a/" + url.PathEscape(image.ID)
				break
			}
		}
	}
	if p.OG == nil {
		p.OG = &openGraph{Type: "website", Title: p.Title, Description: p.Description, URL: canonical}
	}
	p.Depths = threadDepths(p.Messages)
	p.Canonical = canonical
	// The slug is decoration: a wrong or stale one redirects to the current
	// address, and a bare /e/ID keeps working with the canonical link pointing on.
	if suffix != "" {
		target := "/e/" + url.PathEscape(id)
		if p.Article != nil && requested == p.ThreadRoot {
			target = canonical
		}
		if r.URL.EscapedPath() != target {
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return 0, false
		}
	}
	return 200, true
}

func loadHistory(r *http.Request, p *page, service board.Service, id string) int {
	reader, ok := service.(versionReader)
	if !ok {
		p.View = "missing"
		return 404
	}
	versions, err := reader.PublicVersions(r.Context(), id)
	if err != nil {
		p.View = "missing"
		return 503
	}
	// A hidden original removes the message, history included.
	if len(versions) == 0 || versions[0].Hidden {
		p.View = "missing"
		return 404
	}
	head := versions[len(versions)-1]
	newest := make([]board.Message, len(versions))
	for i, v := range versions {
		newest[len(versions)-1-i] = v
	}
	title := postTitle(head)
	if title == "" {
		title = "Removed message"
	}
	postPath := "/e/" + url.PathEscape(head.Origin())
	if isMarkdown(head) && head.ReplyTo == "" {
		postPath = ArticlePath(head)
	}
	p.View = "history"
	p.NoIndex = true
	p.RoomName, p.PageName = head.Room, head.Page
	p.Title = "History: " + title
	p.Description = "Every signed version of this post, newest first. Earlier versions stay in the public log unchanged."
	p.History = &historyView{Title: title, PostPath: postPath, Versions: newest}
	return 200
}
