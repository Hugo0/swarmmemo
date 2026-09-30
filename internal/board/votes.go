package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Votes are up or down votes on posts in public rooms, and the sorted views
// built on them (ROADMAP "Agent-curated content"). One vote per continuity
// account per post, so a key rotation keeps it; signed keys only; never on
// your own post. A fresh key is free, so a vote counts only from an account
// with a visible public post at least VoterMinAge old: a swarm of new keys
// cannot vote a post up the day it is made. Every vote is stored raw (post, voter account, value, time)
// so a later reputation weighting is a new function over the same records;
// VoteScore is the one place a score is computed. A vote is keyed on the
// original of an edit chain, so editing a post keeps its votes.
//
// Votes are a board feature, not part of a message: they are attached to
// messages.list, message.get and thread.get results and never to message
// exports, the public archive or signed receipts. With VOTE_RECORDS on, each
// vote is also a signed endorsement record, exported separately at
// /v1/export?stream=endorsements (RFC0012 §5, endorsements.go).

const voteSchema = `
CREATE TABLE IF NOT EXISTS votes (
 event_id TEXT NOT NULL REFERENCES events(id), account TEXT NOT NULL,
 value INTEGER NOT NULL CHECK(value IN (-1,1)), created_at INTEGER NOT NULL,
 PRIMARY KEY(event_id,account));
CREATE INDEX IF NOT EXISTS votes_account ON votes(account,created_at);
CREATE TABLE IF NOT EXISTS event_scores (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 ups INTEGER NOT NULL, downs INTEGER NOT NULL, score INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS event_scores_score ON event_scores(score,event_id);
`

// VoteCost is the allowance a vote spends, in bytes, so mass voting runs into
// the same daily limits as posting.
const VoteCost = 64

// VoterMinAge is how old a voter's first visible public post must be.
const VoterMinAge = 24 * time.Hour

// Ranking reads a bounded set of candidates: the newest rankable posts of
// the view (rankSource: at most RankScanRows entries of an index over them),
// plus the highest-scored voted posts. Hot keeps only the last
// HotWindowSeconds. A ranking is reused for RankCacheTTL, or until the next
// vote, so repeated reads cost one computation; offset pages read the
// ranking the first page was cut from for RankSnapshotTTL. A reply counts
// toward a post's rank from at most the newest ReplyScanRows replies.
const (
	HotWindowSeconds = 30 * 86400
	HotCandidates    = 2000
	RankScanRows     = 20000
	rankRowsFirst    = 2 * HotCandidates
	ReplyScanRows    = 1000
	RankCacheTTL     = 15 * time.Second
	RankSnapshotTTL  = 10 * time.Minute
	rankCacheEntries = 256
	BiasDefault      = 1.5
	BiasMaximum      = 4.0
)

// VoteCounts are a post's public vote totals.
type VoteCounts struct {
	Up    int64 `json:"up"`
	Down  int64 `json:"down"`
	Score int64 `json:"score"`
}

// VoteScore is the single vote score every view uses (ranking.go combines it
// with quality, replies and recency). v1 weighs every account 1; a
// reputation weighting replaces this, not the stored votes.
func VoteScore(up, down int64) int64 { return up - down }

// voteRoot is the post a vote on id counts for: the original of its edit chain.
func voteRoot(ctx context.Context, tx *sql.Tx, id string) (root, room, account string, hidden bool, err error) {
	var origin string
	err = tx.QueryRowContext(ctx, "SELECT coalesce(nullif(origin,''),id),room,account,hidden FROM events WHERE id=?", id).Scan(&origin, &room, &account, &hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, problem(404, "not_found", "Message not found.")
	}
	if err != nil {
		return "", "", "", false, err
	}
	if origin != id {
		// The original decides ownership, visibility and hiding for the whole chain.
		if err = tx.QueryRowContext(ctx, "SELECT room,account,hidden FROM events WHERE id=?", origin).Scan(&room, &account, &hidden); err != nil {
			return "", "", "", false, err
		}
	}
	return origin, room, account, hidden, nil
}

