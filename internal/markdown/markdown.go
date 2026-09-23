// Package markdown renders the vetted Markdown subset a post may opt into with a
// signed format field. It is deliberately small and hand-written: headings,
// paragraphs, emphasis, lists, blockquotes, fenced code, inline code, pipe
// tables, links and horizontal rules. There is no raw HTML, no image embedding
// and no URL scheme other than http(s) and same-site paths.
//
// Every byte of author text reaches the output through html escaping; the only
// markup emitted is the fixed set of tags written by this file. Output grows
// linearly with input (no padding, no quadratic scanning), and nesting is capped,
// so a hostile 16 KiB post cannot amplify into an expensive page.
package markdown

import (
	"html/template"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds. Depth covers blockquotes and list items together; inline nesting is
// structurally limited to a link label inside a paragraph.
const (
	maxDepth      = 6
	maxTableCols  = 32
	maxURLBytes   = 2048
	maxStackScan  = 64
	maxAnchorSlug = 64
	maxParenDepth = 32 // parentheses nested inside one link destination
)

// Options selects presentation that depends on where a post is shown.
type Options struct {
	// Anchors gives headings stable ids ("md-" + slug) so in-post "#section"
	// links work. Only the single article on a page should set it: two posts
	// rendered with anchors on one page would repeat ids.
	Anchors bool
	// SkipTitle omits a leading heading, for a page that shows it as its h1.
	SkipTitle bool
}

// Render returns the post as HTML. It is safe to place in an html/template
// because every author-supplied string is escaped here.
func Render(src string, opt Options) template.HTML {
	r := newRenderer(false, opt)
	r.blocks(splitLines(src), 0, false)
	return template.HTML(r.out.String()) // #nosec: built only from escaped text and fixed tags
}

// Title is the leading heading's text, or "" when the post does not open with one.
func Title(src string) string {
	lines := splitLines(src)
	for _, line := range lines {
		if isBlank(line) {
			continue
		}
		if level, text, ok := atxHeading(line); ok && level > 0 {
			return collapse(inlineText(text))
		}
		return ""
	}
	return ""
}

// PlainText strips the markup, keeping the words a reader would see. It is
// used for descriptions, previews and quotes, never for HTML.
func PlainText(src string) string {
	r := newRenderer(true, Options{})
	r.blocks(splitLines(src), 0, false)
	return strings.TrimSpace(r.out.String())
}

// Summary is the first block of prose after an optional leading title heading,
// as collapsed plain text.
func Summary(src string) string {
	r := newRenderer(true, Options{SkipTitle: true})
	r.firstOnly = true
	r.blocks(splitLines(src), 0, false)
	return collapse(r.out.String())
}

func inlineText(s string) string {
	r := newRenderer(true, Options{})
	r.inline(s, true)
	return r.out.String()
}

type renderer struct {
	out       strings.Builder
	text      bool // plain-text mode: same parse, no tags
	opt       Options
	anchors   map[string]int
	emitted   bool // a top-level block has been written
	firstOnly bool // plain summary: stop after the first prose block
	done      bool
}

func newRenderer(text bool, opt Options) *renderer {
	return &renderer{text: text, opt: opt, anchors: map[string]int{}}
}

func (r *renderer) tag(s string) {
	if !r.text {
		r.out.WriteString(s)
	}
}

func (r *renderer) escape(s string) {
	if r.text {
		r.out.WriteString(s)
		return
	}
	r.out.WriteString(template.HTMLEscapeString(s))
}

// ---------- lines ----------

func splitLines(src string) []string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	return strings.Split(src, "\n")
}

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

// indent counts leading columns, with a tab advancing to the next multiple of 4.
func indent(line string) int {
	col := 0
	for _, c := range line {
		switch c {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col
		}
	}
	return col
}

// dedent removes up to n columns of leading whitespace.
func dedent(line string, n int) string {
	col := 0
	for i, c := range line {
		if col >= n {
			return line[i:]
		}
		switch c {
		case ' ':
			col++
		case '\t':
			next := col + 4 - col%4
			if next > n {
				return strings.Repeat(" ", next-n) + line[i+1:]
			}
			col = next
		default:
			return line[i:]
		}
	}
	return ""
}

// ---------- block recognisers ----------

