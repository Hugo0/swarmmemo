package roomstyle

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strings"
)

// Values keep every token except the few that can load, reference or escape:
// functions outside an allowlist (image-set, cross-fade, element, src, paint,
// expression, local, ...), url() to anything but this room's attachments or a
// small inline raster image, {} blocks, at-keywords and bad tokens. Custom
// properties get exactly the same treatment, so var() cannot smuggle anything
// the direct form could not.

var (
	valueFunctions = []string{
		"calc", "min", "max", "clamp", "round", "mod", "rem", "abs", "sign",
		"sin", "cos", "tan", "asin", "acos", "atan", "atan2", "pow", "sqrt", "hypot", "log", "exp",
		"var", "env",
		"rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color", "color-mix", "light-dark",
		"linear-gradient", "radial-gradient", "conic-gradient",
		"repeating-linear-gradient", "repeating-radial-gradient", "repeating-conic-gradient",
		"translate", "translatex", "translatey", "translatez", "translate3d",
		"rotate", "rotatex", "rotatey", "rotatez", "rotate3d",
		"scale", "scalex", "scaley", "scalez", "scale3d",
		"skew", "skewx", "skewy", "matrix", "matrix3d", "perspective",
		"cubic-bezier", "steps", "linear",
		"blur", "brightness", "contrast", "drop-shadow", "grayscale", "hue-rotate", "invert", "opacity", "saturate", "sepia",
		"repeat", "minmax", "fit-content", "counter", "counters",
		"inset", "circle", "ellipse", "polygon", "path", "rect", "xywh",
		"scroll", "view",
		"attr", // content only; see walk
	}
	// Properties that would place an element outside the canvas's containment
	// (view transitions draw in a page-wide overlay; anchors tether to elements
	// elsewhere) or that are legacy script hooks.
	deniedProperties = []string{
		"behavior", "-ms-behavior", "-moz-binding", "binding",
		"view-transition-name", "view-transition-class",
		"anchor-name", "anchor-scope", "position-anchor", "position-area", "inset-area",
		"position-try", "position-try-fallbacks", "position-try-options", "position-try-order", "position-visibility",
	}
	fontFaceDescriptors = []string{
		"font-family", "src", "font-style", "font-weight", "font-stretch", "font-display", "unicode-range",
		"ascent-override", "descent-override", "line-gap-override", "size-adjust",
		"font-feature-settings", "font-variation-settings",
	}
	cssWide = []string{"initial", "inherit", "unset", "revert", "revert-layer", "default", "none"}
)

type declMode uint8

const (
	declStyle declMode = iota // a body-zone style rule
	declPage                  // a page-zone style rule: see zones.go
	declFree                  // a free-zone style rule: see free.go
	declKeyframe
	declFontFace
)

func (m declMode) zone() zone {
	switch m {
	case declPage:
		return zonePage
	case declFree:
		return zoneFree
	}
	return zoneBody
}

// declarations sanitizes a style block's contents into output component values.
func (s *sanitizer) declarations(kids []cv, mode declMode) []cv {
	decls, dropped := parseDeclarations(kids)
	for _, line := range dropped {
		s.warn(line, "dropped a nested rule or malformed declaration")
	}
	var out []cv
	for _, d := range decls {
		value, why := s.declaration(d, mode)
		if why != "" {
			s.warn(d.line, "dropped %s: %s", clip(d.name), why)
			continue
		}
		out = append(out, cv{token: token{kind: tIdent, value: s.propertyName(d.name)}}, cv{token: token{kind: tColon}})
		out = append(out, value...)
		if d.important {
			out = append(out, cv{token: token{kind: tDelim, value: "!"}}, cv{token: token{kind: tIdent, value: "important"}})
		}
		out = append(out, cv{token: token{kind: tSemicolon}})
	}
	return out
}

func (s *sanitizer) propertyName(name string) string {
	if strings.HasPrefix(name, "--") {
		return name // custom property names are case-sensitive
	}
	return asciiLower(name)
}

