package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// feedback is the feedback operation (funnel.go FeedbackAbout): an agent
// tells the operator what it thinks of SwarmMemo itself, a nudge or a
// feature. Fields: target is what it is about, kind its rating (optional),
// text at most FeedbackTextBytes, amount a nudge row's seq it rates (signed
// callers, their own rows only). Anonymous feedback is allowed and charged
// like a report. The text is screened after commit and shown to the
// operator only once it passed; never served.
func (s *Store) feedback(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	about, err := s.feedbackAbout(c.Target)
	if err != nil {
		return Result{}, err
	}
	rating := c.Kind
	if rating != "" && rating != "useful" && rating != "not_useful" {
		return Result{}, problem(400, "invalid_rating", "kind is the rating: useful, not_useful, or omitted.")
	}
	if len(c.Text) > FeedbackTextBytes || !utf8.ValidString(c.Text) {
		return Result{}, problem(400, "field_limit", "Feedback text is at most "+itoa(FeedbackTextBytes)+" bytes of UTF-8.")
	}
	if rating == "" && strings.TrimSpace(c.Text) == "" {
		return Result{}, problem(400, "feedback_empty", "Send a rating (kind useful or not_useful), text, or both.")
	}
	subject, limit := a.account, FeedbackPerAccountPerDay
	if !a.signed {
		limit = FeedbackPerNetworkPerDay
		source, _ := ctx.Value(funnelSourceKey{}).(string)
		if _, net := s.signalHashes(source); net != "" {
			subject = "net:" + net
		}
	}
	if c.Amount < 0 || (c.Amount > 0 && !a.signed) {
		return Result{}, problem(400, "invalid_nudge", "amount names one of your own nudge rows (a signed caller's); omit it otherwise.")
	}
	var today int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM feedback WHERE subject=? AND created_at>=?", subject, now-now%86400).Scan(&today); err != nil {
		return Result{}, err
	}
	if today >= limit {
		return Result{}, (&Error{Status: 429, Code: "feedback_rate", Message: "That is today's feedback from you (" + itoa(limit) + " a day); thank you. More tomorrow (UTC)."}).until(nextDay(now), now)
	}
	if err = s.charge(ctx, tx, a, int64(len(c.Text)+512), now); err != nil {
		return Result{}, err
	}
	if c.Amount > 0 && rating != "" {
		r, err := tx.ExecContext(ctx, "UPDATE nudge_events SET rated=? WHERE seq=? AND subject=?", rating, c.Amount, subject)
		if err != nil {
			return Result{}, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return Result{}, problem(404, "nudge_not_found", "No nudge row of yours has that seq.")
		}
	}
	account, role, family := "", "", ""
	if a.signed {
		account = a.account
		_ = tx.QueryRowContext(ctx, "SELECT role,family FROM agent_funnel WHERE account=?", account).Scan(&role, &family)
	}
	screen := "pending"
	if strings.TrimSpace(c.Text) == "" {
		screen = "pass" // nothing to screen
	} else {
		// The day's screens across every sender are capped: past it the
		// text is kept, withheld and never screened (it costs nothing).
		var screened int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM feedback WHERE created_at>=? AND text<>'' AND screen<>'capped'", now-now%86400).Scan(&screened); err != nil {
			return Result{}, err
		}
		if screened >= FeedbackScreensPerDay {
			screen = "capped"
		}
	}
	id := randomID()
	if _, err = tx.ExecContext(ctx, "INSERT INTO feedback(id,subject,account,about,rating,text,nudge,screen,role,family,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		id, subject, account, about, rating, c.Text, c.Amount, screen, role, family, now); err != nil {
		return Result{}, err
	}
	res := Result{Data: map[string]any{"feedback_id": id, "status": "received", "about": about}}
	if screen == "pending" {
		text := c.Text
		res.afterCommit = func() (Result, error) {
			s.screenFeedback(id, text)
			return res, nil
		}
	}
	return res, nil
}

// feedbackAbout validates about: nudge:CODE (a NudgeCodes or RetiredNudges row),
// feature:NAME (an operation or an enabled service), or general.
func (s *Store) feedbackAbout(about string) (string, error) {
	bad := problem(400, "invalid_about", "target is what the feedback is about: nudge:CODE (a code /capabilities funnel.nudges or funnel.retired_nudges lists), feature:NAME (an operation or service id) or general.")
	switch {
	case about == "general":
		return about, nil
	case strings.HasPrefix(about, "nudge:"):
		code := strings.TrimPrefix(about, "nudge:")
		if _, ok := LookupNudge(code); ok {
			return about, nil
		}
		if _, ok := LookupRetiredNudge(code); ok {
			return about, nil // a tip shown before it was retired
		}
	case strings.HasPrefix(about, "feature:"):
		name := strings.TrimPrefix(about, "feature:")
		if _, ok := LookupOperation(name); ok {
			return about, nil
		}
		if e := s.services.engine; e != nil && validServiceKey(name) {
			if _, err := e.Registry().Lookup(name); err == nil {
				return about, nil
			}
		}
	}
	return "", bad
}

