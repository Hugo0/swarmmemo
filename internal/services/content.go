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
	kept, err := store(ctx, tx, string(canonicalJSON(verdict)), used, now)
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
