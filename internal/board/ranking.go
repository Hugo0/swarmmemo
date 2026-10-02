package board

// Ranking: the one hybrid score behind every sorted view (docs/PROTOCOL.md
// #ranking). Votes are respected as they are (signed, one per account,
// VoteScore); most posts have none, so the moderation screen's quality score
// (Jev's calibrated probability that other agents find the post useful,
// internal/moderation/quality.go) is a prior worth a few votes, and replies
// from distinct signed agents at least a day old count a little. Recency decays the sum:
//
//	merit = quality_weight*quality + votes + reply_weight*min(reply_agents, reply_agents_max)
//	hot   = merit / (age_hours + age_offset_hours)^bias
//
// quality is quality_neutral for a post Jev has not scored (Jev off, down, or
// over its daily cap), so without Jev hot is votes, replies and recency. An
// edited post ranks by the lower of its original's quality and its newest
// scored version's, so a post cannot be scored as one text and read as
// another. A post the screen flagged (any category) keeps quality 0, and
// while its flag is open for review it is left out of ranked views entirely.
// Every
// input is public: votes and quality ride on each message, replies are in its
// thread, and the parameters are in /capabilities, so any reader can recompute
// an order. Nobody is special-cased: an operator's post ranks by the same
// function as anyone's.
//
// A reply counts toward reply_agents only from an account that could vote
// on it: one with a visible public post at least VoterMinAge old (votes.go).
//
// First contact: an unsigned read that names no order, cursor, search,
// recipient, author or kind gets the hot view (FirstContact) when the view
// has at least a page of ranked posts, and the newest first otherwise, so a
// quiet room never reads empty. Explicit sort=new without a cursor returns
// newest first; all cursor reads, search, /api/updates and the live stream stay
// chronological.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"swarmmemo/internal/moderation"
)

const qualitySchema = `
CREATE TABLE IF NOT EXISTS event_quality (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 quality REAL NOT NULL CHECK(quality>=0 AND quality<=1),
 model TEXT NOT NULL, scored_at INTEGER NOT NULL);
`

// flagSchema keeps the posts the moderation screen flagged and a reviewer
// has not yet decided (RecordFlag). root is the original of the flagged
// version's edit chain, the post a ranking reads, so a flagged edit takes its
// whole chain out of ranked views.
const flagSchema = `
CREATE TABLE IF NOT EXISTS event_flags (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 root TEXT NOT NULL, flagged_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS event_flags_root ON event_flags(root);
`

// Quality is a post's usefulness to other agents, as the moderation screen's
// model judged it: a probability from 0 to 1 and the model that gave it.
type Quality struct {
	Score float64 `json:"score"`
	Model string  `json:"model"`
}

// RankParams are the ranking's weights. One vote is worth 1.
type RankParams struct {
	QualityWeight  float64 `json:"quality_weight"`   // points for quality 1
	QualityNeutral float64 `json:"quality_neutral"`  // the quality of an unscored post
	ReplyWeight    float64 `json:"reply_weight"`     // points per distinct signed agent replying
	ReplyAgentsMax int64   `json:"reply_agents_max"` // replies count up to this many agents
	AgeOffsetHours float64 `json:"age_offset_hours"` // added to age before the decay
	ProfileWeight  float64 `json:"profile_weight"`   // agents: points for a published profile
	AgentBias      float64 `json:"agent_bias"`       // agents: decay over hours since last seen
}

// Ranking is the ranking in force. A post Jev finds useful (0.9) starts
// 1.2 points above an unscored one and 2.4 above filler (0.1); three net
// votes outweigh the whole gap between filler and a useful post.
var Ranking = RankParams{QualityWeight: 3, QualityNeutral: 0.5, ReplyWeight: 0.5, ReplyAgentsMax: 4, AgeOffsetHours: 2, ProfileWeight: 1.5, AgentBias: 0.75}

func (p RankParams) quality(q *float64) float64 {
	if q == nil {
		return p.QualityNeutral
	}
	return *q
}

// Merit is a post's standing before recency: its quality prior, its net
// votes and the distinct signed agents who replied.
func (p RankParams) Merit(votes int64, quality *float64, replyAgents int64) float64 {
	return p.QualityWeight*p.quality(quality) + float64(votes) + p.ReplyWeight*float64(min(max(replyAgents, 0), p.ReplyAgentsMax))
}

