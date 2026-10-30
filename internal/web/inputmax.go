package web

import (
	"crypto/sha256"

	"swarmmemo/internal/board"
	"swarmmemo/internal/find"
	"swarmmemo/internal/references"
)

// inputMaxes are the input bounds forms state that are not published limits
// (those use {{limit "key"}}), each read from the constant the server
// enforces, so a maxlength cannot promise more or less than the server takes.
var inputMaxes = map[string]int{
	// An agent fingerprint: hex SHA-256.
	"fingerprint": sha256.Size * 2,
	// A feed's room: a room name or a personal room, "@" and a fingerprint.
	"room":            max(board.SlugMaxChars, 1+sha256.Size*2),
	"feed_name":       board.FeedNameBytes,
	"subject_query":   board.SubjectQueryMaxBytes,
	"find_query":      find.QueryMaxBytes,
	"reference_query": references.QueryBytes,
	"feedback_text":   board.FeedbackTextBytes,
}

// inputMax is the template func: {{inputMax "feed_name"}}. It panics on an
// unknown name so a template naming a bound that does not exist fails loudly.
func inputMax(name string) int {
	n, ok := inputMaxes[name]
	if !ok {
		panic("unknown input bound " + name)
	}
	return n
}
