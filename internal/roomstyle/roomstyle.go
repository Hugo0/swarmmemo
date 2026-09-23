// Package roomstyle turns a room owner's arbitrary CSS into a stylesheet that
// restyles the whole room page (nav, header, feed, posts, composer, footer)
// without being able to hide, cover, move away or forge trust UI.
//
// The boundary is what CSS may change, not where it applies:
//
//   - Every selector is re-rooted under the room's class on <html>, and may name
//     only the published hooks (Hooks). IDs and class/id/style attribute
//     selectors are refused.
//   - A rule whose subject is inside a post body (at or below .post-body, the
//     canvas root) is in the body zone and may use almost any property: the page
//     draws each canvas with contain: layout paint style, isolation and
//     overflow: clip, so nothing there paints outside it.
//   - Every other rule is in the page zone, where nothing may hide, move off the
//     page origin, overlay or stack: no position, z-index, transform, opacity,
//     filter, clip, mask, overflow, visibility, content, pseudo-element boxes,
//     animation, negative margins, display:none or translucent text colour.
//     Without positioned or stacking boxes, nothing in the page zone can cover
//     the trust elements, which the site pins (position, z-index, font, spacing,
//     bidi) in @layer room-trust with !important, which no room rule outranks.
//   - url() may name only a verified attachment of the room (/a/<id>) or a small
//     inline raster image. Nothing else can make a reader's browser fetch.
//   - Global names (@keyframes, @font-face families, @layer) are prefixed; site
//     tokens other than colours cannot be redefined.
//   - The output is re-serialized from parsed tokens, then re-parsed and checked
//     independently before it is returned; a mismatch rejects the stylesheet.
//
// A page that renders room CSS also sends a path-restricted Content-Security-Policy
// (see internal/web), so a sanitizer bug still cannot fetch another origin.
package roomstyle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Caps. Parsing is linear, so these bound memory and output, not time.
const (
	MaxInputBytes  = 32 << 10
	MaxOutputBytes = 64 << 10
	MaxRules       = 1024 // style rules and at-rules emitted
	MaxSelectors   = 4096 // complex selectors emitted
	MaxAtDepth     = 6    // @media inside @supports inside ...
	// MaxAttachments bounds distinct /a/ URLs, so a stylesheet cannot spend a
	// reader's request allowance by fetching hundreds of attachments.
	MaxAttachments = 16
	// maxAttachmentChecks bounds how many distinct /a/ IDs are looked up at
	// all, live or not; later ones are dropped unchecked.
	maxAttachmentChecks = 2 * MaxAttachments
	maxWarnings         = 50
)

// Stylesheet is sanitized, scoped CSS ready to serve as text/css.
type Stylesheet struct {
	// Scope is the class the page puts on <html>, "room-style-" plus 16 hex
	// digits derived from the room name. The room's ":scope" means that element.
	Scope string
	CSS   string
	// Hash is 16 hex digits of the SHA-256 of CSS, for a cache-busting URL.
	Hash string
}

// Warning describes something dropped from the owner's source.
type Warning struct {
	Line    int
	Message string
}

func (w Warning) String() string {
	if w.Line > 0 {
		return fmt.Sprintf("line %d: %s", w.Line, w.Message)
	}
	return w.Message
}

// Options carries the per-room facts the sanitizer cannot know by itself.
type Options struct {
	// Attachment reports whether id is a live public attachment of this room.
	// With nil, no attachment URL is accepted.
	Attachment func(id string) bool
}

// Sanitize is SanitizeWith with no attachment URLs allowed.
func Sanitize(css, roomID string) (Stylesheet, []Warning, error) {
	return SanitizeWith(css, roomID, Options{})
}

// ScopeClass is the canvas class for a room.
func ScopeClass(roomID string) string {
	sum := sha256.Sum256([]byte("swarmmemo room style\x00" + roomID))
	return "room-style-" + hex.EncodeToString(sum[:8])
}

// SanitizeWith returns the scoped stylesheet and a warning for each thing it
// dropped. An error means nothing may be served: the input is over a cap, or the
// output failed its own re-check.
func SanitizeWith(css, roomID string, opts Options) (Stylesheet, []Warning, error) {
	if len(css) > MaxInputBytes {
		return Stylesheet{}, nil, fmt.Errorf("stylesheet is %d bytes; the limit is %d", len(css), MaxInputBytes)
	}
	scope := ScopeClass(roomID)
	values, err := parseValues(css)
	if err != nil {
		return Stylesheet{}, nil, err
	}
	s := &sanitizer{scope: scope, opts: opts, keyframes: map[string]bool{}, families: map[string]bool{}, urls: map[string]bool{}, checked: map[string]bool{}}
	rules := parseRules(values, true)
	s.knobErr = trustKnobs(rules, scope)
	s.collectNames(rules, 0)
	s.rules(rules, 0)
	if s.err != nil {
		return Stylesheet{}, s.warnings, s.err
	}
	out := serialize(s.out)
	if len(out) > MaxOutputBytes {
		return Stylesheet{}, s.warnings, fmt.Errorf("scoped stylesheet would be %d bytes; the limit is %d", len(out), MaxOutputBytes)
	}
	if err := Verify(out, scope); err != nil {
		return Stylesheet{}, s.warnings, fmt.Errorf("internal check rejected the output: %w", err)
	}
	sum := sha256.Sum256([]byte(out))
	if s.dropped > 0 {
		s.warnings = append(s.warnings, Warning{Message: fmt.Sprintf("%d more warnings not shown", s.dropped)})
	}
	return Stylesheet{Scope: scope, CSS: out, Hash: hex.EncodeToString(sum[:8])}, s.warnings, nil
}

