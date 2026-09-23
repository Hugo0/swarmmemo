package roomstyle

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Animations. Every zone may animate, through the animation shorthand only, so
// the duration, the timing function and the keyframes are read together:
//
//   - Flashing (WCAG 2.3.1): an animation lasts at least MinAnimationSeconds
//     and changes at most MaxChangesPerSecond times a second, counting each
//     step of steps() and each keyframe interval as a change. The longhands that
//     set a name, duration, timing or timeline are refused, so a later rule
//     cannot speed up a checked animation; per-keyframe steps() is refused too.
//   - What moves: a page-zone animation (which can reach a byline, a control or
//     an ancestor of one) may use only keyframes that change backgrounds and
//     colours (pageAnimatable), which cannot hide, move or stack anything; a
//     free-zone animation only keyframes whose every declaration the free zone
//     accepts; a body-zone animation any of the room's keyframes. A name must be
//     one of the room's own @keyframes.
//   - Reduced motion: the site's pinned layer sets animation and transition to
//     none for readers who ask for reduced motion, whatever the room says.
const (
	MinAnimationSeconds = 1.0
	MaxChangesPerSecond = 3
)

var (
	// What a page-zone animation may change.
	pageAnimatable = []string{"background-color", "background-position", "background-position-x", "background-position-y", "background-size",
		"color", "border-color", "border-top-color", "border-right-color", "border-bottom-color", "border-left-color",
		"border-block-color", "border-inline-color", "border-block-start-color", "border-block-end-color", "border-inline-start-color",
		"border-inline-end-color", "outline-color", "text-decoration-color", "caret-color", "accent-color", "box-shadow", "text-shadow"}
	// Longhands that carry no rate: the shorthand's checks stand whatever they say.
	rateFreeLonghands = []string{"animation-delay", "animation-direction", "animation-fill-mode", "animation-play-state",
		"animation-iteration-count", "animation-composition"}
	timingKeywords    = []string{"linear", "ease", "ease-in", "ease-out", "ease-in-out", "step-start", "step-end"}
	animationKeywords = []string{"normal", "reverse", "alternate", "alternate-reverse", "none", "forwards", "backwards", "both",
		"running", "paused", "infinite"}
)

// keyframeInfo is what the animation check needs to know about one @keyframes
// name, across every rule that defines it.
type keyframeInfo struct {
	stops int  // distinct keyframe offsets, with 0% and 100% always counted
	page  bool // every declaration is one a page-zone animation may change
	free  bool // every declaration is one the free zone accepts
}

type keyframeSet map[string]keyframeInfo

func (k keyframeSet) add(name string, info keyframeInfo) {
	if old, ok := k[name]; ok {
		info.stops = max(info.stops, old.stops)
		info.page = info.page && old.page
		info.free = info.free && old.free
	}
	k[name] = info
}

// isAnimation reports an animation property, prefixed or not.
func isAnimation(name string) bool {
	for _, prefix := range []string{"", "-webkit-", "-moz-", "-o-", "-ms-"} {
		if rest, ok := strings.CutPrefix(name, prefix+"animation"); ok && (rest == "" || strings.HasPrefix(rest, "-")) {
			return true
		}
	}
	return false
}

// animationDeclaration says why an animation property cannot stand in a zone
// (or inside @keyframes when keyframe is true), or "".
func animationDeclaration(name string, value []cv, z zone, keyframe bool, frames keyframeSet) string {
	switch {
	case strings.HasPrefix(name, "-"):
		return "vendor-prefixed animations are not allowed; use animation"
	case keyframe:
		switch name {
		case "animation-timing-function":
			if steps, _ := timingSteps(value); steps != 1 {
				return "steps() inside @keyframes is not allowed: it would multiply the animation's flash rate"
			}
			return ""
		case "animation-composition":
			return ""
		}
		return "only animation-timing-function and animation-composition apply inside @keyframes"
	case name == "animation":
		return checkAnimation(value, z, frames)
	case slices.Contains(rateFreeLonghands, name):
		return ""
	}
	return "set animations with the animation shorthand: its duration, steps and keyframes are checked together"
}

// timingSteps returns how many jumps a timing function makes per interval (1
// for a smooth one), or ok false when value is not a timing function.
func timingSteps(value []cv) (int, bool) {
	value = trim(value)
	if len(value) != 1 {
		return 0, false
	}
	switch v := value[0]; {
	case v.kind == tIdent && slices.Contains(timingKeywords, asciiLower(v.value)):
		return 1, true
	case v.kind == tFunction:
		return timingFunction(v)
	}
	return 0, false
}

func timingFunction(v cv) (int, bool) {
	switch asciiLower(v.value) {
	case "cubic-bezier", "linear":
		for _, k := range v.kids {
			if k.kind != tNumber && k.kind != tPercentage && k.kind != tComma && k.kind != tWhitespace {
				return 0, false
			}
		}
		return 1, true
	case "steps":
		args := splitCommas(v.kids)
		if len(args) < 1 || len(args) > 2 || !integer(trim(args[0]), 1000) {
			return 0, false
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(trim(args[0])[0].num, "+"))
		if n < 1 {
			return 0, false
		}
		if len(args) == 2 {
			if a := trim(args[1]); len(a) != 1 || a[0].kind != tIdent {
				return 0, false
			}
		}
		return n, true
	}
	return 0, false
}