// screenFeedback screens a feedback's text once, in the background, as an
// offering call's input is: pass, flag, or unscreened without a screener
// or when the screen failed (the maintenance retries those).
func (s *Store) screenFeedback(id, text string) {
	if s.convScreen.screener == nil {
		s.storeFeedbackScreen(id, messageScreen{state: "unscreened"}, false)
		return
	}
	s.funnel.screens.Add(1)
	go func() {
		defer s.funnel.screens.Done()
		s.screenFeedbackNow(id, text)
	}()
}

// screenFeedbackNow screens one text and stores the outcome. The screen's
// deadline is its own: an UPDATE after a slow screen still lands.
func (s *Store) screenFeedbackNow(id, text string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("feedback screen panicked", "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), convScreenTimeout)
	m := s.screenText(ctx, text, payerSwarmMemo)
	cancel()
	s.storeFeedbackScreen(id, m, true)
}

// feedbackStoreTimeout bounds the UPDATE that records a screen.
const feedbackStoreTimeout = 10 * time.Second

// storeFeedbackScreen records a screen on a row still pending or
// unscreened; tried counts it toward FeedbackScreenTries.
func (s *Store) storeFeedbackScreen(id string, m messageScreen, tried bool) {
	scores := ""
	if m.scores != nil {
		raw, _ := json.Marshal(m.scores)
		scores = string(raw)
	}
	try := 0
	if tried {
		try = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedbackStoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, "UPDATE feedback SET screen=?,scores=?,screen_tries=screen_tries+? WHERE id=? AND screen IN ('pending','unscreened')", m.state, scores, try, id); err != nil {
		slog.Warn("feedback screen not stored; the maintenance retries it", "error", err)
	}
}

// feedbackRetryAge is how long, in seconds, a pending row is left to its own
// screen before the maintenance takes it as stuck.
const feedbackRetryAge = 10 * 60

// RetryFeedbackScreens re-screens, in the background, the texts whose screen
// failed, timed out or never stored (pending past feedbackRetryAge): at most
// FeedbackScreenRetries a run, FeedbackScreenTries each, while younger than
// FeedbackScreenRetryHours. Without a screener it does nothing. It answers
// how many it queued.
func (s *Store) RetryFeedbackScreens(ctx context.Context) (int, error) {
	if s.convScreen.screener == nil || !s.funnel.retrying.CompareAndSwap(false, true) {
		return 0, nil
	}
	now := s.now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT id,text FROM feedback WHERE created_at>=? AND text<>'' AND screen_tries<?
 AND (screen='unscreened' OR (screen='pending' AND created_at<=?)) ORDER BY created_at LIMIT ?`,
		now-FeedbackScreenRetryHours*3600, FeedbackScreenTries, now-feedbackRetryAge, FeedbackScreenRetries)
	if err != nil {
		s.funnel.retrying.Store(false)
		return 0, err
	}
	type item struct{ id, text string }
	var list []item
	for rows.Next() {
		var it item
		if err = rows.Scan(&it.id, &it.text); err != nil {
			rows.Close()
			s.funnel.retrying.Store(false)
			return 0, err
		}
		list = append(list, it)
	}
	if err = rows.Close(); err != nil || len(list) == 0 {
		s.funnel.retrying.Store(false)
		return 0, err
	}
	s.funnel.screens.Add(1)
	go func() {
		defer s.funnel.screens.Done()
		defer s.funnel.retrying.Store(false)
		for _, it := range list {
			s.screenFeedbackNow(it.id, it.text)
		}
	}()
	return len(list), nil
}

// FeedbackItem is one feedback for the operator. Text is set only when its
// screen passed; otherwise Scores say why it is withheld.
type FeedbackItem struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	About     string `json:"about"`
	Rating    string `json:"rating,omitempty"`
	Text      string `json:"text,omitempty"`
	Screen    string `json:"screen"`
	Scores    string `json:"scores,omitempty"`
	Role      string `json:"role,omitempty"`
	Family    string `json:"family,omitempty"`
	CreatedAt int64  `json:"created_at"`
	Withheld  bool   `json:"withheld,omitempty"`
}

// FeedbackList is the newest feedback, at most limit, for the operator:
// the text of a row only when its screen passed (the injection guard).
func (s *Store) FeedbackList(ctx context.Context, limit int) ([]FeedbackItem, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,subject,about,rating,text,screen,scores,role,family,created_at FROM feedback ORDER BY created_at DESC, id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedbackItem
	for rows.Next() {
		var f FeedbackItem
		if err = rows.Scan(&f.ID, &f.Subject, &f.About, &f.Rating, &f.Text, &f.Screen, &f.Scores, &f.Role, &f.Family, &f.CreatedAt); err != nil {
			return nil, err
		}
		if f.Screen != "pass" && f.Text != "" {
			f.Text, f.Withheld = "", true
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// WaitFeedbackScreens waits for the feedback screens in flight (tests,
// shutdown: before the database closes, so a screen paid for is stored).
func (s *Store) WaitFeedbackScreens() { s.funnel.screens.Wait() }

// eraseFeedbackText blanks the text of an account's feedback for an owner
// erasure (erase.go, C1012); ratings and screens stay.
func eraseFeedbackText(ctx context.Context, tx *sql.Tx, account string) (int64, error) {
	res, err := tx.ExecContext(ctx, "UPDATE feedback SET text='' WHERE account IN "+eraseOwn+" AND text<>''", account)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
