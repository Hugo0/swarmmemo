package services

import (
	"html"
	"net/url"
	"strings"
	"unicode/utf8"
)

// HTML to readable text for fetch: a small, bounded tokenizer, not a
// browser. It runs no script, loads nothing and renders nothing: it keeps
// the text of the page with its structure as Markdown (headings, list items,
// paragraphs, preformatted blocks, links as [text](url)) and drops scripts,
// styles, templates, embedded objects and the page's navigation chrome.
// Every loop advances through the input, so the work is linear in its size
// (at most FetchBodyBytes).

// skippedElements are left out with everything inside them.
var skippedElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true, "math": true,
	"iframe": true, "object": true, "embed": true, "canvas": true, "video": true, "audio": true,
	"head": true, "nav": true, "footer": true, "aside": true, "select": true, "button": true, "form": true,
}

// rawTextElements hold text up to their own closing tag, never markup.
var rawTextElements = map[string]bool{"script": true, "style": true, "textarea": true, "title": true, "xmp": true, "noscript": true, "template": false}

// blockElements end the line they are on.
var blockElements = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true, "header": true, "ul": true, "ol": true,
	"table": true, "dl": true, "dt": true, "dd": true, "figure": true, "figcaption": true, "address": true,
	"details": true, "summary": true, "fieldset": true, "body": true, "html": true, "center": true, "hgroup": true,
}

// htmlTextLimit bounds the text one conversion builds: more than an answer
// returns (FetchTextMax), so a page of short relative links cannot grow
// into megabytes of resolved URLs.
const htmlTextLimit = FetchTextMax + 16<<10

// pageText is a page converted: its title and its text as Markdown.
type pageText struct {
	Title string
	Text  string
}

// htmlToText converts an HTML document (valid UTF-8) to Markdown text.
// base resolves relative links; nil drops them.
func htmlToText(doc string, base *url.URL) pageText {
	w := &mdWriter{base: base}
	var title strings.Builder
	skip := 0      // depth inside skipped elements
	skipName := "" // the outermost skipped element
	i := 0
	for i < len(doc) && w.b.Len() < htmlTextLimit {
		lt := strings.IndexByte(doc[i:], '<')
		if lt < 0 {
			if skip == 0 {
				w.text(doc[i:])
			}
			break
		}
		if lt > 0 {
			if skip == 0 {
				w.text(doc[i : i+lt])
			}
			i += lt
		}
		// doc[i] == '<'
		rest := doc[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				i = len(doc)
			} else {
				i += 4 + end + 3
			}
			continue
		case strings.HasPrefix(rest, "<![CDATA["):
			end := strings.Index(rest, "]]>")
			if end < 0 {
				i = len(doc)
			} else {
				if skip == 0 {
					w.text(rest[9:end])
				}
				i += end + 3
			}
			continue
		case strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, "<?"):
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				i = len(doc)
			} else {
				i += end + 1
			}
			continue
		}
		name, attrs, closing, selfClosing, n := parseTag(rest)
		if n == 0 {
			// Not a tag: a literal '<'.
			if skip == 0 {
				w.text("<")
			}
			i++
			continue
		}
		i += n
		if closing {
			if skip > 0 {
				if name == skipName {
					skip--
					if skip == 0 {
						skipName = ""
					}
				}
				continue
			}
			w.close(name)
			continue
		}
		if rawTextElements[name] && !selfClosing {
			end := indexCloseTag(doc[i:], name)
			body := doc[i:]
			if end >= 0 {
				body = doc[i : i+end]
				i += end
				if gt := strings.IndexByte(doc[i:], '>'); gt >= 0 {
					i += gt + 1
				} else {
					i = len(doc)
				}
			} else {
				i = len(doc)
			}
			switch {
			case name == "title" && title.Len() == 0:
				title.WriteString(collapseSpace(html.UnescapeString(body)))
			case name == "textarea" && skip == 0:
				w.text(body)
			}
			continue
		}
		if skip > 0 {
			if name == skipName && !selfClosing && !voidElements[name] {
				skip++
			}
			continue
		}
		if skippedElements[name] && !selfClosing && !voidElements[name] {
			skip, skipName = 1, name
			continue
		}
		w.open(name, attrs)
	}
	text := w.finish()
	t := title.String()
	if len(t) > 300 {
		t = truncateUTF8(t, 300)
	}
	return pageText{Title: t, Text: text}
}

