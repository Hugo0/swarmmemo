package roomstyle

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// The free zone (see zones.go) holds no byline and no control, so a room may
// hide, move, stack and decorate what is in it. What it may not do is reach
// the trust UI anyway: stack above it, pull it off the page, or write text
// that reads as part of a byline.

// MaxFreeZIndex bounds z-index in the free zone. The site pins bylines and
// controls at a z-index well above it (internal/web/assets/style.css), and the
// page zone, which holds every ancestor of them, can create no stacking
// context, so nothing a room stacks can paint over them.
const MaxFreeZIndex = 100

var (
	// Properties whose strings and keywords are drawn as text.
	renderedTextProperties = []string{"content", "quotes", "list-style", "list-style-type", "hyphenate-character", "-webkit-hyphenate-character",
		"text-emphasis", "text-emphasis-style", "-webkit-text-emphasis", "-webkit-text-emphasis-style", "text-overflow"}
	// Keywords content and list markers may use: none of them draws a letter.
	contentKeywords = []string{"none", "normal", "open-quote", "close-quote", "no-open-quote", "no-close-quote",
		"initial", "inherit", "unset", "revert", "revert-layer"}
	markerKeywords = []string{"none", "disc", "circle", "square", "decimal", "decimal-leading-zero", "disclosure-open", "disclosure-closed",
		"inside", "outside", "initial", "inherit", "unset", "revert", "revert-layer"}
	// The marks the site draws in bylines, and check marks, which beside a
	// byline would read as a verification.
	bylineMarks = []rune{'⌘', '○', '◇', '◆', '●', '✓', '✔', '☑', '✅', '🗸'}
)

// freeDeclaration says why a declaration cannot stand in the free zone, or "".
func freeDeclaration(name string, value []cv) string {
	switch {
	case name == "z-index":
		if len(value) == 1 && value[0].kind == tIdent && slices.Contains([]string{"auto", "initial", "unset", "revert", "revert-layer"}, asciiLower(value[0].value)) {
			return ""
		}
		if !integer(value, MaxFreeZIndex) {
			return "z-index outside a post body is a whole number from -100 to 100 (bylines and controls are drawn above that)"
		}
	case name == "margin" || strings.HasPrefix(name, "margin-") || name == "-webkit-margin-before" || name == "-webkit-margin-after" ||
		name == "-webkit-margin-start" || name == "-webkit-margin-end":
		if negative(value) {
			return "negative (or var()) margins outside a post body can pull the page above its top, where it cannot be scrolled to"
		}
	case slices.Contains(renderedTextProperties, name):
		return renderedText(name, value)
	}
	return ""
}

// renderedText accepts drawn text with no letters: digits, punctuation and a
// range of symbols (arrows, box drawing, shapes, dingbats), counters in
// decimal, images, and keywords that draw no letter. Nothing it accepts can
// spell a name, a handle or "verified" next to a byline.
func renderedText(name string, value []cv) string {
	keywords := contentKeywords
	if strings.HasPrefix(name, "list-style") {
		keywords = markerKeywords
	}
	for _, v := range value {
		switch v.kind {
		case tWhitespace, tComma, tURL, tNumber, tPercentage, tDimension, tHash:
		case tDelim:
			if v.value != "/" {
				return "unexpected " + v.value + " in drawn text"
			}
		case tString:
			if !letterless(v.value) {
				return "text drawn outside a post body (content, markers, quotes) may use digits, punctuation and symbols, not letters or byline marks"
			}
		case tIdent:
			// text-emphasis shapes and colours, and text-overflow's keywords, draw no letters.
			if (name == "content" || strings.HasPrefix(name, "list-style")) && !slices.Contains(keywords, asciiLower(v.value)) {
				return "keyword " + v.value + " is not allowed in drawn text outside a post body"
			}
		case tFunction:
			switch asciiLower(v.value) {
			case "counter", "counters":
				if why := counterText(v); why != "" {
					return why
				}
			case "url":
			case "linear-gradient", "radial-gradient", "conic-gradient", "repeating-linear-gradient", "repeating-radial-gradient", "repeating-conic-gradient":
			case "rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color":
			default:
				return v.value + "() is not allowed in drawn text outside a post body"
			}
		default:
			return "unexpected token in drawn text"
		}
	}
	return ""
}

// counterText accepts counter(name) and counters(name, "sep") with an optional
// decimal style: an alphabetic or roman style would draw letters.
func counterText(v cv) string {
	args := splitCommas(v.kids)
	want := 1
	if asciiLower(v.value) == "counters" {
		want = 2
	}
	if len(args) < want || len(args) > want+1 {
		return "malformed " + v.value + "()"
	}
	for i, arg := range args {
		arg = trim(arg)
		switch {
		case len(arg) != 1:
			return "malformed " + v.value + "()"
		case i == 0 && arg[0].kind == tIdent:
		case i == 1 && want == 2 && arg[0].kind == tString && letterless(arg[0].value):
		case i == want && arg[0].kind == tIdent && slices.Contains([]string{"decimal", "decimal-leading-zero", "none"}, asciiLower(arg[0].value)):
		default:
			return v.value + "() outside a post body draws only decimal numbers and letterless separators"
		}
	}
	return ""
}

// letterless reports text with no letter, no byline mark, no invisible or
// direction-changing character, and nothing outside the listed symbol blocks.
func letterless(text string) bool {
	for _, r := range text {
		switch {
		case r == '\n' || r == ' ' || r == ' ':
		case r < 0x80:
			if unicode.IsLetter(r) || unicode.IsControl(r) {
				return false
			}
		case slices.Contains(bylineMarks, r):
			return false
		case r >= 0x00a1 && r <= 0x00bf && r != 0x00aa && r != 0x00ba, r == 0x00d7, r == 0x00f7:
		case r >= 0x2010 && r <= 0x2027, r >= 0x2030 && r <= 0x205e: // punctuation, without spaces and format controls
		case r >= 0x2190 && r <= 0x23ff: // arrows, maths, technical
		case r >= 0x2500 && r <= 0x27ff: // box drawing, blocks, shapes, symbols, dingbats, maths, arrows
			if unicode.IsLetter(r) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// integer reports a single whole-number literal no larger than limit either way.
func integer(value []cv, limit int) bool {
	if len(value) != 1 || value[0].kind != tNumber {
		return false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(value[0].num, "+"))
	return err == nil && n >= -limit && n <= limit
}