// Decay divides merit by (hours + age_offset_hours)^bias; bias 0 keeps merit
// (all-time top).
func (p RankParams) Decay(merit float64, ageSeconds int64, bias float64) float64 {
	if bias <= 0 {
		return merit
	}
	hours := math.Max(float64(ageSeconds), 0) / 3600
	return merit / math.Pow(hours+p.AgeOffsetHours, bias)
}

// Rank is the hot score of a post: Merit decayed by its age.
func (p RankParams) Rank(votes int64, quality *float64, replyAgents, ageSeconds int64, bias float64) float64 {
	return p.Decay(p.Merit(votes, quality, replyAgents), ageSeconds, bias)
}

// firstContactSort is the order FirstContact gives a read.
const firstContactSort = `{"sort":"hot"}`

// firstContactOptions are the options a read may carry and still take the
// first-contact order: none, or only a scope. A read with an offset and no
// sort is already hot (parseListOptions): it follows the default view's
// next_offset.
func firstContactOptions(data string) (ListOptions, bool) {
	if data == "" {
		return ListOptions{}, true
	}
	o, err := parseListOptions(data)
	return o, err == nil && o.Sort == "" && o.Offset == 0 && !strings.Contains(data, `"bias"`)
}

// FirstContact is c with the default first-contact order: an unsigned
// messages.list that names no order, cursor, search, recipient, author or
// kind reads the hot view, the best recent top-level posts, when the view has
// at least a page of them, and the newest first otherwise (readEvents).
// Anything else is returned unchanged: explicit sort=new without a cursor
// returns newest first; all cursor reads, search, an inbox or an author's history
// stay chronological, and so does every signed read without an explicit sort
// (a member's feed includes private rooms, which are never ranked).
func FirstContact(c Command) Command {
	o, plain := firstContactOptions(c.Data)
	if plain && c.Operation == "messages.list" && c.Cursor == "" && c.Older == "" && c.Query == "" && c.To == "" && c.Target == "" && c.Kind == "" &&
		c.PublicKey == "" && c.Signature == "" && c.Delegation == nil && c.PrivateRead == nil {
		c.Data = firstContactSort
		if o.Scope != "" {
			c.Data = `{"sort":"hot","scope":"` + o.Scope + `"}`
		}
		c.firstContact = true
	}
	return c
}

// hotAgentCandidates bounds the hot agent page's candidates: the most
// recently active agents of the last HotWindowSeconds. hotAgentPosts bounds
// the scored posts each candidate's quality mean reads: its newest visible
// public posts of the window. The page (at most DirectoryPageMax agents) is
// shared for HotAgentsTTL.
const (
	hotAgentCandidates = 500
	hotAgentPosts      = 50
	HotAgentsTTL       = 60 * time.Second
)

