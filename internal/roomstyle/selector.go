package roomstyle

import "slices"

// Selectors are re-rooted under the room's class on <html>: "p a" becomes
// ".room-style-X p a", and a leading ":scope" is <html> itself. Class names
// resolve through Hooks. A selector is in the body zone when its subject is at
// or below the post-body hook (the canvas root) with no sibling step off it;
// every other selector is in the page zone, where declarations are restricted.

type selMode uint8

const (
	selTop      selMode = iota // a rule's complex selector
	selCompound                // an :is(), :where() or :not() argument: one compound, no combinators
	selRelative                // a :has() argument: relative to an element inside a body
)

var (
	// Structural pseudo-classes and user-action states. Anything absent, such as
	// :root, :host or a vendor pseudo-class, is refused.
	pseudoClasses = []string{
		"hover", "active", "focus", "focus-visible", "focus-within", "link", "any-link", "visited", "target",
		"first-child", "last-child", "only-child", "first-of-type", "last-of-type", "only-of-type", "empty",
		"checked", "disabled", "enabled", "default", "indeterminate", "placeholder-shown", "read-only", "read-write",
		"required", "optional", "valid", "invalid", "in-range", "out-of-range", "open", "closed", "defined",
		"playing", "paused",
	}
	// Pseudo-elements that draw a box of their own exist only in the body zone:
	// outside it, generated content could overlay or imitate trust UI.
	bodyPseudoElements = []string{"before", "after", "first-line", "first-letter", "marker", "selection", "placeholder", "file-selector-button"}
	pagePseudoElements = []string{"selection", "placeholder"}
	legacyPseudo       = []string{"before", "after", "first-line", "first-letter"}
	// Elements that are the page root or never render. :scope names <html>.
	forbiddenTypes = []string{"html", "head", "title", "script", "style", "meta", "link", "base", "template"}
	// Attributes that carry the site's own class and id vocabulary.
	forbiddenAttributes = []string{"class", "id", "style"}
)

func delimIs(v cv, s string) bool { return v.kind == tDelim && v.value == s }

func isCombinator(v cv) bool { return delimIs(v, ">") || delimIs(v, "+") || delimIs(v, "~") }

// collapse trims a selector and makes each whitespace run a single space.
func collapse(list []cv) []cv {
	list = trim(list)
	out := make([]cv, 0, len(list))
	for _, v := range list {
		if v.kind == tWhitespace {
			if len(out) > 0 && out[len(out)-1].kind == tWhitespace {
				continue
			}
			v.value = " "
		}
		out = append(out, v)
	}
	return out
}

// selector scopes one complex selector and reports its zone, or says why it
// cannot be kept.
func (s *sanitizer) selector(list []cv) ([]cv, bool, string) {
	list = collapse(list)
	if len(list) == 0 {
		return nil, false, "empty selector"
	}
	root := []cv{{token: token{kind: tDelim, value: "."}}, {token: token{kind: tIdent, value: s.scope}}}
	// ":scope" and the room's own class (as in already-scoped output) both name <html>.
	if len(list) >= 2 && ((list[0].kind == tColon && list[1].kind == tIdent && equalFold(list[1].value, "scope")) ||
		(delimIs(list[0], ".") && list[1].kind == tIdent && list[1].value == s.scope)) {
		rest := list[2:]
		if lead := trim(rest); len(lead) > 0 && (delimIs(lead[0], "+") || delimIs(lead[0], "~")) {
			return nil, false, "<html> has no siblings"
		}
		out, body, why := checkSelector(rest, selTop, 0, true, false)
		if why != "" {
			return nil, false, why
		}
		if len(rest) > 0 && rest[0].kind == tWhitespace {
			root = append(root, rest[0]) // a descendant step off <html>
		}
		return append(root, out...), body, ""
	}
	out, body, why := checkSelector(list, selTop, 0, false, false)
	if why != "" {
		return nil, false, why
	}
	return append(append(root, cv{token: token{kind: tWhitespace, value: " "}}), out...), body, ""
}

