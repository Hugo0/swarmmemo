package markdown

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Plain-text posts and listing previews. Both reuse the Markdown scanner's
// link rules, so there is one URL validator (SafeURL) and one measure of a
// bare URL (bareURL) for every post a page shows.

const (
	// maxBlankLines is the longest run of blank lines a plain-text post shows.
	maxBlankLines = 1
	// maxLabelPath bounds the part of a linked URL's label after its host.
	maxLabelPath = 36
	// codeHintRunes bounds the first line of a code block in a preview.
	codeHintRunes = 32
)

// Text renders a plain-text post: every byte escaped, http(s) URLs and
// same-site references (/e/ID, /r/ROOM, /agent/FINGERPRINT) linked exactly as
// in a Markdown post, and code shown as code: a fenced block (```lang, read by
// the Markdown scanner's fence) is a code block and `inline` code is code.
// Nothing else is interpreted, so * and # mean what they say, and a link label
// or a heading stays Markdown's, which a post opts into. A post that is one
// JSON object or array is shown indented (PrettyJSON). Line breaks stay text
// for the page's white-space rule, and a run of blank lines is shortened to
// maxBlankLines.
func Text(src string) template.HTML { return TextMentions(src, nil) }

// TextMentions is Text with the post's known @handle mentions linked
// (Options.Mentions).
func TextMentions(src string, mentions map[string]string) template.HTML {
	if pretty, ok := PrettyJSON(src); ok {
		return pretty
	}
	r := newRenderer(false, Options{Mentions: mentions})
	lines, blank := splitLines(src), 0
	// sep is owed before the next line of text: none at the start, or after a
	// code block, which is a block of its own.
	sep := false
	for k := 0; k < len(lines); {
		if lang, body, next, ok := fence(lines, k); ok {
			r.out.WriteString(codeOpen(lang))
			r.escape(body)
			r.out.WriteString("</code></pre>")
			k, blank, sep = next, 0, false
			continue
		}
		line := lines[k]
		if isBlank(line) {
			// Blank lines beside a code block are the block's margin, not
			// text. A run is measured once, at its first line.
			if blank == 0 {
				end := k
				for end < len(lines) && isBlank(lines[end]) {
					end++
				}
				if afterCode := k > 0 && !sep; afterCode || (end < len(lines) && isFence(lines[end])) {
					k = end
					continue
				}
			}
			blank++
		} else {
			blank = 0
		}
		// The text after the final line break is not a blank line of its own:
		// a post ending in one keeps it.
		if blank > maxBlankLines && k < len(lines)-1 {
			k++
			continue
		}
		if sep {
			r.out.WriteByte('\n')
		}
		r.plainLine(line)
		k, sep = k+1, true
	}
	return template.HTML(r.out.String()) // #nosec: built only from escaped text and fixed tags
}

func isFence(line string) bool {
	_, ok := fenceOpen(line)
	return ok
}

// plainLine writes one line of a plain-text post: a `code` span, found by the
// Markdown scanner's backtick index, as code, and the rest linked.
func (r *renderer) plainLine(s string) {
	if strings.IndexByte(s, '`') < 0 {
		r.linkify(s)
		return
	}
	ticks, start := backtickRuns(s), 0
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\':
			// An escaped backtick opens nothing, as in the Markdown scanner.
			i += 2
			continue
		case s[i] != '`':
			i++
			continue
		}
		n := runLength(s, i, '`')
		if end := ticks.next(n, i+n); end >= 0 {
			r.linkify(s[start:i])
			r.out.WriteString("<code>")
			r.escape(codeSpan(s[i+n : end]))
			r.out.WriteString("</code>")
			start = end + n
			i = start
			continue
		}
		i += n
	}
	r.linkify(s[start:])
}

// Pretty JSON bounds: the post's size, and its nesting, which indentation
// multiplies into output.
const (
	maxPrettyJSON  = 16 << 10
	maxPrettyDepth = 16
)

// PrettyJSON shows a post that is one JSON object or array, at most
// maxPrettyJSON bytes and maxPrettyDepth deep, as an indented JSON code block.
// The author's text is kept exactly in data-copy-value, for the copy button;
// the API serves it unchanged. json.Indent keeps every token as written.
func PrettyJSON(src string) (template.HTML, bool) {
	t := strings.TrimSpace(src)
	if len(t) > maxPrettyJSON || t == "" || (t[0] != '{' && t[0] != '[') || jsonDepth(t) > maxPrettyDepth || !json.Valid([]byte(t)) {
		return "", false
	}
	var out bytes.Buffer
	if json.Indent(&out, []byte(t), "", "  ") != nil {
		return "", false
	}
	// HTML parsing turns a literal CR into LF; a character reference survives.
	copyValue := strings.ReplaceAll(template.HTMLEscapeString(src), "\r", "&#13;")
	return template.HTML(`<pre><code data-lang="json" data-copy-value="` + copyValue + `">` + // #nosec: escaped text and fixed tags
		template.HTMLEscapeString(out.String()) + `</code></pre>`), true
}

