package roomstyle

import (
	"strconv"
	"strings"
)

// The serializer writes component values back as text a browser tokenizes into
// exactly the same values. Adjacent tokens that would merge get an empty comment
// between them (CSS Syntax §9); whitespace is never inserted, because in a
// selector it is a combinator. Sanitize proves the round trip on every output.

type writer struct {
	b    strings.Builder
	prev token
}

func serialize(list []cv) string {
	w := &writer{prev: token{kind: tWhitespace}}
	w.values(list)
	return w.b.String()
}

func (w *writer) values(list []cv) {
	for _, v := range list {
		w.value(v)
	}
}

func (w *writer) value(v cv) {
	switch v.kind {
	case tFunction:
		w.emit(v.token, escapeIdent(v.value)+"(")
		w.values(v.kids)
		w.emit(token{kind: tRParen}, ")")
	case tLParen, tLBracket, tLBrace:
		open, end := map[kind]string{tLParen: "(", tLBracket: "[", tLBrace: "{"}[v.kind], closer(v.kind)
		w.emit(v.token, open)
		w.values(v.kids)
		w.emit(token{kind: end}, map[kind]string{tRParen: ")", tRBracket: "]", tRBrace: "}"}[end])
	default:
		w.emit(v.token, text(v.token))
	}
}

func (w *writer) emit(t token, s string) {
	if needsComment(w.prev, t) {
		w.b.WriteString("/**/")
	}
	w.b.WriteString(s)
	w.prev = t
}

func text(t token) string {
	switch t.kind {
	case tIdent:
		return escapeIdent(t.value)
	case tAtKeyword:
		return "@" + escapeIdent(t.value)
	case tHash:
		return "#" + escapeName(t.value)
	case tString:
		return quote(t.value)
	case tURL:
		return "url(" + quote(t.value) + ")"
	case tDelim:
		return t.value
	case tNumber:
		return t.num
	case tPercentage:
		return t.num + "%"
	case tDimension:
		return t.num + escapeUnit(t.value)
	case tWhitespace:
		if t.value == "\n" {
			return "\n"
		}
		return " "
	case tCDO:
		return "<!--"
	case tCDC:
		return "-->"
	case tColon:
		return ":"
	case tSemicolon:
		return ";"
	case tComma:
		return ","
	case tRParen:
		return ")"
	case tRBracket:
		return "]"
	case tRBrace:
		return "}"
	}
	// Bad strings and bad URLs never reach the serializer: Sanitize rejects them.
	return ""
}

// needsComment reports whether b, written right after a, would merge with it
// into a different token (the pairs of CSS Syntax §9, plus a few conservatively).
func needsComment(a, b token) bool {
	numeric := b.kind == tNumber || b.kind == tPercentage || b.kind == tDimension
	first := byte(0)
	if numeric {
		first = b.num[0]
	}
	name := b.kind == tIdent || b.kind == tFunction || b.kind == tURL
	// After a name, a number joins it when it starts with a name code point.
	nameNumber := numeric && (isDigit(rune(first)) || first == '-')
	delim := func(t token, s string) bool { return t.kind == tDelim && t.value == s }
	switch {
	case a.kind == tIdent:
		return name || nameNumber || delim(b, "-") || b.kind == tCDC || b.kind == tLParen
	case a.kind == tAtKeyword || a.kind == tHash || a.kind == tDimension:
		return name || nameNumber || delim(b, "-") || b.kind == tCDC
	case a.kind == tNumber:
		return name || (numeric && (isDigit(rune(first)) || first == '.')) || delim(b, "%")
	case delim(a, "#") || delim(a, "@"):
		return name || nameNumber || delim(b, "-")
	case delim(a, "-"):
		return name || numeric || delim(b, "-") || delim(b, ">")
	case delim(a, ".") || delim(a, "+"):
		return b.kind == tNumber || b.kind == tPercentage || b.kind == tDimension
	case delim(a, "/"):
		return delim(b, "*")
	case delim(a, "<"):
		return delim(b, "!")
	case delim(a, "\\"):
		return true
	}
	return false
}

func hexEscape(c rune) string { return `\` + strconv.FormatInt(int64(c), 16) + " " }

// escapeIdent is CSSOM "serialize an identifier".
func escapeIdent(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, c := range runes {
		switch {
		case c == 0:
			b.WriteRune('�')
		case (c >= 1 && c <= 0x1F) || c == 0x7F:
			b.WriteString(hexEscape(c))
		case i == 0 && isDigit(c), i == 1 && isDigit(c) && runes[0] == '-':
			b.WriteString(hexEscape(c))
		case i == 0 && c == '-' && len(runes) == 1:
			b.WriteString(`\-`)
		case isName(c):
			b.WriteRune(c)
		default:
			b.WriteString(`\` + string(c))
		}
	}
	return b.String()
}

// escapeName serializes a hash name, which may begin with a digit.
func escapeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c == 0:
			b.WriteRune('�')
		case (c >= 1 && c <= 0x1F) || c == 0x7F:
			b.WriteString(hexEscape(c))
		case isName(c):
			b.WriteRune(c)
		default:
			b.WriteString(`\` + string(c))
		}
	}
	return b.String()
}

// escapeUnit keeps a dimension's unit from rejoining its number: a unit "e3"
// written raw after "1" would read back as the number 1e3.
func escapeUnit(s string) string {
	out := escapeIdent(s)
	if len(out) >= 2 && (out[0] == 'e' || out[0] == 'E') &&
		(isDigit(rune(out[1])) || (out[1] == '-' && len(out) >= 3 && isDigit(rune(out[2])))) {
		return hexEscape(rune(out[0])) + out[1:]
	}
	return out
}

// quote writes a double-quoted string. "<" is escaped too, so no output can
// contain "</style" even if it is ever inlined into an HTML page.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == 0:
			b.WriteRune('�')
		case (c >= 1 && c <= 0x1F) || c == 0x7F || c == '<':
			b.WriteString(hexEscape(c))
		case c == '"' || c == '\\':
			b.WriteString(`\` + string(c))
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
