package moderation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Schema is the engine's tables. New applies it, so none exists while
// MODERATION is off. Every statement only creates if absent.
const Schema = `
CREATE TABLE IF NOT EXISTS moderation_decisions (
 id TEXT PRIMARY KEY, surface TEXT NOT NULL, subject TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '',
 action TEXT NOT NULL, proposed TEXT NOT NULL, category TEXT NOT NULL DEFAULT '', p REAL NOT NULL DEFAULT 0,
 scores TEXT NOT NULL DEFAULT '{}', policy_version INTEGER NOT NULL, policy_sha256 TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', burst INTEGER NOT NULL DEFAULT 0,
 hard INTEGER NOT NULL DEFAULT 0, degraded TEXT NOT NULL DEFAULT '', content_sha256 TEXT NOT NULL DEFAULT '',
 content_bytes INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS moderation_decisions_surface ON moderation_decisions(surface,created_at);
CREATE INDEX IF NOT EXISTS moderation_decisions_agent ON moderation_decisions(surface,agent,created_at);
CREATE INDEX IF NOT EXISTS moderation_decisions_subject ON moderation_decisions(subject);
CREATE INDEX IF NOT EXISTS moderation_decisions_day ON moderation_decisions(created_at);
CREATE TABLE IF NOT EXISTS moderation_queue (
 id TEXT PRIMARY KEY, decision_id TEXT NOT NULL UNIQUE REFERENCES moderation_decisions(id),
 surface TEXT NOT NULL, subject TEXT NOT NULL, cause TEXT NOT NULL CHECK(cause IN ('flag','hold','burst')),
 state TEXT NOT NULL CHECK(state IN ('pending','approved','rejected')), content TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0, resolved_by TEXT NOT NULL DEFAULT '',
 note TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS moderation_queue_pending ON moderation_queue(created_at) WHERE state='pending';
CREATE INDEX IF NOT EXISTS moderation_queue_subject ON moderation_queue(subject);
CREATE TABLE IF NOT EXISTS moderation_jobs (
 id TEXT PRIMARY KEY, surface TEXT NOT NULL, subject TEXT NOT NULL, agent TEXT NOT NULL DEFAULT '',
 room TEXT NOT NULL DEFAULT '', signed INTEGER NOT NULL DEFAULT 0, content TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('pending','running','done','failed')), attempts INTEGER NOT NULL DEFAULT 0,
 leased_until INTEGER NOT NULL DEFAULT 0, decision_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS moderation_jobs_due ON moderation_jobs(created_at) WHERE state IN ('pending','running');
CREATE TABLE IF NOT EXISTS moderation_spend (
 day INTEGER PRIMARY KEY, spent_microusd INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS moderation_screen_spend (
 day INTEGER PRIMARY KEY, spent_microusd INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0,
 tokens INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS moderation_domains (
 site TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('seen','allowed','denied')),
 first_seen INTEGER NOT NULL, first_agent TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS moderation_alerts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, surface TEXT NOT NULL DEFAULT '',
 detail TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS moderation_alerts_kind ON moderation_alerts(kind,surface,created_at);
`

// QueueContentBytes bounds the private copy of content a queue item keeps
// for its reviewer.
const QueueContentBytes = 16 << 10

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func contentKey(c Content) string {
	if c.Egress != nil {
		ips := make([]string, 0, len(c.Egress.IPs))
		for _, ip := range c.Egress.IPs {
			ips = append(ips, ip.String())
		}
		return fmt.Sprintf("%s:%d [%s]", c.Egress.Host, c.Egress.Port, strings.Join(ips, " "))
	}
	return c.Text
}

