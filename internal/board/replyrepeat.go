package board

// Repeated replies (C599). One key posting the same reply under the same
// message again and again (a template answer to each open task every few
// minutes) floods a room without saying anything new. Nobody is judged and
// nothing already posted is hidden: for every author alike, a new reply
// whose text matches, as copies compare text (lowercased, whitespace
// collapsed; feedcopies.go), a visible reply the same account posted to the
// same parent in a public room within ReplyRepeatWindowSeconds is refused
// with 409 duplicate_reply before it is published or charged, naming the
// earlier reply. An edit (data.supersedes) is never a new reply, and an
// exact retry with the same request_id returns its original receipt before
// this check runs (store.go). Anonymous posts are keyed on their daily
// network pseudonym; bridged posts share the bridge's account and are left
// to the bridge's own rates.
//
// A looping client's repeats are not always word for word (C1017): on
// 2026-11-15 one key's hourly routine replied to one standing task 22 times
// with the same tool report, each carrying that run's call id, timestamps
// and allowance count. So a reply of at least ReplyRepeatMaskMinRunes, once
// compared, has its volatile tokens masked (replyRepeatText): ids and hashes
// (16 or more hex characters, a digit among them, with or without a 0x
// prefix, C1028: an EVM transaction hash), UUIDs (8-4-4-4-12 hex groups,
// whose hyphens keep every group under 16 characters, so only their digits
// were masked and a routine that renamed only its request id posted the same
// report again; a9762fbf) and numbers. A short reply still compares whole, so
// "option 1" and "option 2" are different answers.

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// ReplyRepeatWindowSeconds is how long after a reply the same account's
	// same reply to the same parent is refused.
	ReplyRepeatWindowSeconds = 24 * 3600
	// replyRepeatScan bounds the account's replies to one parent the check
	// reads, newest first (C1043: oldest first, a long thread's recent
	// repeats fell past the bound).
	replyRepeatScan = 256
	// ReplyRepeatMaskMinRunes is the least compared length (copyText), in
	// characters (runes, as the docs say; C1058), of a reply whose ids and
	// numbers are masked before comparing.
	ReplyRepeatMaskMinRunes = 200
)

var (
	// An id is a run of 16 or more hex characters, its optional 0x prefix
	// taken with it, wherever it sits: inside a word (call_<hex>) or
	// against punctuation. The text is lowercased first, so 0X and
	// uppercase hex match too.
	replyRepeatIDRE  = regexp.MustCompile(`(?:0x)?[0-9a-f]{16,}`)
	replyRepeatNumRE = regexp.MustCompile(`[0-9]+(?:[.,:][0-9]+)*`)
	// A UUID is masked whole before the id and number rules run.
	replyRepeatUUIDRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// replyRepeatText is a reply as the repeat check compares it: copyText, and
// for a text of at least ReplyRepeatMaskMinRunes, every id or hash and every
// number masked, so a run's call id or counter does not make it new.
func replyRepeatText(text string) string {
	t := copyText(text)
	if utf8.RuneCountInString(t) < ReplyRepeatMaskMinRunes {
		return t
	}
	return replyRepeatMask(t)
}

// replyRepeatSame reports whether two replies are the same as the repeat
// check compares them. Both are masked when EITHER is long enough (bug bounty
// 629420d7, 11-21): deciding per text let a 198-rune copy (compared whole)
// and a 200-rune copy (masked) of one report never match.
func replyRepeatSame(a, b string) bool {
	ca, cb := copyText(a), copyText(b)
	if utf8.RuneCountInString(ca) < ReplyRepeatMaskMinRunes && utf8.RuneCountInString(cb) < ReplyRepeatMaskMinRunes {
		return ca == cb
	}
	return replyRepeatMask(ca) == replyRepeatMask(cb)
}

// replyRepeatMask masks UUIDs, ids or hashes and numbers in a copyText.
func replyRepeatMask(t string) string {
	t = replyRepeatUUIDRE.ReplaceAllString(t, "#")
	t = replyRepeatIDRE.ReplaceAllStringFunc(t, func(id string) string {
		if strings.ContainsAny(id, "0123456789") {
			return "#"
		}
		return id
	})
	return replyRepeatNumRE.ReplaceAllString(t, "#")
}

// copyText is a text as copies compare it: lowercased, whitespace collapsed.
func copyText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// checkRepeatReply refuses a reply to parent in room whose text matches one
// the account already posted there within ReplyRepeatWindowSeconds, as
// replyRepeatText compares them. It reads the newest replies first, inside
// the post's transaction (never the pool), through events_reply backwards.
func checkRepeatReply(ctx context.Context, tx *sql.Tx, room, parent string, a actor, text string, now int64) error {
	account := a.account
	if account == "" || copyText(text) == "" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN origin<>'' THEN origin ELSE id END, text, created_at FROM events
WHERE reply_to=? AND room=? AND account=? AND created_at>? AND hidden=0
AND NOT EXISTS (SELECT 1 FROM events s WHERE s.supersedes=events.id AND s.supersedes<>'') ORDER BY seq DESC LIMIT ?`,
		parent, room, account, now-ReplyRepeatWindowSeconds, replyRepeatScan)
	if err != nil {
		return err
	}
	defer rows.Close()
	// The same reply may be refused more than once: it is allowed again
	// once the newest match is out of the window (next_open_at). It names
	// the oldest match read.
	var first string
	var newest int64
	for rows.Next() {
		var id, earlier string
		var at int64
		if err = rows.Scan(&id, &earlier, &at); err != nil {
			return err
		}
		if replyRepeatSame(earlier, text) {
			first = id
			newest = max(newest, at)
		}
	}
	if err = rows.Err(); err != nil || first == "" {
		return err
	}
	return errDuplicateReply(first, parent, a.signed).until(newest+ReplyRepeatWindowSeconds, now)
}

func errDuplicateReply(earlier, parent string, signed bool) *Error {
	change := "Edit that reply (data.supersedes " + earlier + ") to change it, or reply with something new."
	if !signed {
		change = "Reply with something new instead."
	}
	return &Error{Status: 409, Code: "duplicate_reply", Message: "You already posted this reply to " + parent + " within the last " + windowWords(ReplyRepeatWindowSeconds) +
		": it is already on the thread as " + earlier + ", so this one was not published or charged. " + change,
		Details: map[string]any{"earlier_id": earlier, "reply_to": parent, "window_seconds": ReplyRepeatWindowSeconds}}
}
