package roomstyle

import "errors"

// maxDepth bounds (), [], {} and function nesting. The parser recurses, so this
// is also its stack bound; deeper input is rejected, never truncated.
const maxDepth = 48

var errTooDeep = errors.New("stylesheet nests brackets or functions more than 48 levels deep")

// cv is a component value (§5.3): a preserved token, a function with its
// arguments, or a simple block (kind tLParen, tLBracket or tLBrace) with contents.
type cv struct {
	token
	kids []cv
}

func closer(k kind) kind {
	switch k {
	case tLParen, tFunction:
		return tRParen
	case tLBracket:
		return tRBracket
	}
	return tRBrace
}

// parseValues consumes the whole input as a list of component values.
func parseValues(css string) ([]cv, error) {
	t := newTokenizer(css)
	var out []cv
	for {
		tok := t.next()
		if tok.kind == tEOF {
			return out, nil
		}
		v, err := consumeValue(t, tok, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func consumeValue(t *tokenizer, tok token, depth int) (cv, error) {
	switch tok.kind {
	case tFunction, tLParen, tLBracket, tLBrace:
	default:
		return cv{token: tok}, nil
	}
	if depth >= maxDepth {
		return cv{}, errTooDeep
	}
	v := cv{token: tok}
	end := closer(tok.kind)
	for {
		next := t.next()
		if next.kind == tEOF || next.kind == end {
			return v, nil
		}
		kid, err := consumeValue(t, next, depth+1)
		if err != nil {
			return cv{}, err
		}
		v.kids = append(v.kids, kid)
	}
}

// rule is an at-rule or qualified rule (§5.4). block is nil for a statement
// at-rule such as "@import x;" or "@layer a, b;".
type rule struct {
	at      string // at-keyword name as written; "" for a qualified rule
	prelude []cv
	block   *cv
	line    int
}

// parseRules reads a rule list from component values: a stylesheet's top level
// or the contents of a grouping at-rule's block. A qualified rule that never
// reaches its {} block is dropped, as a browser drops it.
func parseRules(list []cv, topLevel bool) []rule {
	var out []rule
	for i := 0; i < len(list); i++ {
		v := list[i]
		switch {
		case v.kind == tWhitespace || v.kind == tSemicolon && !topLevel:
			continue
		case topLevel && (v.kind == tCDO || v.kind == tCDC):
			continue
		case v.kind == tAtKeyword:
			r := rule{at: v.value, line: v.line}
			for i++; i < len(list); i++ {
				if list[i].kind == tSemicolon {
					break
				}
				if list[i].kind == tLBrace {
					block := list[i]
					r.block = &block
					break
				}
				r.prelude = append(r.prelude, list[i])
			}
			out = append(out, r)
		default:
			r := rule{line: v.line}
			for ; i < len(list); i++ {
				if list[i].kind == tLBrace {
					block := list[i]
					r.block = &block
					break
				}
				r.prelude = append(r.prelude, list[i])
			}
			if r.block != nil {
				out = append(out, r)
			}
		}
	}
	return out
}

// declaration is one "name: value [!important]" from a style block.
type declaration struct {
	name      string
	value     []cv
	important bool
	line      int
}

// parseDeclarations reads a style block's contents. Anything that is not a
// plain declaration (a nested rule, a nested at-rule) is reported through
// dropped and never reaches the output.
func parseDeclarations(list []cv) (decls []declaration, dropped []int) {
	for i := 0; i < len(list); i++ {
		if list[i].kind == tWhitespace || list[i].kind == tSemicolon {
			continue
		}
		start := i
		for i < len(list) && list[i].kind != tSemicolon {
			i++
		}
		item := list[start:i]
		d, ok := declarationFrom(item)
		if !ok {
			dropped = append(dropped, item[0].line)
			continue
		}
		decls = append(decls, d)
	}
	return decls, dropped
}

func declarationFrom(item []cv) (declaration, bool) {
	if len(item) == 0 || item[0].kind != tIdent {
		return declaration{}, false
	}
	d := declaration{name: item[0].value, line: item[0].line}
	rest := trim(item[1:])
	if len(rest) == 0 || rest[0].kind != tColon {
		return declaration{}, false
	}
	value := trim(rest[1:])
	// A trailing "! important" (any case, whitespace allowed) is the flag.
	if n := len(value); n >= 2 && value[n-1].kind == tIdent && equalFold(value[n-1].value, "important") {
		head := trim(value[:n-1])
		if m := len(head); m >= 1 && head[m-1].kind == tDelim && head[m-1].value == "!" {
			d.important = true
			value = trim(head[:m-1])
		}
	}
	d.value = value
	return d, true
}

func trim(list []cv) []cv {
	for len(list) > 0 && list[0].kind == tWhitespace {
		list = list[1:]
	}
	for len(list) > 0 && list[len(list)-1].kind == tWhitespace {
		list = list[:len(list)-1]
	}
	return list
}

// splitCommas splits a component value list at its top-level commas.
func splitCommas(list []cv) [][]cv {
	var out [][]cv
	start := 0
	for i, v := range list {
		if v.kind == tComma {
			out = append(out, trim(list[start:i]))
			start = i + 1
		}
	}
	return append(out, trim(list[start:]))
}

// equalFold is ASCII case-insensitive comparison, the only folding CSS does.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func asciiLower(s string) string {
	b := []byte(s)
	for i := range b {
		b[i] = lower(b[i])
	}
	return string(b)
}
