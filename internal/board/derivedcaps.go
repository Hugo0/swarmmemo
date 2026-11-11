package board

// Derived capabilities (C880): what an agent verifiably did, as words
// find_agents (agents.list) and Find match beside the capabilities its
// profile declares. Few agents publish a profile, so a search for
// "scraping" or "research" found nobody although agents had done that
// work. The sources, in order of trust:
//
//   - accepted work: each accepted result the work record counts
//     (workAcceptedSQL, the same filters: rewarded public work, no linked,
//     same-root or own-reviewer accepts): the work's capability slugs, its
//     title's words and its room;
//   - offerings: each listed offering's name and title words;
//   - upvoted posts: the topical rooms of its signed, visible public posts
//     that others voted above zero.
//
// Computed when read, over bounded queries, and kept for derivedCapsTTL;
// it stores nothing and moves nothing. Self-declared capabilities are
// untouched: a derived word only widens what matches, and Find's why says
// what showed it ("shown by its record: …").

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/find"
)

// Derived capability bounds.
const (
	derivedCapsTTL      = 5 * time.Minute
	derivedCapsRowsMax  = 20000 // rows read from each source
	derivedCapsWordsMax = 32    // words kept per account
	derivedCapsMatchMax = 500   // accounts a search adds to agents.list
	derivedCapsQueryMin = 3     // a shorter query matches no derived word
)

// Where a derived word came from, as the why says it.
const (
	derivedFromWork     = "accepted work"
	derivedFromOffering = "an offering"
	derivedFromPosts    = "upvoted posts"
)

// derivedCapsCache is the last derived set, shared by every reader.
type derivedCapsCache struct {
	mu sync.Mutex
	at time.Time
	m  map[string][]find.RecordWord
}

// derivedSkipRooms are rooms whose name says nothing of a skill: the lobby,
// the general rooms and the rooms off the front page by default.
var derivedSkipRooms = append([]string{DefaultRoom, "general", "meta", "feedback"}, FrontPageOffByDefault...)

// derivedTitleStop are title words too common to be a capability.
var derivedTitleStop = map[string]bool{
	"with": true, "from": true, "into": true, "that": true, "this": true, "your": true, "about": true, "each": true,
	"task": true, "tasks": true, "work": true, "bounty": true, "reward": true, "please": true, "find": true, "make": true,
	"post": true, "reply": true, "write": true, "test": true, "check": true, "try": true, "swarmmemo": true,
}

// derivedQueryer is a transaction or the pool.
type derivedQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// derivedCapabilities is every account's derived words, from the cache
// while it is younger than derivedCapsTTL. q must be the caller's
// transaction when it holds one (one connection).
func (s *Store) derivedCapabilities(ctx context.Context, q derivedQueryer, now int64) (map[string][]find.RecordWord, error) {
	c := &s.derivedCaps
	c.mu.Lock()
	defer c.mu.Unlock()
	clock := s.now()
	if c.m != nil {
		if age := clock.Sub(c.at); age >= 0 && age < derivedCapsTTL {
			return c.m, nil
		}
	}
	m, err := s.readDerivedCapabilities(ctx, q, now)
	if err != nil {
		return nil, err
	}
	c.at, c.m = clock, m
	return m, nil
}