// record stores the decision and, for a flag or a hold, its queue item.
func (e *Engine) record(ctx context.Context, d Decision, c Content, hard bool) error {
	pol, _ := e.policies.load(ctx)
	scores, _ := json.Marshal(d.Scores)
	text := contentKey(c)
	sum := sha256.Sum256([]byte(text))
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO moderation_decisions(id,surface,subject,agent,action,proposed,category,p,scores,policy_version,policy_sha256,model,reason,burst,hard,degraded,content_sha256,content_bytes,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, string(d.Surface), d.Subject, d.Agent, string(d.Action), string(d.Proposed), d.Category, d.P, string(scores), d.PolicyVersion, pol.SHA256, d.Model, d.Reason, d.Burst, hard, d.Degraded, hex.EncodeToString(sum[:]), len(text), d.CreatedAt); err != nil {
		return err
	}
	if d.Queued {
		cause := string(d.Action)
		if d.Burst {
			cause = "burst"
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO moderation_queue(id,decision_id,surface,subject,cause,state,content,created_at) VALUES(?,?,?,?,?,'pending',?,?)",
			newID(), d.ID, string(d.Surface), d.Subject, cause, truncateUTF8(text, QueueContentBytes), d.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// burst counts this surface's earlier would-be hides and blocks in category
// within the window. When this one is the first past the limit it alerts,
// and queues the earlier ones in the category (already applied) for review.
// repeat reports whether agent itself had one of them: the burst never
// softens a decision for an author who helped trip it.
func (e *Engine) burst(ctx context.Context, s Surface, b *Burst, category, agent string, now int64) (tripped, repeat bool, err error) {
	since := now - b.WindowSeconds
	var n, mine int
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(agent<>'' AND agent=?),0) FROM moderation_decisions WHERE surface=? AND category=? AND created_at>? AND proposed IN ('hide','block') AND hard=0",
		agent, string(s), category, since).Scan(&n, &mine); err != nil {
		return false, false, err
	}
	if n+1 <= b.Max {
		return false, false, nil
	}
	if n == b.Max {
		if _, err := e.db.ExecContext(ctx, `INSERT INTO moderation_queue(id,decision_id,surface,subject,cause,state,created_at)
			SELECT lower(hex(randomblob(16))),d.id,d.surface,d.subject,'burst','pending',? FROM moderation_decisions d
			WHERE d.surface=? AND d.category=? AND d.created_at>? AND d.proposed IN ('hide','block') AND d.hard=0
			AND NOT EXISTS(SELECT 1 FROM moderation_queue q WHERE q.decision_id=d.id)`, now, string(s), category, since); err != nil {
			return true, mine > 0, err
		}
	}
	e.alertOnce(ctx, Alert{Kind: "burst", Surface: s, Detail: fmt.Sprintf("more than %d hides or blocks for %s on %s within %d s; mode %s: further ones in that category below p=%.2f are flagged for review", b.Max, category, s, b.WindowSeconds, b.Mode, burstKeepAt), At: now}, since)
	return true, mine > 0, nil
}

// raise logs, stores and forwards an alert.
func (e *Engine) raise(a Alert) {
	if a.At == 0 {
		a.At = e.now().Unix()
	}
	slog.Error("moderation alert", "kind", a.Kind, "surface", string(a.Surface), "detail", a.Detail)
	if _, err := e.db.Exec("INSERT INTO moderation_alerts(kind,surface,detail,created_at) VALUES(?,?,?,?)", a.Kind, string(a.Surface), bound(a.Detail, 512), a.At); err != nil {
		slog.Error("moderation alert not stored", "error", err)
	}
	if e.opts.Alert != nil {
		e.opts.Alert(a)
	}
}

// alertOnce raises a unless one of its kind and surface was raised since.
func (e *Engine) alertOnce(ctx context.Context, a Alert, since int64) {
	var n int
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM moderation_alerts WHERE kind=? AND surface=? AND detail=? AND created_at>=?", a.Kind, string(a.Surface), bound(a.Detail, 512), since).Scan(&n); err == nil && n > 0 {
		return
	}
	e.raise(a)
}

// Alerts lists the most recent alerts, newest first.
func (e *Engine) Alerts(ctx context.Context, limit int) ([]Alert, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	rows, err := e.db.QueryContext(ctx, "SELECT kind,surface,detail,created_at FROM moderation_alerts ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		var s string
		if err := rows.Scan(&a.Kind, &s, &a.Detail, &a.At); err != nil {
			return nil, err
		}
		a.Surface = Surface(s)
		out = append(out, a)
	}
	return out, rows.Err()
}

