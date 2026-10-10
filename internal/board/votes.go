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
	if !seasoned && !s.standingAdmitsVote(ctx, tx, a.account, now) {
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
	s.dropRankings()
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
// votes, their quality scores (ranking.go) and work marks (workmessages.go),
// in place: what every message read adds to the stored message.
func attachScores(ctx context.Context, tx *sql.Tx, events []Message, now int64) error {
	if err := attachQuality(ctx, tx, events); err != nil {
		return err
	}
	if err := attachWork(ctx, tx, events, now); err != nil {
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
	room    string
	account string // the author's continuity account
	signed  bool
	rank    float64
	merit   float64 // the score's parts (feed.get explain): merit before recency,
	decay   float64 // the recency factor
	weight  float64 // and the room weight
	fresh   bool    // merged into a cached ranking after it was made
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
	gen    int64        // this ranking's generation, unique per build (feed cursors name it)
	scored int64        // the clock base was scored at (feed cursors rescore at it)
	head   int64        // the newest event when base was made
	base   []rankedPost // the ranking as made
	merged []rankedPost // base plus the posts since, for first pages
	top    int64        // the newest event when merged was made
}

// candEntry is one view's candidates with their inputs, unscored: what every
// ranking of the view shares, whatever its weights (candidates).
type candEntry struct {
	at    time.Time
	head  int64 // the newest event when posts were read
	posts []rankedPost
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

// rankInputs are what each candidate carries into a score, computed for at
// most HotCandidates posts per walk: its votes, its original's quality, the
// quality of its newest scored later version (events_origin), whether any
// version has a flag open (event_flags_root), the distinct signed agents
// other than its author among its newest ReplyScanRows replies who could vote
// on it: a visible public post at least VoterMinAge old (events_author, below
// the sequence of that age), counted to FeedReplyAgentsMax (a feed profile's
// highest reply_agents_max), and its room, author account and whether it is
// signed, for a feed profile's room weights and filters.
const rankInputs = `SELECT e.id,e.seq,e.created_at,coalesce(v.score,0),q.quality,
 (SELECT eq.quality FROM events ev JOIN event_quality eq ON eq.event_id=ev.id WHERE ev.origin=e.id AND ev.origin<>'' ORDER BY ev.seq DESC LIMIT 1),
 EXISTS(SELECT 1 FROM event_flags f WHERE f.root=e.id),
 (SELECT count(*) FROM (SELECT DISTINCT x.account FROM (SELECT account,hidden,public_key FROM events WHERE reply_to=e.id AND room=e.room ORDER BY seq DESC LIMIT ?) x
  WHERE x.hidden=0 AND x.public_key<>'' AND x.account<>e.account
  AND EXISTS(SELECT 1 FROM events y JOIN rooms ry ON ry.name=y.room WHERE y.account=x.account AND y.seq<=? AND y.created_at<=? AND y.hidden=0 AND ry.visibility='public') LIMIT ?)),
 e.room,e.account,e.public_key<>''
 FROM (%s) c CROSS JOIN events e ON e.seq=c.seq LEFT JOIN event_scores v ON v.event_id=e.id LEFT JOIN event_quality q ON q.event_id=e.id`

// rankView is one source of candidates: a bounded walk (rankSource), the SQL
// terms every candidate meets beyond the ranked-view rules (selecting what
// the reader may see), whether only the last HotWindowSeconds count, how
// many posts each walk keeps, and whether the highest-scored voted posts are
// read too. A feed's room slice (slice names its room) is its room's newest
// FeedRoomSlice rankable posts of the window.
type rankView struct {
	src      rankSource
	where    []string
	args     []any
	windowed bool
	limit    int
	scored   bool
	slice    string
}

func (v rankView) key() string {
	return fmt.Sprintf("%+v\x00%s\x00%q\x00%t\x00%d\x00%t", v.src, strings.Join(v.where, " AND "), v.args, v.windowed, v.limit, v.scored)
}

// rankRead is one read's clock: the newest event and the seasoned-voter
// bound, read once however many views it collects.
type rankRead struct {
	top, now              int64
	seasoned, seasonedSeq int64
	haveSeasoned          bool
}

func (rd *rankRead) seasonedBound(ctx context.Context, tx *sql.Tx) (int64, int64, error) {
	if !rd.haveSeasoned {
		rd.seasoned = rd.now - int64(VoterMinAge/time.Second)
		seq, err := seqAtOrBefore(ctx, tx, rd.seasoned, rd.top)
		if err != nil {
			return 0, 0, err
		}
		rd.seasonedSeq, rd.haveSeasoned = seq, true
	}
	return rd.seasoned, rd.seasonedSeq, nil
}

// Whether candidates had a view's inputs at hand (or only merged the posts
// since), read them in full, or left them for a later read.
const (
	candWarm = iota
	candBuilt
	candSkipped
)

// candidates are one view's candidate posts with their inputs, unscored,
// from the cache when fresh: the SQL half of a ranking, shared by every
// ranking of the view whatever its weights. Every walk is bounded: the
// source walks at most RankScanRows index entries, the score walk reads the
// score index highest first, and inputs are computed for at most v.limit
// posts of each. Cached inputs stay for RankCacheTTL (a vote or a flag clears
// them); posts made since are read on their own and merged in, so a new post
// costs one short walk, not a new read of every candidate. cold false leaves
// a view with no fresh inputs unread (candSkipped). It reads only tx.
func (s *Store) candidates(ctx context.Context, tx *sql.Tx, v rankView, rd *rankRead, cold bool) (candEntry, int, error) {
	key := v.key()
	clock := s.now()
	s.rankMu.Lock()
	cached, ok := s.candCache[key]
	s.rankMu.Unlock()
	if ok && (clock.Sub(cached.at) < 0 || clock.Sub(cached.at) >= RankCacheTTL) {
		ok = false
	}
	if ok && cached.head == rd.top {
		return cached, candWarm, nil
	}
	if !ok && !cold {
		return candEntry{}, candSkipped, nil
	}
	where := append(append([]string{}, v.where...), "r.visibility='public'", "e.hidden=0", "+e.reply_to=''", "e.supersedes=''")
	args := append([]any{}, v.args...)
	if v.windowed {
		where = append(where, "e.created_at>=?")
		args = append(args, rd.now-HotWindowSeconds)
	}
	cond := strings.Join(where, " AND ")
	seasoned, seasonedSeq, err := rd.seasonedBound(ctx, tx)
	if err != nil {
		return candEntry{}, 0, err
	}
	found := map[string]rankedPost{}
	collect := func(candidates string, cargs []any) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(rankInputs, candidates), append([]any{ReplyScanRows, seasonedSeq, seasoned, FeedReplyAgentsMax}, cargs...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r rankedPost
			if err = rows.Scan(&r.id, &r.seq, &r.at, &r.score, &r.quality, &r.edited, &r.flagged, &r.replies, &r.room, &r.account, &r.signed); err != nil {
				rows.Close()
				return err
			}
			if !r.flagged {
				found[r.id] = r
			}
		}
		return closeRows(rows)
	}
	after := int64(0)
	if ok {
		// Only what arrived since the cached inputs; their posts keep theirs.
		after = cached.head
	}
	// The walk reads twice the view's limit in entries, and all RankScanRows
	// only when those leave the candidates short and there are more to read.
	for n := 2 * v.limit; ; n = RankScanRows {
		source, sargs, err := v.src.sql(ctx, tx, after, n)
		if err != nil {
			return candEntry{}, 0, err
		}
		before := len(found)
		if err := collect("SELECT e.seq FROM ("+source+") s CROSS JOIN events e ON e.seq=s.seq JOIN rooms r ON r.name=e.room WHERE "+cond+" ORDER BY e.seq DESC LIMIT ?", append(append(sargs, args...), v.limit)); err != nil {
			return candEntry{}, 0, err
		}
		if n == RankScanRows || len(found)-before >= v.limit {
			break
		}
		var walked int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ("+source+")", sargs...).Scan(&walked); err != nil {
			return candEntry{}, 0, err
		}
		if walked < n {
			break
		}
	}
	if !ok && v.scored {
		if err := collect("SELECT e.seq FROM (SELECT event_id,score FROM event_scores INDEXED BY event_scores_score ORDER BY score DESC LIMIT ?) v CROSS JOIN events e ON e.id=v.event_id JOIN rooms r ON r.name=e.room WHERE "+cond+" ORDER BY v.score DESC LIMIT ?", append(append([]any{RankScanRows}, args...), v.limit)); err != nil {
			return candEntry{}, 0, err
		}
	}
	entry := candEntry{at: clock, head: rd.top}
	entry.posts = make([]rankedPost, 0, len(found)+len(cached.posts))
	for _, r := range found {
		entry.posts = append(entry.posts, r)
	}
	if ok {
		// A merge does not extend the inputs' life.
		entry.at = cached.at
		for _, r := range cached.posts {
			if _, again := found[r.id]; !again {
				entry.posts = append(entry.posts, r)
			}
		}
	}
	s.rankMu.Lock()
	if s.candCache == nil || len(s.candCache) >= rankCacheEntries {
		s.candCache = map[string]candEntry{}
	}
	s.candCache[key] = entry
	s.rankMu.Unlock()
	if ok {
		return entry, candWarm, nil
	}
	return entry, candBuilt, nil
}

// sortRanked orders scored posts, highest first, newest first on a tie, and
// keeps no more than any page reaches: the largest offset plus one full page.
func sortRanked(posts []rankedPost) []rankedPost {
	sort.Slice(posts, func(i, j int) bool {
		if a, b := posts[i], posts[j]; a.rank != b.rank {
			return a.rank > b.rank
		}
		return posts[i].seq > posts[j].seq
	})
	return posts[:min(len(posts), HotCandidates+PageMax+1)]
}

// rankResult is a ranking as a read gets it: the posts in order, the newest
// event's sequence, the ranking's generation and the room slices left for a
// later read (feed.get's warming).
type rankResult struct {
	list    []rankedPost
	top     int64
	gen     int64
	scored  int64
	warming []string
}

// ranking orders the candidate posts of views by the scorer f, from the cache
// when fresh: the candidates (SQL, shared by every scorer, see candidates)
// scored in memory (feedScorer.score). A cached ranking stays for
// RankCacheTTL from its inputs (a vote or a flag clears it); posts made since
// it was built are scored on their own and merged in (marked fresh), so a new
// post shows on the first page at once without a full ranking per post. A
// pinned read (an offset page) gets the base ranking its first page was cut
// from, without merges, for RankSnapshotTTL, so pages neither repeat nor skip
// as posts arrive. At most FeedColdSlices room slices without fresh inputs
// are read per call; the rest are named in warming, and a ranking that left
// any is neither cached nor pinned.
func (s *Store) ranking(ctx context.Context, tx *sql.Tx, views []rankView, f *feedScorer, now int64, pinned bool) (rankResult, error) {
	keys := make([]string, len(views))
	for i, v := range views {
		keys[i] = v.key()
	}
	key := strings.Join(keys, "\x01") + "\x00" + f.hash
	rd := &rankRead{now: now}
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&rd.top); err != nil {
		return rankResult{}, err
	}
	top := rd.top
	clock := s.now()
	s.rankMu.Lock()
	cached, ok := s.rankCache[key]
	base, hasBase := s.rankPinned[key]
	s.rankMu.Unlock()
	if pinned && hasBase && clock.Sub(base.at) >= 0 && clock.Sub(base.at) < RankSnapshotTTL {
		return rankResult{list: base.base, top: top, gen: base.gen, scored: base.scored}, nil
	}
	if ok && (clock.Sub(cached.at) < 0 || clock.Sub(cached.at) >= RankCacheTTL) {
		ok = false
	}
	if ok && cached.top == top {
		if pinned {
			return rankResult{list: cached.base, top: top, gen: cached.gen, scored: cached.scored}, nil
		}
		return rankResult{list: cached.merged, top: top, gen: cached.gen, scored: cached.scored}, nil
	}
	var (
		all     []rankedPost
		warming []string
		inputs  time.Time
		cold    int
	)
	seen := map[string]bool{}
	for _, v := range views {
		e, state, err := s.candidates(ctx, tx, v, rd, v.slice == "" || cold < FeedColdSlices)
		if err != nil {
			return rankResult{}, err
		}
		if state == candSkipped {
			warming = append(warming, v.slice)
			continue
		}
		if state == candBuilt && v.slice != "" {
			cold++
		}
		if inputs.IsZero() || e.at.Before(inputs) {
			inputs = e.at
		}
		for _, p := range e.posts {
			if !seen[p.id] {
				seen[p.id] = true
				all = append(all, p)
			}
		}
	}
	if inputs.IsZero() {
		inputs = clock
	}
	// The ranking lives as long as its oldest inputs.
	entry := rankEntry{at: inputs, head: top, top: top}
	var list []rankedPost
	if ok {
		// Only the posts since the cached ranking are scored; the base keeps
		// the scores it was made with, so its posts keep their order among
		// themselves (offset pages read the base).
		var since []rankedPost
		for _, p := range all {
			if p.seq > cached.head {
				since = append(since, p)
			}
		}
		list = f.score(since, now)
		again := make(map[string]bool, len(list))
		for i := range list {
			list[i].fresh = true
			again[list[i].id] = true
		}
		entry.at, entry.head, entry.base, entry.gen, entry.scored = cached.at, cached.head, cached.base, cached.gen, cached.scored // a merge does not extend the ranking's life
		for _, r := range cached.base {
			if !again[r.id] {
				list = append(list, r)
			}
		}
	} else {
		list = f.score(all, now)
	}
	list = sortRanked(list)
	s.rankMu.Lock()
	if !ok {
		entry.base = list
		s.rankGen++
		entry.gen, entry.scored = s.rankGen, now
	}
	entry.merged = list
	if len(warming) == 0 {
		if s.rankCache == nil || len(s.rankCache) >= rankCacheEntries {
			s.rankCache = map[string]rankEntry{}
		}
		s.rankCache[key] = entry
		if !ok {
			if s.rankPinned == nil || len(s.rankPinned) >= rankCacheEntries {
				s.rankPinned = map[string]rankEntry{}
			}
			s.rankPinned[key] = rankEntry{at: entry.at, gen: entry.gen, scored: entry.scored, head: entry.head, base: entry.base}
		}
	}
	s.rankMu.Unlock()
	res := rankResult{list: list, top: top, gen: entry.gen, scored: entry.scored, warming: warming}
	if pinned {
		res.list = entry.base
	}
	return res, nil
}

