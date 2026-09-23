package roomstyle

import "slices"

// At-rules: conditional groups (@media, @supports, @container) and @layer
// wrap scoped rules; @keyframes and @font-face get room-prefixed names; every
// other at-rule is dropped. @import, @namespace and @charset would load or
// reinterpret; @property, @counter-style, @font-palette-values, @page,
// @view-transition, @position-try and @scope define or reach page-wide state.

// Functions a condition may use. None of them can load anything.
var conditionFunctions = append([]string{"selector", "font-tech", "font-format", "style", "scroll-state",
	"not", "is", "where", "has", "nth-child", "nth-last-child", "nth-of-type", "nth-last-of-type"}, valueFunctions...)

func (s *sanitizer) atRule(r rule, depth int) {
	name := asciiLower(r.at)
	switch name {
	case "media", "supports", "container":
		if r.block == nil {
			s.warn(r.line, "dropped @%s without a block", name)
			return
		}
		if depth >= MaxAtDepth {
			s.warn(r.line, "dropped @%s: at-rules nest more than %d deep", name, MaxAtDepth)
			return
		}
		prelude := collapse(r.prelude)
		if len(prelude) == 0 || !conditionOK(prelude, 0) {
			s.warn(r.line, "dropped @%s: unsupported condition", name)
			return
		}
		s.group(r, depth, []cv{{token: token{kind: tAtKeyword, value: name}}, {token: token{kind: tWhitespace, value: " "}}}, prelude)
	case "layer":
		names, ok := s.layerNames(r.prelude, r.block == nil)
		if !ok {
			s.warn(r.line, "dropped @layer: malformed layer name")
			return
		}
		head := []cv{{token: token{kind: tAtKeyword, value: "layer"}}}
		if len(names) > 0 {
			head = append(head, cv{token: token{kind: tWhitespace, value: " "}})
		}
		if r.block == nil {
			if !s.count() {
				return
			}
			s.out = append(append(append(s.out, head...), names...), cv{token: token{kind: tSemicolon}})
			s.newline()
			return
		}
		if depth >= MaxAtDepth {
			s.warn(r.line, "dropped @layer: at-rules nest more than %d deep", MaxAtDepth)
			return
		}
		s.group(r, depth, head, names)
	case "keyframes":
		s.keyframesRule(r)
	case "font-face":
		s.fontFace(r)
	case "import":
		s.warn(r.line, "dropped @import: a room stylesheet cannot load another")
	default:
		s.warn(r.line, "dropped @%s: not supported in a room stylesheet", clip(name))
	}
}

// group emits a grouping at-rule whose block holds scoped rules.
func (s *sanitizer) group(r rule, depth int, head, prelude []cv) {
	saved := s.out
	s.out = nil
	s.rules(parseRules(r.block.kids, false), depth+1)
	inner := s.out
	s.out = saved
	if len(inner) == 0 || s.err != nil || !s.count() {
		return
	}
	s.out = append(append(s.out, head...), prelude...)
	s.out = append(s.out, cv{token: token{kind: tLBrace}, kids: inner})
	s.newline()
}

func conditionOK(list []cv, depth int) bool {
	if depth > maxDepth {
		return false
	}
	for _, v := range list {
		switch v.kind {
		case tIdent, tNumber, tPercentage, tDimension, tWhitespace, tComma, tColon, tString, tHash:
		case tDelim:
			if v.value == `\` || v.value == "&" {
				return false
			}
		case tLParen, tLBracket:
			if !conditionOK(v.kids, depth+1) {
				return false
			}
		case tFunction:
			if !slices.Contains(conditionFunctions, asciiLower(v.value)) || !conditionOK(v.kids, depth+1) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// layerNames prefixes each layer name. A statement needs at least one name; a
// block takes at most one (an anonymous layer has none).
func (s *sanitizer) layerNames(prelude []cv, statement bool) ([]cv, bool) {
	p := trim(prelude)
	if len(p) == 0 {
		return nil, !statement
	}
	var out []cv
	items := splitCommas(p)
	if !statement && len(items) != 1 {
		return nil, false
	}
	for i, item := range items {
		if len(item) == 0 || item[0].kind != tIdent || slices.Contains(cssWide, asciiLower(item[0].value)) {
			return nil, false
		}
		if i > 0 {
			out = append(out, cv{token: token{kind: tComma}})
		}
		out = append(out, cv{token: token{kind: tIdent, value: s.prefixed(item[0].value)}})
		for j := 1; j < len(item); j += 2 {
			if j+1 >= len(item) || !delimIs(item[j], ".") || item[j+1].kind != tIdent {
				return nil, false
			}
			out = append(out, item[j], item[j+1])
		}
	}
	return out, true
}

func (s *sanitizer) keyframesRule(r rule) {
	name, ok := keyframesName(r.prelude)
	if !ok || r.block == nil {
		s.warn(r.line, "dropped @keyframes: malformed name")
		return
	}
	var body []cv
	for _, kf := range parseRules(r.block.kids, false) {
		if kf.at != "" {
			s.warn(kf.line, "dropped @%s inside @keyframes", clip(kf.at))
			continue
		}
		var sel []cv
		valid := true
		for i, item := range splitCommas(kf.prelude) {
			if len(item) != 1 || !(item[0].kind == tPercentage || (item[0].kind == tIdent && (equalFold(item[0].value, "from") || equalFold(item[0].value, "to")))) {
				valid = false
				break
			}
			if i > 0 {
				sel = append(sel, cv{token: token{kind: tComma}})
			}
			sel = append(sel, item[0])
		}
		if !valid {
			s.warn(kf.line, "dropped a keyframe: selectors must be from, to or percentages")
			continue
		}
		decls := s.declarations(kf.block.kids, declKeyframe)
		if len(decls) == 0 {
			continue
		}
		body = append(append(body, sel...), cv{token: token{kind: tLBrace}, kids: decls})
	}
	if len(body) == 0 || !s.count() {
		return
	}
	s.out = append(s.out,
		cv{token: token{kind: tAtKeyword, value: "keyframes"}},
		cv{token: token{kind: tWhitespace, value: " "}},
		cv{token: token{kind: tIdent, value: s.prefixed(name)}},
		cv{token: token{kind: tLBrace}, kids: body})
	s.newline()
}

func (s *sanitizer) fontFace(r rule) {
	if r.block == nil || len(trim(r.prelude)) != 0 {
		s.warn(r.line, "dropped malformed @font-face")
		return
	}
	decls := s.declarations(r.block.kids, declFontFace)
	var family, src bool
	for i := 0; i+1 < len(decls); i++ {
		if decls[i].kind == tIdent && decls[i+1].kind == tColon {
			family = family || decls[i].value == "font-family"
			src = src || decls[i].value == "src"
		}
	}
	if !family || !src {
		s.warn(r.line, "dropped @font-face: it needs a font-family and a src naming an attachment of this room")
		return
	}
	if !s.count() {
		return
	}
	s.out = append(s.out, cv{token: token{kind: tAtKeyword, value: "font-face"}}, cv{token: token{kind: tLBrace}, kids: decls})
	s.newline()
}