// checkSelector validates a selector, resolving hook names, and reports whether
// (for a top-level selector) its subject is in the body zone. inCompound means
// the list continues a compound already started (after a leading :scope);
// bodyCtx says a nested argument belongs to a compound already in the body zone.
func checkSelector(list []cv, mode selMode, depth int, inCompound, bodyCtx bool) ([]cv, bool, string) {
	if depth > 3 {
		return nil, false, "selector functions nest too deeply"
	}
	list = trim(list)
	if len(list) == 0 && !inCompound {
		return nil, false, "empty selector"
	}
	out := make([]cv, 0, len(list))
	// inBody: the subject so far is inside a body. curBody: the current compound
	// names the body root. Both matter only for a top-level selector.
	inBody, curBody := false, false
	bodyNow := func() bool { return bodyCtx || inBody || curBody }
	endCompound := func(sibling bool) {
		if curBody {
			inBody = !sibling
		}
		curBody = false
	}
	for i := 0; i < len(list); i++ {
		v := list[i]
		next := func() cv {
			if i+1 < len(list) {
				return list[i+1]
			}
			return cv{}
		}
		switch {
		case v.kind == tWhitespace:
			if mode == selCompound {
				return nil, false, "combinators are not allowed inside :is(), :where() or :not()"
			}
			if !isCombinator(next()) {
				endCompound(false)
			}
			inCompound = false
		case isCombinator(v):
			if mode == selCompound {
				return nil, false, "combinators are not allowed inside :is(), :where() or :not()"
			}
			if !inCompound && i == 0 && mode == selTop && !delimIs(v, ">") {
				return nil, false, "a selector cannot start with + or ~"
			}
			if i == len(list)-1 {
				return nil, false, "dangling combinator"
			}
			endCompound(!delimIs(v, ">"))
			inCompound = false
		case delimIs(v, "*"):
			if delimIs(next(), "|") {
				return nil, false, "namespaces are not supported"
			}
			inCompound = true
		case v.kind == tIdent:
			if slices.Contains(forbiddenTypes, asciiLower(v.value)) {
				return nil, false, "<" + asciiLower(v.value) + "> cannot be styled; :scope is <html>"
			}
			if delimIs(next(), "|") {
				return nil, false, "namespaces are not supported"
			}
			inCompound = true
		case delimIs(v, "."):
			n := next()
			if n.kind != tIdent {
				return nil, false, "malformed class selector"
			}
			class := hookClass(n.value)
			if class == "" {
				return nil, false, "." + n.value + " is not a theme hook; hooks are " + hookList()
			}
			if class == BodyClass && mode == selTop {
				curBody = true
			}
			out = append(out, v, cv{token: token{kind: tIdent, value: class, line: n.line}})
			i++
			inCompound = true
			continue
		case v.kind == tHash:
			return nil, false, "ID selectors are reserved for the site"
		case v.kind == tLBracket:
			if why := checkAttribute(v.kids); why != "" {
				return nil, false, why
			}
			inCompound = true
		case v.kind == tColon:
			n := next()
			if n.kind == tColon || (n.kind == tIdent && slices.Contains(legacyPseudo, asciiLower(n.value))) {
				if n.kind == tColon {
					out = append(out, v)
					i++
				}
				if i+1 >= len(list) || list[i+1].kind != tIdent || mode != selTop {
					return nil, false, "unsupported pseudo-element"
				}
				name := asciiLower(list[i+1].value)
				allowed := pagePseudoElements
				if bodyNow() {
					allowed = bodyPseudoElements
				}
				if !slices.Contains(allowed, name) {
					if slices.Contains(bodyPseudoElements, name) {
						return nil, false, "::" + name + " is allowed only inside .post-body, where generated boxes cannot cover the page"
					}
					return nil, false, "unsupported pseudo-element ::" + name
				}
				out = append(out, list[i:i+2]...)
				i++
				inCompound = true
				continue
			}
			i++
			kid, why := checkPseudo(n, mode, depth, bodyNow())
			if why != "" {
				return nil, false, why
			}
			out = append(out, v, kid)
			inCompound = true
			continue
		case delimIs(v, "&"):
			return nil, false, "nested selectors (&) are not supported"
		case delimIs(v, "|"):
			return nil, false, "namespaces are not supported"
		default:
			return nil, false, "unexpected " + serialize([]cv{v}) + " in selector"
		}
		out = append(out, v)
	}
	if curBody {
		inBody = true
	}
	return out, inBody, ""
}

