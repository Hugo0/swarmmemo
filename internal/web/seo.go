package web

import (
	"encoding/json"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/markdown"
)

// Search and link-preview metadata for indexable pages: a canonical address,
// OpenGraph and Twitter tags on every page, and schema.org JSON-LD where a page
// has a clear shape (the site on /, a forum posting on /e/ID, a collection on
// /r/ROOM). Everything is derived from what the page already shows; hidden
// messages, simulations and noindex pages get no structured data.
//
// JSON-LD is written as a data block, <script type="application/ld+json">,
// which browsers never execute, so the pages' script-src 'self' policy has
// nothing to allow. json.Marshal escapes <, > and & (and U+2028/U+2029), so
// no message text can close the element or start markup.

// siteOrigin is the canonical host, as the canonical link element has always
// named it: other hostnames serving these pages point search engines here.
const siteOrigin = "https://swarmmemo.com"

// defaultImage is the preview image for pages without one of their own: the
// 400x400 logo (link previews and directories need a raster image).
const defaultImage = "/assets/logo-400.png"

const (
	ldTextRunes    = 1000
	ldCommentRunes = 500
	ldComments     = 20
)

// finishMetadata fills the canonical path, preview tags and structured data
// once a page is loaded.
func finishMetadata(p *page, status int) {
	if p.Canonical == "" {
		p.Canonical = p.Path
		if p.ThreadRoot != "" {
			p.Canonical = "/e/" + url.PathEscape(p.ThreadRoot)
		}
	}
	if p.OG == nil {
		p.OG = &openGraph{Type: "website", Title: p.Title, Description: p.Description, URL: p.Canonical}
	}
	if p.OG.Image == "" {
		p.OG.Image, p.OG.Logo = defaultImage, true
	}
	if status == 200 && !p.NoIndex {
		p.StructuredData = structuredData(p)
	}
}

// ldAuthor is a message's author as schema.org expects it. schema.org has no
// type for a software agent and search engines accept only Person or
// Organization here; the name is the key's handle or nickname and claims
// nothing about who or what holds the key.
func ldAuthor(m board.Message) map[string]any {
	if m.PublicKey == "" {
		return map[string]any{"@type": "Person", "name": "Anonymous"}
	}
	name := m.Handle
	if name == "" {
		name = AgentNickname(m.Author)
	}
	author := map[string]any{"@type": "Person", "name": name}
	if validFingerprint(m.Author) {
		author["url"] = siteOrigin + "/agent/" + m.Author
	}
	return author
}

// ldText is a message body as plain text, collapsed and clipped.
func ldText(m board.Message, runes int) string {
	text := displayText(m)
	if isMarkdown(m) {
		text = markdown.PlainText(text)
	}
	return markdown.Clip(strings.Join(strings.Fields(text), " "), runes)
}

func ldBreadcrumbs(items ...[2]string) map[string]any {
	list := make([]map[string]any, 0, len(items))
	for i, item := range items {
		list = append(list, map[string]any{"@type": "ListItem", "position": i + 1, "name": item[0], "item": siteOrigin + item[1]})
	}
	return map[string]any{"@type": "BreadcrumbList", "itemListElement": list}
}

// messagePath is the canonical page of a thread root.
func messagePath(m board.Message) string {
	if isMarkdown(m) && m.ReplyTo == "" {
		return ArticlePath(m)
	}
	return "/e/" + url.PathEscape(m.ID)
}

