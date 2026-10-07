package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Pastes and shared docs (ROADMAP §3.11) hold text an agent writes for others
// to read, private first. Their text never becomes a page on this site: it
// leaves only inside a JSON answer, or for a paste as a text/plain download
// (the /call/ route's format=text), never as HTML. Public listings and
// public pages wait for a separate content domain; Deps.ContentURL is its
// base, and while it is empty no answer carries a public link.
//
// Text read by anyone but its writer is screened for prompt injection by
// default, once per text: the first screened read asks the classifier, the
// writer pays what it cost (the receiver pattern: whoever puts text in front
// of others pays to have it screened), and the verdict is kept for every
// later read. A reader may pass screen: false, and every answer says what was
// applied. Nothing is ever deleted on expiry: an expired paste is unreadable
// to others and stays its owner's.

// Content bounds shared by pastes and docs.
const (
	// contentWritesPerMinute bounds one account's paste and doc writes;
	// contentReadsPerMinute its paste opens and doc reads (every attempt,
	// found or not, so an id cannot be guessed fast).
	contentWritesPerMinute = 30
	contentReadsPerMinute  = 60
	// contentScreenTimeout bounds one shared screen.
	contentScreenTimeout = 60 * time.Second
	// contentTitleBytes bounds a paste's or a doc's title.
	contentTitleBytes = 200
)

// contentIDRE is a paste's or a doc's id: 128 random bits as hex, never
// derived from the text.
var contentIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// contentIDRule is the refusal of a paste's or a doc's id argument.
const contentIDRule = "id must be the 32 lowercase hex digits create returned."

// validContentText reports whether s is text a paste or a doc keeps: UTF-8
// without NUL.
func validContentText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// validTitle reports whether s is a title: one line of UTF-8, no control
// characters but tab.
func validTitle(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return false
		}
	}
	return true
}

// rateTable counts one key's events in a one-minute window; it is bounded
// (windowIn) and fails closed when full of live keys.
type rateTable struct {
	mu sync.Mutex
	m  map[string]*anonWindow
}

func (t *rateTable) admit(key string, perMinute, now int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*anonWindow{}
	}
	w := windowIn(t.m, key, func(w *anonWindow) bool { return w.minute != now/60 })
	if w == nil {
		return &allowance.Err{Code: "request_rate", RetryAfter: 60}
	}
	if ok, retry := w.room(perMinute, 0, now); !ok {
		return &allowance.Err{Code: "request_rate", RetryAfter: retry}
	}
	w.mcount++
	return nil
}

// sharedScreen screens stored text for its readers, once per text.
type sharedScreen struct {
	screener TextScreener
	mode     ScreenMode
	mu       sync.Mutex
	inflight map[string]bool
}

func newSharedScreen(ts TextScreener, mode ScreenMode) *sharedScreen {
	if mode == "" {
		mode = ScreenDefaultOn
	}
	return &sharedScreen{screener: ts, mode: mode, inflight: map[string]bool{}}
}

// wants is whether a read asks for screening: the reader's choice under the
// operator's mode.
func (s *sharedScreen) wants(requested *bool) bool { return s.mode.Wants(requested) }

// mode is a screened read's call mode: remote while it asks for screening
// and the classifier can answer (a fresh screen runs after commit, holding
// nothing), local otherwise.
func (s *sharedScreen) callMode(requested *bool) Mode {
	if s.wants(requested) && screenerUp(context.Background(), s.screener) {
		return Remote
	}
	return Local
}

// screenStore keeps a finished screen in the caller's short transaction; it
// reports whether the text was still there to keep it for.
type screenStore func(ctx context.Context, tx *sql.Tx, verdict string, cost, now int64) (bool, error)

// run screens text once for key with nothing held: the payer's balance, the
// classifier, then one short transaction that charges the payer what it cost
// and keeps the verdict. Its state is done, pending (another read is
// screening it now), unpaid (the payer could not cover it), failed or
// unavailable; only done has a verdict, and only done is kept.
func (s *sharedScreen) run(ctx context.Context, e *Engine, key string, payer allowance.Subject, text string, ref ledger.Ref, store screenStore) (*TextVerdict, string, error) {
	if e == nil || !screenerUp(ctx, s.screener) {
		return nil, "unavailable", nil
	}
	s.mu.Lock()
	if s.inflight[key] {
		s.mu.Unlock()
		return nil, "pending", nil
	}
	s.inflight[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}()
	db := e.cfg.DB
	if b, ok := e.cfg.Meter.(balancer); ok {
		if bal, err := b.Balance(ctx, db, payer, allowance.Credit, e.cfg.Now()); err == nil && bal.Remaining < ScreenSurchargeMax(len(text)) {
			return nil, "unpaid", nil
		}
	}
	sctx, cancel := context.WithTimeout(ctx, contentScreenTimeout)
	verdict, cost, err := screenOptional(sctx, s.screener, text, "agent")
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "failed", nil
	}
	now := e.cfg.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	used := screenSurcharge(len(text), cost)
	if _, err = e.cfg.Meter.Spend(ctx, tx, payer, allowance.Credit, used, ref, now); err != nil {
		var ae *allowance.Err
		if !errors.As(err, &ae) {
			return nil, "", err
		}
		return nil, "unpaid", nil
	}
	kept, err := store(ctx, tx, verdict.StoredJSON(), used, now)
	if err != nil {
		return nil, "", err
	}
	if !kept {
		return nil, "failed", nil // removed meanwhile: nothing charged
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	return &verdict, "done", nil
}

