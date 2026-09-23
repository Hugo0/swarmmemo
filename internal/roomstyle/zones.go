package roomstyle

import (
	"slices"
	"strconv"
	"strings"
)

// Three zones, by where a rule's subject is (selector.go):
//
//   - body: inside a post's canvas, which the site contains and clips. Almost
//     anything goes.
//   - free: at or below a free hook (FreeClasses), which never contains a
//     byline or a control. Almost anything goes there too, including hiding,
//     positioning, transforms and generated boxes, with four limits: z-index is
//     a literal between -100 and 100 (the site draws bylines and controls at
//     1000, above anything the free zone can stack); no negative margins (a
//     free element's margin can pull the page's following content above its
//     origin, where it cannot be scrolled to); generated and list-marker text
//     has no letters (so it cannot spell a name or "verified" beside a byline);
//     and animations pass the flash-rate check.
//   - page: everything else, which may be a byline, a control or an ancestor
//     of one. The rest of this comment is about this zone.
//
// A page-zone rule may restyle colour, type, spacing, borders, backgrounds and
// layout, reorder siblings, and animate backgrounds and colours, but nothing
// that can hide an element, take it off the page origin, lay a box over
// another, or make text unreadable by transparency:
//
//   - no positioned or stacking boxes (position, z-index, transform, opacity,
//     filter, isolation, blend modes, will-change, contain): with none, nothing
//     in the page zone paints above the trust elements, which the site pins as
//     positioned boxes on top;
//   - no clipping or hiding (overflow, clip, clip-path, mask, visibility,
//     content-visibility, display:none or contents, line clamps);
//   - no generated content (content), and animations only of backgrounds and
//     colours (pageAnimatable);
//   - no negative margins, spacing or text-indent, no right floats, reversed
//     flex lines, direction or writing-mode changes, and alignment only "safe":
//     all of these can push content to the left of the page origin, where it
//     cannot be scrolled to;
//   - no box may be made shorter than its content (height, max-height,
//     aspect-ratio) and grids place items only automatically, on tracks no
//     smaller than their content: so no two boxes overlap. The site pins posts
//     and trust elements to at least their content's width;
//   - text colour only as an opaque literal, and site tokens other than colours
//     cannot be redefined.
//
// The body zone keeps everything; its canvas contains it.

var (
	bodyOnlyProperties = []string{
		"position", "top", "right", "bottom", "left", "z-index",
		"transform", "transform-origin", "transform-box", "transform-style", "translate", "rotate", "scale",
		"perspective", "perspective-origin", "backface-visibility", "zoom",
		"filter", "backdrop-filter", "-webkit-backdrop-filter", "opacity", "mix-blend-mode", "isolation", "will-change",
		"clip", "clip-path", "contain", "content-visibility", "visibility", "content",
		"overflow", "overflow-x", "overflow-y", "overflow-block", "overflow-inline", "overflow-clip-margin",
		"line-clamp", "-webkit-line-clamp", "max-lines", "block-ellipsis", "continue",
		"text-indent", "outline-offset", "all", "touch-action", "interactivity",
		"-webkit-box-reflect", "-webkit-text-fill-color",
		"height", "max-height", "block-size", "max-block-size", "aspect-ratio",
		"grid", "grid-template", "grid-template-areas", "grid-area", "grid-row", "grid-row-start", "grid-row-end",
		"grid-column", "grid-column-start", "grid-column-end", "place-content", "place-items", "place-self",
		"direction", "unicode-bidi", "writing-mode", "text-orientation",
		// Text a room could inject outside a body: list markers, counters,
		// quotes, hyphenation strings and emphasis marks; and text masking.
		"list-style", "list-style-type", "list-style-image", "list-style-position",
		"quotes", "hyphenate-character", "-webkit-text-security",
		// SVG geometry, which would redraw an icon.
		"d", "r", "cx", "cy", "rx", "ry", "x", "y",
		// Fragmenting: columns and forced breaks split a post (and its Reply
		// button) across columns; size containment collapses a box to nothing.
		// (order is allowed: it reorders siblings in flow and never overlaps them.)
		"columns", "column-count", "column-width", "column-span", "column-fill",
		"break-before", "break-after", "break-inside", "page-break-before", "page-break-after", "page-break-inside",
		"container", "container-type",
	}
	// Animations are checked in every zone by animation.go.
	bodyOnlyPrefixes = []string{"inset", "mask", "-webkit-mask", "offset", "-webkit-text-stroke", "contain-intrinsic", "text-emphasis"}
	// Page-zone display values: every one draws the element's content.
	pageDisplays = []string{"block", "inline", "inline-block", "flex", "inline-flex", "grid", "inline-grid", "flow-root", "flow",
		"initial", "inherit", "unset", "revert", "revert-layer"}
	alignments     = []string{"justify-content", "align-content", "align-items", "align-self", "justify-items", "justify-self"}
	positional     = []string{"center", "start", "end", "self-start", "self-end", "flex-start", "flex-end", "left", "right"}
	trackKeywords  = []string{"auto", "min-content", "max-content", "none", "initial", "inherit", "unset", "revert", "revert-layer"}
	verticalAligns = []string{"baseline", "sub", "super", "text-top", "text-bottom", "middle", "top", "bottom",
		"initial", "inherit", "unset", "revert", "revert-layer"}
	colorFunctions = []string{"rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color"}
)

