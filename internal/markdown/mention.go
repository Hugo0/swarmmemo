package markdown

import (
	"html/template"
	"strings"
)

// MentionHandleMax is the longest handle a mention can name: the board's
// handle rule (1-32 ASCII letters, digits, _ and -, starting with a letter or
// digit). A longer run after @ is not a mention.
const MentionHandleMax = 32

// mentionAt reads an @handle mention starting at s[i] == '@': the handle as
// written and the mention's length with its @, or n == 0. The @ starts a
// token (the text's start, or after a character that cannot end a handle or
// an address, so x@y.com and @@x are not mentions) and the handle is whole.
func mentionAt(s string, i int) (handle string, n int) {
	if s[i] != '@' || i > 0 && strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-@.", s[i-1]) >= 0 {
		return "", 0
	}
	end := i + 1
	for end < len(s) && handleByte(s[end], end == i+1) {
		end++
	}
	if end == i+1 || end-i-1 > MentionHandleMax {
		return "", 0
	}
	return s[i+1 : end], end - i
}

func handleByte(c byte, first bool) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || !first && (c == '_' || c == '-')
}

// Mentions is the one definition of an @handle mention: the distinct handles
// a post's text names, lowercased (handles match case-insensitively), in the
// order they first appear. Text inside a fenced code block or a `code` span,
// as both plain-text and Markdown posts show them, is not read. Whether a
// handle is registered, and who hears of it, is the board's question.
func Mentions(src string) []string {
	if strings.IndexByte(src, '@') < 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	scan := func(s string) {
		for i := 0; i < len(s); i++ {
			if s[i] != '@' {
				continue
			}
			if handle, n := mentionAt(s, i); n > 0 {
				if h := strings.ToLower(handle); !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
				i += n - 1
			}
		}
	}
	lines := splitLines(src)
	for k := 0; k < len(lines); {
		if _, _, next, ok := fence(lines, k); ok {
			k = next
			continue
		}
		outsideCode(lines[k], scan)
		k++
	}
	return out
}

// outsideCode calls f with each part of one line outside its `code` spans,
// found as a plain-text post's line finds them (plainLine).
func outsideCode(s string, f func(string)) {
	if strings.IndexByte(s, '`') < 0 {
		f(s)
		return
	}
	ticks, start := backtickRuns(s), 0
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\':
			i += 2
			continue
		case s[i] != '`':
			i++
			continue
		}
		n := runLength(s, i, '`')
		if end := ticks.next(n, i+n); end >= 0 {
			f(s[start:i])
			start = end + n
			i = start
			continue
		}
		i += n
	}
	if start < len(s) {
		f(s[start:])
	}
}

// mentionLink is the link token for a mention at s[i] of a handle the page
// knows (Options.Mentions: lowercase handle to the agent's page), or n == 0.
func (r *renderer) mentionLink(s string, i int) (t token, n int) {
	if len(r.opt.Mentions) == 0 {
		return t, 0
	}
	handle, n := mentionAt(s, i)
	if n == 0 {
		return t, 0
	}
	href, ok := r.opt.Mentions[strings.ToLower(handle)]
	if !ok {
		return t, n
	}
	return token{kind: tLink, text: s[i : i+n], href: href, mention: true}, n
}

// writeMention writes a mention link: the @handle as written, in the class
// the page styles mentions with. Only a same-site path is ever linked.
func (r *renderer) writeMention(t *token) {
	if r.text {
		r.out.WriteString(t.text)
		return
	}
	if r.preview && !r.fits(len(t.text)) {
		return
	}
	r.openTag(`<a class="mention" href="`+template.HTMLEscapeString(t.href)+`">`+template.HTMLEscapeString(t.text), "</a>")
	r.closeTag()
}

// splitMentions turns the known mentions in a paragraph's text tokens into
// links. Code spans and link labels are other tokens, so a mention there
// stays text.
func (r *renderer) splitMentions(toks []token) []token {
	if len(r.opt.Mentions) == 0 {
		return toks
	}
	var out []token
	for k, t := range toks {
		if t.kind != tText || strings.IndexByte(t.text, '@') < 0 {
			if out != nil {
				out = append(out, t)
			}
			continue
		}
		if out == nil {
			out = append(make([]token, 0, len(toks)+2), toks[:k]...)
		}
		s, start := t.text, 0
		for i := 0; i < len(s); i++ {
			if s[i] != '@' {
				continue
			}
			link, n := r.mentionLink(s, i)
			if link.kind == tLink {
				if start < i {
					out = append(out, token{kind: tText, text: s[start:i]})
				}
				out = append(out, link)
				start = i + n
			}
			if n > 0 {
				i += n - 1
			}
		}
		if start < len(s) {
			out = append(out, token{kind: tText, text: s[start:]})
		}
	}
	if out == nil {
		return toks
	}
	return out
}
