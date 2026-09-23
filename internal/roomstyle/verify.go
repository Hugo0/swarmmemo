package roomstyle

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Verify is an independent check of sanitized output, run on every result
// before it is returned and by the fuzzer. It re-parses the text a browser will
// read and confirms the properties the whole design rests on, without trusting
// the code that produced it:
//
//   - the text re-serializes to itself (the browser sees what was checked);
//   - every style rule's selectors start at the canvas root and none steps to
//     its siblings or names :root, :scope, :host, html or body;
//   - every url() is an attachment path or an inline raster image;
//   - every function is on the allowlist, and no value holds a {} block;
//   - only the permitted at-rules appear, with room-prefixed global names.
func Verify(css, scope string) error {
	values, err := parseValues(css)
	if err != nil {
		return err
	}
	if again := serialize(values); again != css {
		return errors.New("output does not re-serialize to itself")
	}
	rules := parseRules(values, true)
	if err := trustKnobs(rules, scope); err != nil {
		return err
	}
	frames := keyframeSet{}
	collectFrames(rules, frames, 0)
	return verifyRules(rules, scope, frames, 0)
}

// collectFrames classifies the output's own @keyframes, for the animation check.
func collectFrames(rules []rule, frames keyframeSet, depth int) {
	for _, r := range rules {
		if r.block == nil || depth > MaxAtDepth {
			continue
		}
		switch asciiLower(r.at) {
		case "media", "supports", "container", "layer":
			collectFrames(parseRules(r.block.kids, false), frames, depth+1)
		case "keyframes":
			if p := trim(r.prelude); len(p) == 1 && p[0].kind == tIdent {
				if info, any := classifyFrames(r.block.kids, func(d declaration) ([]cv, bool) { return d.value, true }); any {
					frames.add(p[0].value, info)
				}
			}
		}
	}
}

func verifyRules(rules []rule, scope string, frames keyframeSet, depth int) error {
	if depth > MaxAtDepth {
		return errors.New("at-rules nest too deep")
	}
	for _, r := range rules {
		if r.block == nil && asciiLower(r.at) != "layer" {
			return fmt.Errorf("rule without a block: %q", r.at)
		}
		switch asciiLower(r.at) {
		case "":
			// One output rule is one zone; its most restricted selector decides.
			z := zoneBody
			for _, sel := range splitCommas(r.prelude) {
				sz, err := verifySelector(sel, scope)
				if err != nil {
					return err
				}
				z = min(z, sz)
			}
			if err := verifyDeclarations(r.block.kids, false, z, false, frames); err != nil {
				return err
			}
		case "media", "supports", "container":
			if !conditionOK(r.prelude, 0) {
				return errors.New("unsupported condition")
			}
			if err := verifyRules(parseRules(r.block.kids, false), scope, frames, depth+1); err != nil {
				return err
			}
		case "layer":
			for _, name := range splitCommas(r.prelude) {
				if len(name) > 0 && (name[0].kind != tIdent || !strings.HasPrefix(name[0].value, scope+"-")) {
					return errors.New("unprefixed layer name")
				}
			}
			if r.block != nil {
				if err := verifyRules(parseRules(r.block.kids, false), scope, frames, depth+1); err != nil {
					return err
				}
			}
		case "keyframes":
			if p := trim(r.prelude); len(p) != 1 || p[0].kind != tIdent || !strings.HasPrefix(p[0].value, scope+"-") {
				return errors.New("unprefixed keyframes name")
			}
			for _, kf := range parseRules(r.block.kids, false) {
				if kf.at != "" {
					return errors.New("at-rule inside keyframes")
				}
				if err := verifyDeclarations(kf.block.kids, false, zoneBody, true, frames); err != nil {
					return err
				}
			}
		case "font-face":
			decls, dropped := parseDeclarations(r.block.kids)
			if len(dropped) > 0 {
				return errors.New("malformed @font-face")
			}
			for _, d := range decls {
				if d.name == "font-family" && (len(d.value) != 1 || d.value[0].kind != tString || !strings.HasPrefix(d.value[0].value, scope+"-")) {
					return errors.New("unprefixed font family")
				}
				if d.name == "src" && !fontURLsOnly(d.value) {
					return errors.New("font src is not an attachment")
				}
			}
			if err := verifyDeclarations(r.block.kids, true, zoneBody, false, frames); err != nil {
				return err
			}
		default:
			return fmt.Errorf("at-rule @%s is not allowed", r.at)
		}
	}
	return nil
}

