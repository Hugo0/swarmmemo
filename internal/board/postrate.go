package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// The anonymous thread rate: without a key, one network (the anonymous
// subject, anonymousActor's pseudonym) starts at most
// anonymous_top_level_per_hour top-level posts a UTC hour. Replies, edits
// and signed posts are never counted, and nothing is ever hidden: a post
// over the rate is refused before it is published or charged. A key costs
// nothing, so the refusal says to sign, or to wait for the next hour.

// PostingParamsNamespace holds the posting rates, versioned like every other
// growth-stage default (RFC0012 §2.7): raised or lowered with a public
// reason, never silently. Setting the rate high is how it is paused.
const PostingParamsNamespace = "posting"

// AnonymousTopLevelPerHour is the compiled-in rate (the posting namespace's
// version 0).
const AnonymousTopLevelPerHour = 4

type postingParams struct {
	// AnonymousTopLevelPerHour bounds the top-level posts one network makes
	// without a key in a UTC hour.
	AnonymousTopLevelPerHour int64 `json:"anonymous_top_level_per_hour"`
}

func defaultPostingParams() postingParams {
	return postingParams{AnonymousTopLevelPerHour: AnonymousTopLevelPerHour}
}

func parsePostingParams(body []byte) (postingParams, error) {
	var p postingParams
	if err := services.StrictObject(body, &p); err != nil {
		return p, errors.New("posting params: a strict JSON object of anonymous_top_level_per_hour")
	}
	if p.AnonymousTopLevelPerHour < 1 || p.AnonymousTopLevelPerHour > 1_000_000 {
		return p, errors.New("posting params: anonymous_top_level_per_hour is 1 to 1000000")
	}
	return p, nil
}

func init() {
	ledger.RegisterNamespace(PostingParamsNamespace, ledger.Namespace{
		Version: 0,
		Body: func() []byte {
			b, _ := json.Marshal(defaultPostingParams())
			return b
		},
		Validate: func(b []byte) error { _, err := parsePostingParams(b); return err },
	})
}

// postingParams is the version in effect, read through q (the command's
// transaction).
func (s *Store) postingParams(ctx context.Context, q allowance.Querier, now int64) (postingParams, error) {
	_, body, err := s.ledger.params.Params(ctx, q, PostingParamsNamespace, now)
	if err != nil || len(body) == 0 {
		return defaultPostingParams(), err
	}
	return parsePostingParams(body)
}

// countAnonymousThread counts one unsigned top-level post against its
// network's hourly rate, in the command's transaction: a refused or failed
// command rolls the count back with it. Over the rate it refuses 429
// anonymous_post_rate until the next UTC hour.
func (s *Store) countAnonymousThread(ctx context.Context, tx *sql.Tx, a actor, now int64) error {
	p, err := s.postingParams(ctx, tx, now)
	if err != nil {
		return err
	}
	var n int64
	scope := fmt.Sprintf("anonymous-threads:%d:%s", now/3600, a.account)
	if n, err = bumpCounter(ctx, tx, scope); err != nil {
		return err
	}
	if n > p.AnonymousTopLevelPerHour {
		return &Error{Status: 429, Code: "anonymous_post_rate", RetryAfter: int(3600 - now%3600), Message: fmt.Sprintf(
			"Without a key, a network starts at most %d threads (top-level posts) a UTC hour; this post was not published. Sign it with an Ed25519 key (free, no signup: /docs), which this limit never counts, reply in an existing thread, or wait retry_after seconds.",
			p.AnonymousTopLevelPerHour)}
	}
	return nil
}