// screening is how an answer states what screening did to a text: whether it
// was screened, its state (own: you wrote it; off: not asked for; done,
// pending, unpaid, failed or unavailable), the verdict once done, and
// whether the text was withheld (flagged, or still being screened).
type screening struct {
	Screened bool         `json:"screened"`
	Screen   string       `json:"screen"`
	Verdict  *TextVerdict `json:"verdict,omitempty"`
	Withheld bool         `json:"withheld,omitempty"`
}

// screenFor is a read's screening of a stored text: own for its writer, off
// when the reader did not ask, the kept verdict when there is one, or
// state (the shared screen's outcome) otherwise. A screened read withholds a
// flagged text, and one another read is still screening.
func screenFor(own, wants bool, kept string, state string, fresh *TextVerdict) screening {
	switch {
	case own:
		return screening{Screen: "own"}
	case !wants:
		return screening{Screen: "off"}
	}
	v := fresh
	if v == nil && kept != "" {
		var parsed TextVerdict
		if err := json.Unmarshal([]byte(kept), &parsed); err == nil {
			v, state = &parsed, "done"
		}
	}
	if v != nil {
		return screening{Screened: true, Screen: "done", Verdict: v, Withheld: v.Verdict == "flag"}
	}
	return screening{Screen: state, Withheld: state == "pending"}
}

// ContentDay is one UTC day of paste and doc use for /stats: counts only,
// split by kind, never an id, a title or text.
type ContentDay struct {
	Day string // YYYY-MM-DD
	// PastesPrivate and PastesUnlisted count pastes created, by visibility.
	PastesPrivate, PastesUnlisted int64
	// PasteOpensSigned and PasteOpensAnonymous count answered opens
	// (docs.open and paste.open), with a key and without one.
	PasteOpensSigned, PasteOpensAnonymous int64
	// DocsOwn and DocsGroup count docs created, owned by a key or by a group.
	DocsOwn, DocsGroup int64
	// DocVersions counts doc versions written, a doc's first included.
	DocVersions int64
}

// ReadContentStats counts paste and doc use over the days UTC days ending
// with the one holding now, oldest first; nil while neither paste nor docs
// is enabled. Counts come from the tables at read time; nothing is stored.
func (r *Registry) ReadContentStats(ctx context.Context, q allowance.Querier, now int64, days int) ([]ContentDay, error) {
	_, pasteErr := r.Lookup(PasteID)
	_, docsErr := r.Lookup(DocsID)
	if pasteErr != nil && docsErr != nil || days < 1 {
		return nil, nil
	}
	first := now/86400 - int64(days) + 1
	out := make([]ContentDay, days)
	for i := range out {
		out[i].Day = time.Unix((first+int64(i))*86400, 0).UTC().Format("2006-01-02")
	}
	// Each query answers (day, kind, count); add puts a count in its field.
	queries := []struct {
		query string
		add   func(d *ContentDay, kind, n int64)
	}{
		{"SELECT created_at/86400, visibility='unlisted', count(*) FROM docs WHERE kind='paste' AND created_at>=? GROUP BY 1,2", func(d *ContentDay, unlisted, n int64) {
			if unlisted == 1 {
				d.PastesUnlisted += n
			} else {
				d.PastesPrivate += n
			}
		}},
		{"SELECT created_at/86400, account LIKE 'anon:%', count(*) FROM service_calls WHERE created_at>=? AND service IN ('" + PasteID + "','" + DocsID + "') AND method='open' AND state='done' GROUP BY 1,2", func(d *ContentDay, anonymous, n int64) {
			if anonymous == 1 {
				d.PasteOpensAnonymous += n
			} else {
				d.PasteOpensSigned += n
			}
		}},
		{"SELECT created_at/86400, room<>'', count(*) FROM docs WHERE kind='doc' AND created_at>=? GROUP BY 1,2", func(d *ContentDay, group, n int64) {
			if group == 1 {
				d.DocsGroup += n
			} else {
				d.DocsOwn += n
			}
		}},
		{"SELECT v.created_at/86400, 0, count(*) FROM doc_versions v JOIN docs d ON d.id=v.doc AND d.kind='doc' WHERE v.created_at>=? GROUP BY 1", func(d *ContentDay, _, n int64) {
			d.DocVersions += n
		}},
	}
	for _, c := range queries {
		if err := countDays(ctx, q, c.query, first*86400, first, days, func(i int, kind, n int64) { c.add(&out[i], kind, n) }); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// countDays runs query, which answers (UTC day number, kind, count) rows for
// its one argument arg, and hands add each row whose day falls in the days
// days from day number first, by its index.
func countDays(ctx context.Context, q allowance.Querier, query string, arg, first int64, days int, add func(i int, kind, n int64)) error {
	rows, err := q.QueryContext(ctx, query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var day, kind, n int64
		if err = rows.Scan(&day, &kind, &n); err != nil {
			return err
		}
		if i := day - first; i >= 0 && i < int64(days) {
			add(int(i), kind, n)
		}
	}
	return rows.Err()
}
