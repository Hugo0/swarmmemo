package roomstyle

import (
	"strings"
	"testing"
)

// The free zone keeps what a bold theme needs: hiding, positioning, stacking
// under the cap, transforms, generated boxes, letterless generated text,
// decimal counters and animations of anything the zone allows.
func TestFreeZoneKeepsBoldLayout(t *testing.T) {
	S := "." + ScopeClass(room)
	out, warnings := sanitize(t, `
.sidebar{display:none}
.room-header{position:sticky;top:0;z-index:100;transform:rotate(-1deg);opacity:.9;filter:hue-rotate(30deg);overflow:hidden;height:40px}
.room-header::after{content:"";position:absolute;right:0;bottom:0;width:64px;height:48px;background:url(/a/`+liveID+`) no-repeat}
.post-meta::before{content:counter(item, decimal) ". ▲ │ →";color:#828282}
.post-meta .timestamp{display:none}
.via::before{content:"| "}
.sidebar li{list-style:square inside}
@keyframes walk{from{transform:translateX(0)}to{transform:translateX(600px)}}
.room-header::after{animation:walk 12s linear infinite}
.post{counter-increment:item;order:2}
.feed{counter-reset:item}
`)
	for _, want := range []string{
		S + " .sidebar{display:none;}",
		"z-index:100;transform:rotate(-1deg);opacity:.9;filter:hue-rotate(30deg);overflow:hidden;height:40px;",
		S + ` .page-heading::after{content:"";position:absolute;`,
		`content:counter(item, decimal) ". ▲ │ →";`,
		S + " .memo-meta .memo-time{display:none;}",
		"animation:" + ScopeClass(room) + "-walk 12s linear infinite;",
		S + " .memo{counter-increment:item;order:2;}",
	} {
		if !strings.Contains(out.CSS, want) {
			t.Errorf("missing %q in\n%s", want, out.CSS)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
}

// Zones follow the subject: a sibling step off a free or body root leaves it.
func TestFreeZoneBoundaries(t *testing.T) {
	for css, free := range map[string]bool{
		".sidebar p{position:fixed}":                   true,
		".sidebar ~ .feed-column{position:fixed}":      false,
		".room-header + .layout{position:fixed}":       false,
		".post-meta .kind{position:relative}":          true,
		".post-meta ~ .post-footer{position:relative}": false,
		".post .post-meta{position:relative}":          true,
		".post{position:relative}":                     false,
		":scope .footer a{position:relative}":          true,
		".sidebar:not(.button){position:relative}":     true,
		".panel{position:relative}":                    false, // the composer is a panel
		".post-body ~ .post-meta{position:relative}":   true,
		"div{position:relative}":                       false,
	} {
		out, _ := sanitize(t, css)
		if kept := strings.Contains(out.CSS, "position:"); kept != free {
			t.Errorf("%s: position kept=%v, want free=%v\n%s", css, kept, free, out.CSS)
		}
	}
}

// Page-zone animations may move only backgrounds and colours; every zone is
// held to the flash rate, where each keyframe interval and each step counts.
func TestAnimationRate(t *testing.T) {
	for css, ok := range map[string]bool{
		`@keyframes k{from{background-position:0 0}to{background-position:256px 0}}.layout{animation:k 20s linear infinite}`: true,
		`@keyframes k{50%{background-color:#123}}.site-header{animation:k 1s ease-in-out infinite alternate}`:                true,
		`@keyframes k{to{color:#fff}}.nav{animation:k 1s steps(3) infinite}`:                                                 true,
		`@keyframes k{to{color:#fff}}.nav{animation:k 1s steps(4) infinite}`:                                                 false,
		`@keyframes k{25%{color:#000}50%{color:#fff}75%{color:#000}}.nav{animation:k 1s infinite}`:                           false,
		`@keyframes k{25%{color:#000}50%{color:#fff}75%{color:#000}}.nav{animation:k 1500ms infinite}`:                       true,
		`@keyframes k{to{color:#fff}}.nav{animation:k 999ms}`:                                                                false,
		`@keyframes k{to{color:rgb(0 0 0 / 0)}}.nav{animation:k 2s}`:                                                         false,
		`@keyframes k{to{opacity:.5}}.sidebar{animation:k 2s infinite alternate}`:                                            true,
		`@keyframes k{to{opacity:.5}}.feed{animation:k 2s infinite alternate}`:                                               false,
		`@keyframes k{to{opacity:.5}}.post-body p{animation:k 2s infinite alternate,k 3s}`:                                   true,
		`@keyframes k{to{opacity:.5}}.post-body p{animation:k 2s infinite alternate,k 3s steps(10)}`:                         false,
		`.post-body p{animation:none}`: true,
	} {
		out, warnings := sanitize(t, css)
		kept := strings.Contains(out.CSS, "animation:")
		if kept != ok || (ok && len(warnings) != 0) {
			t.Errorf("%s: kept=%v warnings=%v\n%s", css, kept, warnings, out.CSS)
		}
	}
}

// A keyframes rule the output will not hold (inside a dropped group) cannot
// make an animation pass: the output, sanitized again, must agree.
func TestAnimationNeedsKeyframesTheOutputKeeps(t *testing.T) {
	out, warnings := sanitize(t, `@media (evil:url(x)){@keyframes k{to{color:#fff}}}.nav{animation:k 2s}`)
	if strings.Contains(out.CSS, "animation:") || len(warnings) == 0 {
		t.Errorf("animation kept without its keyframes: %v\n%s", warnings, out.CSS)
	}
}