func (s *sanitizer) declaration(d declaration, mode declMode) ([]cv, string) {
	name := s.propertyName(d.name)
	if len(d.value) == 0 {
		return nil, "empty value"
	}
	switch mode {
	case declFontFace:
		if !slices.Contains(fontFaceDescriptors, name) {
			return nil, "not a supported @font-face descriptor"
		}
		switch name {
		case "font-family":
			family, ok := familyName(d.value)
			if !ok {
				return nil, "malformed font family"
			}
			return []cv{{token: token{kind: tString, value: s.prefixed(strings.ToLower(family))}}}, ""
		case "src":
			return s.fontSrc(d.value)
		}
	case declKeyframe:
		if d.important {
			return nil, "!important is ignored inside @keyframes"
		}
	}
	if slices.Contains(deniedProperties, name) {
		return nil, "this property can place content outside the room"
	}
	out, why := s.walk(name, d.value, 0)
	if why != "" {
		return nil, why
	}
	if name == "cursor" && containsURL(out) {
		return nil, "a cursor image can cover the page outside the room"
	}
	if slices.Contains(trustColorKnobs, name) {
		switch {
		case mode != declPage || !s.scopeRule:
			return nil, "trust colours may be set only in a top-level :scope rule"
		case s.knobErr != nil:
			return nil, s.knobErr.Error()
		}
	}
	if name == TrustFont && !genericFamilyList(out) {
		return nil, TrustFont + " takes only generic families (serif, sans-serif, monospace, system-ui, ...)"
	}
	if isAnimation(name) {
		if why := animationDeclaration(name, out, mode.zone(), mode == declKeyframe, s.frames); why != "" {
			return nil, why
		}
	}
	switch mode {
	case declPage:
		out = pageRewrite(name, out)
		if why := pageDeclaration(name, out); why != "" {
			return nil, why
		}
	case declFree:
		if why := freeDeclaration(name, out); why != "" {
			return nil, why
		}
	}
	if name == "font-family" {
		out = s.renameFamilies(out)
	}
	return out, ""
}

func containsURL(list []cv) bool {
	for _, v := range list {
		if v.kind == tURL || containsURL(v.kids) {
			return true
		}
	}
	return false
}

