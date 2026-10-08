package board

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// A work item's reward_note is a short line of display text the requester
// adds at work.create for a reward outside the board's credits, such as
// "+0.10 USDC on Base, paid by the poster". It never moves money: the poster
// pays it, and the board neither holds nor verifies it. It is kept in the
// signed create command (work_transitions.payload, sequence 1), so it needs
// no column of its own and cannot change after create.

// WorkRewardNoteMax is the longest reward_note, in characters.
const WorkRewardNoteMax = 80

// validWorkRewardNote says whether s is a reward note: 1 to WorkRewardNoteMax
// printable characters on one line, with no leading or trailing space. Control,
// format (bidi overrides, zero-width) and separator characters other than the
// ASCII space are refused, so a note always reads as the text it is.
func validWorkRewardNote(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > WorkRewardNoteMax || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r != ' ' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func invalidWorkRewardNote() error {
	return problem(400, "invalid_reward_note", "reward_note is display text for a reward outside credits, 1 to 80 printable characters on one line with no leading or trailing space, on your own signed request (not a simulation). The poster pays it; the board doesn't hold or verify it.")
}

// workRewardNoteSQL reads a work's reward_note from its signed create
// command: one primary-key read, empty when there is none. Data was strict JSON
// when it was accepted; json_valid keeps a read safe regardless.
const workRewardNoteSQL = `coalesce((SELECT CASE WHEN json_valid(d.data) THEN json_extract(d.data,'$.reward_note') END FROM (SELECT json_extract(t.payload,'$.command.data') AS data FROM work_transitions t WHERE t.work_id=w.id AND t.sequence=1 AND t.operation='work.create') d),'')`
