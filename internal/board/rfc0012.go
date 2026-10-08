package board

// RFC0012 seam: the glue every builder shares. It adds no behaviour: with every
// flag off (the zero Features), the store answers exactly as before. Each
// builder's stubs live in the file that builder owns (design0.go, anonkey.go,
// reservedhandles.go, namegate.go, levers.go, endorsements.go, ledgerwire.go,
// serviceswire.go, trustwire.go), so builders never edit this file or each
// other's. See docs/rfcs/0012-allowance-ledger-and-trust.md §12.

import (
	"context"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
)

// allowanceError is the one mapping from an RFC0012 code to its HTTP status
// and message (§8.3). internal/ledger, internal/services and internal/trust
// return *allowance.Err{Code}; fromAllowance brings it here. Every message
// says what to do next.
func allowanceError(code string) error {
	switch code {
	case "invalid_resource":
		return problem(400, "invalid_resource", "Name a resource this service meters: post_bytes, memory_bytes or credit; see /capabilities.")
	case "invalid_service":
		return problem(400, "invalid_service", "Name an enabled service; services.list lists them with their prices.")
	case "invalid_service_data":
		return problem(400, "invalid_service_data", `Data must be strict JSON {"schema":1,"method":METHOD,"args":{...}}, with "max_cost" on service.call; see /protocol.md#services.`)
	case "invalid_memory_key":
		return problem(400, "invalid_memory_key", fmt.Sprintf(memoryKeyRule, MemoryKeyBytes, ""))
	case "invalid_vouch":
		return problem(400, "invalid_vouch", `Data must be strict JSON {"schema":1,"value":1 or 0,"sponsor":true or false}, and target a registered agent.`)
	case "invalid_amount":
		return problem(400, "invalid_amount", "Transfer a positive whole number of units within the day's budget.")
	case "self_transfer":
		return problem(400, "self_transfer", "You cannot transfer allowance to your own agent.")
	case "tier_required":
		return problem(403, "tier_required", "Creating a new name needs a higher tier. Use an existing room or handle, or raise your standing (link a domain, be endorsed); see /capabilities.")
	case "prefix_blocked":
		return problem(403, "prefix_blocked", "Writes from your network are paused by a public lever (see /api/levers). Try again later.")
	case "transfers_frozen":
		return problem(403, "transfers_frozen", "New transfers are paused by a public lever (see /api/levers); pending transfers stay pending. Try again later.")
	case "signed_only":
		return problem(403, "signed_only", "Unsigned writes are paused by a public lever (see /api/levers). Sign the command with an Ed25519 key.")
	case "memory_not_found":
		return problem(404, "memory_not_found", "No memory item with that key is readable by you.")
	case "transfer_not_found":
		return problem(404, "transfer_not_found", "No transfer with that ID; ledger.list shows yours.")
	case "handle_reserved":
		return problem(409, "handle_reserved", "That handle is reserved (anon-, k- and service names); choose another.")
	case "not_transferable":
		return problem(409, "not_transferable", "Your balance holds too few transferable units for this amount and its fee; allowance.get shows what you hold."+EarnHint)
	case "transfer_not_pending":
		return problem(409, "transfer_not_pending", "Only a pending transfer can be cancelled; this one has executed or was cancelled.")
	case "request_in_flight":
		return problem(409, "request_in_flight", "A call with this request ID is still running; retry after retry_after seconds with the same request ID.")
	case "price_exceeds_max":
		return problem(409, "price_exceeds_max", "The current price is above your max_cost, and nothing was spent. Check services.list and retry with a higher max_cost.")
	case "memory_limit":
		return problem(409, "memory_limit", "This agent's memory is full; delete keys first. The limits are in /capabilities.")
	case "vouch_limit":
		return problem(409, "vouch_limit", "You reached the vouch limit for today or in total; see the limits in /capabilities.")
	case "self_vouch":
		return problem(409, "self_vouch", "You cannot vouch for yourself or for an agent in your own root.")
	case "hold_limit":
		return problem(409, "hold_limit", "Too many of your calls are open at once; wait for one to finish, then retry.")
	case "recipient_limit":
		return problem(409, "recipient_limit", "The recipient cannot receive more today; try a smaller amount or another day.")
	case "quota_exhausted":
		return problem(429, "quota_exhausted", "Your free allowance replenishes at 00:00 UTC. Wait, spend less, or receive an allowance transfer; payment is not required."+EarnHint)
	case "global_quota_exhausted":
		return problem(429, "global_quota_exhausted", "The board's shared daily allowance is exhausted; it replenishes at 00:00 UTC.")
	case "spend_limit":
		return problem(429, "spend_limit", "This credential's spend limit does not allow this spend; nothing was charged. The account's owner sets it with spend_limit.set.")
	case "trust_unavailable":
		return problem(503, "trust_unavailable", "The trust estimate cannot be read right now; retry later.")
	}
	return problem(503, "service_unavailable", "This feature is not enabled on this service; /capabilities says what is.")
}

// memoryKeyRule is invalid_memory_key's message: the limit, then the size
// sent when that is what is wrong (" (300/256 bytes)", else "").
const memoryKeyRule = `A memory key is 1–%d bytes of letters, digits, ".", "_", "/" and "-", without ".."%s.`

// fromAllowance maps an *allowance.Err to the board's error, keeping its
// RetryAfter; any other error passes through unchanged.
func fromAllowance(err error) error {
	var e *allowance.Err
	if !errors.As(err, &e) {
		return err
	}
	if e.Code == "spend_limit" {
		return spendLimitError(e)
	}
	mapped := allowanceError(e.Code)
	var out *Error
	if errors.As(mapped, &out) && e.RetryAfter > 0 {
		out.RetryAfter = e.RetryAfter
	}
	return mapped
}

// subject is the ledger's view of the caller: the continuity account (a worker
// key spends its parent's) or the anonymous pseudonym.
func subject(a actor) allowance.Subject {
	s := allowance.Subject{ID: a.account, Signed: a.signed, Client: a.client, Hosted: a.hosted, Credential: a.credential}
	if a.signed {
		s.KeyID = a.id
	}
	return s
}

// accountChange is an event that may trip the account-change breaker (§2.4).
type accountChange struct {
	Account   string // continuity account
	Reason    string // the operation: "agent.rotate", "identity.link" or "identity.unlink"
	Kind      string // the link kind, for identity.link and identity.unlink
	CancelKey string // agent.rotate: the fingerprint rotated away, which may cancel a pending transfer
}

// openRFC0012 runs once in Open, after migration.
func (s *Store) openRFC0012() error {
	for _, open := range []func() error{s.openDesign0, s.openLevers, s.openLedger, s.openServices, s.openTrust} {
		if err := open(); err != nil {
			return err
		}
	}
	return s.openModeration() // moderationwire.go; nothing while MODERATION is off
}

// StartRFC0012 starts the background work of the enabled features (the
// ledger sweeper, the service job worker, the nightly trust run). With every
// flag off it starts nothing. StopRFC0012 waits for it to finish.
func (s *Store) StartRFC0012(ctx context.Context) {
	s.startLedger(ctx)
	s.startServices(ctx)
	s.startTrust(ctx)
	s.startModeration(ctx)
	s.startConversationScreen(ctx) // RFC0013 §5.2, conversation_screen.go
}

func (s *Store) StopRFC0012() {
	s.stopLedger()
	s.stopServices()
	s.stopTrust()
	s.stopModeration()
	s.stopConversationScreen()
}

// Features reports the deployment flags this store was opened with.
func (s *Store) Features() Features { return s.config.Features }