// walk checks and copies a value, collapsing whitespace.
func (s *sanitizer) walk(prop string, list []cv, depth int) ([]cv, string) {
	if depth > maxDepth {
		return nil, "value nests too deeply"
	}
	out := make([]cv, 0, len(list))
	for _, v := range list {
		switch v.kind {
		case tWhitespace:
			if len(out) == 0 || out[len(out)-1].kind == tWhitespace {
				continue
			}
			v.value = " "
		case tIdent:
			if (prop == "animation" || prop == "animation-name") && s.keyframes[v.value] {
				v.value = s.prefixed(v.value)
			}
		case tString:
			low := strings.ToLower(v.value)
			if strings.Contains(low, "javascript:") || strings.Contains(low, "vbscript:") || strings.Contains(low, "expression(") {
				return nil, "script-like text is not allowed"
			}
			if (prop == "animation" || prop == "animation-name") && s.keyframes[v.value] {
				v = cv{token: token{kind: tIdent, value: s.prefixed(v.value), line: v.line}}
			}
		case tNumber, tPercentage, tDimension, tHash, tComma, tColon:
		case tDelim:
			if v.value == `\` {
				return nil, "stray backslash"
			}
		case tURL:
			u, why := s.url(v.value, false)
			if why != "" {
				return nil, why
			}
			v.value = u
		case tFunction:
			name := asciiLower(v.value)
			if name == "url" {
				args := trim(v.kids)
				if len(args) != 1 || args[0].kind != tString {
					return nil, "malformed url()"
				}
				u, why := s.url(args[0].value, false)
				if why != "" {
					return nil, why
				}
				v = cv{token: token{kind: tURL, value: u, line: v.line}}
				break
			}
			if !slices.Contains(valueFunctions, name) {
				return nil, "function " + name + "() is not allowed"
			}
			if name == "attr" && prop != "content" {
				return nil, "attr() is allowed only in content"
			}
			if name == "var" {
				if a := trim(v.kids); len(a) == 0 || a[0].kind != tIdent || !strings.HasPrefix(a[0].value, "--") {
					return nil, "malformed var()"
				}
			}
			kids, why := s.walk(prop, v.kids, depth+1)
			if why != "" {
				return nil, why
			}
			v.value, v.kids = name, trim(kids)
		case tLParen, tLBracket:
			kids, why := s.walk(prop, v.kids, depth+1)
			if why != "" {
				return nil, why
			}
			v.kids = trim(kids)
		case tLBrace:
			return nil, "{} blocks are not allowed in values"
		default:
			return nil, "unexpected " + describe(v.kind)
		}
		out = append(out, v)
	}
	return trim(out), ""
}

func describe(k kind) string {
	switch k {
	case tBadString:
		return "unterminated string"
	case tBadURL:
		return "malformed url"
	case tAtKeyword:
		return "at-keyword in a value"
	}
	return "token"
}

var imageMagic = map[string][]byte{
	"png":  []byte("\x89PNG\r\n\x1a\n"),
	"jpeg": {0xff, 0xd8, 0xff},
	"gif":  []byte("GIF8"),
	"webp": []byte("RIFF"),
}

// MaxDataURL caps one inline image's base64 text.
const MaxDataURL = 16 << 10

// url returns the canonical form of an allowed URL, or why it is refused.
// Allowed: "/a/<32 hex>" naming a verified attachment of this room, and (not
// for fonts) a base64 PNG, JPEG, GIF or WebP data: URL whose bytes match its type.
// Nothing here can reach another origin, and nothing can reach a same-origin
// endpoint other than a public attachment read.
func (s *sanitizer) url(raw string, font bool) (string, string) {
	if id, ok := strings.CutPrefix(raw, "/a/"); ok && validID(id) {
		// Each check is a database read when serving; a name is checked once,
		// however often the stylesheet repeats it.
		live, seen := s.checked[id]
		if !seen {
			// Each check is a read on the store's only database connection, and
			// room.style.check needs no signature: a sheet of hundreds of made-up
			// IDs would otherwise hold it for tens of milliseconds per call.
			if len(s.checked) >= maxAttachmentChecks {
				return "", "a stylesheet may name at most 16 different attachments"
			}
			live = s.opts.Attachment != nil && s.opts.Attachment(id)
			s.checked[id] = live
		}
		if !live {
			return "", "url() names an attachment that is not a live public attachment of this room"
		}
		if !s.urls[id] && len(s.urls) >= MaxAttachments {
			return "", "a stylesheet may name at most 16 different attachments"
		}
		s.urls[id] = true
		return raw, ""
	}
	if !font && len(raw) > 5 && equalFold(raw[:5], "data:") {
		header, payload, ok := strings.Cut(raw[5:], ",")
		kind, isImage := strings.CutPrefix(asciiLower(header), "image/")
		kind, isBase64 := strings.CutSuffix(kind, ";base64")
		magic, known := imageMagic[kind]
		if !ok || !isImage || !isBase64 || !known {
			return "", "data: URLs may only be base64 PNG, JPEG, GIF or WebP images"
		}
		if len(payload) > MaxDataURL {
			return "", "inline image is over 16 KiB"
		}
		data, err := base64.StdEncoding.Strict().DecodeString(payload)
		if err != nil || !bytes.HasPrefix(data, magic) || (kind == "webp" && (len(data) < 12 || string(data[8:12]) != "WEBP")) {
			return "", "inline image bytes do not match its type"
		}
		return "data:image/" + kind + ";base64," + payload, ""
	}
	if font {
		return "", "font src may only be an attachment of this room (/a/<id>)"
	}
	return "", "url() may only name an attachment of this room (/a/<id>) or an inline image"
}

func validID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

// fontSrc accepts a comma list of url(/a/<id>) [format(...)].
func (s *sanitizer) fontSrc(list []cv) ([]cv, string) {
	var out []cv
	for i, item := range splitCommas(list) {
		if len(item) == 0 {
			return nil, "malformed src"
		}
		var u string
		switch {
		case item[0].kind == tURL:
			u = item[0].value
		case item[0].kind == tFunction && asciiLower(item[0].value) == "url" && len(trim(item[0].kids)) == 1 && trim(item[0].kids)[0].kind == tString:
			u = trim(item[0].kids)[0].value
		default:
			return nil, "font src may only be url(/a/<id>), optionally with format()"
		}
		canon, why := s.url(u, true)
		if why != "" {
			return nil, why
		}
		if i > 0 {
			out = append(out, cv{token: token{kind: tComma}})
		}
		out = append(out, cv{token: token{kind: tURL, value: canon}})
		rest := trim(item[1:])
		if len(rest) == 0 {
			continue
		}
		if len(rest) != 1 || rest[0].kind != tFunction || asciiLower(rest[0].value) != "format" {
			return nil, "font src may only be url(/a/<id>), optionally with format()"
		}
		args := trim(rest[0].kids)
		if len(args) != 1 || (args[0].kind != tString && args[0].kind != tIdent) {
			return nil, "malformed format()"
		}
		out = append(out, cv{token: token{kind: tWhitespace, value: " "}}, cv{token: token{kind: tFunction, value: "format"}, kids: args})
	}
	return out, ""
}

// familyName reads a font family written as one string or a run of identifiers.
func familyName(list []cv) (string, bool) {
	list = trim(list)
	if len(list) == 1 && list[0].kind == tString {
		return list[0].value, list[0].value != ""
	}
	var words []string
	for _, v := range list {
		switch v.kind {
		case tWhitespace:
		case tIdent:
			if slices.Contains(cssWide, asciiLower(v.value)) {
				return "", false
			}
			words = append(words, v.value)
		default:
			return "", false
		}
	}
	return strings.Join(words, " "), len(words) > 0
}

// renameFamilies points references to this room's @font-face families at
// their prefixed names, so a room font can never replace one the site uses.
func (s *sanitizer) renameFamilies(list []cv) []cv {
	var out []cv
	for i, item := range splitCommas(list) {
		if i > 0 {
			out = append(out, cv{token: token{kind: tComma}})
		}
		if name, ok := familyName(item); ok && s.families[strings.ToLower(name)] {
			item = []cv{{token: token{kind: tString, value: s.prefixed(strings.ToLower(name))}}}
		}
		out = append(out, item...)
	}
	return out
}

func keyframesName(prelude []cv) (string, bool) {
	p := trim(prelude)
	if len(p) != 1 || (p[0].kind != tIdent && p[0].kind != tString) || p[0].value == "" {
		return "", false
	}
	if p[0].kind == tIdent && slices.Contains(cssWide, asciiLower(p[0].value)) {
		return "", false
	}
	return p[0].value, true
}
