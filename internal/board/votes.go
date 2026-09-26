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
// messages.list, message.get and thread.get results and never to exports, the
// public archive or signed receipts.

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

// Ranking reads a bounded set of candidates: the newest top-level posts among
// the last RankScanRows events, plus the highest-scored voted posts. Hot keeps
// only the last HotWindowSeconds. A ranking is reused for RankCacheTTL, or
// until the next vote, so paging and repeated reads cost one computation.
const (
	HotWindowSeconds = 30 * 86400
	HotCandidates    = 2000
	RankScanRows     = 20000
	RankCacheTTL     = 15 * time.Second
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

// VoteScore is the single score function every view uses. v1 weighs every
// account 1; a reputation weighting replaces this, not the stored votes.
func VoteScore(up, down int64) int64 { return up - down }

// HotRank orders by score / (age_hours + 2)^bias. bias 0 is all-time top.
func HotRank(score int64, ageSeconds int64, bias float64) float64 {
	if bias <= 0 {
		return float64(score)
	}
	hours := math.Max(float64(ageSeconds), 0) / 3600
	return float64(score) / math.Pow(hours+2, bias)
}

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

// attachVotes sets the vote totals on public, visible messages that have any
// votes, in place.
func attachVotes(ctx context.Context, tx *sql.Tx, events []Message) error {
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

// ListOptions are the sorted-view options a messages.list read carries in data.
type ListOptions struct {
	Sort   string   `json:"sort,omitempty"`
	Bias   *float64 `json:"bias,omitempty"`
	Offset int      `json:"offset,omitempty"`
}

func parseListOptions(data string) (ListOptions, error) {
	var o ListOptions
	if data == "" {
		return o, nil
	}
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return o, problem(400, "invalid_list_options", `data for messages.list is {"sort":"new"|"hot"|"top","bias":0-4,"offset":N}.`)
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
	if o.Offset != 0 && (o.Sort == "" || o.Sort == "new") {
		return o, problem(400, "invalid_offset", "offset pages hot and top; sort=new pages with cursor.")
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
}

type rankEntry struct {
	at   time.Time
	list []rankedPost
}

// ranking orders the candidate posts for one view, from the cache when fresh.
// Both queries are bounded: the first walks at most RankScanRows events
// newest first, the second reads the score index highest first.
func (s *Store) ranking(ctx context.Context, tx *sql.Tx, where []string, args []any, bias float64, now int64) ([]rankedPost, error) {
	key := fmt.Sprintf("%s\x00%q\x00%g", strings.Join(where, " AND "), args, bias)
	s.rankMu.Lock()
	if e, ok := s.rankCache[key]; ok && s.now().Sub(e.at) >= 0 && s.now().Sub(e.at) < RankCacheTTL {
		s.rankMu.Unlock()
		return e.list, nil
	}
	s.rankMu.Unlock()
	where = append(append([]string{}, where...), "r.visibility='public'", "e.hidden=0", "+e.reply_to=''", "e.supersedes=''")
	if bias > 0 {
		where = append(where, "e.created_at>=?")
		args = append(append([]any{}, args...), now-HotWindowSeconds)
	}
	var top int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&top); err != nil {
		return nil, err
	}
	found := map[string]rankedPost{}
	collect := func(query string, args []any) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r rankedPost
			if err = rows.Scan(&r.id, &r.seq, &r.at, &r.score); err != nil {
				rows.Close()
				return err
			}
			found[r.id] = r
		}
		return closeRows(rows)
	}
	cond := strings.Join(where, " AND ")
	if err := collect("SELECT e.id,e.seq,e.created_at,coalesce(v.score,0) FROM events e JOIN rooms r ON r.name=e.room LEFT JOIN event_scores v ON v.event_id=e.id WHERE "+cond+" AND e.seq>? ORDER BY e.seq DESC LIMIT ?", append(append([]any{}, args...), top-RankScanRows, HotCandidates)); err != nil {
		return nil, err
	}
	if err := collect("SELECT e.id,e.seq,e.created_at,v.score FROM (SELECT event_id,score FROM event_scores INDEXED BY event_scores_score ORDER BY score DESC LIMIT ?) v CROSS JOIN events e ON e.id=v.event_id JOIN rooms r ON r.name=e.room WHERE "+cond+" ORDER BY v.score DESC LIMIT ?", append(append([]any{RankScanRows}, args...), HotCandidates)); err != nil {
		return nil, err
	}
	list := make([]rankedPost, 0, len(found))
	for _, r := range found {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if bias > 0 {
			ra, rb := HotRank(a.score, now-a.at, bias), HotRank(b.score, now-b.at, bias)
			if ra != rb {
				return ra > rb
			}
		} else if a.score != b.score {
			return a.score > b.score
		}
		return a.seq > b.seq
	})
	// No page reaches past the largest offset plus one full page.
	list = list[:min(len(list), HotCandidates+PageMax+1)]
	s.rankMu.Lock()
	if s.rankCache == nil || len(s.rankCache) >= rankCacheEntries {
		s.rankCache = map[string]rankEntry{}
	}
	s.rankCache[key] = rankEntry{at: s.now(), list: list}
	s.rankMu.Unlock()
	return list, nil
}

// readRanked lists top-level posts (not replies, not later versions) in public
// rooms by hot rank or all-time score. where and args already select what the
// caller may read. Ranking reads only ids, times and scores; the page's posts
// are loaded afterwards and held to the same byte budget as any page. Pages
// are by offset, since ranks move as votes arrive.
func (s *Store) readRanked(ctx context.Context, tx *sql.Tx, where []string, args []any, o ListOptions, limit int, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	list, err := s.ranking(ctx, tx, where, args, *o.Bias, now)
	if errors.Is(err, context.DeadlineExceeded) {
		return Result{}, &Error{Status: 503, Code: "rank_read_timeout", Message: "Ranking exceeded its two-second work budget; retry or read sort=new.", RetryAfter: 2}
	}
	if err != nil {
		return Result{}, err
	}
	if o.Offset >= len(list) {
		list = nil
	} else {
		list = list[o.Offset:]
	}
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
		// hidden is checked again: a cached ranking can predate a removal.
		rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE e.hidden=0 AND e.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
		if err != nil {
			return Result{}, err
		}
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				rows.Close()
				return Result{}, err
			}
			events = append(events, e)
		}
		if err = closeRows(rows); err != nil {
			return Result{}, err
		}
		sort.Slice(events, func(i, j int) bool { return order[events[i].ID] < order[events[j].ID] })
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	if err = attachVotes(ctx, tx, events); err != nil {
		return Result{}, err
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
	sortName := "hot"
	if *o.Bias == 0 {
		sortName = "top"
	}
	return Result{Messages: events, Data: map[string]any{"has_more": hasMore, "sort": sortName, "bias": *o.Bias, "offset": o.Offset, "next_offset": o.Offset + delivered}}, nil
}
