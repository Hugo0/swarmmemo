package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Message search (C879). A query is cut into words, each folded to a
// light stem (lowercase, no possessive, no plural or -ing/-ed ending): a
// post matches when its text holds every stem, so "Flippa" finds
// "Flippa's" and "flippa.com", "wordles" finds "Wordle-style" and
// "websites" finds "website". The stem is always a prefix of the word it
// came from, so one substring test per word in SQL keeps every inflection
// (no index, no schema: the scan is the one the literal search ran).
//
// A search that names no order, cursor or offset reads its newest
// searchCandidates matches and ranks them: relevance first (a whole-word
// match over a word that starts with the stem over one that only holds it,
// and the whole query as a phrase), then the post's merit (its quality
// score, net votes and its author's standing). A post the screen flagged,
// scored low or voted down follows every other match. A word shorter than
// searchSubstringMin matches there only as a whole word, so "ai" does not
// find "said". Hidden posts are never search results. sort=new, a cursor
// or an offset read the same matches in their usual order.
const (
	searchCandidates   = 200
	searchTermsMax     = 8
	searchSubstringMin = 4
	searchLowQuality   = 0.2
	searchDownvoted    = -2
)

// searchStop are words a query of other words drops.
var searchStop = map[string]bool{"a": true, "an": true, "and": true, "are": true, "for": true, "how": true, "in": true, "is": true, "of": true, "on": true, "or": true, "the": true, "to": true, "what": true, "with": true}

// asciiLower lowercases A-Z only, as SQLite's lower() does, so Go and SQL
// fold a text the same way.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// searchWords cuts asciiLower(text) into words: runs of letters, digits and
// apostrophes (so a possessive stays on its word until searchStem).
func searchWords(text string) []string {
	return strings.FieldsFunc(asciiLower(text), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '’')
	})
}

// searchStem folds one lowercased word to its light stem, always a prefix
// of the word without its possessive: "flippa's" flippa, "companies"
// compan, "company" compan, "websites" websit, "running" run, "posted"
// post, "wordle" wordl. A stem is never cut below three letters.
func searchStem(w string) string {
	w = strings.Trim(w, "'’")
	for _, p := range []string{"'s", "’s"} {
		w = strings.TrimSuffix(w, p)
	}
	w = strings.Trim(w, "'’")
	cut := func(s string) string {
		if utf8.RuneCountInString(s) < 3 {
			return w
		}
		return s
	}
	s := w
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 4:
		s = s[:len(s)-3]
	case strings.HasSuffix(s, "sses"):
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"):
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") && !strings.HasSuffix(s, "us") && !strings.HasSuffix(s, "is") && len(s) > 3:
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "ing") && len(s) > 5:
		s = undouble(s[:len(s)-3])
	case strings.HasSuffix(s, "ed") && len(s) > 4:
		s = undouble(s[:len(s)-2])
	}
	s = cut(s)
	// A final e or y (company, website) is dropped, so the stem stays a
	// prefix of every form (companies, websites).
	if n := len(s); n > 3 && (s[n-1] == 'e' || s[n-1] == 'y') {
		s = s[:n-1]
	}
	return cut(s)
}

// undouble drops one of a doubled final consonant (runn, stopp) other than
// l, s or z.
func undouble(s string) string {
	n := len(s)
	if n >= 2 && s[n-1] == s[n-2] && strings.IndexByte("bdfgmnprt", s[n-1]) >= 0 {
		return s[:n-1]
	}
	return s
}

// searchTerms are the distinct stems of a query's words, stop words dropped
// when other words remain, at most searchTermsMax. None for a query of only
// punctuation (searchClause then matches it literally).
func searchTerms(query string) []string {
	words := searchWords(query)
	kept := words[:0:0]
	for _, w := range words {
		if !searchStop[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		kept = words
	}
	seen := map[string]bool{}
	terms := []string{}
	for _, w := range kept {
		s := searchStem(w)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		terms = append(terms, s)
		if len(terms) == searchTermsMax {
			break
		}
	}
	return terms
}

// searchClause is the SQL filter of a message search on e.text: every term
// held, and a visible post. The text test first: hidden is stored after the
// payload, so a search scan reads it only for the rows that match (C688).
func searchClause(query string) (string, []any) {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return "instr(lower(e.text),lower(?))>0", []any{query}
	}
	parts := make([]string, len(terms))
	args := make([]any, len(terms))
	for i, t := range terms {
		parts[i], args[i] = "instr(lower(e.text),lower(?))>0", t
	}
	return strings.Join(parts, " AND "), args
}

// searchRelevance scores how well text answers the query's terms: for each
// term 3 for a word with the same stem, 2 for a word that starts with it, 1
// for a word that holds it (terms of searchSubstringMin letters or more), and
// 2 more when the whole query appears as written. 0 when a term is missing.
func searchRelevance(text, query string, terms []string) int {
	if len(terms) == 0 {
		if strings.Contains(asciiLower(text), asciiLower(query)) {
			return 1
		}
		return 0
	}
	words := searchWords(text)
	stems := make([]string, len(words))
	for i, w := range words {
		stems[i] = searchStem(w)
	}
	total := 0
	for _, t := range terms {
		best := 0
		long := utf8.RuneCountInString(t) >= searchSubstringMin
		for i, w := range words {
			switch {
			case stems[i] == t:
				best = 3
			case long && strings.HasPrefix(w, t):
				best = max(best, 2)
			case long && strings.Contains(w, t):
				best = max(best, 1)
			}
			if best == 3 {
				break
			}
		}
		if best == 0 {
			return 0
		}
		total += best
	}
	if q := strings.TrimSpace(asciiLower(query)); len(terms) > 1 && strings.Contains(asciiLower(text), q) {
		total += 2
	}
	return total
}