// voidElements never have content or a closing tag.
var voidElements = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true}

// parseTag reads a tag at the start of s ("<a href=x>", "</p>", "<br/>"):
// its lowercase name, its attributes (only href and the like are used),
// whether it closes or closes itself, and its length; n is 0 when s does
// not start a tag.
func parseTag(s string) (name string, attrs map[string]string, closing, selfClosing bool, n int) {
	i := 1
	if i < len(s) && s[i] == '/' {
		closing = true
		i++
	}
	start := i
	for i < len(s) && isNameByte(s[i]) {
		i++
	}
	if i == start || !isASCIILetter(s[start]) {
		return "", nil, false, false, 0
	}
	name = strings.ToLower(s[start:i])
	for i < len(s) {
		c := s[i]
		switch {
		case c == '>':
			return name, attrs, closing, selfClosing, i + 1
		case c == '/':
			selfClosing = true
			i++
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		default:
			selfClosing = false
			ks := i
			for i < len(s) && s[i] != '=' && s[i] != '>' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' && s[i] != '/' {
				i++
			}
			key := strings.ToLower(s[ks:i])
			for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
				i++
			}
			val := ""
			if i < len(s) && s[i] == '=' {
				i++
				for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
					i++
				}
				if i < len(s) && (s[i] == '"' || s[i] == '\'') {
					q := s[i]
					end := strings.IndexByte(s[i+1:], q)
					if end < 0 {
						return name, attrs, closing, selfClosing, len(s)
					}
					val = s[i+1 : i+1+end]
					i += end + 2
				} else {
					vs := i
					for i < len(s) && s[i] != '>' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
						i++
					}
					val = s[vs:i]
				}
			}
			if key != "" && (key == "href" || key == "alt") {
				if attrs == nil {
					attrs = map[string]string{}
				}
				if _, seen := attrs[key]; !seen {
					attrs[key] = html.UnescapeString(val)
				}
			}
			if i == ks { // a stray byte: step over it
				i++
			}
		}
	}
	return name, attrs, closing, selfClosing, len(s)
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isNameByte(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '-' || c == ':' || c == '_'
}

// indexCloseTag finds "</name" (any case) in s, or -1.
func indexCloseTag(s, name string) int {
	for i := 0; i+2+len(name) <= len(s); {
		j := strings.Index(s[i:], "</")
		if j < 0 {
			return -1
		}
		k := i + j
		if k+2+len(name) <= len(s) && strings.EqualFold(s[k+2:k+2+len(name)], name) {
			end := k + 2 + len(name)
			if end == len(s) || !isNameByte(s[end]) {
				return k
			}
		}
		i = k + 2
	}
	return -1
}

// mdWriter builds the Markdown text: whitespace collapsed outside pre,
// blank lines between blocks, at most one blank line in a row.
type mdWriter struct {
	base     *url.URL
	b        strings.Builder
	pre      int    // depth inside <pre>
	listPfx  string // the pending list-item or heading prefix
	link     string // the open link's target
	linkText strings.Builder
	inLink   bool
	space    bool // a space is pending before the next word
	lineDone bool // the current line has text
}

func (w *mdWriter) out() *strings.Builder {
	if w.inLink {
		return &w.linkText
	}
	return &w.b
}

