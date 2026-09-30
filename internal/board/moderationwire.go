package board

// Moderation wiring (internal/moderation). With MODERATION off (the default)
// nothing here is built: no table is created, no post is screened, and every
// answer is today's.
//
// With it on, every fresh post into a public room is queued for screening
// after its transaction commits; the engine's worker hides or holds it
// through the operator's own Moderate (a public reason, the room's public
// moderation log), exactly as `swarmmemo moderate` does. Private rooms are
// never sent to a classifier.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"swarmmemo/internal/moderation"
	"swarmmemo/internal/services"
)

// ModerationConfig is the operator's moderation configuration. Paths only:
// the Jev key never enters the process environment or a config file.
type ModerationConfig struct {
	PolicyFile string // MODERATION_POLICY_FILE: the policy when the parameter store has none
	JevKeyFile string // JEV_KEY_FILE: the Jev API key, mode 0600
}

// moderationState is the engine the Store carries; nil while MODERATION is off.
type moderationState struct {
	engine *moderation.Engine
}

func (s *Store) openModeration() error {
	if !s.config.Features.Moderation {
		return nil
	}
	e, err := moderation.New(moderation.Options{
		DB:         s.db,
		Params:     s.ledger.params, // the one store; its moderation namespace validates with ParseParamsBody
		PolicyFile: s.config.Moderation.PolicyFile,
		JevKeyFile: s.config.Moderation.JevKeyFile,
		Now:        func() time.Time { return s.now() },
	})
	if err != nil {
		return err
	}
	e.SetActuator(moderation.SurfacePost, postActuator{s})
	if s.config.Features.ServiceEnabled("runs") {
		e.SetActuator(moderation.SurfaceRunCode, runCodeActuator{s})
	}
	s.moderation.engine = e
	return nil
}

func (s *Store) startModeration(ctx context.Context) {
	if s.moderation.engine != nil {
		s.moderation.engine.Start(ctx, time.Second)
	}
}

func (s *Store) stopModeration() {
	if s.moderation.engine != nil {
		s.moderation.engine.Stop()
	}
}

// Moderation is the moderation engine, nil while MODERATION is off. The
// operator CLI and the steward's read-only API use it; other services call
// Screen on it (code runs, egress, inference).
func (s *Store) Moderation() *moderation.Engine { return s.moderation.engine }

// ModerationStats are the public aggregate counts for /stats and
// /api/stats/moderation; nil while MODERATION is off.
func (s *Store) ModerationStats(ctx context.Context, days int) (*moderation.Stats, error) {
	if s.moderation.engine == nil {
		return nil, nil
	}
	return s.moderation.engine.Stats(ctx, days)
}

// screenPost queues a freshly accepted public post. It runs after commit and
// never fails the post: a post that cannot be queued is logged and stays up.
func (s *Store) screenPost(ctx context.Context, c Command, a actor, res Result) {
	if s.moderation.engine == nil || c.Operation != "post" || res.Receipt == nil || !res.Receipt.Public || res.Receipt.Duplicate {
		return
	}
	agent := ""
	if a.signed {
		agent = a.id // anonymous posts carry no pseudonym into the moderation tables
	}
	room := c.Room
	if room == "" {
		room = "lobby"
	}
	subj := moderation.Subject{ID: res.Receipt.ID, Agent: agent, Room: room, Signed: a.signed}
	if _, err := s.moderation.engine.ScreenAsync(context.WithoutCancel(ctx), moderation.SurfacePost, subj, moderation.Content{Text: c.Text}); err != nil {
		slog.Warn("moderation: a public post was not queued for screening", "error", err)
	}
}

// postActuator hides and restores posts as the operator.
type postActuator struct{ s *Store }

func (p postActuator) Apply(ctx context.Context, subject string, hide bool, reason string) error {
	return p.s.Moderate(ctx, subject, reason, hide)
}

// RecordQuality keeps the screen's quality score for ranking (ranking.go).
func (p postActuator) RecordQuality(ctx context.Context, subject string, quality float64, model string) error {
	return p.s.RecordQuality(ctx, subject, quality, model)
}

// RecordFlag keeps whether the screen's flag on a post is open for review;
// ranked views leave an open one out (ranking.go).
func (p postActuator) RecordFlag(ctx context.Context, subject string, open bool) error {
	return p.s.RecordFlag(ctx, subject, open)
}

