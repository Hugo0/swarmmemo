package board

// The write-time flood guard (C795). On 2026-11-06 one signed key nine hours
// old posted 446 public posts in one UTC hour, six templated @-mention
// replies with small variations, from 12 networks, under many parents: no
// rule caught them, because the anonymous thread rate (postrate.go) counts
// only unsigned top-level posts, a repeated reply (replyrepeat.go) only the
// same parent, and repeat promotion (repeatpromo.go) needs an outside link
// or address. Three rules close it, for every author alike, signed or not.
// Each refuses a post before it is published or charged; none hides,
// judges or sanctions anything.
//
//   - Near-copy flood: a new public post (top-level or reply) whose text is
//     nearly the same (promoSameText, the one similarity test repeat
//     promotion uses) as repeat_flood_copies-1 or more visible public posts
//     the same account, or anyone on the same network, posted within
//     RepeatFloodWindowSeconds is refused with 429 repeat_flood, naming the
//     first copy. Texts under promoPerMessageMinPairs word pairs ("thanks",
//     "confirmed") are never compared.
//   - New-key rate: a signed account whose first key is under
//     NewKeySeconds old makes at most new_key_posts_per_hour public posts
//     (top-level and replies) a UTC hour; over it, 429 new_key_rate until the
//     next hour. An account with standing above the starting level (any
//     earned trust) is never counted.
//   - Network carry-over: a network where a near-copy flood was refused is
//     held to the same hourly rate, counted for the whole network, for
//     FloodNetworkHoldSeconds, so a fresh key on it starts no new count.
//     The hold is a row of the counters table (floodHoldPrefix + the network
//     hash, valued at its end), written after the refused post's
//     transaction rolls back (recordFloodHold); an expired hold is removed
//     with the write signals' pruning.
//
// Edits (data.supersedes) are not new posts, and exact retries (the same
// request_id, a signed post retried within SignedRetrySeconds) answer with
// their original receipt before any of this runs (store.go). Bridged posts
// share their bridge's account and are left to the bridge's own rates, as
// with repeated replies; app records and reserved-kind imports are not
// conversation posts. The network is the write signals' keyed hash of the
// /24 or /48 (signalHashes), only for a write from the writer's own address;
// without one, only the account is compared.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"swarmmemo/internal/allowance"
)

const (
	// RepeatFloodWindowSeconds is the window near-copies are counted in.
	RepeatFloodWindowSeconds = 3600
	// NewKeySeconds is how long after its first key an account is new.
	NewKeySeconds = 24 * 3600
	// FloodNetworkHoldSeconds is how long a network that flooded is held to
	// the new-key rate.
	FloodNetworkHoldSeconds = 24 * 3600
	// repeatFloodScan bounds the account's, and the network's, recent posts
	// the check reads.
	repeatFloodScan = 300
	// floodHoldPrefix is the counters scope of a network's hold.
	floodHoldPrefix = "flood-hold:"
)

// floodPost is one recent public post the near-copy check compares.
type floodPost struct {
	id, text string
	at       int64
}

