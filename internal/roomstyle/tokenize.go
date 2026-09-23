package roomstyle

import (
	"strings"
	"unicode/utf8"
)

// A tokenizer for CSS Syntax Level 3 (§4). Escapes are decoded here, so every
// later check sees the name a browser would see: "u\72l(" is the function url.

type kind uint8

const (
	tEOF kind = iota
	tIdent
	tFunction
	tAtKeyword
	tHash
	tString
	tBadString
	tURL
	tBadURL
	tDelim
	tNumber
	tPercentage
	tDimension
	tWhitespace
	tCDO
	tCDC
	tColon
	tSemicolon
	tComma
	tLBracket
	tRBracket
	tLParen
	tRParen
	tLBrace
	tRBrace
)

type token struct {
	kind kind
	// value: ident, function or at-keyword name, hash name, string or URL
	// contents, dimension unit, the delim rune, or whitespace (" " or "\n").
	value string
	// num is the source text of a number, percentage or dimension. It contains
	// only [+-.0-9eE], so it re-tokenizes to the same number.
	num  string
	line int
}

type tokenizer struct {
	src  []rune
	pos  int
	line int
}

func newTokenizer(css string) *tokenizer {
	// §3.3 preprocessing: CRLF, CR and FF become LF; NUL and invalid UTF-8 become U+FFFD.
	css = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\f", "\n", "\x00", "�").Replace(css)
	if !utf8.ValidString(css) {
		css = strings.ToValidUTF8(css, "�")
	}
	return &tokenizer{src: []rune(css), line: 1}
}

func (t *tokenizer) peek(n int) rune {
	if t.pos+n < len(t.src) {
		return t.src[t.pos+n]
	}
	return -1
}

func (t *tokenizer) advance() rune {
	c := t.peek(0)
	if c >= 0 {
		t.pos++
		if c == '\n' {
			t.line++
		}
	}
	return c
}