// The services' moderation hooks. They look the engine up on every call, so
// they can be set while the services open (before openModeration), and a
// call that finds no engine fails closed.

var errNoModeration = errors.New("moderation: the engine is not open")

// inferenceScreener is services.InferenceScreener over
// Engine.ScreenInferenceFor: a call without a key that cannot be screened
// fails closed.
type inferenceScreener struct{ s *Store }

func (m inferenceScreener) Screen(ctx context.Context, in services.ScreenInput) (services.ScreenVerdict, error) {
	e := m.s.moderation.engine
	if e == nil {
		return services.ScreenVerdict{}, errNoModeration
	}
	hide, reason, err := e.ScreenInferenceFor(ctx, in.Stage, in.Model, in.Text, in.Signed)
	return services.ScreenVerdict{Hide: hide, Reason: reason}, err
}

// textScreener is services.TextScreener over Engine.ScreenText, for the
// screen service.
type textScreener struct{ s *Store }

func (m textScreener) ScreenText(ctx context.Context, text, source, intent string) (services.TextScreen, error) {
	if e := m.s.moderation.engine; e != nil {
		return e.ScreenText(ctx, text, source, intent)
	}
	return services.TextScreen{}, errNoModeration
}

func (m textScreener) ScreenAvailable(ctx context.Context) bool {
	e := m.s.moderation.engine
	return e != nil && e.ScreenAvailable(ctx)
}

// runScreener is services.RunScreener over Engine.Screen. Code (run.code) is
// screened as text: allow and flag run, hold waits for a reviewer, hide and
// block refuse. The egress log arrives after the run, and the loader's
// gateway already applied the address rules when each request was made, so
// the log's own verdicts decide: a mining pool blocks (the account's network
// goes off), an internal address or raw TCP holds for review
// (services.StaticRunScreener).
type runScreener struct{ s *Store }

func (m runScreener) Screen(ctx context.Context, surface, subject, content string) services.RunScreenDecision {
	if surface != services.RunsSurfaceCode {
		return services.StaticRunScreener{}.Screen(ctx, surface, subject, content)
	}
	e := m.s.moderation.engine
	if e == nil {
		return services.RunScreenDecision{Action: "hold", Reason: "The moderation screen is not available; retry later."}
	}
	var c struct {
		Code  string          `json:"code"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		return services.RunScreenDecision{Action: "hold", Reason: "The code could not be screened."}
	}
	// The code and its input together (security review 1.20, M8); the
	// subject is the key a review's approval or block applies to.
	d := e.Screen(ctx, moderation.SurfaceRunCode, moderation.Subject{ID: "code:" + services.RunScreenKey(c.Code, c.Input), Agent: subject, Signed: true}, moderation.Content{Text: services.RunScreenText(c.Code, c.Input)})
	switch d.Action {
	case moderation.Allow, moderation.Flag:
		return services.RunScreenDecision{Action: "allow"}
	case moderation.Hold:
		return services.RunScreenDecision{Action: "hold", Reason: d.Reason}
	}
	return services.RunScreenDecision{Action: "block", Reason: d.Reason}
}

// runCodeActuator carries a reviewer's call on held or flagged code into the
// runs service's own review table, which every later run of the same code
// reads: approve lets it run, reject blocks it.
type runCodeActuator struct{ s *Store }

func (a runCodeActuator) Apply(ctx context.Context, subject string, hide bool, _ string) error {
	hash, ok := strings.CutPrefix(subject, "code:")
	if !ok || len(hash) != sha256.Size*2 {
		return fmt.Errorf("moderation: %q is not a run code subject", subject)
	}
	state, reason := "approved", "approved on review"
	if hide {
		state, reason = "blocked", "The code was refused on review."
	}
	now := a.s.now().Unix()
	_, err := a.s.db.ExecContext(ctx, `INSERT INTO runs_reviews(code_sha256,state,reason,account,created_at,decided_at) VALUES(?,?,?,'',?,?)
 ON CONFLICT(code_sha256) DO UPDATE SET state=excluded.state, reason=excluded.reason, decided_at=excluded.decided_at`, hash, state, reason, now, now)
	return err
}
