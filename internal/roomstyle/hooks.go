package roomstyle

import (
	"slices"
	"sort"
	"strings"
	"sync"
)

// Hooks is the theme API: the class names a room stylesheet may use, mapped to
// the classes the page renders. A room writes ".post" or ".byline"; the output
// names the page's own class. The public names stay stable when markup changes;
// only this map moves. Every other class on the page is reserved and a selector
// naming it is dropped. Adding a hook is a security decision: a hook on a trust
// element must also be pinned in internal/web/assets/style.css (@layer room-trust).
var Hooks = map[string]string{
	// Page chrome.
	"site-header": "topbar",
	"brand":       "brand",
	"nav":         "site-nav",
	"account":     "workspace-link",
	"main":        "shell",
	"footer":      "footer",
	// The room and its layout.
	"room-header":     "page-heading",
	"layout":          "board-grid",
	"feed-column":     "feed-column",
	"feed":            "feed",
	"thread":          "thread-feed",
	"sidebar":         "sidebar",
	"panel":           "panel",
	"section-heading": "section-heading",
	"composer":        "composer",
	"pagination":      "pagination",
	"button":          "button",
	// A post. post-body is the canvas root: everything inside it is the body zone.
	"post":         "memo",
	"post-meta":    "memo-meta",
	"post-body":    "room-body",
	"post-text":    "memo-text",
	"post-title":   "memo-title",
	"post-images":  "memo-images",
	"post-image":   "memo-image",
	"post-footer":  "memo-bottom",
	"post-actions": "memo-actions",
	"quote":        "memo-quote",
	"byline":       "author",
	"timestamp":    "memo-time",
	"kind":         "kind",
	"badge":        "badge",
	"reply-button": "reply-button",
	// Long-form posts.
	"article":        "post-article",
	"article-header": "article-header",
	"article-title":  "article-title",
	"article-byline": "article-byline",
	"markdown":       "md",
	"md-table":       "md-table",
	"md-left":        "md-left",
	"md-right":       "md-right",
	"md-center":      "md-center",
}

// BodyClass is the canvas root's class: selectors whose subject is at or below
// it are in the body zone.
const BodyClass = "room-body"

// BodyClasses are every class rendered inside a canvas. internal/web's canvas
// test holds the rendered page to this list plus BodyTrustClasses.
var BodyClasses = []string{BodyClass, "memo-text", "memo-title", "md", "article-body", "md-table", "md-left", "md-right", "md-center", "memo-images", "memo-image", "multi"}

// BodyTrustClasses are trust marks that must sit inside a body (the destination
// host beside a Markdown link). The site pins them with all:revert !important.
var BodyTrustClasses = []string{"md-host"}

// hookClass resolves a class a room wrote to the page class it names, or "".
// Both the public name and the page class are accepted, so sanitizing output
// again (which names page classes) returns it unchanged.
func hookClass(name string) string {
	if target, ok := Hooks[name]; ok {
		return target
	}
	for _, target := range Hooks {
		if target == name {
			return target
		}
	}
	return ""
}

// hookList is built once: a stylesheet of thousands of non-hook selectors
// otherwise re-sorted the hook names for each one (half the sanitizer's time,
// which every styled page view pays).
var hookList = sync.OnceValue(func() string {
	names := make([]string, 0, len(Hooks))
	for name := range Hooks {
		names = append(names, "."+name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
})

// Site design tokens. Colour tokens may be restyled in the page zone, but only
// to opaque literal colours; every other token (type scale, spacing, fonts) is
// the site's, because trust UI is laid out with it.
var (
	colorTokens = []string{"--ink", "--ink-2", "--muted", "--faint", "--line", "--line-strong", "--surface", "--surface-2", "--surface-3",
		"--field-border", "--field-border-strong", "--focus", "--primary-fill", "--primary-ink", "--primary-hover"}
	fixedTokens = []string{"--mono", "--sans", "--serif", "--t-meta", "--t-ui", "--t-body", "--t-heading", "--t-title", "--t-display",
		"--s-1", "--s-2", "--s-3", "--s-4", "--s-5", "--s-6", "--s-7", "--s-8", "--control-h", "--quiet-h", "--radius"}
)

// SiteTokens lists every custom property the site declares on :root, for the
// test that keeps this file in step with style.css.
func SiteTokens() []string { return slices.Concat(colorTokens, fixedTokens) }

// TrustFont is the one knob a room has over trust text: its family, chosen from
// the generic families only, so no room font can redraw a byline's glyphs.
const TrustFont = "--trust-font"

var genericFamilies = []string{"serif", "sans-serif", "monospace", "system-ui", "ui-serif", "ui-sans-serif", "ui-monospace", "ui-rounded", "cursive", "fantasy", "math"}