// checkAnimation reads an animation shorthand, a comma list of single
// animations, and checks each against the rules above.
func checkAnimation(value []cv, z zone, frames keyframeSet) string {
	items := splitCommas(value)
	if len(items) == 1 {
		if t := trim(items[0]); len(t) == 1 && t[0].kind == tIdent && slices.Contains(cssWide, asciiLower(t[0].value)) {
			return ""
		}
	}
	for _, item := range items {
		var name string
		var seconds []float64
		steps := 1
		for _, v := range item {
			switch v.kind {
			case tWhitespace:
			case tDimension:
				n, err := strconv.ParseFloat(v.num, 64)
				unit := asciiLower(v.value)
				if err != nil || (unit != "s" && unit != "ms") || len(seconds) == 2 {
					return "malformed animation time"
				}
				if unit == "ms" {
					n /= 1000
				}
				seconds = append(seconds, n)
			case tNumber:
				if n, err := strconv.ParseFloat(v.num, 64); err != nil || n < 0 {
					return "malformed animation iteration count"
				}
			case tIdent:
				word := asciiLower(v.value)
				switch {
				case word == "step-start" || word == "step-end" || slices.Contains(timingKeywords, word) || slices.Contains(animationKeywords, word):
				case name != "":
					return "an animation names one @keyframes"
				default:
					if _, ok := frames[v.value]; !ok {
						return "animation names " + v.value + ", which is not one of this room's @keyframes"
					}
					name = v.value
				}
			case tFunction:
				n, ok := timingFunction(v)
				if !ok {
					return v.value + "() is not an animation timing function (var() and calc() are not allowed here)"
				}
				steps = n
			default:
				return "unexpected token in animation"
			}
		}
		if name == "" {
			continue
		}
		info := frames[name]
		switch {
		case z == zonePage && !info.page:
			return "outside a post body or a free hook, an animation may change only backgrounds, colours and shadows"
		case z == zoneFree && !info.free:
			return "these @keyframes change something the free zone does not allow"
		}
		changes := (info.stops - 1) * steps
		if len(seconds) == 0 || seconds[0] < MinAnimationSeconds || seconds[0]*MaxChangesPerSecond < float64(changes) {
			duration := 0.0
			if len(seconds) > 0 {
				duration = seconds[0]
			}
			return fmt.Sprintf("an animation must last at least %gs and change at most %d times a second (WCAG 2.3.1, flashing): this one changes %d times in %gs",
				MinAnimationSeconds, MaxChangesPerSecond, changes, duration)
		}
	}
	return ""
}

// classifyFrames summarizes one @keyframes block. value returns a
// declaration's checked value, or false when it is dropped; offsets are the
// keyframe selectors that survive.
func classifyFrames(block []cv, value func(d declaration) ([]cv, bool)) (keyframeInfo, bool) {
	info := keyframeInfo{page: true, free: true}
	offsets := map[string]bool{"0": true, "100": true}
	any := false
	for _, kf := range parseRules(block, false) {
		if kf.at != "" || kf.block == nil {
			continue
		}
		keys, ok := keyframeOffsets(kf.prelude)
		if !ok {
			continue
		}
		decls, _ := parseDeclarations(kf.block.kids)
		kept := false
		for _, d := range decls {
			v, ok := value(d)
			if !ok {
				continue
			}
			kept = true
			name := d.name
			if !strings.HasPrefix(name, "--") {
				name = asciiLower(name)
			}
			if isAnimation(name) {
				continue // a keyframe's own easing, already refused if it steps
			}
			info.page = info.page && slices.Contains(pageAnimatable, name) && pageDeclaration(name, v) == ""
			info.free = info.free && freeDeclaration(name, v) == ""
		}
		if !kept {
			continue
		}
		any = true
		for _, key := range keys {
			offsets[key] = true
		}
	}
	info.stops = len(offsets)
	return info, any
}

// keyframeOffsets reads a keyframe selector list: from, to and percentages.
func keyframeOffsets(prelude []cv) ([]string, bool) {
	var keys []string
	for _, item := range splitCommas(prelude) {
		item = trim(item)
		if len(item) != 1 {
			return nil, false
		}
		switch v := item[0]; {
		case v.kind == tPercentage:
			n, err := strconv.ParseFloat(v.num, 64)
			if err != nil {
				return nil, false
			}
			keys = append(keys, strconv.FormatFloat(n, 'g', -1, 64))
		case v.kind == tIdent && equalFold(v.value, "from"):
			keys = append(keys, "0")
		case v.kind == tIdent && equalFold(v.value, "to"):
			keys = append(keys, "100")
		default:
			return nil, false
		}
	}
	return keys, len(keys) > 0
}