func isDigit(c rune) bool        { return c >= '0' && c <= '9' }
func isHex(c rune) bool          { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }
func isLetter(c rune) bool       { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isNameStart(c rune) bool    { return isLetter(c) || c == '_' || c >= 0x80 }
func isName(c rune) bool         { return isNameStart(c) || isDigit(c) || c == '-' }
func isSpace(c rune) bool        { return c == '\n' || c == '\t' || c == ' ' }
func validEscape(a, b rune) bool { return a == '\\' && b != '\n' }

func nonPrintable(c rune) bool {
	return (c >= 0 && c <= 8) || c == 0x0B || (c >= 0x0E && c <= 0x1F) || c == 0x7F
}

func startsIdent(a, b, c rune) bool {
	switch {
	case a == '-':
		return isNameStart(b) || b == '-' || validEscape(b, c)
	case isNameStart(a):
		return true
	case a == '\\':
		return validEscape(a, b)
	}
	return false
}

func startsNumber(a, b, c rune) bool {
	switch {
	case a == '+' || a == '-':
		return isDigit(b) || (b == '.' && isDigit(c))
	case a == '.':
		return isDigit(b)
	}
	return isDigit(a)
}

func (t *tokenizer) next() token {
	for t.peek(0) == '/' && t.peek(1) == '*' {
		t.pos += 2
		for {
			c := t.advance()
			if c < 0 || (c == '*' && t.peek(0) == '/') {
				t.advance()
				break
			}
		}
	}
	line := t.line
	tok := t.consume()
	tok.line = line
	return tok
}

func (t *tokenizer) consume() token {
	c := t.peek(0)
	switch {
	case c < 0:
		return token{kind: tEOF}
	case isSpace(c):
		ws := " "
		for isSpace(t.peek(0)) {
			if t.advance() == '\n' {
				ws = "\n"
			}
		}
		return token{kind: tWhitespace, value: ws}
	case c == '"' || c == '\'':
		t.advance()
		return t.string(c)
	case c == '#':
		t.advance()
		if isName(t.peek(0)) || validEscape(t.peek(0), t.peek(1)) {
			return token{kind: tHash, value: t.name()}
		}
		return token{kind: tDelim, value: "#"}
	case c == '(':
		t.advance()
		return token{kind: tLParen}
	case c == ')':
		t.advance()
		return token{kind: tRParen}
	case c == '[':
		t.advance()
		return token{kind: tLBracket}
	case c == ']':
		t.advance()
		return token{kind: tRBracket}
	case c == '{':
		t.advance()
		return token{kind: tLBrace}
	case c == '}':
		t.advance()
		return token{kind: tRBrace}
	case c == ',':
		t.advance()
		return token{kind: tComma}
	case c == ':':
		t.advance()
		return token{kind: tColon}
	case c == ';':
		t.advance()
		return token{kind: tSemicolon}
	case c == '+' || c == '.':
		if startsNumber(c, t.peek(1), t.peek(2)) {
			return t.numeric()
		}
	case c == '-':
		if startsNumber(c, t.peek(1), t.peek(2)) {
			return t.numeric()
		}
		if t.peek(1) == '-' && t.peek(2) == '>' {
			t.pos += 3
			return token{kind: tCDC}
		}
		if startsIdent(c, t.peek(1), t.peek(2)) {
			return t.identLike()
		}
	case c == '<':
		if t.peek(1) == '!' && t.peek(2) == '-' && t.peek(3) == '-' {
			t.pos += 4
			return token{kind: tCDO}
		}
	case c == '@':
		if startsIdent(t.peek(1), t.peek(2), t.peek(3)) {
			t.advance()
			return token{kind: tAtKeyword, value: t.name()}
		}
	case c == '\\':
		if validEscape(c, t.peek(1)) {
			return t.identLike()
		}
	case isDigit(c):
		return t.numeric()
	case isNameStart(c):
		return t.identLike()
	}
	t.advance()
	return token{kind: tDelim, value: string(c)}
}

// escape consumes the code points after a backslash (§4.3.7).
func (t *tokenizer) escape() rune {
	c := t.advance()
	if c < 0 {
		return utf8.RuneError
	}
	if !isHex(c) {
		return c
	}
	v := hexVal(c)
	for i := 0; i < 5 && isHex(t.peek(0)); i++ {
		v = v*16 + hexVal(t.advance())
	}
	if isSpace(t.peek(0)) {
		t.advance()
	}
	if v == 0 || (v >= 0xD800 && v <= 0xDFFF) || v > 0x10FFFF {
		return utf8.RuneError
	}
	return v
}

func hexVal(c rune) rune {
	switch {
	case isDigit(c):
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func (t *tokenizer) name() string {
	var b strings.Builder
	for {
		c := t.peek(0)
		switch {
		case isName(c):
			b.WriteRune(t.advance())
		case validEscape(c, t.peek(1)):
			t.advance()
			b.WriteRune(t.escape())
		default:
			return b.String()
		}
	}
}

func (t *tokenizer) string(end rune) token {
	var b strings.Builder
	for {
		c := t.peek(0)
		switch {
		case c < 0:
			return token{kind: tString, value: b.String()}
		case c == end:
			t.advance()
			return token{kind: tString, value: b.String()}
		case c == '\n':
			return token{kind: tBadString}
		case c == '\\':
			t.advance()
			switch next := t.peek(0); {
			case next < 0:
			case next == '\n':
				t.advance()
			default:
				b.WriteRune(t.escape())
			}
		default:
			b.WriteRune(t.advance())
		}
	}
}

func (t *tokenizer) number() string {
	start := t.pos
	if c := t.peek(0); c == '+' || c == '-' {
		t.advance()
	}
	for isDigit(t.peek(0)) {
		t.advance()
	}
	if t.peek(0) == '.' && isDigit(t.peek(1)) {
		t.advance()
		for isDigit(t.peek(0)) {
			t.advance()
		}
	}
	if c := t.peek(0); c == 'e' || c == 'E' {
		if isDigit(t.peek(1)) || ((t.peek(1) == '+' || t.peek(1) == '-') && isDigit(t.peek(2))) {
			t.advance()
			t.advance()
			for isDigit(t.peek(0)) {
				t.advance()
			}
		}
	}
	return string(t.src[start:t.pos])
}

func (t *tokenizer) numeric() token {
	num := t.number()
	if startsIdent(t.peek(0), t.peek(1), t.peek(2)) {
		return token{kind: tDimension, num: num, value: t.name()}
	}
	if t.peek(0) == '%' {
		t.advance()
		return token{kind: tPercentage, num: num}
	}
	return token{kind: tNumber, num: num}
}

func (t *tokenizer) identLike() token {
	name := t.name()
	if strings.EqualFold(name, "url") && t.peek(0) == '(' {
		t.advance()
		for isSpace(t.peek(0)) && isSpace(t.peek(1)) {
			t.advance()
		}
		q := t.peek(0)
		if isSpace(q) {
			q = t.peek(1)
		}
		if q == '"' || q == '\'' {
			return token{kind: tFunction, value: name}
		}
		return t.url()
	}
	if t.peek(0) == '(' {
		t.advance()
		return token{kind: tFunction, value: name}
	}
	return token{kind: tIdent, value: name}
}

func (t *tokenizer) url() token {
	var b strings.Builder
	for isSpace(t.peek(0)) {
		t.advance()
	}
	for {
		c := t.advance()
		switch {
		case c < 0 || c == ')':
			return token{kind: tURL, value: b.String()}
		case isSpace(c):
			for isSpace(t.peek(0)) {
				t.advance()
			}
			if n := t.peek(0); n < 0 || n == ')' {
				t.advance()
				return token{kind: tURL, value: b.String()}
			}
			t.badURLRemnants()
			return token{kind: tBadURL}
		case c == '"' || c == '\'' || c == '(' || nonPrintable(c):
			t.badURLRemnants()
			return token{kind: tBadURL}
		case c == '\\':
			if !validEscape(c, t.peek(0)) {
				t.badURLRemnants()
				return token{kind: tBadURL}
			}
			b.WriteRune(t.escape())
		default:
			b.WriteRune(c)
		}
	}
}

func (t *tokenizer) badURLRemnants() {
	for {
		c := t.peek(0)
		if c < 0 {
			return
		}
		if validEscape(c, t.peek(1)) {
			t.advance()
			t.escape()
			continue
		}
		t.advance()
		if c == ')' {
			return
		}
	}
}
