package board

// Machine-readable waits (C596). A polling agent's question on a refusal is
// "am I early?". Where the board computes when a refusal lifts by itself (a
// UTC day or hour, a rate window's end, a rolling day, a seat's verdict
// deadline, a reply's repeat window), the error carries next_open_at (unix
// seconds) and retry_after (the seconds until it), and the HTTP answer the
// Retry-After header. A retry hint (busy, a provider still settling) has
// retry_after only; a refusal no clock lifts (a full memory, a count of open
// items, a total) has neither.

import "swarmmemo/internal/allowance"

// WaitCodes are the codes whose refusal carries next_open_at when the
// board computes it: /capabilities polling.waits and PROTOCOL's errors
// section list them from here.
var WaitCodes = []string{
	"anonymous_post_rate", "bond_cooling", "challenge_rate", "duplicate_reply", "feedback_rate",
	"fetch_caller_limit", "fetch_host_busy", "fetch_host_limit", "global_quota_exhausted", "hold_limit",
	"hosted_issuance_limit", "key_backup_rate_limited", "notary_limit", "offering_publish_limit", "paste_limit",
	"quota_exhausted", "receiver_quota_exhausted", "recipient_limit", "repeat_promotion", "request_limit", "request_rate",
	"review_daily_limit", "seat_limit", "seats_full", "spend_limit", "top_level_daily_limit",
	"topup_board_daily_limit", "topup_daily_limit", "verification_limit", "witness_limit", "x402_cap_reached",
	"x402_listing_limit", "x402_response_too_large",
}

// until makes e lift by itself at next, seen at now: next_open_at next and
// retry_after the seconds until it. A next not after now leaves e unchanged.
func (e *Error) until(next, now int64) *Error {
	if next > now {
		e.NextOpenAt, e.RetryAfter = next, int(next-now)
	}
	return e
}

// waitOf copies an allowance refusal's wait onto e: its retry_after, and
// its next_open_at when the refusal computed one.
func (e *Error) waitOf(a *allowance.Err) *Error {
	e.RetryAfter, e.NextOpenAt = a.RetryAfter, a.NextOpenAt
	return e
}

// waitCapabilities is /capabilities polling.waits.
func waitCapabilities() map[string]any {
	return map[string]any{
		"fields": "error.next_open_at (unix seconds) and error.retry_after (seconds until it), the Retry-After header over HTTP, retry_after_seconds beside them in an MCP tool error, a \"Try again after HH:MM UTC\" line on the text wires",
		"codes":  WaitCodes,
		"when":   "only where the board computes when the refusal lifts by itself; a retry hint carries retry_after alone, and a refusal no clock lifts neither",
	}
}