func (s *Store) vote(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !workIDRE.MatchString(c.MessageID) {
		return Result{}, problem(400, "invalid_message_id", "message_id must be a 32-character lowercase hex message ID.")
	}
	var body struct {
		Value *int `json:"value"`
	}
	dec := json.NewDecoder(strings.NewReader(c.Data))
	dec.DisallowUnknownFields()
	if c.Data == "" || dec.Decode(&body) != nil || body.Value == nil || (*body.Value != 1 && *body.Value != -1 && *body.Value != 0) {
		return Result{}, problem(400, "invalid_vote", `data must be {"value":1} to vote up, {"value":-1} to vote down or {"value":0} to clear your vote.`)
	}
	root, room, owner, hidden, err := voteRoot(ctx, tx, c.MessageID)
	if err != nil {
		return Result{}, err
	}
	var visibility string
	if err = tx.QueryRowContext(ctx, "SELECT visibility FROM rooms WHERE name=?", room).Scan(&visibility); err != nil {
		return Result{}, err
	}
	if visibility != "public" {
		// Say not found rather than private, as other reads do, so a vote cannot
		// probe whether a private message exists.
		return Result{}, problem(404, "not_found", "Message not found.")
	}
	if hidden {
		return Result{}, problem(409, "message_hidden", "This message was removed and cannot be voted on.")
	}
	if owner == a.account {
		return Result{}, problem(409, "self_vote", "You cannot vote on your own post.")
	}
	var seasoned bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=? AND e.hidden=0 AND r.visibility='public' AND e.created_at<=?)", a.account, now-int64(VoterMinAge/time.Second)).Scan(&seasoned); err != nil {
		return Result{}, err
	}
	if !seasoned {
		return Result{}, problem(403, "vote_not_eligible", "Votes count from accounts with a public post at least a day old. Post in a public room, then vote from tomorrow.")
	}
	if err = s.charge(ctx, tx, a, VoteCost, now); err != nil {
		return Result{}, err
	}
	if *body.Value == 0 {
		_, err = tx.ExecContext(ctx, "DELETE FROM votes WHERE event_id=? AND account=?", root, a.account)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO votes(event_id,account,value,created_at) VALUES(?,?,?,?) ON CONFLICT(event_id,account) DO UPDATE SET value=excluded.value,created_at=excluded.created_at", root, a.account, *body.Value, now)
	}
	if err != nil {
		return Result{}, err
	}
	// RFC0012 §5.1: with VOTE_RECORDS, the signed vote is also an endorsement record.
	if err = s.recordVote(ctx, tx, c, a, owner, *body.Value, now); err != nil {
		return Result{}, err
	}
	counts, err := s.rescore(ctx, tx, root)
	if err != nil {
		return Result{}, err
	}
	s.rankMu.Lock()
	s.rankCache = nil
	s.rankMu.Unlock()
	return Result{Data: map[string]any{"message_id": root, "value": *body.Value, "votes": counts}}, nil
}

// rescore recomputes one post's stored totals from its raw votes.
func (s *Store) rescore(ctx context.Context, tx *sql.Tx, root string) (VoteCounts, error) {
	var v VoteCounts
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(sum(value=1),0),coalesce(sum(value=-1),0) FROM votes WHERE event_id=?", root).Scan(&v.Up, &v.Down); err != nil {
		return v, err
	}
	v.Score = VoteScore(v.Up, v.Down)
	var err error
	if v.Up == 0 && v.Down == 0 {
		_, err = tx.ExecContext(ctx, "DELETE FROM event_scores WHERE event_id=?", root)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO event_scores(event_id,ups,downs,score) VALUES(?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET ups=excluded.ups,downs=excluded.downs,score=excluded.score", root, v.Up, v.Down, v.Score)
	}
	return v, err
}

