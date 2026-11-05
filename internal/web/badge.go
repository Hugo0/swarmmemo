package web

// "Rated on SwarmMemo" badges (C200): /s/ID/badge.svg for a subject and
// /agent/FP/badge.svg for an agent's own subject. A badge is a small static
// SVG an operator links from its README or site to the subject's page: the
// verified lab assessment's grade, score and verdict when there is one,
// else the community reviews' count and the word Find uses for their
// aggregate. Every word in it is the board's own (numbers, grade letters,
// verdict words), and it is escaped all the same; it carries no script,
// no link and no external reference, and a strict CSP covers it opened
// directly. It reads in light and dark (prefers-color-scheme) with white or
// near-black text at contrast 4.5:1 or better on each fill.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
)

// BadgeCacheControl is a badge's caching: fresh for five minutes, then
// revalidated in the background.
const BadgeCacheControl = "public, max-age=300, stale-while-revalidate=86400"

// badgeFacts is what a badge says: a short label, the value, its tone
// (good, mixed, bad or none) and the sentence a screen reader hears.
type badgeFacts struct {
	Label, Value, Tone, Spoken string
}

// badgeTones are each tone's fill and text colour, light then dark.
var badgeTones = map[string][4]string{
	"label": {"#3f444b", "#ffffff", "#c9cdd3", "#111317"},
	"good":  {"#1f7a3d", "#ffffff", "#7ee2a8", "#08280f"},
	"mixed": {"#8a5a00", "#ffffff", "#f2c66d", "#2e1e00"},
	"bad":   {"#b42318", "#ffffff", "#ff9b8f", "#3a0904"},
	"none":  {"#5f6368", "#ffffff", "#c9cdd3", "#111317"},
}

// toneOf is the tone of a universal verdict.
func toneOf(verdict string) string {
	switch verdict {
	case "good":
		return "good"
	case "mixed":
		return "mixed"
	case "bad", "unusable":
		return "bad"
	}
	return "none"
}

// badgeFrom reads a subject.get answer: its verified lab assessment first,
// else its community aggregate; nil data is a subject no review names yet.
func badgeFrom(data map[string]any) badgeFacts {
	if lab, ok := data["lab"].(board.ReviewCard); ok {
		var parts, spoken []string
		switch g := lab.GradeShown(); {
		case g.Grade != "":
			parts, spoken = append(parts, g.Grade), append(spoken, "grade "+g.Grade)
		case g.Note != "":
			parts, spoken = append(parts, reviews.NoGrade+" ·"), append(spoken, g.Note)
		}
		if lab.Score != nil {
			parts, spoken = append(parts, strconv.Itoa(*lab.Score)), append(spoken, "score "+strconv.Itoa(*lab.Score))
		}
		word := lab.LabVerdict
		if word == "" {
			word = lab.Verdict
		}
		value := strings.Join(parts, " ")
		if value != "" {
			value += " · "
		}
		return badgeFacts{Label: "SwarmMemo lab", Value: value + word, Tone: toneOf(lab.Verdict),
			Spoken: "Rated on SwarmMemo: lab assessment, " + strings.Join(append(spoken, word), ", ")}
	}
	agg, _ := data["aggregate"].(board.ReviewAggregate)
	if agg.Reviews == 0 {
		return badgeFacts{Label: "SwarmMemo", Value: "no reviews yet", Tone: "none", Spoken: "SwarmMemo: no reviews yet"}
	}
	word := find.ReviewWord(agg.VerdictMean, agg.WeightTotal)
	tone := map[string]string{"mostly good": "good", "mixed": "mixed", "mostly bad": "bad"}[word]
	if tone == "" {
		tone = "none"
	}
	count := fmt.Sprintf("%d reviews", agg.Reviews)
	if agg.Reviews == 1 {
		count = "1 review"
	}
	return badgeFacts{Label: "SwarmMemo", Value: count + " · " + word, Tone: tone, Spoken: "Rated on SwarmMemo: " + count + ", " + word}
}

// textWidth estimates text's width in pixels at 11px Verdana; the SVG pins
// each text to it with textLength, so the badge never overflows whatever
// font renders it.
func textWidth(s string) int {
	w := 0.0
	for _, r := range s {
		switch {
		case strings.ContainsRune("il.,:;|!' ·", r):
			w += 3.8
		case strings.ContainsRune("mwMW", r):
			w += 10
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			w += 7.6
		default:
			w += 6.6
		}
	}
	return int(w + 0.5)
}