// verifySelector checks one output selector and reports its zone, recomputing
// the zone from the text rather than trusting the sanitizer's decision.
func verifySelector(sel []cv, scope string) (zone, error) {
	if len(sel) < 2 || !delimIs(sel[0], ".") || sel[1].kind != tIdent || sel[1].value != scope {
		return zonePage, errors.New("selector does not start at the room root")
	}
	rest := trim(sel[2:])
	if len(rest) > 0 && (delimIs(rest[0], "+") || delimIs(rest[0], "~")) {
		return zonePage, errors.New("selector reaches a sibling of the room root")
	}
	// The subject is in a body (or free element) when some compound names the
	// body root (or a free hook) and the step after that compound is a
	// descendant or child step; a sibling step off it leaves.
	in, cur := zonePage, zonePage
	for i, v := range rest {
		switch {
		case v.kind == tIdent && i > 0 && delimIs(rest[i-1], "."):
			cur = max(cur, classZone(v.value))
		case v.kind == tWhitespace || isCombinator(v):
			if v.kind == tWhitespace && i+1 < len(rest) && isCombinator(rest[i+1]) {
				continue
			}
			if cur != zonePage {
				if !delimIs(v, "+") && !delimIs(v, "~") {
					in = max(in, cur)
				} else if in < cur {
					in = zonePage
				}
			}
			cur = zonePage
		}
	}
	z := max(in, cur)
	return z, verifySelectorTokens(rest, z)
}

func verifySelectorTokens(list []cv, z zone) error {
	for i, v := range list {
		afterColon := i > 0 && list[i-1].kind == tColon
		afterDot := i > 0 && delimIs(list[i-1], ".")
		switch {
		case v.kind == tHash:
			return errors.New("ID selector")
		case v.kind == tIdent && afterDot:
			if hookClass(v.value) != v.value {
				return fmt.Errorf("class .%s is not a hook", v.value)
			}
		case v.kind == tIdent && afterColon:
			name := asciiLower(v.value)
			switch {
			case name == "scope" || name == "root" || name == "host":
				return errors.New("selector names :scope, :root or :host")
			case z == zonePage && slices.Contains(bodyPseudoElements, name) && !slices.Contains(pagePseudoElements, name):
				return fmt.Errorf("::%s in the page zone", name)
			}
		case v.kind == tIdent && slices.Contains(forbiddenTypes, asciiLower(v.value)):
			return errors.New("selector names the page root or a non-rendered element")
		case delimIs(v, "&") || delimIs(v, "|"):
			return errors.New("nesting or namespace in selector")
		case v.kind == tLBrace || v.kind == tLParen || v.kind == tAtKeyword || v.kind == tBadString || v.kind == tBadURL:
			return errors.New("unexpected token in selector")
		case v.kind == tFunction:
			name := asciiLower(v.value)
			if !slices.Contains(conditionFunctions, name) || name == "url" {
				return errors.New("unexpected function in selector")
			}
			if name == "has" && z != zoneBody {
				return errors.New(":has() outside a body")
			}
			// Attribute values in [] can name anything; they select nothing.
			if err := verifySelectorTokens(v.kids, z); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyDeclarations(kids []cv, fontFace bool, z zone, keyframe bool, frames keyframeSet) error {
	decls, dropped := parseDeclarations(kids)
	if len(dropped) > 0 {
		return errors.New("malformed declaration")
	}
	for _, d := range decls {
		if slices.Contains(deniedProperties, asciiLower(d.name)) {
			return fmt.Errorf("denied property %s", d.name)
		}
		if err := verifyValue(d.value, fontFace && d.name == "src"); err != nil {
			return err
		}
		if isAnimation(d.name) {
			if why := animationDeclaration(d.name, d.value, z, keyframe, frames); why != "" {
				return fmt.Errorf("%s: %s", d.name, why)
			}
		}
		switch {
		case fontFace || keyframe:
		case z == zonePage:
			if why := pageDeclaration(d.name, d.value); why != "" {
				return fmt.Errorf("page-zone %s: %s", d.name, why)
			}
		case z == zoneFree:
			if why := freeDeclaration(d.name, d.value); why != "" {
				return fmt.Errorf("free-zone %s: %s", d.name, why)
			}
		}
	}
	return nil
}

func verifyValue(list []cv, fontSrc bool) error {
	for _, v := range list {
		switch v.kind {
		case tURL:
			return errors.New("raw url token in output")
		case tFunction:
			name := asciiLower(v.value)
			if name == "url" {
				if a := trim(v.kids); len(a) != 1 || a[0].kind != tString || !allowedURLShape(a[0].value, fontSrc) {
					return errors.New("url() outside the allowlist")
				}
				continue
			}
			if !slices.Contains(valueFunctions, name) && !(fontSrc && name == "format") {
				return fmt.Errorf("function %s() in output", name)
			}
		case tLBrace, tAtKeyword, tBadString, tBadURL, tCDO, tCDC, tSemicolon:
			return errors.New("unexpected token in value")
		}
		if err := verifyValue(v.kids, fontSrc); err != nil {
			return err
		}
	}
	return nil
}

func fontURLsOnly(list []cv) bool {
	for _, v := range list {
		if v.kind == tFunction && asciiLower(v.value) == "url" {
			if a := trim(v.kids); len(a) != 1 || a[0].kind != tString || !allowedURLShape(a[0].value, true) {
				return false
			}
		}
	}
	return true
}

func allowedURLShape(u string, font bool) bool {
	if id, ok := strings.CutPrefix(u, "/a/"); ok {
		return validID(id)
	}
	if font {
		return false
	}
	for kind := range imageMagic {
		if payload, ok := strings.CutPrefix(u, "data:image/"+kind+";base64,"); ok {
			return strings.Trim(payload, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") == ""
		}
	}
	return false
}