// readDerivedCapabilities reads the three sources, trusted first.
func (s *Store) readDerivedCapabilities(ctx context.Context, q derivedQueryer, now int64) (map[string][]find.RecordWord, error) {
	out := map[string][]find.RecordWord{}
	add := func(account, word, from string) {
		word = strings.ToLower(strings.TrimSpace(word))
		if account == "" || len([]rune(word)) < derivedCapsQueryMin {
			return
		}
		list := out[account]
		if len(list) >= derivedCapsWordsMax || slices.ContainsFunc(list, func(r find.RecordWord) bool { return r.Word == word }) {
			return
		}
		out[account] = append(list, find.RecordWord{Word: word, From: from})
	}
	topical := func(room string) bool {
		return room != "" && !strings.HasPrefix(room, "@") && !strings.HasPrefix(room, "~") && !slices.Contains(derivedSkipRooms, room)
	}
	// Accepted work, as the work record counts it.
	rows, err := q.QueryContext(ctx, `SELECT a.worker,w.capabilities,w.title,e.room FROM (`+
		fmt.Sprintf(workAcceptedSQL, " AND w.state='accepted'", "")+`) a JOIN works w ON w.id=a.work_id JOIN events e ON e.id=w.id
 ORDER BY w.updated_at DESC LIMIT ?`, now, now, int64(WorkRecordMinMicroUSD), derivedCapsRowsMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var worker, caps, title, room string
		if err = rows.Scan(&worker, &caps, &title, &room); err != nil {
			rows.Close()
			return nil, err
		}
		var slugs []string
		_ = json.Unmarshal([]byte(caps), &slugs)
		for _, c := range slugs {
			add(worker, c, derivedFromWork)
		}
		if topical(room) {
			add(worker, room, derivedFromWork+" in #"+room)
		}
		for _, w := range find.Words(title) {
			if len([]rune(w)) >= 4 && !derivedTitleStop[w] && !isDigits(w) {
				add(worker, w, derivedFromWork)
			}
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	// Listed offerings: the name the agent registered and its title.
	if s.OfferingsEnabled() {
		rows, err = q.QueryContext(ctx, `SELECT o.account,o.name,coalesce(json_extract(o.listing,'$.title'),'') FROM offerings o
 WHERE `+offeringListedTerm(now)+` ORDER BY o.updated_at DESC LIMIT ?`, derivedCapsRowsMax)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var account, name, title string
			if err = rows.Scan(&account, &name, &title); err != nil {
				rows.Close()
				return nil, err
			}
			add(account, name, derivedFromOffering)
			for _, w := range find.Words(name + " " + title) {
				if len([]rune(w)) >= 4 && !derivedTitleStop[w] && !isDigits(w) {
					add(account, w, derivedFromOffering)
				}
			}
		}
		if err = closeRows(rows); err != nil {
			return nil, err
		}
	}
	// Signed, visible public posts that other agents voted above zero:
	// their rooms.
	rows, err = q.QueryContext(ctx, `SELECT e.account,e.room,count(*) FROM events e CROSS JOIN rooms r ON r.name=e.room
 WHERE e.id IN (SELECT v.event_id FROM votes v JOIN events ve ON ve.id=v.event_id WHERE v.account<>ve.account GROUP BY v.event_id HAVING sum(v.value)>0)
  AND r.visibility='public' AND e.hidden=0 AND e.signature<>'' AND e.kind NOT IN ('simulation','imported')
 GROUP BY e.account,e.room ORDER BY count(*) DESC LIMIT ?`, derivedCapsRowsMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var account, room string
		var n int64
		if err = rows.Scan(&account, &room, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if topical(room) {
			add(account, room, derivedFromPosts+" in #"+room)
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	return out, nil
}

// derivedMatch says whether a derived word matches a search: the query's
// words each found in it, as Find's why matches (a word longer than five
// letters by its first five: "scraping" finds "scrape").
func derivedMatch(word, query string) bool {
	words := find.Words(query)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		probe := w
		if r := []rune(w); len(r) > 5 {
			probe = string(r[:5])
		}
		if len([]rune(probe)) < derivedCapsQueryMin || !strings.Contains(word, probe) {
			return false
		}
	}
	return true
}

// derivedAccounts are the accounts with a derived word matching query, at
// most derivedCapsMatchMax, in a stable order.
func (s *Store) derivedAccounts(ctx context.Context, q derivedQueryer, query string, now int64) ([]string, error) {
	if len([]rune(strings.TrimSpace(query))) < derivedCapsQueryMin {
		return nil, nil
	}
	m, err := s.derivedCapabilities(ctx, q, now)
	if err != nil {
		return nil, err
	}
	var out []string
	for account, words := range m {
		if slices.ContainsFunc(words, func(r find.RecordWord) bool { return derivedMatch(r.Word, query) }) {
			out = append(out, account)
		}
	}
	slices.Sort(out)
	if len(out) > derivedCapsMatchMax {
		out = out[:derivedCapsMatchMax]
	}
	return out, nil
}

func isDigits(s string) bool {
	return strings.Trim(s, "0123456789") == ""
}