// rankTimeout is a ranking's error as the reader sees it: past the two-second
// work budget, 503 rank_read_timeout.
func rankTimeout(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "rank_read_timeout", Message: "Ranking exceeded its two-second work budget; retry or read sort=new.", RetryAfter: 2}
	}
	return err
}

// rankBudget is the work budget of one ranked read.
const rankBudget = 2 * time.Second

// rankedPageOut is one page cut from a ranking: its messages, the ranked
// posts it delivered, the offset the next page starts at, whether there are
// more, and how many posts the ranking holds from the offset.
type rankedPageOut struct {
	events  []Message
	page    []rankedPost
	next    int
	hasMore bool
	ranked  int
}

// rankedPage loads the posts of list from offset, up to limit, held to the
// same byte budget as any page. hidden and flags are checked again: a cached
// or pinned ranking can predate a removal or a flag. next counts only the
// base ranking's posts a page delivered (fresh ones are merged into first
// pages alone), so a post merged into a first page since its ranking was
// made neither shifts nor repeats the pages after it.
func (s *Store) rankedPage(ctx context.Context, tx *sql.Tx, list []rankedPost, offset, limit int, now int64) (rankedPageOut, error) {
	var out rankedPageOut
	if offset >= len(list) {
		list = nil
	} else {
		list = list[offset:]
	}
	out.ranked = len(list)
	out.hasMore = len(list) > limit
	if out.hasMore {
		list = list[:limit]
	}
	events := make([]Message, 0, len(list))
	order := map[string]int{}
	if len(list) > 0 {
		ids := make([]any, len(list))
		for i, r := range list {
			ids[i], order[r.id] = r.id, i
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.hidden=0 AND NOT EXISTS(SELECT 1 FROM event_flags f WHERE f.root=e.id) AND e.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				rows.Close()
				return out, err
			}
			events = append(events, e)
		}
		if err = closeRows(rows); err != nil {
			return out, err
		}
		sort.Slice(events, func(i, j int) bool { return order[events[i].ID] < order[events[j].ID] })
	}
	if err := s.loadAttachments(ctx, tx, events, now); err != nil {
		return out, err
	}
	if err := attachScores(ctx, tx, events, now); err != nil {
		return out, err
	}
	// The byte budget can end a page early; next follows what was sent.
	delivered := len(list)
	events, cut := boundPage(events, "ASC", len(events), -1)
	if cut {
		// Count from the ranking, not the page: a post hidden since the ranking
		// was cached is skipped, not repeated.
		out.hasMore, delivered = true, 0
		if len(events) > 0 {
			delivered = order[events[len(events)-1].ID] + 1
		}
	}
	out.events, out.page, out.next = events, list[:delivered], offset
	for _, r := range out.page {
		if !r.fresh {
			out.next++
		}
	}
	return out, nil
}