func checkPseudo(v cv, mode selMode, depth int, body bool) (cv, string) {
	switch v.kind {
	case tIdent:
		name := asciiLower(v.value)
		switch {
		case name == "scope":
			return v, ":scope is allowed only at the start of a selector"
		case name == "root" || name == "host":
			return v, ":" + name + " cannot be styled; :scope is <html>"
		case !slices.Contains(pseudoClasses, name):
			return v, "unsupported pseudo-class :" + name
		}
		return v, ""
	case tFunction:
		name := asciiLower(v.value)
		switch name {
		case "not", "is", "where":
			return checkArgs(v, selCompound, depth, body)
		case "has":
			if mode == selRelative {
				return v, ":has() cannot nest"
			}
			if !body {
				return v, ":has() is allowed only inside .post-body: elsewhere it would probe the page"
			}
			return checkArgs(v, selRelative, depth, body)
		case "nth-child", "nth-last-child", "nth-of-type", "nth-last-of-type":
			for _, k := range v.kids {
				switch {
				case k.kind == tWhitespace || k.kind == tNumber || k.kind == tDimension || delimIs(k, "+") || delimIs(k, "-"):
				case k.kind == tIdent && !equalFold(k.value, "of"):
				default:
					return v, "unsupported :" + name + "() argument"
				}
			}
			return v, ""
		case "lang":
			for _, k := range v.kids {
				if k.kind != tIdent && k.kind != tString && k.kind != tComma && k.kind != tWhitespace {
					return v, "unsupported :lang() argument"
				}
			}
			return v, ""
		case "dir":
			if a := trim(v.kids); len(a) == 1 && a[0].kind == tIdent {
				return v, ""
			}
			return v, "unsupported :dir() argument"
		}
		return v, "unsupported pseudo-class :" + name + "()"
	}
	return v, "malformed pseudo-class"
}

// checkArgs validates and rewrites a selector-list argument.
func checkArgs(v cv, mode selMode, depth int, body bool) (cv, string) {
	var kids []cv
	for i, arg := range splitCommas(v.kids) {
		out, _, why := checkSelector(arg, mode, depth+1, false, body)
		if why != "" {
			return v, why
		}
		if i > 0 {
			kids = append(kids, cv{token: token{kind: tComma}})
		}
		kids = append(kids, out...)
	}
	v.kids = kids
	return v, ""
}

// checkAttribute accepts [name], [name=value], [name op= value], with an
// optional i or s flag, for any attribute but class, id and style.
func checkAttribute(kids []cv) string {
	var list []cv
	for _, k := range kids {
		if k.kind != tWhitespace {
			list = append(list, k)
		}
	}
	if len(list) == 0 || list[0].kind != tIdent {
		return "malformed attribute selector"
	}
	if slices.Contains(forbiddenAttributes, asciiLower(list[0].value)) {
		return "[" + asciiLower(list[0].value) + "] selectors are reserved for the site"
	}
	rest := list[1:]
	if len(rest) == 0 {
		return ""
	}
	switch {
	case delimIs(rest[0], "="):
		rest = rest[1:]
	case len(rest) >= 2 && rest[0].kind == tDelim && (rest[0].value == "~" || rest[0].value == "|" || rest[0].value == "^" || rest[0].value == "$" || rest[0].value == "*") && delimIs(rest[1], "="):
		rest = rest[2:]
	default:
		return "malformed attribute selector"
	}
	if len(rest) == 0 || (rest[0].kind != tIdent && rest[0].kind != tString) {
		return "malformed attribute selector"
	}
	rest = rest[1:]
	if len(rest) == 1 && rest[0].kind == tIdent && (equalFold(rest[0].value, "i") || equalFold(rest[0].value, "s")) {
		return ""
	}
	if len(rest) != 0 {
		return "malformed attribute selector"
	}
	return ""
}
