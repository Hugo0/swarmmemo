package find

// The index: an in-memory SQLite FTS5 table built from the sources' docs,
// immutable once built and replaced whole by the next build. It is a cache
// of the sources, never a source: nothing is written to the board's
// database, and losing it costs one rebuild.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // the board's SQLite, which carries FTS5
)

// Column weights for BM25: a word in the name counts most, then the
// category, the description and the other terms (URLs, keywords).
const bm25Weights = "10.0, 5.0, 2.0, 1.0"

// candidatesMax bounds the matches one search ranks.
const candidatesMax = 500

// shortMax bounds the rows a substring scan for a one- or two-letter
// non-Latin word (中, 天气) reads: the trigram index needs three.
const shortMax = 200

// docsColumns are both FTS tables' columns: docs, with the porter stemmer
// for words split by spaces, and docs_tri, trigrams for text in any
// language (Chinese has no spaces; Russian inflects past a stemmer).
const docsColumns = `name, category, body, terms,
 type UNINDEXED, price UNINDEXED, output UNINDEXED, network UNINDEXED, protocols UNINDEXED, door UNINDEXED`

var indexSeq atomic.Int64

// Index is one built index.
type Index struct {
	db      *sql.DB
	docs    []Doc
	vocab   Vocab
	BuiltAt time.Time
	Counts  map[string]int
}