// readRanked lists top-level posts (not replies, not later versions) in public
// rooms by hot rank or all-time score: the default feed profile at the
// read's bias (feed.go), so messages.list sort=hot and feed.get with no
// override are one ranking. where and args already select what the caller
// may read. Ranking reads only ids, times and scores; the page's posts are
// loaded afterwards (rankedPage). Pages are by offset into the ranking the
// first page was cut from (its base, see ranking). ranked is how many posts
// the view ranks (from the offset), for the first-contact fallback.
func (s *Store) readRanked(ctx context.Context, tx *sql.Tx, src rankSource, where []string, args []any, o ListOptions, limit int, now int64) (res Result, ranked int, err error) {
	ctx, cancel := context.WithTimeout(ctx, rankBudget)
	defer cancel()
	view := rankView{src: src, where: where, args: args, windowed: *o.Bias > 0, limit: HotCandidates, scored: true}
	rr, err := s.ranking(ctx, tx, []rankView{view}, defaultFeedScorer(*o.Bias), now, o.Offset > 0)
	if err != nil {
		return Result{}, 0, rankTimeout(err)
	}
	pg, err := s.rankedPage(ctx, tx, rr.list, o.Offset, limit, now)
	if err != nil {
		return Result{}, 0, err
	}
	sortName := "hot"
	if *o.Bias == 0 {
		sortName = "top"
	}
	// next_cursor is where the chronological feed (a cursor read) resumes
	// from now, so a reader that starts ranked can poll for what is new.
	return Result{Messages: pg.events, NextCursor: s.cursor(rr.top), Data: map[string]any{"has_more": pg.hasMore, "sort": sortName, "bias": *o.Bias, "offset": o.Offset, "next_offset": pg.next}}, pg.ranked, nil
}