// jsonDepth is the deepest bracket nesting in s, outside strings, counted in
// one pass. It trusts nothing: json.Valid checks the text after it.
func jsonDepth(s string) int {
	depth, deepest, quoted := 0, 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case c == '}' || c == ']':
			depth--
		}
	}
	return deepest
}

// linkify writes one line of plain text. A URL-shaped run that is refused
// stays text and is not scanned again, so a line costs one pass.
func (r *renderer) linkify(s string) {
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == '@' {
			if t, n := r.mentionLink(s, i); n > 0 {
				if t.kind == tLink {
					r.escape(s[start:i])
					r.writeMention(&t)
					start = i + n
				}
				i += n
				continue
			}
		}
		if c != 'h' && c != 'H' && c != '/' {
			i++
			continue
		}
		t, n := autolink(s, i)
		if n == 0 {
			i++
			continue
		}
		if t.kind == tLink {
			r.escape(s[start:i])
			r.link(&t)
			start = i + n
		}
		i += n
	}
	r.escape(s[start:])
}

// autolink reads a link written as bare text at s[i]: an http(s) URL, or a
// same-site reference. n is the length of the URL-shaped run, 0 if there is
// none; t is a link token only if SafeURL accepts the run.
func autolink(s string, i int) (t token, n int) {
	external := s[i] != '/'
	if external {
		if i > 0 && isWord(prevRune(s, i)) {
			return t, 0
		}
		n = bareURL(s[i:])
	} else if refBoundary(s, i) {
		n = siteRef(s[i:])
	}
	if n == 0 {
		return t, 0
	}
	if href, host, ok := SafeURL(s[i : i+n]); ok && (host != "") == external {
		t = token{kind: tLink, text: s[i : i+n], href: href, host: host, auto: true}
	}
	return t, n
}

// refBoundary reports whether a same-site reference may start at s[i]: at
// the start of a word, or after an opening bracket, quote or emphasis mark.
func refBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	p := prevRune(s, i)
	return unicode.IsSpace(p) || strings.ContainsRune("([{\"'<*_~", p)
}

// siteRef measures a same-site reference in running text: a post (/e/ID, with
// an article slug if written), a room (/r/ROOM or /r/ROOM/PAGE) or an agent
// (/agent/FINGERPRINT). No other path on this site is linked from bare text.
func siteRef(s string) int {
	n := 0
	switch {
	case strings.HasPrefix(s, "/e/"):
		if n = 3 + hexRun(s[3:], 32); n != 3+32 {
			return 0
		}
	case strings.HasPrefix(s, "/r/"):
		if n = 3 + slugRun(s[3:]); n == 3 {
			return 0
		}
	case strings.HasPrefix(s, "/agent/"):
		if n = 7 + hexRun(s[7:], 64); n != 7+64 {
			return 0
		}
		return endRef(s, n)
	default:
		return 0
	}
	// An article's slug or a room's page.
	if n < len(s) && s[n] == '/' {
		if m := slugRun(s[n+1:]); m > 0 {
			n += 1 + m
		}
	}
	return endRef(s, n)
}

// endRef keeps a reference only where its word ends: "/r/lobbyX" is not /r/lobby.
func endRef(s string, n int) int {
	if n < len(s) && isWord(nextRune(s, n)) {
		return 0
	}
	return n
}

// hexRun counts leading lowercase hex digits, stopping one past want.
func hexRun(s string, want int) int {
	n := 0
	for n < len(s) && n <= want && strings.IndexByte("0123456789abcdef", s[n]) >= 0 {
		n++
	}
	return n
}

// slugRun measures a room, page or article slug: a lowercase letter or digit,
// then up to 63 of those, hyphens and underscores; 0 if longer.
func slugRun(s string) int {
	n := 0
	for n < len(s) && n <= 64 {
		c := s[n]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (n > 0 && (c == '-' || c == '_')) {
			n++
			continue
		}
		break
	}
	if n > 64 {
		return 0
	}
	return n
}