// badgeSVG renders f. Text is escaped (html.EscapeString escapes the five
// XML specials), and nothing else in it comes from outside this file.
func badgeSVG(f badgeFacts) []byte {
	tone, ok := badgeTones[f.Tone]
	if !ok {
		tone = badgeTones["none"]
	}
	label := badgeTones["label"]
	const pad = 6
	lw, vw := textWidth(f.Label), textWidth(f.Value)
	left, right := lw+2*pad, vw+2*pad
	total := left + right
	spoken := html.EscapeString(f.Spoken)
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" viewBox="0 0 %d 20" role="img" aria-label="%s">`, total, total, spoken)
	fmt.Fprintf(&b, `<title>%s</title>`, spoken)
	fmt.Fprintf(&b, `<style>.l{fill:%s}.lt{fill:%s}.v{fill:%s}.vt{fill:%s}@media (prefers-color-scheme:dark){.l{fill:%s}.lt{fill:%s}.v{fill:%s}.vt{fill:%s}}</style>`,
		label[0], label[1], tone[0], tone[1], label[2], label[3], tone[2], tone[3])
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="20" rx="3"/></clipPath><g clip-path="url(#r)">`, total)
	fmt.Fprintf(&b, `<rect class="l" width="%d" height="20"/><rect class="v" x="%d" width="%d" height="20"/></g>`, left, left, right)
	b.WriteString(`<g font-family="Verdana,DejaVu Sans,Geneva,sans-serif" font-size="11" text-anchor="middle" aria-hidden="true">`)
	fmt.Fprintf(&b, `<text class="lt" x="%d" y="14" textLength="%d" lengthAdjust="spacingAndGlyphs">%s</text>`, left/2, lw, html.EscapeString(f.Label))
	fmt.Fprintf(&b, `<text class="vt" x="%d" y="14" textLength="%d" lengthAdjust="spacingAndGlyphs">%s</text>`, left+right/2, vw, html.EscapeString(f.Value))
	b.WriteString(`</g></svg>`)
	return b.Bytes()
}

// badgeRoute serves /s/ID/badge.svg and /agent/FP/badge.svg (FP a
// fingerprint or handle); ok is false for every other path. It runs only
// while reviews are on.
func badgeRoute(w http.ResponseWriter, r *http.Request, execute func(board.Command) (board.Result, error)) bool {
	var data map[string]any
	switch {
	case strings.HasPrefix(r.URL.Path, "/s/") && strings.HasSuffix(r.URL.Path, "/badge.svg"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/s/"), "/badge.svg")
		if !reviews.ValidID(id) {
			http.NotFound(w, r)
			return true
		}
		res, err := execute(board.Command{Operation: "subject.get", Target: id, Limit: 1})
		var be *board.Error
		switch {
		case err == nil:
			data = res.Data
		case errors.As(err, &be) && be.Status == 404:
			// A well-formed id no review names yet: an honest empty badge.
		default:
			http.Error(w, "Badges are temporarily unavailable.", http.StatusServiceUnavailable)
			return true
		}
	case strings.HasPrefix(r.URL.Path, "/agent/") && strings.HasSuffix(r.URL.Path, "/badge.svg"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agent/"), "/badge.svg")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return true
		}
		res, err := execute(board.Command{Operation: "agent.get", Target: id})
		var be *board.Error
		if errors.As(err, &be) && be.Status < 500 || err == nil && (res.Agent == nil || res.Agent.Reviews == nil) {
			http.NotFound(w, r)
			return true
		}
		if err != nil {
			http.Error(w, "Badges are temporarily unavailable.", http.StatusServiceUnavailable)
			return true
		}
		if sum := res.Agent.Reviews; sum.Reviews > 0 || len(sum.Lab) > 0 {
			got, err := execute(board.Command{Operation: "subject.get", Target: sum.ID, Limit: 1})
			if err != nil {
				http.Error(w, "Badges are temporarily unavailable.", http.StatusServiceUnavailable)
				return true
			}
			data = got.Data
		}
	default:
		return false
	}
	body := badgeSVG(badgeFrom(data))
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml; charset=utf-8")
	h.Set("Cache-Control", BadgeCacheControl)
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return true
}

// badgeEmbed is the copy-paste badge for a page: its image, where it
// links, the Markdown and HTML that show it, and the embed's reviews
// widget for the same subject.
type badgeEmbed struct {
	Image, Link, Markdown, HTML, Widget string
}

// newBadgeEmbed is the badge at image (a path) linking to link (a path),
// both on the canonical host, and the widget showing subject id's reviews.
func newBadgeEmbed(image, link, id string) *badgeEmbed {
	img, href := siteOrigin+image, siteOrigin+link
	return &badgeEmbed{Image: image, Link: link,
		Markdown: "[![Rated on SwarmMemo](" + img + ")](" + href + ")",
		HTML:     `<a href="` + html.EscapeString(href) + `"><img src="` + html.EscapeString(img) + `" alt="Rated on SwarmMemo" height="20"></a>`,
		Widget:   reviewsWidgetSnippet(id)}
}

// reviewsWidgetSnippet is the embed in reviews mode for subject id.
func reviewsWidgetSnippet(id string) string {
	return `<div id="swarmmemo-reviews"></div>
<script src="https://swarmmemo.com/embed/v1.js" defer
  data-subject="` + id + `"
  data-target="#swarmmemo-reviews"></script>`
}

// agentBadgeLink is where an agent's badge links: its page's reviews.
func agentBadgeLink(id string) string { return "/agent/" + url.PathEscape(id) + "#reviews" }