func fenceOpen(line string) (marker string, ok bool) {
	if indent(line) > 3 {
		return "", false
	}
	t := strings.TrimLeft(line, " \t")
	if len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return "", false
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 || (t[0] == '`' && strings.Contains(t[n:], "`")) {
		return "", false
	}
	return t[:n], true
}

func fenceClose(line, marker string) bool {
	if indent(line) > 3 {
		return false
	}
	t := strings.TrimSpace(line)
	return len(t) >= len(marker) && strings.Trim(t, marker[:1]) == ""
}

// atxHeading maps # to level 1 and so on; level 0 means not a heading.
func atxHeading(line string) (int, string, bool) {
	if indent(line) > 3 {
		return 0, "", false
	}
	t := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(t) && t[n] != ' ' && t[n] != '\t') {
		return 0, "", false
	}
	text := strings.TrimSpace(t[n:])
	// An optional closing run of # preceded by a space is not content.
	if trimmed := strings.TrimRight(text, "#"); trimmed != text && (trimmed == "" || strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t")) {
		text = strings.TrimSpace(trimmed)
	}
	return n, text, true
}

func thematicBreak(line string) bool {
	if indent(line) > 3 {
		return false
	}
	t := strings.TrimSpace(line)
	if len(t) < 3 || (t[0] != '-' && t[0] != '*' && t[0] != '_') {
		return false
	}
	count := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case t[0]:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

func quoteLine(line string) (string, bool) {
	if indent(line) > 3 {
		return "", false
	}
	t := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(t, ">") {
		return "", false
	}
	t = t[1:]
	if strings.HasPrefix(t, " ") {
		t = t[1:]
	}
	return t, true
}

type marker struct {
	ordered  bool
	bullet   byte // '-', '*', '+' or the ordered delimiter '.' / ')'
	start    int
	content  int // column where item content begins
	text     string
	markerAt int
}

func listMarker(line string) (marker, bool) {
	col := indent(line)
	if col > 3 {
		return marker{}, false
	}
	t := strings.TrimLeft(line, " \t")
	m := marker{markerAt: col}
	width := 0
	switch {
	case len(t) > 0 && (t[0] == '-' || t[0] == '*' || t[0] == '+'):
		m.bullet, width = t[0], 1
	default:
		n := 0
		for n < len(t) && n < 9 && t[n] >= '0' && t[n] <= '9' {
			n++
		}
		if n == 0 || n >= len(t) || (t[n] != '.' && t[n] != ')') {
			return marker{}, false
		}
		m.ordered, m.bullet, width = true, t[n], n+1
		m.start, _ = strconv.Atoi(t[:n])
	}
	rest := t[width:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return marker{}, false
	}
	spaces := indent(rest)
	body := strings.TrimLeft(rest, " \t")
	if body == "" || spaces > 4 {
		spaces = 1
	}
	m.content = col + width + spaces
	m.text = body
	return m, true
}

func sameList(a, b marker) bool { return a.ordered == b.ordered && a.bullet == b.bullet }

// splitRow splits a pipe-table row on unescaped pipes outside code spans.
func splitRow(line string) []string {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "|") {
		t = t[1:]
	}
	if strings.HasSuffix(t, "|") && !strings.HasSuffix(t, "\\|") {
		t = t[:len(t)-1]
	}
	cells := []string{}
	start, ticks := 0, 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '\\':
			i++
		case '`':
			ticks ^= 1
		case '|':
			if ticks == 0 {
				cells = append(cells, strings.TrimSpace(t[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(t) || len(cells) > 0 || t != "" {
		cells = append(cells, strings.TrimSpace(t[start:]))
	}
	return cells
}

func delimiterRow(line string) ([]string, bool) {
	if !strings.Contains(line, "-") {
		return nil, false
	}
	cells := splitRow(line)
	aligns := make([]string, len(cells))
	for i, cell := range cells {
		c := strings.TrimSpace(cell)
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		c = strings.Trim(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			aligns[i] = "center"
		case right:
			aligns[i] = "right"
		case left:
			aligns[i] = "left"
		}
	}
	return aligns, true
}

func tableStart(lines []string, i int) ([]string, []string, bool) {
	if i+1 >= len(lines) || !strings.Contains(lines[i], "|") || indent(lines[i]) > 3 {
		return nil, nil, false
	}
	header := splitRow(lines[i])
	aligns, ok := delimiterRow(lines[i+1])
	if !ok || len(header) != len(aligns) || len(header) == 0 || len(header) > maxTableCols {
		return nil, nil, false
	}
	return header, aligns, true
}

// startsBlock reports whether a line interrupts a paragraph.
func startsBlock(lines []string, i int) bool {
	line := lines[i]
	if _, ok := fenceOpen(line); ok {
		return true
	}
	if _, _, ok := atxHeading(line); ok {
		return true
	}
	if thematicBreak(line) {
		return true
	}
	if _, ok := quoteLine(line); ok {
		return true
	}
	if _, ok := listMarker(line); ok {
		return true
	}
	_, _, ok := tableStart(lines, i)
	return ok
}

// ---------- blocks ----------

// blocks renders lines. bare renders a first paragraph without <p>, as a tight
// list item does.
func (r *renderer) blocks(lines []string, depth int, bare bool) {
	first := true
	for i := 0; i < len(lines) && !r.done; {
		line := lines[i]
		if isBlank(line) {
			i++
			continue
		}
		top := depth == 0 && !r.emitted
		if depth > maxDepth {
			// Past the nesting cap the remainder is plain escaped prose.
			r.paragraph(strings.Join(lines[i:], "\n"), false)
			return
		}
		if marker, ok := fenceOpen(line); ok {
			start := i + 1
			end := start
			for end < len(lines) && !fenceClose(lines[end], marker) {
				end++
			}
			width := indent(line)
			body := make([]string, 0, end-start)
			for _, l := range lines[start:min(end, len(lines))] {
				body = append(body, dedent(l, width))
			}
			r.code(strings.Join(body, "\n"))
			i = min(end+1, len(lines))
			r.after(depth)
			first = false
			continue
		}
		if level, text, ok := atxHeading(line); ok {
			i++
			if top && r.opt.SkipTitle {
				r.emitted = true
				continue
			}
			r.heading(level, text)
			r.after(depth)
			first = false
			continue
		}
		if thematicBreak(line) {
			r.tag("<hr>\n")
			i++
			r.after(depth)
			first = false
			continue
		}
		if _, ok := quoteLine(line); ok {
			inner := []string{}
			for i < len(lines) {
				if q, ok := quoteLine(lines[i]); ok {
					inner = append(inner, q)
				} else if !isBlank(lines[i]) && len(inner) > 0 && !isBlank(inner[len(inner)-1]) && !startsBlock(lines, i) {
					inner = append(inner, lines[i]) // lazy continuation
				} else {
					break
				}
				i++
			}
			r.tag("<blockquote>\n")
			r.blocks(inner, depth+1, false)
			r.tag("</blockquote>\n")
			r.after(depth)
			first = false
			continue
		}
		if m, ok := listMarker(line); ok {
			i = r.list(lines, i, m, depth)
			r.after(depth)
			first = false
			continue
		}
		if header, aligns, ok := tableStart(lines, i); ok {
			i = r.table(lines, i, header, aligns)
			r.after(depth)
			first = false
			continue
		}
		start := i
		for i++; i < len(lines) && !isBlank(lines[i]) && !startsBlock(lines, i); i++ {
		}
		para := make([]string, 0, i-start)
		for _, l := range lines[start:i] {
			para = append(para, strings.TrimLeft(l, " \t"))
		}
		r.paragraph(strings.Join(para, "\n"), bare && first)
		r.after(depth)
		if r.firstOnly {
			r.done = true
		}
		first = false
	}
}

func (r *renderer) after(depth int) {
	if depth == 0 {
		r.emitted = true
	}
	if r.text {
		r.out.WriteString("\n")
	}
}

func (r *renderer) code(body string) {
	if r.firstOnly {
		return
	}
	r.tag("<pre><code>")
	r.escape(body)
	r.tag("</code></pre>\n")
}

func (r *renderer) heading(level int, text string) {
	if r.firstOnly {
		return
	}
	// Inside a post, # is a section: the page owns h1.
	n := min(level+1, 4)
	open := "<h" + strconv.Itoa(n)
	if r.opt.Anchors {
		if id := r.anchor(inlineText(text)); id != "" {
			open += ` id="` + id + `"`
		}
	}
	r.tag(open + ">")
	r.inline(text, true)
	r.tag("</h" + strconv.Itoa(n) + ">\n")
}

func (r *renderer) anchor(text string) string {
	base := Slug(text, maxAnchorSlug)
	if base == "" {
		return ""
	}
	id := "md-" + base
	if n := r.anchors[base]; n > 0 {
		id += "-" + strconv.Itoa(n+1)
	}
	r.anchors[base]++
	return id
}

func (r *renderer) paragraph(text string, bare bool) {
	if !bare {
		r.tag("<p>")
	}
	r.inline(text, true)
	if !bare {
		r.tag("</p>\n")
	}
}

// list consumes one list starting at lines[i] and returns the next index.
func (r *renderer) list(lines []string, i int, m marker, depth int) int {
	if r.firstOnly {
		// A summary wants prose; a list's first item is a fair stand-in.
		r.inline(m.text, true)
		r.done = true
		return len(lines)
	}
	if m.ordered {
		if m.start != 1 {
			r.tag(`<ol start="` + strconv.Itoa(m.start) + `">` + "\n")
		} else {
			r.tag("<ol>\n")
		}
	} else {
		r.tag("<ul>\n")
	}
	current := m
	for {
		body := []string{current.text}
		i++
		for i < len(lines) {
			line := lines[i]
			if isBlank(line) {
				// A blank line continues the item only if indented content follows.
				j := i
				for j < len(lines) && isBlank(lines[j]) {
					j++
				}
				if j < len(lines) && indent(lines[j]) >= current.content {
					for ; i < j; i++ {
						body = append(body, "")
					}
					continue
				}
				if j < len(lines) {
					if next, ok := listMarker(lines[j]); ok && sameList(next, m) && next.markerAt < current.content {
						i = j
					}
				}
				break
			}
			if indent(line) >= current.content {
				body = append(body, dedent(line, current.content))
				i++
				continue
			}
			if startsBlock(lines, i) {
				break
			}
			if isBlank(body[len(body)-1]) {
				break
			}
			body = append(body, strings.TrimLeft(line, " \t")) // lazy continuation
			i++
		}
		r.tag("<li>")
		r.blocks(body, depth+1, true)
		r.tag("</li>\n")
		if i >= len(lines) {
			break
		}
		next, ok := listMarker(lines[i])
		if !ok || !sameList(next, m) {
			break
		}
		current = next
	}
	if m.ordered {
		r.tag("</ol>\n")
	} else {
		r.tag("</ul>\n")
	}
	return i
}

func (r *renderer) table(lines []string, i int, header, aligns []string) int {
	if r.firstOnly {
		r.inline(strings.Join(header, " · "), true)
		r.done = true
		return len(lines)
	}
	cell := func(tag, align, text string) {
		if align != "" {
			r.tag("<" + tag + ` class="md-` + align + `">`)
		} else {
			r.tag("<" + tag + ">")
		}
		r.inline(text, true)
		r.tag("</" + tag + ">")
		if r.text {
			r.out.WriteString(" ")
		}
	}
	r.tag(`<div class="md-table"><table>` + "\n<thead><tr>")
	for c, text := range header {
		cell("th", aligns[c], text)
	}
	r.tag("</tr></thead>\n")
	i += 2
	body := false
	for ; i < len(lines) && !isBlank(lines[i]) && !startsBlock(lines, i); i++ {
		if !body {
			r.tag("<tbody>\n")
			body = true
		}
		r.tag("<tr>")
		// No padding: a short row stays short, so output is linear in input.
		for c, text := range splitRow(lines[i]) {
			if c >= len(header) {
				break
			}
			cell("td", aligns[c], text)
		}
		r.tag("</tr>\n")
		if r.text {
			r.out.WriteString("\n")
		}
	}
	if body {
		r.tag("</tbody>\n")
	}
	r.tag("</table></div>\n")
	return i
}

// ---------- inline ----------

const (
	tText = iota
	tCode
	tLink
	tBreak
	tDelim
)

type token struct {
	kind  int
	text  string // text, code, link label (raw markdown), or link href
	href  string
	host  string
	auto  bool // an autolink shows its own URL as the label
	ch    byte
	count int
	open  bool
	close bool
	// closes are emitted before the literal leftover run, innermost first;
	// opens after it, outermost first.
	opens, closes []string
}

func isPunct(b byte) bool { return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", b) >= 0 }

func (r *renderer) inline(s string, links bool) {
	toks := tokenize(s, links)
	resolveEmphasis(toks)
	for i := range toks {
		t := &toks[i]
		switch t.kind {
		case tText:
			r.escape(t.text)
		case tBreak:
			if r.text {
				r.out.WriteString("\n")
			} else {
				r.out.WriteString("<br>\n")
			}
		case tCode:
			r.tag("<code>")
			r.escape(t.text)
			r.tag("</code>")
		case tLink:
			r.link(t)
		case tDelim:
			for _, tag := range t.closes {
				r.tag(tag)
			}
			r.escape(strings.Repeat(string(t.ch), t.count))
			for k := len(t.opens) - 1; k >= 0; k-- {
				r.tag(t.opens[k])
			}
		}
	}
}

func (r *renderer) link(t *token) {
	if r.text {
		if t.auto {
			r.out.WriteString(t.text)
		} else {
			r.inline(t.text, false)
		}
		return
	}
	r.out.WriteString(`<a href="` + template.HTMLEscapeString(t.href) + `"`)
	if t.host != "" {
		r.out.WriteString(` rel="nofollow noopener ugc"`)
	}
	r.out.WriteString("><bdi>")
	if t.auto {
		r.escape(t.text)
	} else {
		r.inline(t.text, false)
	}
	r.out.WriteString("</bdi></a>")
	// The destination host is shown beside a labelled external link, in its own
	// left-to-right isolate so surrounding bidi controls cannot reorder it.
	if t.host != "" && !t.auto {
		r.out.WriteString(`<bdi class="md-host" dir="ltr">` + template.HTMLEscapeString(t.host) + `</bdi>`)
	}
}

// tokenize splits one paragraph's text. Every scan is bounded or indexed, so
// adversarial runs of brackets or backticks stay near-linear.
func tokenize(s string, links bool) []token {
	toks := []token{}
	// A byte slice, not a strings.Builder: a line break trims trailing spaces
	// in place, where rebuilding a Builder copied the whole pending run at
	// every newline (quadratic in a long paragraph).
	var text []byte
	flush := func() {
		if len(text) > 0 {
			toks = append(toks, token{kind: tText, text: string(text)})
			text = text[:0]
		}
	}
	ticks := backtickRuns(s)
	brackets := matchBrackets(s)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			flush()
			toks = append(toks, token{kind: tBreak})
			i += 2
			continue
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			text = append(text, s[i+1])
			i += 2
			continue
		case c == '\n':
			// Two trailing spaces make a hard break; otherwise a soft one.
			end := len(text)
			for end > 0 && text[end-1] == ' ' {
				end--
			}
			hard := len(text)-end >= 2
			text = text[:end]
			if hard {
				flush()
				toks = append(toks, token{kind: tBreak})
			} else {
				text = append(text, '\n')
			}
			i++
			continue
		case c == '`':
			n := runLength(s, i, '`')
			if end := ticks.next(n, i+n); end >= 0 {
				flush()
				toks = append(toks, token{kind: tCode, text: codeSpan(s[i+n : end])})
				i = end + n
			} else {
				text = append(text, s[i:i+n]...)
				i += n
			}
			continue
		case c == '*' || c == '_':
			n := runLength(s, i, c)
			flush()
			before, after := prevRune(s, i), nextRune(s, i+n)
			open := !unicode.IsSpace(after) && after != utf8.RuneError
			closeOK := i > 0 && !unicode.IsSpace(before)
			if c == '_' {
				open = open && !isWord(before)
				closeOK = closeOK && !isWord(after)
			}
			toks = append(toks, token{kind: tDelim, ch: c, count: n, open: open, close: closeOK})
			i += n
			continue
		case links && c == '<':
			if end := strings.IndexByte(s[i:min(len(s), i+maxURLBytes+2)], '>'); end > 1 {
				raw := s[i+1 : i+end]
				if href, host, ok := SafeURL(raw); ok && host != "" && !strings.ContainsAny(raw, " <\n") {
					flush()
					toks = append(toks, token{kind: tLink, text: raw, href: href, host: host, auto: true})
					i += end + 1
					continue
				}
			}
		case links && (c == '[' || (c == '!' && i+1 < len(s) && s[i+1] == '[')):
			open := i
			if c == '!' {
				open++ // an image is shown as a link to it, never embedded
			}
			if close := brackets[open]; close > open && close+1 < len(s) && s[close+1] == '(' {
				if dest, next, ok := destination(s, close+2); ok {
					label := s[open+1 : close]
					flush()
					if href, host, ok := SafeURL(dest); ok {
						toks = append(toks, token{kind: tLink, text: label, href: href, host: host})
					} else {
						// A refused destination keeps its words and loses the link.
						toks = append(toks, tokenize(label, false)...)
					}
					i = next
					continue
				}
			}
		case links && (c == 'h' || c == 'H') && (i == 0 || !isWord(prevRune(s, i))):
			if n := bareURL(s[i:]); n > 0 {
				if href, host, ok := SafeURL(s[i : i+n]); ok && host != "" {
					flush()
					toks = append(toks, token{kind: tLink, text: s[i : i+n], href: href, host: host, auto: true})
					i += n
					continue
				}
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		text = append(text, s[i:i+size]...)
		i += size
	}
	flush()
	return toks
}

func runLength(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

func prevRune(s string, i int) rune {
	if i == 0 {
		return ' '
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func nextRune(s string, i int) rune {
	if i >= len(s) {
		return ' '
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func codeSpan(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) >= 2 && s[0] == ' ' && s[len(s)-1] == ' ' && strings.Trim(s, " ") != "" {
		s = s[1 : len(s)-1]
	}
	return s
}

// tickIndex finds, for a backtick run of length n at or after a position, the
// next run of exactly that length, by binary search over precomputed positions.
type tickIndex map[int][]int

func backtickRuns(s string) tickIndex {
	idx := tickIndex{}
	for i := 0; i < len(s); {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if s[i] != '`' {
			i++
			continue
		}
		n := runLength(s, i, '`')
		idx[n] = append(idx[n], i)
		i += n
	}
	return idx
}

func (idx tickIndex) next(n, from int) int {
	positions := idx[n]
	k := sort.SearchInts(positions, from)
	if k < len(positions) {
		return positions[k]
	}
	return -1
}

// matchBrackets pairs [ and ] in one linear pass, skipping escapes.
func matchBrackets(s string) map[int]int {
	pairs := map[int]int{}
	stack := []int{}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			stack = append(stack, i)
		case ']':
			if len(stack) > 0 {
				pairs[stack[len(stack)-1]] = i
				stack = stack[:len(stack)-1]
			}
		}
	}
	return pairs
}

// destination reads "(url)" or "(url "title")" starting after the "(".
func destination(s string, i int) (string, int, bool) {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start, depth := i, 0
	for ; i < len(s) && i-start <= maxURLBytes; i++ {
		c := s[i]
		if c == '(' {
			// CommonMark's bound. Without it each "[](" candidate scanned
			// maxURLBytes through the "[](" candidates after it.
			if depth++; depth > maxParenDepth {
				return "", 0, false
			}
		} else if c == ')' {
			if depth == 0 {
				break
			}
			depth--
		} else if c == ' ' || c == '\t' || c == '\n' || c == '<' {
			break
		}
	}
	dest := s[start:i]
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
		i++
	}
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		quote := s[i]
		end := strings.IndexByte(s[i+1:min(len(s), i+1+maxURLBytes)], quote)
		if end < 0 {
			return "", 0, false
		}
		i += end + 2
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	if i >= len(s) || s[i] != ')' || dest == "" {
		return "", 0, false
	}
	return dest, i + 1, true
}

// bareURL measures an http(s) URL in running text, leaving trailing
// punctuation and an unbalanced closing parenthesis to the sentence.
func bareURL(s string) int {
	lower := strings.ToLower(s[:min(len(s), 8)])
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return 0
	}
	n := 0
	for n < len(s) && n < maxURLBytes {
		c := s[n]
		if c <= ' ' || c == '<' || c == '>' || c == '"' || c == '`' || c >= 0x80 {
			break
		}
		n++
	}
	// Counted once and updated as the tail shrinks: recounting per trimmed
	// parenthesis made a run of them quadratic in the URL length.
	opens, closes := strings.Count(s[:n], "("), strings.Count(s[:n], ")")
	for n > 0 {
		c := s[n-1]
		if strings.IndexByte(".,:;!?'*_~", c) >= 0 {
			n--
			continue
		}
		if c == ')' && opens < closes {
			n--
			closes--
			continue
		}
		break
	}
	if n <= len("https://") {
		return 0
	}
	return n
}

// resolveEmphasis pairs * and _ runs with a bounded delimiter stack. Matching a
// closer discards unmatched openers between the pair, which keeps the emitted
// tags properly nested.
func resolveEmphasis(toks []token) {
	stack := []int{}
	for i := range toks {
		t := &toks[i]
		if t.kind != tDelim {
			continue
		}
		for t.close && t.count > 0 {
			found := -1
			for k := len(stack) - 1; k >= 0 && len(stack)-k <= maxStackScan; k-- {
				o := &toks[stack[k]]
				if o.ch == t.ch && o.count > 0 {
					found = k
					break
				}
			}
			if found < 0 {
				break
			}
			o := &toks[stack[found]]
			use := 1
			if o.count >= 2 && t.count >= 2 {
				use = 2
			}
			open, closeTag := "<em>", "</em>"
			if use == 2 {
				open, closeTag = "<strong>", "</strong>"
			}
			o.count -= use
			t.count -= use
			o.opens = append(o.opens, open)
			t.closes = append(t.closes, closeTag)
			stack = stack[:found+1]
			if o.count == 0 {
				stack = stack[:found]
			}
		}
		if t.open && t.count > 0 {
			stack = append(stack, i)
		}
	}
}

// ---------- URLs ----------

// writePaths are this board's GET-write and privileged prefixes. A link to one
// would be a clickable write, so it is refused on any host.
var writePaths = []string{"/w/", "/w64/", "/c64/", "/v1/", "/admin/"}

func refusedPath(p string) bool {
	for _, candidate := range []string{p, path.Clean("/" + p)} {
		if !strings.HasSuffix(candidate, "/") && strings.Count(candidate, "/") == 1 {
			candidate += "/"
		}
		for _, prefix := range writePaths {
			if strings.HasPrefix(candidate, prefix) {
				return true
			}
		}
	}
	return false
}

// SafeURL accepts an absolute http(s) URL with a plain ASCII host and no
// credentials, a same-site path, or an in-post #fragment. host is empty for
// same-site links. Anything else is refused, not rewritten.
func SafeURL(raw string) (href, host string, ok bool) {
	if raw == "" || len(raw) > maxURLBytes {
		return "", "", false
	}
	for _, c := range raw {
		if c <= ' ' || c == 0x7f || c == '\\' || (c >= 0x80 && !unicode.IsPrint(c)) || bidiControl(c) {
			return "", "", false
		}
	}
	if strings.HasPrefix(raw, "#") {
		frag := raw[1:]
		if frag == "" || len(frag) > maxAnchorSlug || strings.Trim(strings.ToLower(frag), "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return "", "", false
		}
		slug := Slug(frag, maxAnchorSlug)
		if slug == "" {
			return "", "", false
		}
		return "#md-" + slug, "", true
	}
	if strings.HasPrefix(raw, "/") {
		if strings.HasPrefix(raw, "//") {
			return "", "", false
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || refusedPath(u.Path) {
			return "", "", false
		}
		// Re-encoding can decode %2F: "/%2Fevil.example/{" becomes the
		// protocol-relative "//evil.example/%7B", an off-site link shown as a
		// same-site one. Check what the browser will actually receive.
		href := u.String()
		if !strings.HasPrefix(href, "/") || strings.HasPrefix(href, "//") {
			return "", "", false
		}
		return href, "", true
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	u.Scheme = scheme
	name := strings.ToLower(u.Hostname())
	if name == "" || len(name) > 253 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		return "", "", false
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", "", false
		}
	}
	if refusedPath(u.Path) {
		return "", "", false
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), name, true
}

func bidiControl(c rune) bool {
	return (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069) || c == 0x200e || c == 0x200f || c == 0x061c
}

// ---------- text helpers ----------

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// Clip shortens collapsed text to at most n runes, ending with an ellipsis on
// a word boundary when it can.
func Clip(s string, n int) string {
	s = collapse(s)
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	cut := string(runes[:n-1])
	if space := strings.LastIndexByte(cut, ' '); space > len(cut)/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:.-") + "…"
}

// Slug is a lowercase ASCII rendering for URLs and anchors: letters and digits
// kept, every other run becomes one hyphen, at most n bytes, never a trailing
// hyphen. Non-ASCII letters are dropped rather than transliterated.
func Slug(s string, n int) string {
	var b strings.Builder
	hyphen := false
	for _, c := range strings.ToLower(s) {
		if b.Len() >= n {
			break
		}
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(c)
		default:
			hyphen = true
		}
	}
	out := b.String()
	if len(out) > n {
		out = out[:n]
	}
	return strings.Trim(out, "-")
}