func (w *mdWriter) text(raw string) {
	s := html.UnescapeString(raw)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if s == "" {
		return
	}
	if w.inLink {
		if w.linkText.Len() < 4096 {
			w.linkText.WriteString(" " + s + " ")
		}
		return
	}
	if w.pre > 0 {
		w.b.WriteString(s)
		w.lineDone = true
		return
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	fields := strings.FieldsFunc(s, isSpace)
	if isSpace(first) && w.lineDone {
		w.space = true
	}
	for i, field := range fields {
		if w.listPfx != "" {
			w.b.WriteString(w.listPfx)
			w.listPfx, w.space = "", false
		}
		if w.space && w.lineDone {
			w.b.WriteByte(' ')
		}
		w.b.WriteString(field)
		w.lineDone = true
		w.space = i < len(fields)-1 || isSpace(last)
	}
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == ' '
}

// newline ends the current line; blank makes it a paragraph break.
func (w *mdWriter) newline(blank bool) {
	if w.inLink {
		return
	}
	s := w.b.String()
	switch {
	case len(s) == 0:
	case blank && !strings.HasSuffix(s, "\n\n"):
		if strings.HasSuffix(s, "\n") {
			w.b.WriteByte('\n')
		} else {
			w.b.WriteString("\n\n")
		}
	case !blank && !strings.HasSuffix(s, "\n"):
		w.b.WriteByte('\n')
	}
	w.lineDone, w.space = false, false
}

func (w *mdWriter) open(name string, attrs map[string]string) {
	switch name {
	case "br":
		if w.pre > 0 {
			w.out().WriteByte('\n')
			return
		}
		w.newline(false)
	case "hr":
		w.newline(true)
		w.b.WriteString("---")
		w.newline(true)
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.newline(true)
		w.listPfx = strings.Repeat("#", int(name[1]-'0')) + " "
	case "li":
		w.newline(false)
		w.listPfx = "- "
	case "blockquote":
		w.newline(true)
		w.listPfx = "> "
	case "pre":
		w.newline(true)
		if w.pre == 0 {
			w.b.WriteString("```\n")
		}
		w.pre++
	case "tr":
		w.newline(false)
	case "td", "th":
		if w.lineDone {
			w.out().WriteString(" | ")
			w.space = false
		}
	case "a":
		if w.inLink {
			return
		}
		target := resolveLink(w.base, attrs["href"])
		if target == "" {
			return
		}
		if w.space && w.lineDone {
			w.b.WriteByte(' ')
			w.space = false
		}
		w.inLink, w.link = true, target
		w.linkText.Reset()
	case "img":
		if alt := collapseSpace(attrs["alt"]); alt != "" {
			w.text(" [image: " + alt + "] ")
		}
	default:
		if blockElements[name] || name == "p" {
			w.newline(true)
		}
	}
}

func (w *mdWriter) close(name string) {
	switch name {
	case "a":
		if !w.inLink {
			return
		}
		w.inLink = false
		label := collapseSpace(w.linkText.String())
		if label == "" {
			return
		}
		if w.listPfx != "" {
			w.b.WriteString(w.listPfx)
			w.listPfx = ""
		} else if w.space && w.lineDone {
			w.b.WriteByte(' ')
		}
		w.b.WriteString("[" + strings.NewReplacer("[", "(", "]", ")").Replace(label) + "](" + w.link + ")")
		w.lineDone, w.space = true, false
	case "pre":
		if w.pre > 0 {
			w.pre--
			if w.pre == 0 {
				if !strings.HasSuffix(w.b.String(), "\n") {
					w.b.WriteByte('\n')
				}
				w.b.WriteString("```")
				w.newline(true)
			}
		}
	case "h1", "h2", "h3", "h4", "h5", "h6", "li", "blockquote", "tr":
		w.listPfx = ""
		w.newline(name != "li" && name != "tr")
	default:
		if blockElements[name] {
			w.newline(true)
		}
	}
}

func (w *mdWriter) finish() string {
	if w.inLink {
		w.inLink = false
		w.b.WriteString(collapseSpace(w.linkText.String()))
	}
	if w.pre > 0 {
		w.b.WriteString("\n```")
	}
	lines := strings.Split(w.b.String(), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
			l = ""
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// resolveLink is href as an absolute http(s) URL, or "" for anything else
// (javascript:, data:, mailto:, a fragment alone).
func resolveLink(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") || len(href) > 2048 {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	u.Fragment = ""
	s := u.String()
	if strings.ContainsAny(s, " ()<>\n") {
		return ""
	}
	return s
}

func collapseSpace(s string) string { return strings.Join(strings.FieldsFunc(s, isSpace), " ") }
