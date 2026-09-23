package roomstyle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzSanitize: for any input, Sanitize never panics; its output always passes
// the independent Verify (an "internal check" error is a sanitizer bug, not a
// rejection); and sanitizing the output again returns it unchanged, with no
// warnings. Run: go test -run '^$' -fuzz FuzzSanitize -fuzztime 120s ./internal/roomstyle
func FuzzSanitize(f *testing.F) {
	paths, _ := filepath.Glob("examples/*.css")
	for _, path := range paths {
		if src, err := os.ReadFile(path); err == nil {
			f.Add(string(src))
		}
	}
	for _, seed := range []string{
		`input[value^=a]{background:url(//evil/a)}`, `@import url(x);`, `a{background:u\72l(//e)}`,
		"a{b:ur\\\nl(x)}", `a{b:u/**/rl(x)}`, `a{content:"x`, `a{b:url(x`, `:scope{--x:url(//e)}p{b:var(--x)}`,
		`p:has(+ a) > b ~ c{d:e!important}`, `@media (min-width:1px){@supports (display:grid){p{a:b}}}`,
		`@keyframes k{from{a:b}50%{c:d}}p{animation:k 1s}`, `@font-face{font-family:F;src:url(/a/` + liveID + `)}p{font-family:F}`,
		`p{margin:1\65 3;width:1e3px;x:-\31 a;y:#\31 23;z:2n+1}`, `@layer a.b,c;@layer{p{a:b}}`,
		`p{background:url("data:image/png;base64,` + tinyPNG + `")}`, `li:nth-child(-n+3 of p){}`,
		`<!-- p{a:b} -->`, `p::before{content:"</style>\"\\" attr(title)}`, `\@media{} @\media{} .\-{} -\-a{b:c}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, css string) {
		out, _, err := SanitizeWith(css, room, roomOptions)
		if err != nil {
			if strings.Contains(err.Error(), "internal check") {
				t.Fatalf("sanitizer produced output its verifier rejects: %v\ninput: %q", err, css)
			}
			return
		}
		again, warnings, err := SanitizeWith(out.CSS, room, roomOptions)
		if err != nil || again.CSS != out.CSS || len(warnings) != 0 {
			t.Fatalf("not idempotent: %v %v\ninput: %q\nfirst:  %q\nsecond: %q", err, warnings, css, out.CSS, again.CSS)
		}
	})
}