// Build indexes docs. The index lives in its own in-memory database on one
// connection.
func Build(ctx context.Context, docs []Doc, vocab Vocab, at time.Time) (*Index, error) {
	// A named in-memory database per index: each build is its own.
	db, err := sql.Open("sqlite", fmt.Sprintf("file:find-%d?mode=memory&cache=private", indexSeq.Add(1)))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	fail := func(err error) (*Index, error) {
		db.Close()
		return nil, err
	}
	if _, err = db.ExecContext(ctx, `CREATE VIRTUAL TABLE docs USING fts5(`+docsColumns+`,
 tokenize='porter unicode61 remove_diacritics 2')`); err != nil {
		return fail(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE VIRTUAL TABLE docs_tri USING fts5(`+docsColumns+`, tokenize='trigram')`); err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO docs(rowid,name,category,body,terms,type,price,output,network,protocols,door) VALUES(?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		tx.Rollback()
		return fail(err)
	}
	tri, err := tx.PrepareContext(ctx, "INSERT INTO docs_tri(rowid,name,category,body,terms,type,price,output,network,protocols,door) VALUES(?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		stmt.Close()
		tx.Rollback()
		return fail(err)
	}
	ix := &Index{db: db, vocab: vocab, BuiltAt: at, Counts: map[string]int{}}
	for _, d := range docs {
		if d.Type == "" || d.ID == "" {
			continue
		}
		if !ValidDoor(d.Door) || d.DoorBy == nil {
			d.Door, d.DoorBy = "", nil
		}
		ix.docs = append(ix.docs, d)
		row := []any{len(ix.docs), d.Name, d.Category, d.Description, strings.TrimSpace(d.Terms + " " + d.recordText()), d.Type, d.Price, d.Output, d.Network, protocolFamilies(d.Protocols), d.Door}
		if _, err = stmt.ExecContext(ctx, row...); err == nil {
			_, err = tri.ExecContext(ctx, row...)
		}
		if err != nil {
			stmt.Close()
			tri.Close()
			tx.Rollback()
			return fail(err)
		}
		ix.Counts[d.Type]++
	}
	stmt.Close()
	tri.Close()
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return ix, nil
}

// Close frees the index.
func (ix *Index) Close() error {
	if ix == nil || ix.db == nil {
		return nil
	}
	return ix.db.Close()
}

// Len is the number of docs.
func (ix *Index) Len() int { return len(ix.docs) }

// match is one candidate: the doc's place and its BM25 (lower is better,
// as FTS5 reports it; 0 when browsing without words).
type match struct {
	doc  int
	bm25 float64
}

// query is the FTS5 expression for the words: each word, a prefix of it
// (four letters or more) and the categories it stands for, OR'd, so a doc
// matching more of them ranks higher. Words are quoted: nothing in a query
// is FTS syntax.
func (ix *Index) query(words []string) (string, map[string][]string) {
	var groups []string
	expanded := map[string][]string{}
	for _, w := range words {
		alts := []string{`"` + w + `"`}
		if len([]rune(w)) >= 4 {
			alts = append(alts, `"`+w+`"*`)
		}
		for _, syn := range ix.vocab.synonyms(w) {
			alts = append(alts, `category:"`+syn+`"`)
			expanded[w] = append(expanded[w], syn)
		}
		groups = append(groups, "("+strings.Join(alts, " OR ")+")")
	}
	return strings.Join(groups, " OR "), expanded
}

// triQuery is the trigram index's expression for words with a non-Latin
// letter (and the Latin words beside them), and the words too short for
// trigrams: each word of three letters or more; a long word of a spaced
// script also by its stem (all but its last two letters, at least four:
// "погоду" finds "погода"); a run of a script without spaces also by
// its three-letter windows. It answers too the two-letter windows of those
// runs, scanned for when no trigram matches ("城市天气服务" finds "城市的
// 天气预报服务"). Words are quoted: nothing in a query is FTS syntax.
func triQuery(words []string) (string, []string, []string) {
	var alts, short, pairs []string
	add := func(t string) {
		if t = `"` + t + `"`; !slices.Contains(alts, t) {
			alts = append(alts, t)
		}
	}
	for _, w := range words {
		r := []rune(w)
		if len(r) < 3 {
			if nonLatin(w) {
				short = append(short, w)
			}
			continue
		}
		add(w)
		switch {
		case cjk(r[0]):
			for i := 0; i+3 <= len(r) && len(r) > 3 && i < 16; i++ {
				add(string(r[i : i+3]))
			}
			for i := 0; i+2 <= len(r) && len(pairs) < 8; i++ {
				if p := string(r[i : i+2]); !slices.Contains(pairs, p) {
					pairs = append(pairs, p)
				}
			}
		case len(r) >= 6:
			add(string(r[:len(r)-2]))
		}
	}
	return strings.Join(alts, " OR "), short, pairs
}

// search returns the candidates for words under f, best BM25 first; with no
// words, every doc f admits (relevance is then equal).
func (ix *Index) search(ctx context.Context, words []string, f Filters) ([]match, map[string][]string, error) {
	where, args := []string{}, []any{}
	if len(f.Types) > 0 {
		where = append(where, "type IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Types)), ",")+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	switch {
	case f.Category != "":
		where, args = append(where, "category = ?"), append(args, f.Category)
	default:
		// Adult listings are found only by asking for that category.
		where = append(where, "category <> 'adult'")
	}
	if p := f.maxMicro(); p >= 0 {
		where, args = append(where, "price >= 0 AND price <= ?"), append(args, p)
	}
	if f.Output != "" {
		where, args = append(where, "output = ?"), append(args, f.Output)
	}
	if f.Network != "" {
		where, args = append(where, "network = ?"), append(args, f.Network)
	}
	if f.Protocol != "" {
		where, args = append(where, "instr(protocols, ?) > 0"), append(args, " "+f.Protocol+" ")
	}
	if f.Door != "" {
		set := doorSet(f.Door)
		where = append(where, "door IN ("+strings.TrimSuffix(strings.Repeat("?,", len(set)), ",")+")")
		for _, d := range set {
			args = append(args, d)
		}
	}
	if f.Language != "" {
		where, args = append(where, "instr(' '||terms||' ', ?) > 0"), append(args, " "+LanguageCapabilityPrefix+f.Language+" ")
	}
	if nonLatin(strings.Join(words, " ")) {
		return ix.searchAnyScript(ctx, words, where, args)
	}
	var expanded map[string][]string
	q := "SELECT rowid, 0.0 FROM docs"
	if len(words) > 0 {
		var expr string
		expr, expanded = ix.query(words)
		q = "SELECT rowid, bm25(docs, " + bm25Weights + ") FROM docs WHERE docs MATCH ?"
		args = append([]any{expr}, args...)
		if len(where) > 0 {
			q += " AND " + strings.Join(where, " AND ")
		}
		q += " ORDER BY bm25(docs, " + bm25Weights + ") LIMIT ?"
		args = append(args, candidatesMax)
	} else {
		if len(where) > 0 {
			q += " WHERE " + strings.Join(where, " AND ")
		}
		q += " LIMIT ?"
		args = append(args, candidatesMax*4)
	}
	out, err := ix.matches(ctx, q, args, nil)
	return out, expanded, err
}

// searchAnyScript matches words with a non-Latin letter on the trigram
// index, BM25 best first, then the words too short for trigrams as
// substrings (up to shortMax rows, after the trigram matches), and when
// no trigram matched, the two-letter windows of Chinese, Japanese, Korean
// and Thai runs too.
func (ix *Index) searchAnyScript(ctx context.Context, words []string, where []string, args []any) ([]match, map[string][]string, error) {
	expr, short, pairs := triQuery(words)
	var out []match
	seen := map[int]bool{}
	if expr != "" {
		q := "SELECT rowid, bm25(docs_tri, " + bm25Weights + ") FROM docs_tri WHERE docs_tri MATCH ?"
		if len(where) > 0 {
			q += " AND " + strings.Join(where, " AND ")
		}
		q += " ORDER BY bm25(docs_tri, " + bm25Weights + ") LIMIT ?"
		var err error
		if out, err = ix.matches(ctx, q, append(append([]any{expr}, args...), candidatesMax), seen); err != nil {
			return nil, nil, err
		}
	}
	if len(out) == 0 {
		short = append(short, pairs...)
	}
	if len(short) > 0 && len(out) < candidatesMax {
		var like []string
		var largs []any
		for _, w := range short {
			p := "%" + w + "%"
			like = append(like, "name LIKE ? OR category LIKE ? OR body LIKE ? OR terms LIKE ?")
			largs = append(largs, p, p, p, p)
		}
		q := "SELECT rowid, 0.0 FROM docs WHERE (" + strings.Join(like, " OR ") + ")"
		if len(where) > 0 {
			q += " AND " + strings.Join(where, " AND ")
		}
		q += " LIMIT ?"
		more, err := ix.matches(ctx, q, append(append(largs, args...), shortMax), seen)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, more...)
	}
	return out, nil, nil
}

// matches runs a candidate query: rowid and BM25 a row, rows seen skipped
// (seen may be nil).
func (ix *Index) matches(ctx context.Context, q string, args []any, seen map[int]bool) ([]match, error) {
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []match
	for rows.Next() {
		var m match
		if err = rows.Scan(&m.doc, &m.bm25); err != nil {
			return nil, err
		}
		m.doc--
		if m.doc < 0 || m.doc >= len(ix.docs) || seen != nil && seen[m.doc] {
			continue
		}
		if seen != nil {
			seen[m.doc] = true
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