func bodyOnly(name string) bool {
	if slices.Contains(bodyOnlyProperties, name) {
		return true
	}
	for _, prefix := range bodyOnlyPrefixes {
		if name == prefix || strings.HasPrefix(name, prefix+"-") {
			return true
		}
	}
	return false
}

// pageDeclaration says why a declaration cannot stand in the page zone, or "".
func pageDeclaration(name string, value []cv) string {
	switch {
	case bodyOnly(name):
		return "allowed only inside .post-body, where it cannot hide, move or cover the page"
	case strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--"):
		// Browsers alias many prefixed names to properties the page zone refuses
		// (-webkit-opacity, -webkit-transform, -webkit-animation, -webkit-margin-start,
		// -webkit-logical-height, -webkit-order, ...), so none is checked by name here.
		return "vendor-prefixed properties are allowed only inside .post-body"
	case strings.HasPrefix(name, "--"):
		switch {
		case slices.Contains(colorTokens, name):
			if !opaqueColor(value) {
				return "site colour tokens take only an opaque colour literal"
			}
		case slices.Contains(fixedTokens, name):
			return "site token " + name + " lays out trust UI and cannot be changed; colour tokens can"
		}
	case name == "color":
		if !opaqueColor(value) {
			return "text colour outside a post body must be an opaque colour literal (no transparency, var() or color-mix())"
		}
	case name == "display":
		if len(value) != 1 && !(len(value) == 3 && value[1].kind == tWhitespace) {
			return "unsupported display value"
		}
		for _, v := range value {
			if v.kind != tWhitespace && (v.kind != tIdent || !slices.Contains(pageDisplays, asciiLower(v.value))) {
				return "display outside a post body must draw the element (not none, contents or var())"
			}
		}
	case name == "vertical-align":
		if len(value) != 1 || value[0].kind != tIdent || !slices.Contains(verticalAligns, asciiLower(value[0].value)) {
			return "vertical-align outside a post body takes only keywords"
		}
	case name == "margin" || strings.HasPrefix(name, "margin-") || name == "letter-spacing" || name == "word-spacing":
		if negative(value) {
			return "negative (or var()) values outside a post body can move content where it cannot be scrolled to"
		}
	case name == "outline" || name == "outline-width":
		if !boundedLengths(value, 8) {
			return "outline width outside a post body is limited to 8px"
		}
	case name == "text-decoration" || name == "text-decoration-thickness" || name == "text-underline-offset":
		if !boundedLengths(value, 8) {
			return "text decoration outside a post body is limited to 8px, so it cannot draw over text"
		}
		if strikesText(value) {
			return "line-through outside a post body would strike out bylines in the plate's colour"
		}
	case name == "text-decoration-line":
		for _, v := range value {
			if v.kind != tWhitespace && v.kind != tIdent {
				return "text-decoration-line outside a post body takes only keywords"
			}
		}
		if strikesText(value) {
			return "line-through outside a post body would strike out bylines in the plate's colour"
		}
	case name == "flex-direction" || name == "flex-flow" || name == "flex-wrap":
		for _, v := range value {
			if v.kind != tIdent && v.kind != tWhitespace {
				return "unsupported flex value"
			}
			if strings.HasSuffix(asciiLower(v.value), "-reverse") {
				return "reversed flex lines outside a post body can push content off the page's left edge"
			}
		}
	case name == "float":
		if len(value) != 1 || value[0].kind != tIdent || !slices.Contains([]string{"left", "none", "inline-start", "initial", "inherit", "unset", "revert", "revert-layer"}, asciiLower(value[0].value)) {
			return "outside a post body only float: left or none (a right float wider than its column overflows the page's left edge)"
		}
	case name == "order":
		if !integer(value, 1000) {
			return "order takes a whole number between -1000 and 1000"
		}
	case name == "counter-reset" || name == "counter-increment" || name == "counter-set":
		for _, v := range value {
			if v.kind != tWhitespace && v.kind != tIdent && !(v.kind == tNumber && integer([]cv{v}, 1<<20)) {
				return "counters take names and whole numbers"
			}
		}
	case slices.Contains(alignments, name):
		if !safeAlignment(value) {
			return "alignment outside a post body must be safe (it is made safe automatically; unsafe and var() are refused)"
		}
	case name == "grid-template-columns" || name == "grid-template-rows" || name == "grid-auto-columns" || name == "grid-auto-rows":
		if !contentTracks(value, 0) {
			return "grid tracks outside a post body may not be smaller than their content: use fr, auto, min-content, max-content, fit-content() or minmax(auto|min-content, ...)"
		}
	}
	return ""
}

