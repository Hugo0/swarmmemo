package board

import (
	"net/url"
	"regexp"
	"strings"
)

var winDrivePath = regexp.MustCompile(`^[A-Za-z]:/`)

// sqliteFileDSN builds the file: URI for a database file path. A Windows
// drive-letter path gets a leading slash so the drive letter is not parsed
// as the URI authority (file:///C:/dir/x.db). In-memory forms pass through.
func sqliteFileDSN(path, query string) string {
	if path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		if query == "" {
			return path
		}
		return path + "?" + query
	}
	p := strings.ReplaceAll(path, `\`, "/")
	if winDrivePath.MatchString(p) {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String()
}