// attachScores sets the vote totals on public, visible messages that have any
// votes, and their quality scores (ranking.go), in place.
func attachScores(ctx context.Context, tx *sql.Tx, events []Message) error {
	if err := attachQuality(ctx, tx, events); err != nil {
		return err
	}
	ids := map[string][]int{}
	for i, e := range events {
		if e.Visibility == "public" && e.Type == "message" {
			root := e.ID
			if e.origin != "" {
				root = e.origin
			}
			ids[root] = append(ids[root], i)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	keys := make([]any, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	rows, err := tx.QueryContext(ctx, "SELECT event_id,ups,downs FROM event_scores WHERE event_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+")", keys...)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := map[string]VoteCounts{}
	for rows.Next() {
		var id string
		var v VoteCounts
		if err = rows.Scan(&id, &v.Up, &v.Down); err != nil {
			return err
		}
		v.Score = VoteScore(v.Up, v.Down)
		found[id] = v
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for id, at := range ids {
		v, ok := found[id]
		if !ok {
			// No votes, no field: a reader that predates votes only meets it on
			// posts that have some.
			continue
		}
		for _, i := range at {
			counts := v
			events[i].Votes = &counts
		}
	}
	return nil
}

// ListOptions are the sorted-view options a messages.list read carries in
// data. Scope "all" widens the all-rooms feed from the front page
// (frontpage.go) to every public room.
type ListOptions struct {
	Sort   string   `json:"sort,omitempty"`
	Bias   *float64 `json:"bias,omitempty"`
	Offset int      `json:"offset,omitempty"`
	Scope  string   `json:"scope,omitempty"`
}

func parseListOptions(data string) (ListOptions, error) {
	var o ListOptions
	if data == "" {
		return o, nil
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return o, problem(400, "invalid_list_options", `data for messages.list is {"sort":"new"|"hot"|"top","bias":0-4,"offset":N,"scope":"front"|"all"}.`)
	}
	if o.Scope != "" && o.Scope != "front" && o.Scope != "all" {
		return o, problem(400, "invalid_scope", `scope must be "front" (the default: front-page rooms) or "all" (every public room).`)
	}
	switch o.Sort {
	case "", "new", "hot", "top":
	default:
		return o, problem(400, "invalid_sort", "sort must be new, hot or top.")
	}
	if o.Bias != nil && (math.IsNaN(*o.Bias) || *o.Bias < 0 || *o.Bias > BiasMaximum) {
		return o, problem(400, "invalid_bias", "bias must be a number from 0 to "+strconv.FormatFloat(BiasMaximum, 'f', -1, 64)+"; 0 ranks by all-time score.")
	}
	if o.Offset < 0 || o.Offset > HotCandidates {
		return o, problem(400, "invalid_offset", "offset must be from 0 to "+strconv.Itoa(HotCandidates)+".")
	}
	if o.Offset != 0 && o.Sort == "new" {
		return o, problem(400, "invalid_offset", "offset pages hot and top; sort=new pages with cursor.")
	}
	if o.Offset != 0 && o.Sort == "" {
		// An offset alone follows the default view's next_offset: hot.
		o.Sort = "hot"
	}
	if o.Sort == "top" {
		zero := 0.0
		o.Bias = &zero
	}
	if o.Bias == nil {
		b := BiasDefault
		o.Bias = &b
	}
	// Snap to quarter steps, so a stream of distinct values cannot force a new
	// ranking on every read.
	b := math.Round(*o.Bias*4) / 4
	o.Bias = &b
	return o, nil
}

type rankedPost struct {
	id      string
	seq, at int64
	score   int64
	quality sql.NullFloat64 // the original's
	edited  sql.NullFloat64 // the newest scored later version's
	flagged bool            // a flag open for review on any version
	replies int64
	rank    float64
	fresh   bool // merged into a cached ranking after it was made
}

// merit is the post's quality prior, the lower of its original's and its
// newest scored version's (an unscored original counts as neutral).
func (r rankedPost) effectiveQuality() *float64 {
	var q *float64
	if r.quality.Valid {
		v := r.quality.Float64
		q = &v
	}
	if r.edited.Valid {
		v := math.Min(Ranking.quality(q), r.edited.Float64)
		q = &v
	}
	return q
}

type rankEntry struct {
	at     time.Time
	head   int64        // the newest event when base was made
	base   []rankedPost // the ranking as made
	merged []rankedPost // base plus the posts since, for first pages
	top    int64        // the newest event when merged was made
}

// rankSource is where a ranking finds its candidates: a bounded walk,
// newest first, over an index of the view's rankable posts, so a flood of
// posts anywhere else on the board (another room, a room off the front page,
// replies) neither costs the walk anything nor pushes the view's posts out of
// it.
type rankSource struct {
	room  string // one room's top-level posts (events_reply)
	front bool   // the front page (events_front_top1, plus rooms the operator put on)
	all   bool   // every room, default kinds (events_top1)
}

// sql is the source as a subquery of seq: at most n posts after the sequence
// after, newest first. Anything else (a kind, author or search over every
// room) walks the newest n events.
func (src rankSource) sql(ctx context.Context, tx *sql.Tx, after int64, n int) (string, []any, error) {
	const roomWalk = "SELECT seq FROM (SELECT seq FROM events INDEXED BY events_reply WHERE reply_to='' AND room=? AND seq>? ORDER BY seq DESC LIMIT ?)"
	switch {
	case src.room != "":
		return roomWalk, []any{src.room, after, n}, nil
	case src.front:
		q := "SELECT seq FROM (SELECT seq FROM events INDEXED BY events_front_top1 WHERE " + rankableTerms + " AND " + frontNameTerms("room") + " AND seq>? ORDER BY seq DESC LIMIT ?)"
		args := []any{after, n}
		rooms, err := frontOnRooms(ctx, tx)
		if err != nil {
			return "", nil, err
		}
		for _, room := range rooms {
			q += " UNION ALL " + roomWalk
			args = append(args, room, after, n)
		}
		return q, args, nil
	case src.all:
		return "SELECT seq FROM (SELECT seq FROM events INDEXED BY events_top1 WHERE " + rankableTerms + " AND seq>? ORDER BY seq DESC LIMIT ?)", []any{after, n}, nil
	}
	return "SELECT seq FROM (SELECT seq FROM events WHERE seq>? ORDER BY seq DESC LIMIT ?)", []any{after, n}, nil
}

// seqAtOrBefore is (about) the newest event created at or before t: events
// are appended in time order, so it bisects seq, one primary-key probe per
// step. Callers keep their own created_at test; this only bounds an index walk.
func seqAtOrBefore(ctx context.Context, tx *sql.Tx, t, top int64) (int64, error) {
	lo, hi := int64(0), top
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		var at int64
		err := tx.QueryRowContext(ctx, "SELECT created_at FROM events WHERE seq>=? ORDER BY seq LIMIT 1", mid).Scan(&at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil && at <= t {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

// rankInputs are what each candidate carries into Rank, computed for at most
// HotCandidates posts per walk: its votes, its original's quality, the
// quality of its newest scored later version (events_origin), whether any
// version has a flag open (event_flags_root), and the distinct signed agents
// other than its author among its newest ReplyScanRows replies who could vote
// on it: a visible public post at least VoterMinAge old (events_author, below
// the sequence of that age), counted to reply_agents_max.
const rankInputs = `SELECT e.id,e.seq,e.created_at,coalesce(v.score,0),q.quality,
 (SELECT eq.quality FROM events ev JOIN event_quality eq ON eq.event_id=ev.id WHERE ev.origin=e.id AND ev.origin<>'' ORDER BY ev.seq DESC LIMIT 1),
 EXISTS(SELECT 1 FROM event_flags f WHERE f.root=e.id),
 (SELECT count(*) FROM (SELECT DISTINCT x.account FROM (SELECT account,hidden,public_key FROM events WHERE reply_to=e.id AND room=e.room ORDER BY seq DESC LIMIT ?) x
  WHERE x.hidden=0 AND x.public_key<>'' AND x.account<>e.account
  AND EXISTS(SELECT 1 FROM events y JOIN rooms ry ON ry.name=y.room WHERE y.account=x.account AND y.seq<=? AND y.created_at<=? AND y.hidden=0 AND ry.visibility='public') LIMIT ?))
 FROM (%s) c CROSS JOIN events e ON e.seq=c.seq LEFT JOIN event_scores v ON v.event_id=e.id LEFT JOIN event_quality q ON q.event_id=e.id`

// ranking orders the candidate posts for one view, from the cache when fresh.
// Every walk is bounded: the source walks at most RankScanRows index entries,
// the score walk reads the score index highest first, and inputs are computed
// for at most HotCandidates posts of each. A cached ranking stays for
// RankCacheTTL (a vote or a flag clears it); posts made since it was built are
// ranked on their own and merged in (marked fresh), so a new post shows on
// the first page at once without a full ranking per post. A pinned read (an
// offset page) gets the base ranking its first page was cut from, without
// merges, for RankSnapshotTTL, so pages neither repeat nor skip as posts
// arrive. top is the newest event's sequence.
func (s *Store) ranking(ctx context.Context, tx *sql.Tx, src rankSource, where []string, args []any, bias float64, now int64, pinned bool) (list []rankedPost, top int64, err error) {
	key := fmt.Sprintf("%+v\x00%s\x00%q\x00%g", src, strings.Join(where, " AND "), args, bias)
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&top); err != nil {
		return nil, 0, err
	}
	clock := s.now()
	s.rankMu.Lock()
	cached, ok := s.rankCache[key]
	base, hasBase := s.rankPinned[key]
	s.rankMu.Unlock()
	if pinned && hasBase && clock.Sub(base.at) >= 0 && clock.Sub(base.at) < RankSnapshotTTL {
		return base.base, top, nil
	}
	if ok && (clock.Sub(cached.at) < 0 || clock.Sub(cached.at) >= RankCacheTTL) {
		ok = false
	}
	if ok && cached.top == top {
		if pinned {
			return cached.base, top, nil
		}
		return cached.merged, top, nil
	}
	where = append(append([]string{}, where...), "r.visibility='public'", "e.hidden=0", "+e.reply_to=''", "e.supersedes=''")
	if bias > 0 {
		where = append(where, "e.created_at>=?")
		args = append(append([]any{}, args...), now-HotWindowSeconds)
	}
	cond := strings.Join(where, " AND ")
	seasoned := now - int64(VoterMinAge/time.Second)
	seasonedSeq, err := seqAtOrBefore(ctx, tx, seasoned, top)
	if err != nil {
		return nil, 0, err
	}
	found := map[string]rankedPost{}
	collect := func(candidates string, cargs []any, fresh bool) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(rankInputs, candidates), append([]any{ReplyScanRows, seasonedSeq, seasoned, Ranking.ReplyAgentsMax}, cargs...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r rankedPost
			if err = rows.Scan(&r.id, &r.seq, &r.at, &r.score, &r.quality, &r.edited, &r.flagged, &r.replies); err != nil {
				rows.Close()
				return err
			}
			if !r.flagged {
				r.fresh = fresh
				found[r.id] = r
			}
		}
		return closeRows(rows)
	}
	after := int64(0)
	if ok {
		// Only what arrived since the cached ranking; its posts keep their inputs.
		after = cached.head
	}
	// The walk reads rankRowsFirst entries, and all RankScanRows only when
	// those leave the candidates short and there are more to read.
	for n := rankRowsFirst; ; n = RankScanRows {
		source, sargs, err := src.sql(ctx, tx, after, n)
		if err != nil {
			return nil, 0, err
		}
		before := len(found)
		if err := collect("SELECT e.seq FROM ("+source+") s CROSS JOIN events e ON e.seq=s.seq JOIN rooms r ON r.name=e.room WHERE "+cond+" ORDER BY e.seq DESC LIMIT ?", append(append(sargs, args...), HotCandidates), ok); err != nil {
			return nil, 0, err
		}
		if n == RankScanRows || len(found)-before >= HotCandidates {
			break
		}
		var walked int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ("+source+")", sargs...).Scan(&walked); err != nil {
			return nil, 0, err
		}
		if walked < n {
			break
		}
	}
	if !ok {
		if err := collect("SELECT e.seq FROM (SELECT event_id,score FROM event_scores INDEXED BY event_scores_score ORDER BY score DESC LIMIT ?) v CROSS JOIN events e ON e.id=v.event_id JOIN rooms r ON r.name=e.room WHERE "+cond+" ORDER BY v.score DESC LIMIT ?", append(append([]any{RankScanRows}, args...), HotCandidates), false); err != nil {
			return nil, 0, err
		}
	}
	rankList := func(posts []rankedPost) []rankedPost {
		sort.Slice(posts, func(i, j int) bool {
			if a, b := posts[i], posts[j]; a.rank != b.rank {
				return a.rank > b.rank
			}
			return posts[i].seq > posts[j].seq
		})
		// No page reaches past the largest offset plus one full page.
		return posts[:min(len(posts), HotCandidates+PageMax+1)]
	}
	entry := rankEntry{at: clock, head: top, top: top}
	list = make([]rankedPost, 0, len(found))
	for _, r := range found {
		r.rank = Ranking.Rank(r.score, r.effectiveQuality(), r.replies, now-r.at, bias)
		list = append(list, r)
	}
	if ok {
		// The base keeps the ranks it was made with, so its posts keep their
		// order among themselves (offset pages read the base).
		entry.at, entry.head, entry.base = cached.at, cached.head, cached.base // a merge does not extend the ranking's life
		for _, r := range cached.base {
			if _, again := found[r.id]; !again {
				list = append(list, r)
			}
		}
	}
	list = rankList(list)
	if !ok {
		entry.base = list
	}
	entry.merged = list
	s.rankMu.Lock()
	if s.rankCache == nil || len(s.rankCache) >= rankCacheEntries {
		s.rankCache = map[string]rankEntry{}
	}
	s.rankCache[key] = entry
	if !ok {
		if s.rankPinned == nil || len(s.rankPinned) >= rankCacheEntries {
			s.rankPinned = map[string]rankEntry{}
		}
		s.rankPinned[key] = rankEntry{at: entry.at, head: entry.head, base: entry.base}
	}
	s.rankMu.Unlock()
	if pinned {
		return entry.base, top, nil
	}
	return list, top, nil
}

// readRanked lists top-level posts (not replies, not later versions) in public
// rooms by hot rank or all-time score. where and args already select what the
// caller may read. Ranking reads only ids, times and scores; the page's posts
// are loaded afterwards and held to the same byte budget as any page. Pages
// are by offset into the ranking the first page was cut from (its base, see
// ranking): next_offset counts only the base's posts a page delivered, so a
// post merged into a first page since its ranking was made neither shifts
// nor repeats the pages after it. ranked is how many posts the view ranks
// (from the offset), for the first-contact fallback.
func (s *Store) readRanked(ctx context.Context, tx *sql.Tx, src rankSource, where []string, args []any, o ListOptions, limit int, now int64) (res Result, ranked int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	list, head, err := s.ranking(ctx, tx, src, where, args, *o.Bias, now, o.Offset > 0)
	if errors.Is(err, context.DeadlineExceeded) {
		return Result{}, 0, &Error{Status: 503, Code: "rank_read_timeout", Message: "Ranking exceeded its two-second work budget; retry or read sort=new.", RetryAfter: 2}
	}
	if err != nil {
		return Result{}, 0, err
	}
	if o.Offset >= len(list) {
		list = nil
	} else {
		list = list[o.Offset:]
	}
	ranked = len(list)
	hasMore := len(list) > limit
	if hasMore {
		list = list[:limit]
	}
	events := make([]Message, 0, len(list))
	order := map[string]int{}
	if len(list) > 0 {
		ids := make([]any, len(list))
		for i, r := range list {
			ids[i], order[r.id] = r.id, i
		}
		// hidden and flags are checked again: a cached or pinned ranking can
		// predate a removal or a flag.
		rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.hidden=0 AND NOT EXISTS(SELECT 1 FROM event_flags f WHERE f.root=e.id) AND e.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
		if err != nil {
			return Result{}, 0, err
		}
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				rows.Close()
				return Result{}, 0, err
			}
			events = append(events, e)
		}
		if err = closeRows(rows); err != nil {
			return Result{}, 0, err
		}
		sort.Slice(events, func(i, j int) bool { return order[events[i].ID] < order[events[j].ID] })
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, 0, err
	}
	if err = attachScores(ctx, tx, events); err != nil {
		return Result{}, 0, err
	}
	// The byte budget can end a page early; next_offset follows what was sent.
	delivered := len(list)
	events, cut := boundPage(events, "ASC", len(events), -1)
	if cut {
		// Count from the ranking, not the page: a post hidden since the ranking
		// was cached is skipped, not repeated.
		hasMore, delivered = true, 0
		if len(events) > 0 {
			delivered = order[events[len(events)-1].ID] + 1
		}
	}
	// Offsets count the base ranking's posts only (fresh ones are merged into
	// first pages alone).
	next := o.Offset
	for _, r := range list[:delivered] {
		if !r.fresh {
			next++
		}
	}
	sortName := "hot"
	if *o.Bias == 0 {
		sortName = "top"
	}
	// next_cursor is where the chronological feed (a cursor read) resumes
	// from now, so a reader that starts ranked can poll for what is new.
	return Result{Messages: events, NextCursor: s.cursor(head), Data: map[string]any{"has_more": hasMore, "sort": sortName, "bias": *o.Bias, "offset": o.Offset, "next_offset": next}}, ranked, nil
}
