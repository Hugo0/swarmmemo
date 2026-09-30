package moderation

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

// Async screening: a durable job per subject, so a restart loses nothing.
// Posts use it: the post is accepted at once, and the worker screens it and
// applies hide or hold through the surface's Actuator.

const (
	jobContentBytes = 256 << 10
	jobAttemptsMax  = 5
	jobLease        = 2 * time.Minute
)

// ScreenAsync queues content on surface s for screening and returns the job
// ID. The decision is logged and applied by the worker (Start, or Work).
func (e *Engine) ScreenAsync(ctx context.Context, s Surface, subj Subject, c Content) (string, error) {
	if _, ok := surfaceSpec(s); !ok {
		return "", errors.New("moderation: unknown surface")
	}
	if c.Egress != nil {
		return "", errors.New("moderation: egress is screened synchronously")
	}
	id := newID()
	_, err := e.db.ExecContext(ctx, "INSERT INTO moderation_jobs(id,surface,subject,agent,room,signed,content,state,created_at) VALUES(?,?,?,?,?,?,?,'pending',?)",
		id, string(s), bound(subj.ID, 128), bound(subj.Agent, 128), bound(subj.Room, 256), subj.Signed, truncateUTF8(c.Text, jobContentBytes), e.now().Unix())
	if err != nil {
		return "", err
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return id, nil
}

// Start runs the async workers until ctx ends or Stop is called.
func (e *Engine) Start(ctx context.Context, poll time.Duration) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	ctx, e.cancel = context.WithCancel(ctx)
	e.running = true
	e.mu.Unlock()
	if poll <= 0 {
		poll = time.Second
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(policyReloadEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = e.ReloadPolicy(ctx)
			}
		}
	}()
	for i := 0; i < e.opts.Workers; i++ {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			ticker := time.NewTicker(poll)
			defer ticker.Stop()
			for {
				for {
					did, err := e.workOne(ctx)
					if err != nil && ctx.Err() == nil {
						slog.Warn("moderation: async screen failed; it will be retried", "error", err)
					}
					if !did || ctx.Err() != nil {
						break
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-e.wake:
				}
			}
		}()
	}
}

// ReloadPolicy rereads the policy in force now (the parameter store, else the
// file) and swaps it in; on an error the previous policy stays. It reads on
// the pool, so it must not run inside a transaction holding the connection.
// Start runs it every policyReloadEvery.
func (e *Engine) ReloadPolicy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return e.policies.refresh(ctx)
}

// Stop ends the workers and waits for the job in hand to finish.
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.running = false
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
}

// Work screens every due job now and reports how many it handled.
func (e *Engine) Work(ctx context.Context) (int, error) {
	n := 0
	for {
		did, err := e.workOne(ctx)
		if err != nil {
			return n, err
		}
		if !did {
			return n, nil
		}
		n++
	}
}

type job struct {
	id, surface, subject, agent, room, content, decision string
	signed                                               bool
	attempts                                             int
}

// workOne leases one due job, screens it (once: a retry reuses the logged
// decision) and applies the decision.
func (e *Engine) workOne(ctx context.Context) (bool, error) {
	now := e.now().Unix()
	var j job
	err := e.db.QueryRowContext(ctx, `UPDATE moderation_jobs SET state='running', attempts=attempts+1, leased_until=?
		WHERE id=(SELECT id FROM moderation_jobs WHERE (state='pending' AND leased_until<=?) OR (state='running' AND leased_until<?) ORDER BY created_at, id LIMIT 1)
		RETURNING id,surface,subject,agent,room,signed,content,decision_id,attempts`, now+int64(jobLease/time.Second), now, now).
		Scan(&j.id, &j.surface, &j.subject, &j.agent, &j.room, &j.signed, &j.content, &j.decision, &j.attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s := Surface(j.surface)
	var d Decision
	if j.decision != "" {
		d, err = e.Decision(ctx, j.decision)
	} else {
		d = e.Screen(ctx, s, Subject{ID: j.subject, Agent: j.agent, Room: j.room, Signed: j.signed}, Content{Text: j.content})
		if d.ID == "" {
			err = errors.New("decision not recorded")
		} else {
			_, err = e.db.ExecContext(ctx, "UPDATE moderation_jobs SET decision_id=? WHERE id=?", d.ID, j.id)
		}
	}
	if err == nil {
		err = e.apply(ctx, d)
	}
	if err != nil {
		state, retryAt := "pending", now+int64(j.attempts*j.attempts)*30
		if j.attempts >= jobAttemptsMax {
			state = "failed"
			e.raise(Alert{Kind: "job", Surface: s, Detail: "an async screen failed " + "five times and was dropped; its decision, if any, is in the log", At: now})
		}
		_, _ = e.db.ExecContext(ctx, "UPDATE moderation_jobs SET state=?, leased_until=? WHERE id=?", state, retryAt, j.id)
		return true, err
	}
	// The content was a working copy; the decision keeps its hash and size.
	_, err = e.db.ExecContext(ctx, "UPDATE moderation_jobs SET state='done', content='' WHERE id=?", j.id)
	return true, err
}

// apply makes a hide or a hold take effect on a surface with an Actuator.
func (e *Engine) apply(ctx context.Context, d Decision) error {
	act := e.actuator(d.Surface)
	if act == nil || (d.Action != Hide && d.Action != Hold) {
		return nil
	}
	return act.Apply(ctx, d.Subject, true, d.Reason)
}