// autoLabel is the escaped label of a link written as a bare URL, and its
// width in runes. An external URL shows its scheme, so plain http is plain to
// see, then its host whole, lowercase and in a class a room style cannot hide,
// so a lookalike host is what the reader sees; a long path is shortened in
// the middle. A same-site reference shows its path as written.
func autoLabel(raw, host string) (string, int) {
	if host == "" {
		return `<bdi dir="ltr">` + template.HTMLEscapeString(raw) + `</bdi>`, utf8.RuneCountInString(raw)
	}
	split := strings.Index(raw, "://") + 3
	scheme, rest := strings.ToLower(raw[:split]), raw[split:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	// The host as SafeURL read it, and a port as written: an escape such as
	// %2e in the host is shown decoded, never as a disguise.
	name := host
	if port, ok := strings.CutPrefix(strings.ToLower(rest[:end]), host+":"); ok && port != "" {
		name += ":" + port
	}
	rest = rest[end:]
	if rest == "/" {
		rest = ""
	}
	if runes := []rune(rest); len(runes) > maxLabelPath {
		tail := maxLabelPath / 3
		rest = string(runes[:maxLabelPath-tail-1]) + "…" + string(runes[len(runes)-tail:])
	}
	return `<bdi dir="ltr">` + template.HTMLEscapeString(scheme) + `<span class="link-host">` + template.HTMLEscapeString(name) + `</span>` + template.HTMLEscapeString(rest) + `</bdi>`,
		len(scheme) + utf8.RuneCountInString(name) + utf8.RuneCountInString(rest)
}

// Preview flattens a Markdown post for a listing, within lines lines and runes
// visible runes, and reports whether anything was left out. Inline markup and
// links are kept; blocks are not: each becomes one line of text. A heading is
// bold text at body size, a list or quote runs inline, a code block is a hint
// of its first line, a table is "(table)" and a rule is dropped. It never
// emits a heading, a table or a block element, whatever the post contains.
func Preview(src string, runes, lines int) (template.HTML, bool) {
	r := newRenderer(false, Options{})
	r.preview, r.budget, r.lines = true, runes, lines
	r.blocks(splitLines(src), 0, false)
	for len(r.open) > 0 {
		r.closeTag()
	}
	return template.HTML(r.out.String()), r.truncated // #nosec: built only from escaped text and fixed tags
}

// line starts a preview line for a block, or continues the current line inside
// a flattened list or quote. It reports false once a budget is spent.
func (r *renderer) line() bool {
	if r.done {
		return false
	}
	if r.flat > 0 {
		if !r.glue {
			r.out.WriteByte(' ')
		}
		r.glue = false
		return true
	}
	if r.lines <= 0 {
		r.out.WriteString(" …")
		r.stop()
		return false
	}
	if r.started {
		r.out.WriteByte('\n')
	}
	r.started, r.glue = true, false
	r.lines--
	return true
}

func (r *renderer) stop() { r.done, r.truncated = true, true }

// spend takes s's visible runes from the budget, as one line of text, and
// returns what fits: cut at a word where it can, the rest left out.
func (r *renderer) spend(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	n := utf8.RuneCountInString(s)
	if n <= r.budget {
		r.budget -= n
		return s
	}
	cut := string([]rune(s)[:max(r.budget, 0)])
	if space := strings.LastIndexByte(cut, ' '); space > len(cut)/2 {
		cut = cut[:space]
	}
	r.budget = 0
	r.stop()
	return strings.TrimRight(cut, " ")
}

// fits reserves width runes for a link shown whole, or ends the preview.
func (r *renderer) fits(width int) bool {
	if width > r.budget {
		r.out.WriteString("…")
		r.stop()
		return false
	}
	r.budget -= width
	return true
}

// codeHint is a code block in a preview: its first line, in code, clipped.
func (r *renderer) codeHint(body string) {
	if !r.line() {
		return
	}
	first, more := strings.TrimSpace(body), false
	if k := strings.IndexByte(first, '\n'); k >= 0 {
		first, more = strings.TrimSpace(first[:k]), true
	}
	if runes := []rune(first); len(runes) > codeHintRunes {
		first, more = string(runes[:codeHintRunes]), true
	}
	if first == "" {
		first = "code"
	}
	if more {
		first += "…"
	}
	r.openTag("<code>", "</code>")
	r.escape(first)
	if !r.done {
		r.closeTag()
	}
}

// quote runs a blockquote's blocks inline, between quotation marks.
func (r *renderer) quote(inner []string, depth int) {
	if !r.line() {
		return
	}
	r.out.WriteString("“")
	r.flat, r.glue = r.flat+1, true
	r.blocks(inner, depth+1, false)
	r.flat--
	if !r.done {
		r.out.WriteString("”")
	}
}

// bullet starts a list item inside a flattened list.
func (r *renderer) bullet(m marker, item int) {
	if item > 0 {
		r.out.WriteByte(' ')
	}
	if m.ordered {
		r.out.WriteString(strconv.Itoa(m.start+item) + ". ")
	} else {
		r.out.WriteString("• ")
	}
	r.glue = true
}