func structuredData(p *page) template.JS {
	var graph []any
	switch p.View {
	case "home":
		graph = append(graph,
			map[string]any{"@type": "WebSite", "@id": siteOrigin + "/#website", "name": "SwarmMemo", "url": siteOrigin + "/", "description": p.Description,
				"publisher": map[string]any{"@id": siteOrigin + "/#organization"},
				"potentialAction": map[string]any{"@type": "SearchAction",
					"target":      map[string]any{"@type": "EntryPoint", "urlTemplate": siteOrigin + "/?q={search_term_string}"},
					"query-input": "required name=search_term_string"}},
			map[string]any{"@type": "Organization", "@id": siteOrigin + "/#organization", "name": "SwarmMemo", "url": siteOrigin + "/",
				"logo": siteOrigin + defaultImage, "sameAs": []string{"https://github.com/Hugo0/swarmmemo"}})
	case "event":
		root, replies := threadRoot(p)
		if root == nil || root.Hidden || root.Kind == "simulation" {
			return ""
		}
		posting := map[string]any{"@type": "DiscussionForumPosting", "@id": siteOrigin + p.Canonical,
			"url": siteOrigin + p.Canonical, "mainEntityOfPage": siteOrigin + p.Canonical,
			"headline": postTitle(*root), "text": ldText(*root, ldTextRunes), "author": ldAuthor(*root),
			"datePublished": iso(root.CreatedAt), "isPartOf": siteOrigin + roomURL(root.Room)}
		if edit := p.Edits[root.ID]; edit != nil {
			posting["dateModified"] = iso(edit.At)
		}
		comments := []map[string]any{}
		for _, reply := range replies {
			if reply.Hidden || reply.Kind == "simulation" || len(comments) == ldComments {
				continue
			}
			comments = append(comments, map[string]any{"@type": "Comment", "url": siteOrigin + p.Canonical + "#e-" + reply.ID,
				"text": ldText(reply, ldCommentRunes), "author": ldAuthor(reply), "datePublished": iso(reply.CreatedAt)})
		}
		if len(comments) > 0 {
			posting["comment"] = comments
		}
		// A count is stated only when the whole conversation is on this page.
		if !p.HasMore {
			posting["interactionStatistic"] = map[string]any{"@type": "InteractionCounter",
				"interactionType": "https://schema.org/CommentAction", "userInteractionCount": len(replies)}
		}
		graph = append(graph, posting, ldBreadcrumbs([2]string{"SwarmMemo", "/"}, [2]string{roomLabel(root.Room), roomURL(root.Room)}, [2]string{postTitle(*root), p.Canonical}))
	case "room":
		items := []map[string]any{}
		for _, m := range p.Messages {
			if m.ReplyTo != "" || m.Hidden || m.Kind == "simulation" {
				continue
			}
			items = append(items, map[string]any{"@type": "ListItem", "position": len(items) + 1, "url": siteOrigin + messagePath(m), "name": postTitle(m)})
		}
		collection := map[string]any{"@type": "CollectionPage", "@id": siteOrigin + p.Canonical, "url": siteOrigin + p.Canonical,
			"name": p.Title, "description": p.Description, "isPartOf": map[string]any{"@id": siteOrigin + "/#website"}}
		if len(items) > 0 {
			collection["mainEntity"] = map[string]any{"@type": "ItemList", "itemListElement": items}
		}
		graph = append(graph, collection, ldBreadcrumbs([2]string{"SwarmMemo", "/"}, [2]string{p.Title, p.Canonical}))
	}
	if len(graph) == 0 {
		return ""
	}
	raw, err := json.Marshal(map[string]any{"@context": "https://schema.org", "@graph": graph})
	if err != nil {
		return ""
	}
	// Marshal already escaped <, > and &; this is the only place a JSON-LD
	// block is made, and the escaping is what makes template.JS safe here.
	return template.JS(raw)
}

// threadRoot finds the conversation's root on an event page and the replies
// shown with it.
func threadRoot(p *page) (*board.Message, []board.Message) {
	if p.Article != nil {
		root := p.Article.Message
		return &root, p.Messages
	}
	for i := range p.Messages {
		if p.Messages[i].ID == p.ThreadRoot {
			return &p.Messages[i], append(append([]board.Message(nil), p.Messages[:i]...), p.Messages[i+1:]...)
		}
	}
	return nil, nil
}

// roomDescription is a room page's meta description: its size, freshness and,
// if the owner wrote rules, their opening words.
func roomDescription(room *board.Room) string {
	text := roomLabel(room.Name) + " on SwarmMemo, a public message board for AI agents: " + strconv.FormatInt(room.Count, 10) + " public messages"
	if room.Count == 1 {
		text = strings.TrimSuffix(text, "s")
	}
	if room.UpdatedAt > 0 && room.Count > 0 {
		text += ", newest " + time.Unix(room.UpdatedAt, 0).UTC().Format("2 Jan 2006")
	}
	text += "."
	if room.Policy != nil && strings.TrimSpace(room.Policy.Rules) != "" {
		text += " " + strings.Join(strings.Fields(room.Policy.Rules), " ")
	}
	return markdown.Clip(text, descriptionRunes)
}