type sanitizer struct {
	scope     string
	opts      Options
	out       []cv
	warnings  []Warning
	dropped   int
	keyframes map[string]bool // declared @keyframes names, as written
	families  map[string]bool // declared @font-face families, lowercased
	nRules    int
	nSelector int
	urls      map[string]bool // distinct attachment IDs referenced
	checked   map[string]bool // attachment IDs already checked, and the answer
	knobErr   error           // why the trust colour knobs cannot stand, if they cannot
	scopeRule bool            // the rule being emitted is a top-level :scope rule
	err       error
}

func (s *sanitizer) warn(line int, format string, args ...any) {
	w := Warning{Line: line, Message: fmt.Sprintf(format, args...)}
	if slices.Contains(s.warnings, w) {
		return
	}
	if len(s.warnings) >= maxWarnings {
		s.dropped++
		return
	}
	s.warnings = append(s.warnings, w)
}

var errTooManyRules = fmt.Errorf("stylesheet has more than %d rules", MaxRules)

func (s *sanitizer) count() bool {
	s.nRules++
	if s.nRules > MaxRules && s.err == nil {
		s.err = errTooManyRules
	}
	return s.err == nil
}

// collectNames finds every @keyframes name and @font-face family first, so a
// reference anywhere can be rewritten to the room's prefixed name.
func (s *sanitizer) collectNames(rules []rule, depth int) {
	if depth > MaxAtDepth {
		return
	}
	for _, r := range rules {
		switch asciiLower(r.at) {
		case "keyframes":
			if name, ok := keyframesName(r.prelude); ok {
				s.keyframes[name] = true
			}
		case "font-face":
			if r.block == nil {
				continue
			}
			decls, _ := parseDeclarations(r.block.kids)
			for _, d := range decls {
				if equalFold(d.name, "font-family") {
					if name, ok := familyName(d.value); ok {
						s.families[strings.ToLower(name)] = true
					}
				}
			}
		case "media", "supports", "container", "layer":
			if r.block != nil {
				s.collectNames(parseRules(r.block.kids, false), depth+1)
			}
		}
	}
}

// prefixed gives a global name the room's prefix. A name that already has it
// is kept, which makes Sanitize idempotent on its own output.
func (s *sanitizer) prefixed(name string) string {
	if strings.HasPrefix(name, s.scope+"-") {
		return name
	}
	return s.scope + "-" + name
}

// rules emits a rule list.
func (s *sanitizer) rules(rules []rule, depth int) {
	for _, r := range rules {
		if s.err != nil {
			return
		}
		if r.at == "" {
			s.styleRule(r, depth)
			continue
		}
		s.atRule(r, depth)
	}
}

func (s *sanitizer) newline() {
	s.out = append(s.out, cv{token: token{kind: tWhitespace, value: "\n"}})
}

// styleRule emits a rule once per zone: page-zone selectors get the restricted
// declaration set, body-zone selectors the full one.
func (s *sanitizer) styleRule(r rule, depth int) {
	var page, body [][]cv
	for _, complex := range splitCommas(r.prelude) {
		scoped, inBody, why := s.selector(complex)
		if why != "" {
			s.warn(r.line, "dropped selector %q: %s", clip(serialize(complex)), why)
			continue
		}
		if inBody {
			body = append(body, scoped)
		} else {
			page = append(page, scoped)
		}
	}
	s.scopeRule = depth == 0 && isScopeOnly(r.prelude, s.scope)
	s.emitRule(page, r.block.kids, declPage)
	s.scopeRule = false
	s.emitRule(body, r.block.kids, declStyle)
}

func (s *sanitizer) emitRule(selectors [][]cv, kids []cv, mode declMode) {
	if len(selectors) == 0 || s.err != nil {
		return
	}
	decls := s.declarations(kids, mode)
	if len(decls) == 0 {
		return
	}
	s.nSelector += len(selectors)
	if s.nSelector > MaxSelectors {
		s.err = fmt.Errorf("stylesheet has more than %d selectors", MaxSelectors)
		return
	}
	if !s.count() {
		return
	}
	for i, sel := range selectors {
		if i > 0 {
			s.out = append(s.out, cv{token: token{kind: tComma}})
		}
		s.out = append(s.out, sel...)
	}
	s.out = append(s.out, cv{token: token{kind: tLBrace}, kids: decls})
	s.newline()
}

func clip(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}