// reserveSpend adds estimate to the day's spend if it stays within the cap.
// The reservation is durable, so a crash never forgets money spent. A screen
// service call is also added to the screen's own row, within its sub-cap, in
// the same transaction: screening never spends past either, and board
// moderation always keeps the cap less the sub-cap.
func (e *Engine) reserveSpend(ctx context.Context, day, estimate int64, j JevPolicy, screen bool) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = reserveIn(ctx, tx, "moderation_spend", day, estimate, j.DailySpendCapMicroUSD); err == nil && screen {
		err = reserveIn(ctx, tx, "moderation_screen_spend", day, estimate, j.ScreenDailySpendCapMicroUSD)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// reserveIn adds estimate to table's row for day if it stays within cap.
// table is one of the two spend tables, never input.
func reserveIn(ctx context.Context, tx *sql.Tx, table string, day, estimate, cap int64) error {
	if cap <= 0 || estimate > cap {
		return errSpendCap
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(day,spent_microusd,calls,tokens) VALUES(?,?,0,0)
		ON CONFLICT(day) DO UPDATE SET spent_microusd=spent_microusd+excluded.spent_microusd WHERE spent_microusd+excluded.spent_microusd<=?`, day, estimate, cap)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errSpendCap
	}
	return nil
}

// settleSpend corrects the day's spend by delta (the actual cost minus the
// reservation, or minus the reservation for a call that failed), and the
// screen's row too for a screen call.
func (e *Engine) settleSpend(ctx context.Context, day, delta, tokens int64, called, screen bool) {
	calls := 0
	if called {
		calls = 1
	}
	tables := []string{"moderation_spend"}
	if screen {
		tables = append(tables, "moderation_screen_spend")
	}
	for _, table := range tables {
		if _, err := e.db.ExecContext(ctx, "UPDATE "+table+" SET spent_microusd=max(0,spent_microusd+?),calls=calls+?,tokens=tokens+? WHERE day=?", delta, calls, tokens, day); err != nil {
			slog.Error("moderation: spend not settled", "error", err)
		}
	}
}

// Spend is a day's Jev spend.
type Spend struct {
	Day           string `json:"day"`
	SpentMicroUSD int64  `json:"spent_microusd"`
	CapMicroUSD   int64  `json:"cap_microusd"`
	Calls         int64  `json:"calls"`
	Tokens        int64  `json:"tokens"`
}

// SpendToday is today's Jev spend against the cap.
func (e *Engine) SpendToday(ctx context.Context) (Spend, error) {
	now := e.now().UTC()
	day := now.Unix() / 86400
	pol := e.Policy(ctx)
	s := Spend{Day: now.Format("2006-01-02"), CapMicroUSD: pol.Jev.DailySpendCapMicroUSD}
	err := e.db.QueryRowContext(ctx, "SELECT spent_microusd,calls,tokens FROM moderation_spend WHERE day=?", day).Scan(&s.SpentMicroUSD, &s.Calls, &s.Tokens)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return s, err
}

// LogQuery selects decisions: newest first, at most Limit (1–500).
type LogQuery struct {
	Surface Surface
	Subject string
	Action  Action
	Before  int64 // created_at < Before; 0 is now
	Limit   int
}

const decisionColumns = "id,surface,subject,agent,action,proposed,category,p,scores,policy_version,model,reason,burst,degraded,created_at"

func scanDecision(sc interface{ Scan(...any) error }) (Decision, error) {
	var d Decision
	var surface, action, proposed, scores string
	if err := sc.Scan(&d.ID, &surface, &d.Subject, &d.Agent, &action, &proposed, &d.Category, &d.P, &scores, &d.PolicyVersion, &d.Model, &d.Reason, &d.Burst, &d.Degraded, &d.CreatedAt); err != nil {
		return d, err
	}
	d.Surface, d.Action, d.Proposed = Surface(surface), Action(action), Action(proposed)
	d.Scores = map[string]float64{}
	_ = json.Unmarshal([]byte(scores), &d.Scores)
	d.Queued = d.Action == Flag || d.Action == Hold
	return d, nil
}

// Log reads the decision log.
func (e *Engine) Log(ctx context.Context, q LogQuery) ([]Decision, error) {
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 50
	}
	if q.Before <= 0 {
		q.Before = 1 << 62
	}
	rows, err := e.db.QueryContext(ctx, "SELECT "+decisionColumns+" FROM moderation_decisions WHERE created_at<? AND (?='' OR surface=?) AND (?='' OR subject=?) AND (?='' OR action=?) ORDER BY created_at DESC, id DESC LIMIT ?",
		q.Before, string(q.Surface), string(q.Surface), q.Subject, q.Subject, string(q.Action), string(q.Action), q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Decision{}
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Decision reads one logged decision.
func (e *Engine) Decision(ctx context.Context, id string) (Decision, error) {
	return scanDecision(e.db.QueryRowContext(ctx, "SELECT "+decisionColumns+" FROM moderation_decisions WHERE id=?", id))
}

// QueueItem is one review item: the decision it came from and its state.
type QueueItem struct {
	ID           string   `json:"id"`
	Cause        string   `json:"cause"` // flag, hold or burst
	State        string   `json:"state"` // pending, approved or rejected
	CreatedAt    int64    `json:"created_at"`
	ResolvedAt   int64    `json:"resolved_at,omitempty"`
	ResolvedBy   string   `json:"resolved_by,omitempty"`
	Note         string   `json:"note,omitempty"`
	ContentBytes int      `json:"content_bytes"`
	Decision     Decision `json:"decision"`
}

// QueueQuery selects queue items, oldest first.
type QueueQuery struct {
	Surface Surface
	State   string // default pending; "all" for every state
	Limit   int
}

const queueColumns = "q.id,q.cause,q.state,q.created_at,q.resolved_at,q.resolved_by,q.note,length(q.content)"

func (e *Engine) scanQueue(sc interface{ Scan(...any) error }) (QueueItem, error) {
	var it QueueItem
	var d Decision
	var surface, action, proposed, scores string
	err := sc.Scan(&it.ID, &it.Cause, &it.State, &it.CreatedAt, &it.ResolvedAt, &it.ResolvedBy, &it.Note, &it.ContentBytes,
		&d.ID, &surface, &d.Subject, &d.Agent, &action, &proposed, &d.Category, &d.P, &scores, &d.PolicyVersion, &d.Model, &d.Reason, &d.Burst, &d.Degraded, &d.CreatedAt)
	if err != nil {
		return it, err
	}
	d.Surface, d.Action, d.Proposed = Surface(surface), Action(action), Action(proposed)
	d.Scores = map[string]float64{}
	_ = json.Unmarshal([]byte(scores), &d.Scores)
	d.Queued = true
	it.Decision = d
	return it, nil
}

var qdColumns = queueColumns + "," + strings.ReplaceAll("d."+decisionColumns, ",", ",d.")

// Queue lists review items.
func (e *Engine) Queue(ctx context.Context, q QueueQuery) ([]QueueItem, error) {
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 50
	}
	state := q.State
	if state == "" {
		state = "pending"
	}
	if state == "all" {
		state = ""
	}
	rows, err := e.db.QueryContext(ctx, "SELECT "+qdColumns+" FROM moderation_queue q JOIN moderation_decisions d ON d.id=q.decision_id WHERE (?='' OR q.state=?) AND (?='' OR q.surface=?) ORDER BY q.created_at, q.rowid LIMIT ?",
		state, state, string(q.Surface), string(q.Surface), q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueItem{}
	for rows.Next() {
		it, err := e.scanQueue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ErrNotFound is a queue item or decision that does not exist.
var ErrNotFound = errors.New("moderation: not found")

// ErrResolved is a queue item already approved or rejected.
var ErrResolved = errors.New("moderation: already resolved")

// Item reads one queue item and its private content copy.
func (e *Engine) Item(ctx context.Context, id string) (QueueItem, string, error) {
	var content string
	row := e.db.QueryRowContext(ctx, "SELECT "+qdColumns+",q.content FROM moderation_queue q JOIN moderation_decisions d ON d.id=q.decision_id WHERE q.id=?", id)
	var it QueueItem
	var d Decision
	var surface, action, proposed, scores string
	err := row.Scan(&it.ID, &it.Cause, &it.State, &it.CreatedAt, &it.ResolvedAt, &it.ResolvedBy, &it.Note, &it.ContentBytes,
		&d.ID, &surface, &d.Subject, &d.Agent, &action, &proposed, &d.Category, &d.P, &scores, &d.PolicyVersion, &d.Model, &d.Reason, &d.Burst, &d.Degraded, &d.CreatedAt, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return it, "", ErrNotFound
	}
	if err != nil {
		return it, "", err
	}
	d.Surface, d.Action, d.Proposed = Surface(surface), Action(action), Action(proposed)
	d.Scores = map[string]float64{}
	_ = json.Unmarshal([]byte(scores), &d.Scores)
	d.Queued = true
	it.Decision = d
	return it, content, nil
}

// Approve resolves a queue item in the content's favour: a hidden post is
// restored, a held run may proceed, a new domain is allowed.
func (e *Engine) Approve(ctx context.Context, id, by, note string) (QueueItem, error) {
	return e.resolve(ctx, id, by, note, true)
}

// Reject resolves a queue item against the content: a flagged post is
// hidden (a hidden one stays hidden with a reviewed reason), a held run is
// refused, a new domain is denied from then on.
func (e *Engine) Reject(ctx context.Context, id, by, note string) (QueueItem, error) {
	return e.resolve(ctx, id, by, note, false)
}

var noteRE = strings.NewReplacer("\n", " ", "\r", " ", "\x00", "")

func (e *Engine) resolve(ctx context.Context, id, by, note string, approve bool) (QueueItem, error) {
	it, content, err := e.Item(ctx, id)
	if err != nil {
		return it, err
	}
	if it.State != "pending" {
		return it, ErrResolved
	}
	d := it.Decision
	now := e.now().Unix()
	// The effect first: a failure leaves the item pending, to retry.
	if act := e.actuator(d.Surface); act != nil {
		shown := d.Action == Hide || d.Action == Hold
		label := d.Category
		pol := e.Policy(ctx)
		if label != "" {
			label = pol.surface(d.Surface).label(label)
		}
		switch {
		case approve && shown:
			err = act.Apply(ctx, d.Subject, false, fmt.Sprintf("review: restored; the auto-screen call (%s) was reversed (policy=v%d)", orNone(label), d.PolicyVersion))
		case !approve:
			err = act.Apply(ctx, d.Subject, true, fmt.Sprintf("review: hidden after human review: %s (p=%.2f, model=%s, policy=v%d); policy: hide only clearly malicious", orNone(label), d.P, orNone(d.Model), d.PolicyVersion))
		}
		if err != nil {
			return it, err
		}
	}
	if d.Surface == SurfaceRunEgress && d.Category == "new_domain" {
		if host := strings.SplitN(content, ":", 2)[0]; host != "" {
			if h, _, literal, bad := normalizeHost(host); bad == "" && !literal {
				state := "denied"
				if approve {
					state = "allowed"
				}
				if _, err := e.db.ExecContext(ctx, "INSERT INTO moderation_domains(site,state,first_seen,first_agent,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(site) DO UPDATE SET state=excluded.state,updated_at=excluded.updated_at", site(h), state, now, d.Agent, now); err != nil {
					return it, err
				}
			}
		}
	}
	state := "rejected"
	if approve {
		state = "approved"
	}
	res, err := e.db.ExecContext(ctx, "UPDATE moderation_queue SET state=?,resolved_at=?,resolved_by=?,note=? WHERE id=? AND state='pending'", state, now, bound(noteRE.Replace(by), 64), bound(noteRE.Replace(note), 512), id)
	if err != nil {
		return it, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return it, ErrResolved
	}
	it.State, it.ResolvedAt, it.ResolvedBy, it.Note = state, now, bound(by, 64), bound(note, 512)
	return it, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Status is a held subject's review state: "none" when nothing is queued for
// it, else pending, approved or rejected (the latest item). A run that was
// held proceeds on approved and is refused on rejected.
func (e *Engine) Status(ctx context.Context, s Surface, subject string) (string, error) {
	var state string
	err := e.db.QueryRowContext(ctx, "SELECT state FROM moderation_queue WHERE surface=? AND subject=? ORDER BY created_at DESC, rowid DESC LIMIT 1", string(s), subject).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "none", nil
	}
	return state, err
}

// Stats are the public aggregate counts behind /stats and
// /api/stats/moderation: decisions per day by action, and per surface. No
// subject, agent, category or content is in them.
type Stats struct {
	Days          int             `json:"days"`
	PolicyVersion int64           `json:"policy_version"`
	PendingReview int64           `json:"pending_review"`
	Reviewed      int64           `json:"reviewed"`
	Surfaces      []SurfaceCounts `json:"surfaces"`
	Daily         []DayCounts     `json:"daily"`
	Actions       []Action        `json:"actions"`
}

// Counts are decisions by action.
type Counts struct {
	Allow int64 `json:"allow"`
	Flag  int64 `json:"flag"`
	Hold  int64 `json:"hold"`
	Hide  int64 `json:"hide"`
	Block int64 `json:"block"`
}

func (c *Counts) add(a Action, n int64) {
	switch a {
	case Allow:
		c.Allow += n
	case Flag:
		c.Flag += n
	case Hold:
		c.Hold += n
	case Hide:
		c.Hide += n
	case Block:
		c.Block += n
	}
}

// Total is every decision counted.
func (c Counts) Total() int64 { return c.Allow + c.Flag + c.Hold + c.Hide + c.Block }

type SurfaceCounts struct {
	Surface Surface `json:"surface"`
	Counts
}

type DayCounts struct {
	Day string `json:"day"`
	Counts
}

// StatsDaysMax bounds the stats range.
const StatsDaysMax = 90

// Stats counts the last days UTC days (today included), oldest day first,
// surfaces in name order.
func (e *Engine) Stats(ctx context.Context, days int) (*Stats, error) {
	if days < 1 || days > StatsDaysMax {
		days = 7
	}
	now := e.now().UTC()
	today := now.Unix() / 86400
	first := today - int64(days) + 1
	st := &Stats{Days: days, PolicyVersion: e.Policy(ctx).Version, Actions: []Action{Allow, Flag, Hold, Hide, Block}, Surfaces: []SurfaceCounts{}}
	rows, err := e.db.QueryContext(ctx, "SELECT created_at/86400, surface, action, COUNT(*) FROM moderation_decisions WHERE created_at>=? GROUP BY 1,2,3", first*86400)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	perDay := map[int64]*Counts{}
	perSurface := map[Surface]*Counts{}
	for rows.Next() {
		var day, n int64
		var surface, action string
		if err := rows.Scan(&day, &surface, &action, &n); err != nil {
			return nil, err
		}
		if perDay[day] == nil {
			perDay[day] = &Counts{}
		}
		perDay[day].add(Action(action), n)
		if perSurface[Surface(surface)] == nil {
			perSurface[Surface(surface)] = &Counts{}
		}
		perSurface[Surface(surface)].add(Action(action), n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for d := first; d <= today; d++ {
		c := Counts{}
		if perDay[d] != nil {
			c = *perDay[d]
		}
		st.Daily = append(st.Daily, DayCounts{Day: time.Unix(d*86400, 0).UTC().Format("2006-01-02"), Counts: c})
	}
	for _, s := range Surfaces() {
		if c := perSurface[s]; c != nil {
			st.Surfaces = append(st.Surfaces, SurfaceCounts{Surface: s, Counts: *c})
		}
	}
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FILTER (WHERE state='pending'), COUNT(*) FILTER (WHERE state<>'pending' AND resolved_at>=?) FROM moderation_queue", first*86400).Scan(&st.PendingReview, &st.Reviewed); err != nil {
		return nil, err
	}
	return st, nil
}