// searchRanked reports whether a messages.list reads its search ranked:
// it names a query and no order, cursor, offset or in-order first_post view.
func searchRanked(c Command, opts ListOptions) bool {
	return c.Operation == "messages.list" && c.Query != "" && opts.Sort == "" && opts.Offset == 0 && c.Cursor == "" && c.Older == "" &&
		opts.FirstPost != FirstPostOnly && opts.FirstPost != FirstPostUnanswered
}

// searchHit is one ranked candidate.
type searchHit struct {
	msg       Message
	relevance int
	demoted   bool
	merit     float64
}

// readSearch is the ranked search page: the newest searchCandidates posts
// the filter matches, scored and ordered (search.go header), at most a page.
// next_cursor is the newest match's position, so a cursor read polls for new
// matches; has_more says more matched than the page holds.
func (s *Store) readSearch(ctx context.Context, tx *sql.Tx, c Command, a actor, opts ListOptions, where []string, args []any, now int64) (Result, error) {
	limit := limitValue(c.Limit)
	query := "SELECT " + eventColumns + " FROM events e JOIN rooms r ON r.name=e.room WHERE " + strings.Join(where, " AND ") + " ORDER BY e.seq DESC LIMIT ?"
	qargs := append(append([]any{}, args...), searchCandidates)
	if eventsPageHook != nil {
		eventsPageHook(query, whereShape(where, "DESC"), qargs)
	}
	started := time.Now()
	rows, err := tx.QueryContext(ctx, query, qargs...)
	if err != nil {
		return Result{}, err
	}
	scanned := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		scanned = append(scanned, e)
	}
	if err = closeRows(rows); err != nil {
		return Result{}, err
	}
	logSlowRead(c.Operation, where, "DESC", time.Since(started))
	var seq int64
	if len(scanned) > 0 {
		seq = scanned[0].internalSequence
	}
	terms := searchTerms(c.Query)
	hits := []searchHit{}
	kept := []Message{}
	for _, e := range scanned {
		if r := searchRelevance(e.Text, c.Query, terms); r > 0 {
			hits = append(hits, searchHit{msg: e, relevance: r})
			kept = append(kept, e)
		}
	}
	if err = s.attachScores(ctx, tx, kept, now); err != nil {
		return Result{}, err
	}
	flagged, standing, err := searchSignals(ctx, tx, kept)
	if err != nil {
		return Result{}, err
	}
	for i := range hits {
		e := kept[i]
		h := &hits[i]
		h.msg = e
		var q *float64
		if e.Quality != nil {
			q = &e.Quality.Score
		}
		quality := Ranking.quality(q)
		var votes int64
		if e.Votes != nil {
			votes = e.Votes.Score
		}
		h.demoted = flagged[e.ID] || (q != nil && *q < searchLowQuality) || votes <= searchDownvoted
		h.merit = Ranking.QualityWeight*quality + float64(min(max(votes, -5), 5)) + 2*standing[e.ID]
	}
	sort.SliceStable(hits, func(i, j int) bool {
		x, y := hits[i], hits[j]
		if x.demoted != y.demoted {
			return !x.demoted
		}
		if x.relevance != y.relevance {
			return x.relevance > y.relevance
		}
		if x.merit != y.merit {
			return x.merit > y.merit
		}
		return x.msg.internalSequence > y.msg.internalSequence
	})
	more := len(hits) > limit || len(scanned) == searchCandidates
	page := make([]Message, 0, min(len(hits), limit))
	budget := 0
	for _, h := range hits {
		if len(page) == limit {
			break
		}
		encoded, _ := json.Marshal(h.msg)
		if len(page) > 0 && budget+len(encoded) > 64<<10 {
			more = true
			break
		}
		budget += len(encoded)
		page = append(page, h.msg)
	}
	if err = s.loadAttachments(ctx, tx, page, now); err != nil {
		return Result{}, err
	}
	if err = s.screenConversationMessages(ctx, tx, a, page); err != nil {
		return Result{}, err
	}
	res := Result{Messages: page, NextCursor: s.cursor(seq), Data: map[string]any{"has_more": more}}
	if err = attachListTotal(ctx, tx, &res, c, opts, where, args); err != nil {
		return Result{}, err
	}
	return res, nil
}

// searchSignals reads, for each post, whether a screen flag on its edit
// chain is open and its author's vote weight v(standing) from 0 to 1.
func searchSignals(ctx context.Context, tx *sql.Tx, events []Message) (map[string]bool, map[string]float64, error) {
	flagged, standing := map[string]bool{}, map[string]float64{}
	if len(events) == 0 {
		return flagged, standing, nil
	}
	ids := make([]any, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	rows, err := tx.QueryContext(ctx, "SELECT e.id,EXISTS(SELECT 1 FROM event_flags f WHERE f.root=coalesce(nullif(e.origin,''),e.id)),"+standingVSQL("e.account")+
		" FROM events e WHERE e.id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var flag bool
		var v float64
		if err = rows.Scan(&id, &flag, &v); err != nil {
			return nil, nil, err
		}
		flagged[id], standing[id] = flag, v
	}
	return flagged, standing, rows.Err()
}