// pageRewrite adjusts a page-zone value before it is checked: alignment gets
// "safe", so content that does not fit overflows at the end, never off the
// page origin.
func pageRewrite(name string, value []cv) []cv {
	if slices.Contains(alignments, name) && len(value) == 1 && value[0].kind == tIdent && slices.Contains(positional, asciiLower(value[0].value)) {
		return []cv{{token: token{kind: tIdent, value: "safe"}}, {token: token{kind: tWhitespace, value: " "}}, value[0]}
	}
	return value
}

func safeAlignment(value []cv) bool {
	var words []string
	for _, v := range value {
		switch v.kind {
		case tWhitespace:
		case tIdent:
			words = append(words, asciiLower(v.value))
		default:
			return false
		}
	}
	for i, w := range words {
		if w == "unsafe" || w == "legacy" {
			return false
		}
		if slices.Contains(positional, w) && (i == 0 || words[i-1] != "safe") {
			return false
		}
	}
	return len(words) > 0
}

// contentTracks accepts grid track lists whose every track is at least as
// large as its content: fr, auto, min-content, max-content, fit-content(),
// minmax() with a content-based minimum, repeat() of those, and line names.
func contentTracks(list []cv, depth int) bool {
	if depth > 4 {
		return false
	}
	for _, v := range list {
		switch v.kind {
		case tWhitespace, tComma:
		case tIdent:
			if !slices.Contains(trackKeywords, asciiLower(v.value)) {
				return false
			}
		case tDimension:
			if asciiLower(v.value) != "fr" || strings.HasPrefix(v.num, "-") {
				return false
			}
		case tNumber:
			// Only as a repeat() count, which the repeat case below checks.
			return false
		case tLBracket:
			for _, k := range v.kids {
				if k.kind != tIdent && k.kind != tWhitespace {
					return false
				}
			}
		case tFunction:
			args := splitCommas(v.kids)
			switch asciiLower(v.value) {
			case "repeat":
				if len(args) != 2 || len(args[0]) != 1 {
					return false
				}
				count := args[0][0]
				if !(count.kind == tNumber && !strings.HasPrefix(count.num, "-")) && !(count.kind == tIdent && (equalFold(count.value, "auto-fill") || equalFold(count.value, "auto-fit"))) {
					return false
				}
				if !contentTracks(args[1], depth+1) {
					return false
				}
			case "minmax":
				if len(args) != 2 || len(args[0]) != 1 || args[0][0].kind != tIdent ||
					!slices.Contains([]string{"auto", "min-content", "max-content"}, asciiLower(args[0][0].value)) {
					return false
				}
				if len(args[1]) != 1 || (args[1][0].kind != tIdent && args[1][0].kind != tDimension && args[1][0].kind != tPercentage) ||
					(args[1][0].kind == tIdent && !slices.Contains(trackKeywords, asciiLower(args[1][0].value))) {
					return false
				}
			case "fit-content":
				if len(args) != 1 || len(args[0]) != 1 || (args[0][0].kind != tDimension && args[0][0].kind != tPercentage) {
					return false
				}
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}

// opaqueColor accepts one literal colour with no transparency: a hash of 3 or 6
// digits (or 4 or 8 with full alpha), a keyword other than transparent, or a
// colour function of plain numbers whose alpha, if any, is 1 or 100%.
func opaqueColor(value []cv) bool {
	if len(value) != 1 {
		return false
	}
	v := value[0]
	switch v.kind {
	case tHash:
		h := strings.ToLower(v.value)
		if strings.Trim(h, "0123456789abcdef") != "" {
			return false
		}
		switch len(h) {
		case 3, 6:
			return true
		case 4:
			return h[3] == 'f'
		case 8:
			return h[6:] == "ff"
		}
		return false
	case tIdent:
		return !equalFold(v.value, "transparent")
	case tFunction:
		if !slices.Contains(colorFunctions, asciiLower(v.value)) {
			return false
		}
		var args [][]cv
		current := []cv{}
		alphaNext := false
		for _, k := range v.kids {
			switch {
			case k.kind == tWhitespace:
				continue
			case k.kind == tComma || delimIs(k, "/"):
				args = append(args, current)
				current = []cv{}
				alphaNext = alphaNext || delimIs(k, "/")
				continue
			case k.kind == tNumber || k.kind == tPercentage || k.kind == tDimension:
			case k.kind == tIdent && equalFold(k.value, "none"):
			case k.kind == tIdent && asciiLower(v.value) == "color" && len(args) == 0 && len(current) == 0:
			default:
				return false // var(), calc(), relative colour syntax
			}
			current = append(current, k)
		}
		args = append(args, current)
		// The alpha is after "/" or is the fourth comma argument.
		commas := 0
		for _, k := range v.kids {
			if k.kind == tComma {
				commas++
			}
		}
		if !alphaNext && commas < 3 {
			return true
		}
		alpha := args[len(args)-1]
		if len(alpha) != 1 {
			return false
		}
		n, err := strconv.ParseFloat(alpha[0].num, 64)
		return err == nil && ((alpha[0].kind == tNumber && n >= 1) || (alpha[0].kind == tPercentage && n >= 100))
	}
	return false
}

// negative reports a value that is or could be negative: a negative number, a
// minus sign, a negative constant such as -infinity, or any function but
// calc(), min(), max() and clamp() (a substitution, or maths such as
// cos(180deg) or log(.5) that turns positive arguments negative).
func negative(list []cv) bool {
	for _, v := range list {
		switch {
		case (v.kind == tNumber || v.kind == tPercentage || v.kind == tDimension) && strings.HasPrefix(v.num, "-"):
			return true
		case delimIs(v, "-"):
			return true
		case v.kind == tIdent && strings.HasPrefix(v.value, "-"):
			return true
		case v.kind == tFunction && !slices.Contains([]string{"calc", "min", "max", "clamp"}, asciiLower(v.value)):
			return true
		}
		if negative(v.kids) {
			return true
		}
	}
	return false
}

// boundedLengths accepts a value whose lengths are all at most maxPx (px, or
// em/rem at 16px), with no calculation or substitution.
func boundedLengths(list []cv, maxPx float64) bool {
	for _, v := range list {
		switch v.kind {
		case tDimension:
			n, err := strconv.ParseFloat(v.num, 64)
			if err != nil || n < 0 {
				return false
			}
			switch asciiLower(v.value) {
			case "px":
			case "em", "rem":
				n *= 16
			default:
				return false
			}
			if n > maxPx {
				return false
			}
		case tNumber:
			if n, err := strconv.ParseFloat(v.num, 64); err != nil || n != 0 {
				return false
			}
		case tFunction:
			if !slices.Contains(colorFunctions, asciiLower(v.value)) || !opaqueColor([]cv{v}) {
				return false
			}
			continue
		case tIdent, tHash, tWhitespace:
		default:
			return false
		}
	}
	return true
}

// strikesText reports a line-through, which (unlike underline and overline) is
// painted over the glyphs and is propagated to every descendant's text, trust
// text included, where no pinned declaration can remove it.
func strikesText(list []cv) bool {
	for _, v := range list {
		if v.kind == tIdent && equalFold(v.value, "line-through") {
			return true
		}
	}
	return false
}

func genericFamilyList(list []cv) bool {
	for _, item := range splitCommas(list) {
		if len(item) != 1 || item[0].kind != tIdent || !slices.Contains(genericFamilies, asciiLower(item[0].value)) {
			return false
		}
	}
	return true
}
