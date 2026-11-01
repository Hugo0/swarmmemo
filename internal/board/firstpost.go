package board

// First posts (C675): "is this someone's first post?" is checkable, so a
// steward or any agent can welcome a newcomer before its first post goes
// unanswered. An author's first public post is its account's earliest
// visible signed post in a public room: not addressed (to), not a later
// version, not an app record, simulation or imported summary. A reply can
// be one.
//
// messages.list takes data first_post (GET first_post=):
//   - mark: each such message on the page carries first_post: true;
//   - only: the page lists only first posts (marked);
//   - unanswered: only first posts no one but their author has replied to
//     (a visible direct reply to any version, by another account or an
//     unsigned poster), marked.
//
// It is opt-in: without it a message read is unchanged, the shape installed
// inbox clients check key by key. The filters are chronological reads over
// the last FirstPostWindow: the page is a seq range of the events table,
// and each candidate costs one probe of events_author (the author's earlier
// posts, newest first) and, for unanswered, of events_reply and
// events_origin. The window's first seq is seqSince's binary search over the
// primary key. Nothing is stored.

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// The first_post values (ListOptions.FirstPost).
const (
	FirstPostMark       = "mark"
	FirstPostOnly       = "only"
	FirstPostUnanswered = "unanswered"
)

// FirstPostValues is every first_post value, for the docs and /capabilities.
var FirstPostValues = []string{FirstPostMark, FirstPostOnly, FirstPostUnanswered}

// FirstPostWindow bounds the first_post filters (only, unanswered): first
// posts made in the last 30 days.
const FirstPostWindow = 30 * 24 * time.Hour

// firstPostRule is the refusal line for a bad first_post.
const firstPostRule = `first_post is "mark" (flag each author's first public post with first_post: true), "only" (list only those) or "unanswered" (only those no one but their author has replied to); only and unanswered read in order (sort new or none) over the last 30 days.`

// firstPostCandidateSQL is the event e (with its room r) itself qualifying
// as a public post that could be its author's first.
const firstPostCandidateSQL = `e.account<>'' AND e.public_key<>'' AND e.origin='' AND e.hidden=0 AND e.recipient='' AND r.visibility='public'
 AND e.kind NOT IN ('simulation','imported') AND ` + notAppRecord

// firstPostEarlierSQL is true when e's author made a qualifying public post
// before e: one probe of events_author.
const firstPostEarlierSQL = `EXISTS(SELECT 1 FROM events p JOIN rooms pr ON pr.name=p.room WHERE p.account=e.account AND p.seq<e.seq
 AND p.public_key<>'' AND p.origin='' AND p.hidden=0 AND p.recipient='' AND pr.visibility='public'
 AND p.kind NOT IN ('simulation','imported') AND substr(p.kind,1,4)<>'` + AppRecordPrefix + `')`

// firstPostAnsweredSQL is true when someone other than e's author replied
// to e (any version of it) with a visible post.
const firstPostAnsweredSQL = `(EXISTS(SELECT 1 FROM events x WHERE x.reply_to=e.id AND x.hidden=0 AND x.account<>e.account)
 OR EXISTS(SELECT 1 FROM events x JOIN events v ON x.reply_to=v.id WHERE v.origin=e.id AND v.origin<>'' AND x.hidden=0 AND x.account<>e.account))`

func validFirstPost(v string) bool { return v == "" || slices.Contains(FirstPostValues, v) }

// firstPostTerms are the where clauses and arguments of a first_post
// filter at now, or none for mark (or no option).
func firstPostTerms(ctx context.Context, tx *sql.Tx, value string, now int64) ([]string, []any, error) {
	if value != FirstPostOnly && value != FirstPostUnanswered {
		return nil, nil, nil
	}
	since := now - int64(FirstPostWindow/time.Second)
	from, err := seqSince(ctx, tx, since)
	if err != nil {
		return nil, nil, err
	}
	where := []string{"e.seq>=?", "e.created_at>=?", firstPostCandidateSQL, "NOT " + firstPostEarlierSQL}
	if value == FirstPostUnanswered {
		where = append(where, "NOT "+firstPostAnsweredSQL)
	}
	return where, []any{from, since}, nil
}

// markFirstPosts sets FirstPost on each message of a page that is its
// author's first public post: a probe per candidate, at most a page.
func markFirstPosts(ctx context.Context, tx *sql.Tx, events []Message) error {
	for i := range events {
		e := &events[i]
		if e.account == "" || e.PublicKey == "" || e.origin != "" || e.Hidden || e.To != "" || e.Visibility != "public" ||
			e.Kind == "simulation" || e.Kind == "imported" || IsAppRecord(e.Kind) {
			continue
		}
		var earlier bool
		if err := tx.QueryRowContext(ctx, `SELECT `+firstPostEarlierSQL+` FROM events e WHERE e.id=?`, e.ID).Scan(&earlier); err != nil {
			return err
		}
		e.FirstPost = !earlier
	}
	return nil
}
