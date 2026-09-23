package roomstyle

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Trust colour knobs. Trust text is drawn in the site's pinned layer, in these
// colours on its own plate, so a room can theme bylines, timestamps and labels
// without being able to make them unreadable: each may be set once, on :scope
// at the top level, as a hex or rgb() literal, and the ink and muted colours
// must each have at least 4.5:1 contrast (WCAG AA) with the plate. Unset knobs
// keep the site's colours.
const (
	TrustInk         = "--trust-ink"
	TrustMuted       = "--trust-muted"
	TrustPlate       = "--trust-plate"
	minTrustContrast = 4.5
)

var (
	trustColorKnobs = []string{TrustInk, TrustMuted, TrustPlate}
	trustDefaults   = map[string]string{TrustInk: "#171717", TrustMuted: "#707070", TrustPlate: "#ffffff"}
)

// TrustKnobs lists the custom properties the site's trust pins read.
func TrustKnobs() []string { return append(slices.Clone(trustColorKnobs), TrustFont) }

// isScopeOnly reports a top-level rule whose only selector is :scope (or, in
// already-scoped output, the room's own class).
func isScopeOnly(prelude []cv, scope string) bool {
	p := collapse(prelude)
	return len(p) == 2 && ((p[0].kind == tColon && p[1].kind == tIdent && equalFold(p[1].value, "scope")) ||
		(delimIs(p[0], ".") && p[1].kind == tIdent && p[1].value == scope))
}

// trustKnobs checks every trust colour declaration in a stylesheet together and
// returns nil when they may all stand, or why none of them can.
func trustKnobs(rules []rule, scope string) error {
	values := map[string]string{}
	var walk func(rules []rule, top bool) error
	walk = func(rules []rule, top bool) error {
		for _, r := range rules {
			if r.block == nil {
				continue
			}
			if r.at != "" {
				if err := walk(parseRules(r.block.kids, false), false); err != nil {
					return err
				}
				continue
			}
			decls, _ := parseDeclarations(r.block.kids)
			for _, d := range decls {
				if !slices.Contains(trustColorKnobs, d.name) {
					continue
				}
				if !top || !isScopeOnly(r.prelude, scope) {
					return errors.New("trust colours may be set only in a top-level :scope rule")
				}
				if _, dup := values[d.name]; dup {
					return errors.New(d.name + " is set more than once")
				}
				values[d.name] = serialize(collapse(d.value))
			}
		}
		return nil
	}
	if err := walk(rules, true); err != nil {
		return err
	}
	rgb := map[string][3]float64{}
	for _, name := range trustColorKnobs {
		text, ok := values[name]
		if !ok {
			text = trustDefaults[name]
		}
		c, ok := parseRGB(text)
		if !ok {
			return errors.New(name + " must be a hex or rgb() colour with no transparency")
		}
		rgb[name] = c
	}
	for _, name := range []string{TrustInk, TrustMuted} {
		if contrast(rgb[name], rgb[TrustPlate]) < minTrustContrast {
			return errors.New(name + " needs at least 4.5:1 contrast with " + TrustPlate)
		}
	}
	return nil
}

// parseRGB reads #rgb, #rrggbb or rgb(r g b) / rgb(r, g, b) with 0-255 numbers.
func parseRGB(text string) ([3]float64, bool) {
	var c [3]float64
	text = strings.ToLower(strings.TrimSpace(text))
	if hex, ok := strings.CutPrefix(text, "#"); ok {
		if len(hex) == 3 {
			hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
		}
		if len(hex) != 6 {
			return c, false
		}
		for i := range 3 {
			n, err := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
			if err != nil {
				return c, false
			}
			c[i] = float64(n)
		}
		return c, true
	}
	inner, ok := strings.CutPrefix(text, "rgb(")
	if !ok || !strings.HasSuffix(inner, ")") {
		return c, false
	}
	parts := strings.FieldsFunc(strings.TrimSuffix(inner, ")"), func(r rune) bool { return r == ',' || r == ' ' })
	if len(parts) != 3 {
		return c, false
	}
	for i, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil || n < 0 || n > 255 {
			return c, false
		}
		c[i] = n
	}
	return c, true
}

// contrast is the WCAG 2 contrast ratio of two sRGB colours.
func contrast(a, b [3]float64) float64 {
	lum := func(c [3]float64) float64 {
		var l [3]float64
		for i, v := range c {
			v /= 255
			if v <= 0.04045 {
				l[i] = v / 12.92
			} else {
				l[i] = math.Pow((v+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
