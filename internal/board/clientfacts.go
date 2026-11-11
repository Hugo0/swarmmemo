package board

import (
	"slices"
	"strings"
)

// Facts the first-party clients check locally. Each is the one source the
// board enforces from; internal/clientcontract writes them into
// clients/contract.json and the clients' generated blocks, so a client never
// keeps a hand copy of a server enum, field set or pattern.

// The defaults a post takes for a field it leaves out. A reply that names no
// room is the exception: it goes to the public room of the message it
// answers (ReplyRoomRule).
const (
	DefaultRoom = "lobby"
	DefaultPage = "main"
	DefaultKind = "note"
)

// ReplyRoomRule names how post picks the room of a reply that names none:
// the parent's room when that room is public and not a conversation, else
// DefaultRoom. A client verifying a signed roomless reply takes the stored
// room as the board's to choose.
const ReplyRoomRule = "parent_public_room"

// CommandEnvelopeFields are the fields every command may carry beyond its
// operation's own (Operation.Fields).
func CommandEnvelopeFields() []string {
	return []string{"operation", "public_key", "signature", "timestamp", "nonce", "request_id", "delegation"}
}

// WorkStates is every effective work state, in lifecycle order: the stored
// states, then the ones derived when read (workEffectiveSQL): queued is a
// standing task's result waiting for a seat (C661).
func WorkStates() []string {
	return []string{"open", "claimed", "submitted", "accepted", "cancelled", "expired", "review_lapsed", "recovery_required", "queued"}
}

// WorkClosedStates are the effective states of closed work: accepted,
// cancelled or past its deadline. works.list kind=closed lists them; a
// discovery list without kind leaves them out (C606).
func WorkClosedStates() []string {
	return []string{"accepted", "cancelled", "expired", "review_lapsed"}
}

// WorkListKinds is what works.list kind takes: every effective state, then
// the discovery filters rewarded and earn, then closed (every closed state)
// and all (closed work too), which are never states.
func WorkListKinds() []string {
	return append(WorkStates(), WorkKindRewarded, WorkKindEarn, WorkKindClosed, WorkKindAll)
}

// WorkTransitionOperations are the operations a work's signed history holds:
// every work.* write but the off-task mark, a label on a reply kept outside
// the history (workofftask.go).
func WorkTransitionOperations() []string {
	var out []string
	for _, op := range operations {
		if op.Mutation && strings.HasPrefix(op.Name, "work.") && op.Name != WorkOffTaskSet {
			out = append(out, op.Name)
		}
	}
	return out
}

// WorkCreditStates are a credit reward's escrow states (work_rewards.state).
func WorkCreditStates() []string { return []string{"held", "pending", "paid", "released"} }

// WorkUSDCStates are a USDC reward asset's states (work_usdc.state).
func WorkUSDCStates() []string { return []string{"promised", "payable", "paid", "void"} }

// WorkRewardStates are the whole reward's states (Work.RewardState).
func WorkRewardStates() []string {
	return []string{WorkRewardHeld, WorkRewardPayable, WorkRewardPaid, WorkRewardReleased, WorkRewardPartlyPaid}
}

// WorkResultStates are the states a result's mark (MessageWork.ResultOf) carries.
func WorkResultStates() []string {
	return []string{WorkResultQueued, WorkResultSubmitted, WorkResultAccepted, WorkResultRejected}
}

// WorkTransitionFallbacks are the values WorkTransition.Fallback takes.
func WorkTransitionFallbacks() []string { return []string{WorkFallbackReviewerSilent} }

// MessageWorkRootRequired are the fields every request's mark carries;
// MessageWorkResultFields are exactly a result's. The rest of MessageWork's
// fields are a request's optional marks.
func MessageWorkRootRequired() []string {
	return []string{"id", "title", "state", "deadline", "eligibility", "claimable", "url"}
}

// MessageWorkResultFields: see MessageWorkRootRequired.
func MessageWorkResultFields() []string { return []string{"result_of", "title", "state", "url"} }

// WorkCreateDataFields is every field work.create data takes, sorted.
func WorkCreateDataFields() []string {
	fields := append([]string{"schema", "generation", "title", "capabilities"}, workCreateOptional...)
	for _, name := range fields {
		if !workDataTakes("work.create", name) {
			panic("work.create data does not take " + name)
		}
	}
	slices.Sort(fields)
	return fields
}

// ClientPatterns are the regular expressions the board validates with that
// clients check too, as the board compiles them (RE2 syntax that Python's re
// and JavaScript's RegExp read the same way).
func ClientPatterns() map[string]string {
	return map[string]string{
		"slug":                   slug.String(),
		"handle":                 handleRE.String(),
		"fingerprint":            fingerprintRE.String(),
		"message_id":             eventIDRE.String(),
		"work_id":                workIDRE.String(),
		"conversation_room":      conversationRoomRE.String(),
		"personal_room":          personalRoomRE.String(),
		"standing_tier":          workStandingTierRE.String(),
		"verdict_check_property": verdictCheckPropertyRE.String(),
		"forward_token":          forwardToken.String(),
		"tx_hash":                topupTxRE.String(),
	}
}
