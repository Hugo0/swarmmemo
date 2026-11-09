package services

import (
	"regexp"
	"strings"
)

// placeholderRE is a bare upper-case token, optionally in {} or <>: what an
// example's SOURCE_URL, {URL} or <PAGE_URL> looks like when it is sent as
// typed instead of filled in (C837).
var placeholderRE = regexp.MustCompile(`^[{<]?[A-Z][A-Z0-9_]{2,}[}>]?$`)

// PlaceholderBytes bounds a placeholder: a longer token is not named back.
const PlaceholderBytes = 64

// Placeholder is s, trimmed, when it looks like an unfilled placeholder
// copied from an example (SOURCE_URL, {URL}, <PAGE_URL>,
// SHA256_OF_THE_PAGE_TEXT), else "". Fetch names it in fetch_invalid_url;
// a notary stamp of it carries the placeholder_text hint. The one matcher
// both use.
func Placeholder(s string) string {
	t := strings.TrimSpace(s)
	if len(t) <= PlaceholderBytes && placeholderRE.MatchString(t) {
		return t
	}
	return ""
}