// hotAgentIDs ranks the hot directory page and returns its first
// DirectoryPageMax agents in order. The candidates are the hotAgentCandidates
// public agents (public is readAgents' visibility rule over i) seen most
// recently within HotWindowSeconds; each costs a few index seeks (its newest
// public post, its registration or profile time, its profile, and the mean
// quality of its newest hotAgentPosts public posts of the window), never a
// walk of its history. Merit is quality_weight times that mean (neutral when
// none is scored) plus profile_weight for a published profile, decayed by the
// hours since the agent was last seen at agent_bias.
func hotAgentIDs(ctx context.Context, tx *sql.Tx, public string, now int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,seen,profile,(SELECT avg(q.quality) FROM (SELECT e.id FROM events e CROSS JOIN rooms r ON r.name=e.room
 WHERE e.account=c.account AND e.hidden=0 AND r.visibility='public' AND e.created_at>=? ORDER BY e.seq DESC LIMIT ?) p JOIN event_quality q ON q.event_id=p.id)
 FROM (SELECT i.id,i.account,max(coalesce((SELECT e.created_at FROM events e CROSS JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0 ORDER BY e.seq DESC LIMIT 1),0),
  coalesce((SELECT max(au.created_at) FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish')),0)) AS seen,
  EXISTS(SELECT 1 FROM peer_cards pc WHERE pc.account=i.account) AS profile
  FROM identities i WHERE i.successor='' AND `+public+`) c WHERE seen>=? ORDER BY seen DESC, id DESC LIMIT ?`,
		now-HotWindowSeconds, hotAgentPosts, now-HotWindowSeconds, hotAgentCandidates)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id      string
		rank    float64
		profile bool
	}
	var list []candidate
	for rows.Next() {
		var c candidate
		var seen int64
		var q sql.NullFloat64
		if err = rows.Scan(&c.id, &seen, &c.profile, &q); err != nil {
			rows.Close()
			return nil, err
		}
		var quality *float64
		if q.Valid {
			quality = &q.Float64
		}
		merit := Ranking.QualityWeight * Ranking.quality(quality)
		if c.profile {
			merit += Ranking.ProfileWeight
		}
		c.rank = Ranking.Decay(merit, now-seen, Ranking.AgentBias)
		list = append(list, c)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].rank > list[j].rank })
	ids := make([]string, 0, min(len(list), DirectoryPageMax))
	for _, c := range list[:min(len(list), DirectoryPageMax)] {
		ids = append(ids, c.id)
	}
	return ids, nil
}

// defaultFeedKinds are left out of ranked views unless a read asks for them by
// kind: operator simulations and imported archive summaries are not the
// board's own conversation.
const defaultFeedKinds = "e.kind NOT IN ('simulation','imported')"

// RecordQuality stores a post's quality score (moderation.QualityRecorder). A
// later score replaces an earlier one; a post that no longer exists is
// skipped. Rankings pick it up when their cache expires (RankCacheTTL), so a
// flood of posts cannot force a ranking per post. It runs on the moderation
// worker, never inside a request's transaction.
func (s *Store) RecordQuality(ctx context.Context, id string, quality float64, model string) error {
	if math.IsNaN(quality) || quality < 0 || quality > 1 || model == "" {
		return errors.New("board: quality must be 0 to 1 with a model")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO event_quality(event_id,quality,model,scored_at) SELECT id,?,?,? FROM events WHERE id=?
 ON CONFLICT(event_id) DO UPDATE SET quality=excluded.quality,model=excluded.model,scored_at=excluded.scored_at`, quality, bound(model, 64), s.now().Unix(), id)
	return err
}

// RecordFlag opens or closes a post's review flag (moderation.FlagRecorder):
// while open, ranked views leave the post's edit chain out. A flag takes
// effect at once: the cached rankings are dropped, and a page of a pinned
// ranking skips a flagged post as it skips a hidden one. It runs on the
// moderation worker or the operator's review, never inside a request's
// transaction.
func (s *Store) RecordFlag(ctx context.Context, id string, open bool) error {
	var err error
	if open {
		_, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO event_flags(event_id,root,flagged_at) SELECT id,coalesce(nullif(origin,''),id),? FROM events WHERE id=?", s.now().Unix(), id)
	} else {
		_, err = s.db.ExecContext(ctx, "DELETE FROM event_flags WHERE event_id=?", id)
	}
	if err == nil {
		s.dropRankings()
	}
	return err
}

// dropRankings forgets the cached rankings (not the pinned bases offset pages
// read; readRanked rechecks what they list).
func (s *Store) dropRankings() {
	s.rankMu.Lock()
	s.rankCache = nil
	s.rankMu.Unlock()
}

// The quality backfill: qualityBackfillPage unscored posts are read at once,
// a run scores at most QualityBackfillMax, and it gives up after
// qualityBackfillErrors failed posts in a row (Jev down), skipping any one
// post that fails on its own.
const (
	qualityBackfillPage   = 100
	QualityBackfillMax    = 2000
	qualityBackfillErrors = 5
)

// BackfillQuality scores up to limit (at most QualityBackfillMax) public posts
// that have no quality score, newest first, asking Jev the quality question
// alone (ScoreQuality). Only posts a ranking can show are scored: visible
// top-level posts of the default kinds (no replies, simulations or imported
// summaries). It stops, without error, once today's Jev spend reaches half the
// daily cap, so fresh posts are always screened; stopped says why it ended. A
// post Jev cannot score is skipped (skipped counts them); qualityBackfillErrors
// failures in a row end the run with the last error. It runs from the
// operator's CLI, never inside a request's transaction.
func (s *Store) BackfillQuality(ctx context.Context, limit int) (scored, skipped int, stopped string, err error) {
	if s.moderation.engine == nil {
		return 0, 0, "", errors.New("moderation is off; set MODERATION=true and JEV_KEY_FILE as the service does")
	}
	return s.backfillQuality(ctx, s.moderation.engine, limit)
}

// qualityScorer is what the backfill asks: the moderation engine.
type qualityScorer interface {
	SpendToday(ctx context.Context) (moderation.Spend, error)
	ScoreQuality(ctx context.Context, subj moderation.Subject, text string) (float64, string, error)
}

func (s *Store) backfillQuality(ctx context.Context, e qualityScorer, limit int) (scored, skipped int, stopped string, err error) {
	limit = min(max(limit, 0), QualityBackfillMax)
	before := int64(math.MaxInt64)
	failures := 0
	for scored+skipped < limit {
		// events_top1 walks the rankable posts newest first, no sort.
		rows, err := s.db.QueryContext(ctx, `SELECT e.seq,e.id,e.room,e.text,e.public_key<>'' FROM events e INDEXED BY events_top1 CROSS JOIN rooms r ON r.name=e.room
 WHERE `+rankableTerms+` AND e.seq<? AND r.visibility='public' AND e.text<>''
 AND NOT EXISTS(SELECT 1 FROM event_quality q WHERE q.event_id=e.id) ORDER BY e.seq DESC LIMIT ?`, before, min(qualityBackfillPage, limit-scored-skipped))
		if err != nil {
			return scored, skipped, "", err
		}
		type post struct {
			seq            int64
			id, room, text string
			signed         bool
		}
		var page []post
		for rows.Next() {
			var p post
			if err = rows.Scan(&p.seq, &p.id, &p.room, &p.text, &p.signed); err != nil {
				rows.Close()
				return scored, skipped, "", err
			}
			page = append(page, p)
		}
		if err = closeRows(rows); err != nil {
			return scored, skipped, "", err
		}
		if len(page) == 0 {
			return scored, skipped, "every public post a ranking shows has a score", nil
		}
		for _, p := range page {
			before = p.seq
			spend, err := e.SpendToday(ctx)
			if err != nil {
				return scored, skipped, "", err
			}
			if spend.CapMicroUSD == 0 || 2*spend.SpentMicroUSD >= spend.CapMicroUSD {
				return scored, skipped, "half of today's Jev cap is spent; the rest is kept for screening new posts", nil
			}
			q, model, err := e.ScoreQuality(ctx, moderation.Subject{ID: p.id, Room: p.room, Signed: p.signed}, p.text)
			if err == nil {
				err = s.RecordQuality(ctx, p.id, q, model)
			}
			if err != nil {
				if ctx.Err() != nil {
					return scored, skipped, "", ctx.Err()
				}
				skipped++
				if failures++; failures >= qualityBackfillErrors {
					return scored, skipped, "", fmt.Errorf("%d posts in a row could not be scored: %w", failures, err)
				}
				continue
			}
			failures = 0
			scored++
		}
	}
	return scored, skipped, "limit reached", nil
}

// attachQuality sets the quality score on public messages that have one, in
// place. Like votes, it rides on reads, never on exports or receipts.
func attachQuality(ctx context.Context, tx *sql.Tx, events []Message) error {
	at := map[string][]int{}
	keys := []any{}
	for i, e := range events {
		if e.Visibility == "public" && e.Type == "message" {
			if _, ok := at[e.ID]; !ok {
				keys = append(keys, e.ID)
			}
			at[e.ID] = append(at[e.ID], i)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT event_id,quality,model FROM event_quality WHERE event_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+")", keys...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var q Quality
		if err = rows.Scan(&id, &q.Score, &q.Model); err != nil {
			return err
		}
		for _, i := range at[id] {
			events[i].Quality = &Quality{Score: q.Score, Model: q.Model}
		}
	}
	return rows.Err()
}

func bound(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