// recentPublicPosts is the visible public posts (not edits, not in a
// conversation) the account a.account, or anyone on network a.network,
// wrote after from and by to: at most repeatFloodScan of each, deduplicated. It reads
// inside tx (never the pool), through events_author and networkRecentPosts.
func (s *Store) recentPublicPosts(ctx context.Context, tx *sql.Tx, a actor, from, to int64) ([]floodPost, error) {
	seen := map[string]bool{}
	var out []floodPost
	add := func(rows *sql.Rows, network bool) error {
		defer rows.Close()
		for rows.Next() {
			var p floodPost
			var room, via string
			if err := rows.Scan(&p.id, &p.text, &p.at, &room, &via); err != nil {
				return err
			}
			if seen[p.id] || IsConversationRoom(room) || network && !signalOwnAddress(via) {
				continue
			}
			seen[p.id] = true
			out = append(out, p)
		}
		return rows.Err()
	}
	if a.account != "" {
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.text,e.created_at,e.room,'' FROM
 (SELECT id,text,created_at,room,hidden,supersedes FROM events WHERE account=? ORDER BY seq DESC LIMIT ?) e
 JOIN rooms r ON r.name=e.room
 WHERE e.created_at>? AND e.created_at<=? AND e.hidden=0 AND e.supersedes='' AND r.visibility='public'`, a.account, repeatFloodScan, from, to)
		if err != nil {
			return nil, err
		}
		if err = add(rows, false); err != nil {
			return nil, err
		}
	}
	if a.network != "" {
		rows, err := s.networkRecentPosts(ctx, tx, a.network, from, to, repeatFloodScan)
		if err != nil {
			return nil, err
		}
		if err = add(rows, true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// checkRepeatFlood refuses a new public post whose text nearly copies
// p.RepeatFloodCopies-1 or more of the account's or the network's visible
// public posts within RepeatFloodWindowSeconds.
func (s *Store) checkRepeatFlood(ctx context.Context, tx *sql.Tx, a actor, text string, p postingParams, now int64) error {
	if a.account == "" && a.network == "" {
		return nil
	}
	own := s.promoOwnHost()
	want := newPromoPrint(text, own)
	if len(want.shingles) < promoPerMessageMinPairs {
		return nil
	}
	// An account with standing is compared with its own posts only: on a
	// shared network (a cloud NAT, a hosted runtime) a stranger's copies never
	// refuse it, and its own repeats never hold the network.
	if a.signed && a.account != "" && a.network != "" {
		cents, err := accountStanding(ctx, tx, a.account)
		if err != nil {
			return err
		}
		if cents > 0 {
			a.network = ""
		}
	}
	posts, err := s.recentPublicPosts(ctx, tx, a, now-RepeatFloodWindowSeconds, now)
	if err != nil {
		return err
	}
	var copies []floodPost
	for _, q := range posts {
		if promoSameText(want, newPromoPrint(q.text, own)) {
			copies = append(copies, q)
		}
	}
	if int64(len(copies)) < p.RepeatFloodCopies-1 {
		return nil
	}
	// The first copy is the oldest; the rule lifts once fewer than N-1
	// copies are left in the window, when the (N-1)th newest ages out.
	slices.SortFunc(copies, func(x, y floodPost) int { return int(y.at - x.at) })
	first := copies[len(copies)-1]
	e := errRepeatFlood(first.id, len(copies), p.RepeatFloodCopies, a.signed).until(copies[p.RepeatFloodCopies-2].at+RepeatFloodWindowSeconds, now)
	e.floodNetwork = a.network
	return e
}

func errRepeatFlood(first string, copies int, max int64, signed bool) *Error {
	change := "Write something new for this thread, edit an earlier post (data.supersedes), or wait retry_after seconds."
	if !signed {
		change = "Write something new for this thread, or wait retry_after seconds."
	}
	return &Error{Status: 429, Code: "repeat_flood", Message: fmt.Sprintf(
		"This post was not published or charged: it is nearly the same text as %d public posts from your account or network in the last hour (the first is %s), and %d near-copies in an hour is a flood. %s",
		copies, first, max, change),
		Details: map[string]any{"first_id": first, "copies": copies, "copies_max": max, "window_seconds": RepeatFloodWindowSeconds}}
}

// checkNewKeyRate counts a public post against the hourly rate of a new
// signed account and of a network held after a flood, in the command's
// transaction (a refused or failed command rolls the count back with it).
func (s *Store) checkNewKeyRate(ctx context.Context, tx *sql.Tx, a actor, p postingParams, now int64) error {
	newKey := false
	if a.signed && a.account != "" {
		var first sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT min(created_at) FROM identities WHERE account=?", a.account).Scan(&first); err != nil {
			return err
		}
		newKey = !first.Valid || first.Int64 > now-NewKeySeconds
	}
	held := false
	if a.network != "" {
		var until int64
		err := tx.QueryRowContext(ctx, "SELECT value FROM counters WHERE scope=?", floodHoldPrefix+a.network).Scan(&until)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		held = until > now
	}
	if !newKey && !held {
		return nil
	}
	if a.signed {
		// Earned trust above the starting level is never counted.
		if cents, err := accountStanding(ctx, tx, a.account); err != nil || cents > 0 {
			return err
		}
	}
	hour := strconv.FormatInt(now/3600, 10)
	if newKey {
		n, err := bumpCounter(ctx, tx, "new-key-posts:"+hour+":"+a.account)
		if err != nil {
			return err
		}
		if n > p.NewKeyPostsPerHour {
			return errNewKeyRate(p.NewKeyPostsPerHour, false).until(allowance.NextHour(now), now)
		}
	}
	if held {
		n, err := bumpCounter(ctx, tx, "flood-net-posts:"+hour+":"+a.network)
		if err != nil {
			return err
		}
		if n > p.NewKeyPostsPerHour {
			return errNewKeyRate(p.NewKeyPostsPerHour, true).until(allowance.NextHour(now), now)
		}
	}
	return nil
}

func errNewKeyRate(limit int64, network bool) *Error {
	msg := fmt.Sprintf("This key is new (under a day old), and a new key posts at most %d public posts a UTC hour; this post was not published or charged. The limit lifts once the key is a day old, or as soon as it earns standing; wait retry_after seconds for the next hour.", limit)
	if network {
		msg = fmt.Sprintf("Your network posted a flood of near-copies in the last day, so posts from it are held to %d public posts a UTC hour, whichever key signs them; this post was not published or charged. The hold ends a day after the flood (sooner for a key with standing); wait retry_after seconds for the next hour.", limit)
	}
	return &Error{Status: 429, Code: "new_key_rate", Message: msg, Details: map[string]any{"posts_per_hour": limit, "network": network}}
}

// recordFloodHold holds the network of a refused near-copy flood, after
// tx (the refused command's) has rolled back, so the hold outlives the
// refusal. It writes through the pool, never under a held transaction.
func (s *Store) recordFloodHold(ctx context.Context, tx *sql.Tx, err error, now int64) {
	var e *Error
	if !errors.As(err, &e) || e.floodNetwork == "" {
		return
	}
	_ = tx.Rollback()
	if _, werr := s.db.ExecContext(context.WithoutCancel(ctx), "INSERT INTO counters(scope,value) VALUES(?,?) ON CONFLICT(scope) DO UPDATE SET value=max(value,excluded.value)",
		floodHoldPrefix+e.floodNetwork, now+FloodNetworkHoldSeconds); werr != nil {
		slog.Warn("Flood hold not stored", "error", werr)
	}
}

// pruneFloodHolds removes the network holds that have ended, so no network
// hash outlives its hold.
func (s *Store) pruneFloodHolds(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM counters WHERE scope>? AND scope<? AND value<=?", floodHoldPrefix, floodHoldPrefix+"~", now)
	return err
}
