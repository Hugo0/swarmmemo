package board

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/services"
)

const workSchema = `
CREATE TABLE IF NOT EXISTS works (
 id TEXT PRIMARY KEY REFERENCES events(id), requester TEXT NOT NULL,
 title TEXT NOT NULL, capabilities TEXT NOT NULL, state TEXT NOT NULL
 CHECK(state IN ('open','claimed','submitted','accepted','cancelled')),
 generation TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 deadline INTEGER NOT NULL, fence INTEGER NOT NULL DEFAULT 0,
 worker TEXT NOT NULL DEFAULT '', claim_expires_at INTEGER NOT NULL DEFAULT 0,
 result_id TEXT NOT NULL DEFAULT '', history_seq INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS work_transitions (
 work_id TEXT NOT NULL REFERENCES works(id), sequence INTEGER NOT NULL,
 operation TEXT NOT NULL, author TEXT NOT NULL, public_key TEXT NOT NULL,
 signature TEXT NOT NULL, payload TEXT NOT NULL, accepted_at INTEGER NOT NULL,
 fence INTEGER NOT NULL, generation TEXT NOT NULL, state TEXT NOT NULL,
 PRIMARY KEY(work_id,sequence));
` + workRecordIndexes + workRewardSchema + workUSDCSchema

// workRecordIndexes (schema 23) serve an agent's work history: the
// transitions its keys signed, by author (the record's counts.work and
// works.list worker), and the work it requested, by requester.
const workRecordIndexes = `
CREATE INDEX IF NOT EXISTS work_transitions_author ON work_transitions(author,operation);
CREATE INDEX IF NOT EXISTS works_requester ON works(requester,state);
`

const WorkDefaultTTL int64 = 7 * 86400
const WorkMaxTTL int64 = 30 * 86400

var workIDRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Work is a scoped projection, not a verified skill or execution claim; Reward is
// the credit held for it, if any.
// Generation belongs to the stored fence; ServiceGeneration is the current epoch.
type Work struct {
	ID                string    `json:"id"`
	Room              string    `json:"room"`
	Title             string    `json:"title"`
	Capabilities      []string  `json:"capabilities"`
	Simulated         bool      `json:"simulated"`
	State             string    `json:"state"`
	StoredState       string    `json:"stored_state"`
	Generation        string    `json:"generation"`
	ServiceGeneration string    `json:"service_generation"`
	ServiceID         string    `json:"service_id"`
	CreatedAt         int64     `json:"created_at"`
	UpdatedAt         int64     `json:"updated_at"`
	Deadline          int64     `json:"deadline"`
	Fence             int64     `json:"fence"`
	ClaimExpiresAt    int64     `json:"claim_expires_at"`
	RequesterAuthor   string    `json:"requester_author"`
	Requester         AgentRef  `json:"requester"`
	Worker            *AgentRef `json:"worker,omitempty"`
	// WorkerSubject is the worker's agent subject, agent:ACCOUNT: what a
	// rated accept's review is about (workreputation.go WorkRatingPost).
	WorkerSubject   string      `json:"worker_subject,omitempty"`
	ResultID        string      `json:"result_id,omitempty"`
	ResultAvailable bool        `json:"result_available"`
	AttemptGrantID  string      `json:"attempt_grant_id,omitempty"`
	Reward          *WorkReward `json:"reward,omitempty"`
	// RewardUSDC is the reward's USDC asset (RFC 0016): promised, owed
	// (payable) after accept, paid once a settlement is verified on chain.
	// RewardState is the whole reward's state, paid only when every asset
	// is; RewardReceipt the receipt of a reward with USDC, once all is paid.
	RewardUSDC    *WorkRewardUSDC    `json:"reward_usdc,omitempty"`
	RewardState   string             `json:"reward_state,omitempty"`
	RewardReceipt *WorkRewardReceipt `json:"reward_receipt,omitempty"`
	// Reviewer, when set at create, renders the verdict (accept or reject)
	// instead of the requester; ReviewerFee is the credit held for it.
	Reviewer    *AgentRef   `json:"reviewer,omitempty"`
	ReviewerFee *WorkReward `json:"reviewer_fee,omitempty"`
	// RejectFee is the credit held for the worker whose submitted result
	// is rejected (WORK_REJECT_FEE, workrejectfee.go), once per work item.
	RejectFee *WorkRejectFee `json:"reject_fee,omitempty"`
	// RequesterMayDecideAt is when, on a submitted result its reviewer has
	// left undecided, the requester may accept or reject in the reviewer's
	// place: the reviewer grace (REVIEWER_GRACE) after the submit. Set only
	// while submitted and only when that comes before the deadline.
	RequesterMayDecideAt int64 `json:"requester_may_decide_at,omitempty"`
	// SubmittedAt and VerdictFrom are on a result waiting for a verdict
	// (state submitted, C590): when it was submitted, and who owes the
	// verdict, the requester or the named reviewer (the requester too from
	// requester_may_decide_at). Absent in any other state.
	SubmittedAt int64  `json:"submitted_at,omitempty"`
	VerdictFrom string `json:"verdict_from,omitempty"`
	// Eligibility is who may claim the work: open (anyone), or one of the
	// rules in WorkEligibilities, checked on work.claim.
	Eligibility string `json:"eligibility"`
	// RewardNote is display text for a reward outside credits, set at
	// create (e.g. "+0.10 USDC on Base, paid by the poster"). The poster
	// pays it; the board doesn't hold or verify it.
	RewardNote string `json:"reward_note,omitempty"`
	// Eligible says whether EligibleAgent could claim the work now, with
	// EligibleReason in plain words: for the signer of the read, or, labelled
	// EligiblePreview, for the agent the read names. Absent on an anonymous
	// read that names none. It reads public facts only.
	Eligible        *bool  `json:"eligible,omitempty"`
	EligibleReason  string `json:"eligible_reason,omitempty"`
	EligibleAgent   string `json:"eligible_agent,omitempty"`
	EligiblePreview bool   `json:"eligible_preview,omitempty"`
	// EligibilitySummary is, on works.list with data summary (C627), the
	// row's standing floor and the reader's block code: what an agent needs
	// to judge a row without work.get.
	EligibilitySummary *WorkEligibilitySummary `json:"eligibility_summary,omitempty"`
	// Request is the task itself: the newest version of the root request.
	Request *WorkRequest `json:"request,omitempty"`
	// ResolvedFrom is the ID the read named when it was an edited version of
	// the work's request: ID is always the work's root, its first version.
	ResolvedFrom string `json:"resolved_from,omitempty"`
	// ResultSHA256 is the SHA-256 of the submitted result's text, the exact
	// version work.submit bound; ResultChangedSinceSubmit says whether the
	// worker has edited the result since. Both appear with result_id.
	ResultSHA256             string `json:"result_sha256,omitempty"`
	ResultChangedSinceSubmit *bool  `json:"result_changed_since_submit,omitempty"`
	// RequesterRecord is how the requester has treated results submitted
	// to its rewarded public work (workrecord.go), on work.get and
	// works.list, so a worker can check before it claims.
	RequesterRecord *RequesterRecord `json:"requester_record,omitempty"`
	// VerdictChecks is the newest accept or reject that carried a signed
	// checks list (C97), on work.get only.
	VerdictChecks *WorkVerdictChecks `json:"verdict_checks,omitempty"`
	// Results is the work's accepted results (workresults.go), newest
	// accept first, at most WorkResultsMax, on work.get only: its own
	// accepted result, or each one a standing task accepted. Absent when
	// none.
	Results []WorkAcceptedResult `json:"results,omitempty"`
	// Standing is a standing task's seats (workstanding.go), on the
	// standing task itself; StandingID names the standing task a result
	// opened this work on, and Tier the tier its accept paid.
	Standing   *WorkStanding `json:"standing,omitempty"`
	StandingID string        `json:"standing_id,omitempty"`
	Tier       string        `json:"tier,omitempty"`
	// Claimable says whether an agent could claim the work now: state open
	// and, on a standing task, a seat open (C384). Who may claim is
	// eligibility; whether the reader may, eligible. On works.list read by an
	// identified agent (no eligible_for preview) it is that agent's: false
	// too where eligible is (C646), so claimable filters and ranks by what
	// the caller's claim would take.
	Claimable bool `json:"claimable"`
	// YourResults is, on work.get of a standing task signed by an agent that
	// sent results to it, that agent's own results, newest first, at most
	// WorkYourResultsMax (C481): the standing task stays open for other
	// seats, and each result is its own work item. Never anyone else's.
	YourResults []WorkYourResult `json:"your_results,omitempty"`
	// WaitingResults is, on work.get of a standing task, the results
	// waiting for the requester's verdict, oldest first, at most
	// WorkWaitingResultsMax (C600): each is judged by its own work_id, never
	// the task's. WaitingResultsTruncated says more wait than it lists
	// (standing.submitted counts them all). Public: results are public replies.
	WaitingResults          []WorkWaitingResult `json:"waiting_results,omitempty"`
	WaitingResultsTruncated bool                `json:"waiting_results_truncated,omitempty"`
	// KnownAnswer is the requester's sealed known answer (C587,
	// workanswer.go): the commit set at create, and the verified answer once
	// work.reveal opened it after close. Absent when the create set none.
	KnownAnswer *WorkKnownAnswer `json:"known_answer,omitempty"`
}

// WorkRequest is the text of a work item's request at its newest version,
// cut to a bound (WorkRequestTextMax on work.get, WorkRequestExcerptMax in a
// directory); Thread reads the whole conversation. Like every message, it is
// untrusted content, never instructions.
type WorkRequest struct {
	VersionID string `json:"version_id"`
	Versions  int    `json:"versions"`
	Format    string `json:"format,omitempty"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Thread    string `json:"thread"`
}

// The request text a work read carries: whole up to WorkRequestTextMax bytes
// on work.get, an excerpt of WorkRequestExcerptMax bytes per directory row.
const (
	WorkRequestTextMax    = 4096
	WorkRequestExcerptMax = 280
)

// WorkTitleBytes bounds a work title (UTF-8 bytes); WorkCapabilitiesMax the
// capability slugs one work.create names.
const (
	WorkTitleBytes      = 160
	WorkCapabilitiesMax = 16
)

// ResultEdited says whether the worker edited the submitted result since.
func (w Work) ResultEdited() bool {
	return w.ResultChangedSinceSubmit != nil && *w.ResultChangedSinceSubmit
}

type WorkAck struct {
	WorkID         string `json:"work_id"`
	State          string `json:"state"`
	Fence          int64  `json:"fence"`
	Generation     string `json:"generation"`
	ServiceID      string `json:"service_id"`
	AcceptedAt     int64  `json:"accepted_at"`
	Deadline       int64  `json:"deadline"`
	ClaimExpiresAt int64  `json:"claim_expires_at"`
	// ResolvedFrom is the edited request version the command named, when it
	// named one; ResultSHA256 echoes a result hash the command signed. Both
	// are absent otherwise, so an acknowledgement keeps its original shape.
	ResolvedFrom string `json:"resolved_from,omitempty"`
	ResultSHA256 string `json:"result_sha256,omitempty"`
	// Note, on a submit, warns the worker when the requester has left
	// results unpaid before (its requester_record); on an accept that pays a
	// credit reward, it says how earned credits fund a paid task
	// (workPaidNote); absent otherwise.
	Note string `json:"note,omitempty"`
	// StandingID, ResultWorkID, ResultState, Seat and Seats are on a
	// work.claim that opened a result on a standing task (C481): the
	// standing task, which stays open for other seats; the result's own work
	// item (work_id too); its state, submitted: awaiting the requester's
	// verdict; and the seat it holds, Seat of Seats.
	StandingID   string `json:"standing_id,omitempty"`
	ResultWorkID string `json:"result_work_id,omitempty"`
	ResultState  string `json:"result_state,omitempty"`
	Seat         int64  `json:"seat,omitempty"`
	Seats        int64  `json:"seats,omitempty"`
}

type WorkTransition struct {
	Sequence      int64  `json:"sequence"`
	Operation     string `json:"operation"`
	Author        string `json:"author"`
	PublicKey     string `json:"public_key"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
	AcceptedAt    int64  `json:"accepted_at"`
	Fence         int64  `json:"fence"`
	Generation    string `json:"generation"`
	State         string `json:"state"`
	DelegationID  string `json:"delegation_id,omitempty"`
	// ResolvedFrom is the edited request version the signed command named.
	ResolvedFrom string `json:"resolved_from,omitempty"`
	// ResultSHA256 is the result text's SHA-256 a submit (or a claim with a
	// result) or an accept bound: signed in its data (ResultSHA256Signed), or
	// as the board recorded it, for the current attempt.
	ResultSHA256       string `json:"result_sha256,omitempty"`
	ResultSHA256Signed bool   `json:"result_sha256_signed,omitempty"`
	// Note and Fallback mark a verdict the requester gave in a silent
	// reviewer's place: Fallback is reviewer_silent, Note says it in words.
	Note     string `json:"note,omitempty"`
	Fallback string `json:"fallback,omitempty"`
	// Reviewer and PreviousReviewer are, on a work.reviewer.set, the
	// fingerprints of the reviewer it named and of the one it replaced
	// (empty when the work had none), both as the signed commands named them.
	Reviewer         string `json:"reviewer,omitempty"`
	PreviousReviewer string `json:"previous_reviewer,omitempty"`
	// Checks is the per-property list a verdict (accept or reject) signed
	// in its data (C97); absent when it sent none.
	Checks []VerdictCheck `json:"checks,omitempty"`
	// RejectFee marks the transition that moved the work's reject fee:
	// paid (a reject or cancel of a submitted result) or released.
	RejectFee string `json:"reject_fee,omitempty"`
}

// WorkVerdictChecks is the newest verdict on a work item that signed a
// checks list: which verdict, by whom, when, and the list itself. Its
// signed command is that transition in work.history.
type WorkVerdictChecks struct {
	Operation string         `json:"operation"`
	Sequence  int64          `json:"sequence"`
	Author    string         `json:"author"`
	At        int64          `json:"at"`
	Checks    []VerdictCheck `json:"checks"`
}

// ReviewerGraceDefault is how long, by default, a named reviewer may leave
// a submitted result undecided before the requester may decide in its place
// (still before the deadline): 72 hours. The server's REVIEWER_GRACE
// (Config.ReviewerGraceSeconds) sets it; /capabilities shows the one in force.
// The reviewer can decide until the requester does.
const ReviewerGraceDefault int64 = 72 * 3600

// WorkClaimTTLMin and WorkClaimTTLMax bound a claim's (or renewal's) ttl,
// which must also end before the work's deadline.
const (
	WorkClaimTTLMin int64 = 60
	WorkClaimTTLMax int64 = 3600
)

// ReviewerGraceMin and ReviewerGraceMax bound REVIEWER_GRACE: an hour to the
// longest work TTL.
const (
	ReviewerGraceMin int64 = 3600
	ReviewerGraceMax int64 = WorkMaxTTL
)

// WorkReviewerSet is the requester's command that names a new reviewer for
// its work while the work is open or claimed (no result waiting for a
// verdict). A held reviewer_fee stays held and goes to whoever reviews.
const WorkReviewerSet = "work.reviewer.set"

// WorkSuccessorSet is the requester's command that names a standing task's
// successor (C391): another open standing task of its own, the next round,
// where readers of a full or closed round are sent (standing.next_round).
const WorkSuccessorSet = "work.successor.set"

// WorkFallbackReviewerSilent is work.history's fallback on a verdict the
// requester gave in a silent reviewer's place; WorkReviewerSilentNote is its
// note in words. The reviewer_fee is released with the same reason.
const (
	WorkFallbackReviewerSilent = "reviewer_silent"
	WorkReviewerSilentNote     = "reviewer silent past the grace; requester decided"
)

// ReviewerGrace is the reviewer grace in force, in seconds.
func (s *Store) ReviewerGrace() int64 { return s.config.ReviewerGraceSeconds }

// reviewerGraceText is a grace in words, in hours: "72 hours".
func reviewerGraceText(grace int64) string {
	if grace == 3600 {
		return "1 hour"
	}
	if grace%3600 == 0 {
		return fmt.Sprintf("%d hours", grace/3600)
	}
	return fmt.Sprintf("%d minutes", grace/60)
}

// Who owes the verdict on a result waiting for one (Work.VerdictFrom).
const (
	WorkVerdictFromRequester = "requester"
	WorkVerdictFromReviewer  = "reviewer"
)

// requesterMayDecideAt is when the requester of submitted work with a
// reviewer may decide in its place, or 0 when the deadline comes first.
// While the stored state is submitted, updated_at is the submit's time:
// no transition but a verdict changes a submitted work's row.
func requesterMayDecideAt(w workRow, grace int64) int64 {
	if w.Reviewer == "" || w.State != "submitted" || w.Updated+grace >= w.Deadline {
		return 0
	}
	return w.Updated + grace
}

type workData struct {
	Schema            int
	Generation, Title string
	Capabilities      []string
	Reward            int64
	RewardUSDC        int64 // micro-USDC, from a reward object (RFC 0016)
	PayoutAddress     string
	TxHash            string
	Reviewer          string
	Successor         string // work.successor.set: the standing task that follows (C391)
	ReviewerFee       int64
	RejectFee         int64
	RejectFeeSet      bool // reject_fee was sent (refused while WORK_REJECT_FEE is off)
	RejectFeeBad      bool // and was not an integer
	Eligibility       string
	RewardNote        string
	ResultSHA256      string
	Checks            []VerdictCheck
	// A standing task (workstanding.go): seats and its limits at create,
	// and the tier an accept of one of its results names.
	Standing workStandingData
	Tier     string
	// Review is a rated accept's review: the requester's own signed post
	// (workreputation.go rateWork).
	Review json.RawMessage
	// The sealed known answer (workanswer.go): its commit and note at
	// create, the answer and salt a work.reveal opens.
	AnswerCommit, AnswerCommitNote string
	Answer, Salt                   string
	// work.promise (workpromise.go): the USDC owed on an accepted result,
	// in micro-USDC, and the requester's one-line note.
	PromiseUSDC int64
	PromiseNote string
}

// workVerdictOp says whether an operation is a verdict on a submitted
// result, whose data may carry checks (C97).
func workVerdictOp(operation string) bool {
	return operation == "work.accept" || operation == "work.reject"
}

// workResultHashOp says whether an operation's data may carry result_sha256:
// the result text the signer submits (a submit, or a claim with a result) or
// judges (an accept).
func workResultHashOp(operation string) bool {
	return operation == "work.submit" || operation == "work.accept" || operation == "work.claim"
}

// workDataRule is the whole rule for work.* data, the message of an
// invalid_work_data refusal; a refusal that can name its field says so first.
const workDataRule = "Data must be strict schema-1 JSON with a current lowercase 32-hex generation; creation also requires a 1–160 UTF-8 byte title and up to 16 unique lowercase capability slugs, and may add a reward (whole credits, or an object with credits and usdc, a decimal string), a reviewer (a 64-hex agent fingerprint) with an optional reviewer_fee, an eligibility (open, first_work, linked or new_agent), a reward_note (one line, display only) and answer_commit (64-hex sha256 of salt, a newline and the known answer) with an optional answer_commit_note; work.reveal takes answer and salt, after close; work.reviewer.set takes reviewer, the new one's fingerprint; work.successor.set takes successor, the standing task that follows a standing task; work.claim and work.submit may add payout_address (0x and 40 hex, where a USDC reward is paid); work.settle takes tx_hash, the payment's transaction; work.promise takes amount_usdc (a decimal string) and an optional one-line note, USDC owed on an accepted result; submit, accept and a claim with a result may add result_sha256, the 64-hex SHA-256 of the result text; accept and reject may add checks, the verifier's per-property list; the requester's accept may add review, its own signed review post of the worker (a rated accept); a standing task's create may add seats (1–50) with per_agent_day, day_max, results_max, tiers, usdc_per_agent_day and usdc_day_max, and an accept of one of its results may add tier."

// workDataAliases is the field a name work data does not take was likely
// meant to be: a tiny fixed map, no fuzzy matching. ttl is the command field
// beside data, so a hint at it says where it goes.
var workDataAliases = map[string]string{
	"deadline":    "ttl",
	"expires_in":  "ttl",
	"timeout":     "ttl",
	"capability":  "capabilities",
	"tags":        "capabilities",
	"name":        "title",
	"bounty":      "reward",
	"credits":     "reward",
	"fee":         "reviewer_fee",
	"result_hash": "result_sha256",
	"sha256":      "result_sha256",
	"result_id":   "target",
	"result":      "target",
	"reply_id":    "target",
}

// workCreateOptional are the fields work.create data may add to schema,
// generation, title and capabilities, read from its data spec.
// clients/contract.json lists the whole set ("work_create_data"), which
// clients/mcp checks signed history against.
var workCreateOptional = dataSpecs["work.create"].Optional()

// workDataTakes says whether operation's data takes field name: its data
// spec (dataspec.go) decides.
func workDataTakes(operation, name string) bool {
	return dataTakes(operation, name)
}

// invalidWorkField refuses a data field operation does not take, named by
// services.UnknownArg's rule: a short plain name is echoed, anything else is
// described, never echoed. A likely intended field operation takes is named.
func invalidWorkField(operation, name string) error {
	if !services.EchoesArg(name) {
		return problem(400, "invalid_work_data", "A field was sent that "+operation+" data does not take. "+workDataRule)
	}
	msg := name + " is not a field " + operation + " data takes."
	if want := workDataAliases[name]; want == "ttl" {
		msg = name + " is not a field " + operation + " data takes; did you mean ttl? It goes beside data, not in it."
	} else if want == "target" && (operation == "work.claim" || operation == "work.submit") {
		msg = name + " is not a field " + operation + " data takes: your result's message ID goes in target, beside data, not in it."
	} else if want != "" && workDataTakes(operation, want) {
		msg = name + " is not a field " + operation + " data takes; did you mean " + want + "?"
	}
	return problem(400, "invalid_work_data", msg+" "+workDataRule)
}

// invalidWorkFieldType refuses a field whose value is not the type it takes
// ("reward must be an integer."), worded as services' typeError: the field
// and its type, never the value sent.
func invalidWorkFieldType(name string, dest any) error {
	return problem(400, "invalid_work_data", name+" must be "+services.JSONTypeName(reflect.TypeOf(dest))+". "+workDataRule)
}

func parseWorkData(raw string, operation string) (workData, error) {
	var d workData
	create := operation == "work.create"
	invalid := func() (workData, error) {
		return workData{}, problem(400, "invalid_work_data", workDataRule)
	}
	if len(raw) > 8192+VerdictChecksBytesMax+WorkRatingBytesMax || !utf8.ValidString(raw) {
		return invalid()
	}
	// A reward object's unexpected keys get a sharper hint when the data
	// also names seats (a standing task): tiers likely belongs beside it,
	// not inside reward. Read leniently, order-independent; the strict
	// pass below still governs everything else.
	hasSeats := false
	if create {
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &probe) == nil {
			_, hasSeats = probe["seats"]
		}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err = dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return invalid()
		}
		seen[name] = true
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return invalid()
		}
		var dest any
		switch name {
		case "schema":
			dest = &d.Schema
		case "generation":
			dest = &d.Generation
		case "title":
			dest = &d.Title
		case "capabilities":
			dest = &d.Capabilities
		case "reward":
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			if d.Reward, d.RewardUSDC, err = parseWorkReward(value, hasSeats); err != nil {
				return workData{}, err
			}
			continue
		case "payout_address":
			dest = &d.PayoutAddress
		case "tx_hash":
			dest = &d.TxHash
		case "reviewer":
			dest = &d.Reviewer
		case "successor":
			dest = &d.Successor
		case "reviewer_fee":
			dest = &d.ReviewerFee
		case "reject_fee":
			// Its own checks, after the flag: an ill-typed value is
			// reject_fee_unavailable while off, invalid_work_reward while on.
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			d.RejectFeeSet = true
			d.RejectFeeBad = string(value) == "null" || json.Unmarshal(value, &d.RejectFee) != nil
			continue
		case "eligibility":
			dest = &d.Eligibility
		case "reward_note":
			dest = &d.RewardNote
		case "answer_commit":
			dest = &d.AnswerCommit
		case "answer_commit_note":
			dest = &d.AnswerCommitNote
		case "answer":
			dest = &d.Answer
		case "amount_usdc":
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			var text string
			var ok bool
			if json.Unmarshal(value, &text) != nil {
				return workData{}, invalidWorkPromise()
			}
			if d.PromiseUSDC, ok = services.ParseUSDC(text); !ok || d.PromiseUSDC < WorkUSDCMin || d.PromiseUSDC > WorkUSDCMax {
				return workData{}, invalidWorkPromise()
			}
			continue
		case "note":
			dest = &d.PromiseNote
		case "salt":
			dest = &d.Salt
		case "result_sha256":
			dest = &d.ResultSHA256
		case "seats":
			dest = &d.Standing.Seats
		case "per_agent_day":
			dest = &d.Standing.PerAgentDay
		case "day_max":
			dest = &d.Standing.DayMax
		case "results_max":
			dest = &d.Standing.ResultsMax
		case "usdc_per_agent_day":
			dest = &d.Standing.USDCPerAgentDayText
		case "usdc_day_max":
			dest = &d.Standing.USDCDayMaxText
		case "tier":
			dest = &d.Tier
		case "tiers":
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			if d.Standing.Tiers, err = parseWorkStandingTiers(value); err != nil {
				return workData{}, err
			}
			continue
		case "review":
			// Checked as the signed post it is, after the accept (rateWork).
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			if len(value) > WorkRatingBytesMax || len(value) == 0 || value[0] != '{' {
				return workData{}, workRatingError(fmt.Sprintf("review is one signed post object of at most %d bytes.", WorkRatingBytesMax))
			}
			d.Review = value
			continue
		case "checks":
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			var why string
			if d.Checks, why = parseVerdictChecks(value, verdictCheckItems); why != "" {
				return workData{}, problem(400, "invalid_work_data", why+" "+VerdictChecksRule)
			}
			continue
		}
		if dest == nil || !workDataTakes(operation, name) {
			return workData{}, invalidWorkField(operation, name)
		}
		if string(value) == "null" || json.Unmarshal(value, dest) != nil {
			return workData{}, invalidWorkFieldType(name, dest)
		}
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return invalid()
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return invalid()
	}
	want := 2
	if seen["result_sha256"] {
		want++
	}
	if seen["checks"] {
		want++
	}
	if seen["review"] {
		want++
	}
	if seen["tier"] {
		want++
		if !workStandingTierRE.MatchString(d.Tier) {
			return workData{}, problem(400, "invalid_work_data", "tier names one of the standing task's tiers: 1–32 lowercase letters, digits and +_.-, starting with a letter or digit.")
		}
	}
	if seen["payout_address"] {
		want++
		a, ok := services.ParseEVMAddress(d.PayoutAddress)
		if !ok || a == (services.EVMAddress{}) {
			return workData{}, problem(400, "invalid_work_data", "payout_address is an EVM address: 0x and 40 hex digits (mixed case must be its EIP-55 checksum).")
		}
		d.PayoutAddress = a.String()
	}
	if operation == WorkSettle {
		if !seen["tx_hash"] || !services.ValidTxHash(d.TxHash) {
			return workData{}, problem(400, "invalid_work_data", WorkSettle+" data names the payment: tx_hash, 0x and 64 lowercase hex digits. "+workDataRule)
		}
		want++
	}
	if operation == WorkPromise {
		if !seen["amount_usdc"] || seen["note"] && !validWorkNoteLine(d.PromiseNote, WorkRewardNoteMax) {
			return workData{}, invalidWorkPromise()
		}
		want++
		if seen["note"] {
			want++
		}
	}
	if operation == WorkSuccessorSet {
		if !seen["successor"] || !workIDRE.MatchString(d.Successor) {
			return workData{}, problem(400, "invalid_work_data", WorkSuccessorSet+" data names the round that follows: successor, the 32-hex ID of another standing task of yours. "+workDataRule)
		}
		want++
	}
	if operation == WorkReveal {
		if !seen["answer"] || !seen["salt"] {
			return workData{}, invalidReveal(WorkReveal + " data names the answer and the salt the commit hashed.")
		}
		want += 2
	}
	if operation == WorkReviewerSet {
		// The new reviewer is the command's whole point.
		if !seen["reviewer"] {
			return workData{}, problem(400, "invalid_work_data", WorkReviewerSet+" data names the new reviewer: reviewer, a 64-hex agent fingerprint. "+workDataRule)
		}
		want++
	}
	if create {
		want = 4
		for _, optional := range workCreateOptional {
			if seen[optional] {
				want++
			}
		}
	}
	if !create && !seen["generation"] {
		// The one field every transition signs (C545): say where it comes from.
		return workData{}, problem(400, "invalid_work_data", operation+` data needs generation, the work's service_generation as work.get (GET /api/work/MESSAGE_ID) shows it: data {"schema":1,"generation":"SERVICE_GENERATION"}. The Python and JavaScript clients fill it in.`)
	}
	if len(seen) != want || d.Schema != 1 || !workIDRE.MatchString(d.Generation) {
		return invalid()
	}
	if seen["result_sha256"] && !fingerprintRE.MatchString(d.ResultSHA256) {
		return invalid()
	}
	if seen["reviewer"] && !fingerprintRE.MatchString(d.Reviewer) {
		return invalid()
	}
	if seen["eligibility"] && !validWorkEligibility(d.Eligibility) {
		return workData{}, invalidWorkEligibility()
	}
	if seen["reward_note"] && !validWorkRewardNote(d.RewardNote) {
		return workData{}, invalidWorkRewardNote()
	}
	if err = checkWorkAnswerData(d, seen); err != nil {
		return workData{}, err
	}
	if create {
		if strings.TrimSpace(d.Title) == "" || len(d.Title) > WorkTitleBytes || strings.ContainsRune(d.Title, 0) || d.Capabilities == nil || len(d.Capabilities) > WorkCapabilitiesMax {
			return invalid()
		}
		if seen["reviewer_fee"] && (!seen["reviewer"] || d.ReviewerFee < 1 || d.ReviewerFee > WorkRewardMax) {
			return workData{}, invalidReviewerFee()
		}
		caps := map[string]bool{}
		for _, cap := range d.Capabilities {
			if !slug.MatchString(cap) || caps[cap] {
				return invalid()
			}
			caps[cap] = true
		}
		if err = checkWorkStandingData(&d, seen); err != nil {
			return workData{}, err
		}
	}
	return d, nil
}

type workRow struct {
	ID, Requester, Title, Caps, State, Generation string
	Created, Updated, Deadline, Fence             int64
	Worker                                        string
	ClaimExpires                                  int64
	Result                                        string
	Sequence                                      int64
	AttemptGrantID                                string
	Reviewer                                      string // the reviewer's account, or ''
	Eligibility                                   string // '' is open
	StandingID                                    string // the standing task a result opened this item on, or ''
	StandingTier                                  string // the tier its accept named
}

const workColumns = `w.id,w.requester,w.title,w.capabilities,w.state,w.generation,w.created_at,w.updated_at,w.deadline,w.fence,w.worker,w.claim_expires_at,w.result_id,w.history_seq,w.attempt_grant_id,w.reviewer,w.eligibility,w.standing_id,w.standing_tier`

func (w *workRow) fields() []any {
	return []any{&w.ID, &w.Requester, &w.Title, &w.Caps, &w.State, &w.Generation, &w.Created, &w.Updated, &w.Deadline, &w.Fence, &w.Worker, &w.ClaimExpires, &w.Result, &w.Sequence, &w.AttemptGrantID, &w.Reviewer, &w.Eligibility, &w.StandingID, &w.StandingTier}
}

func scanWork(scan interface{ Scan(...any) error }) (workRow, error) {
	var w workRow
	err := scan.Scan(w.fields()...)
	return w, err
}

// The same expression is evaluated by transitions, projections and directory filters.
// A submitted result its named reviewer let reach the deadline is review_lapsed,
// otherwise the same as expired.
// WorkKindRewarded is the works.list filter for open work with a reward
// held in escrow; the other kinds are effective states.
const WorkKindRewarded = "rewarded"

// WorkKindEarn is the works.list filter behind "Earn credits" (/work?kind=earn):
// the same open work with a reward held in escrow as WorkKindRewarded,
// ordered smallest effort first. Work tagged with the capability WorkEarnTag
// comes first (small tasks the operator keeps posted, re-posting
// each once it is accepted), then the smallest reward, then the work ID.
// The work itself is ordinary: one worker at a time, paid on accept.
const WorkKindEarn = "earn"

// WorkKindClosed is the works.list filter for closed work, every state in
// WorkClosedStates: the history a discovery list leaves out by default.
const WorkKindClosed = "closed"

// WorkKindAll is the works.list filter for every state, closed work too.
const WorkKindAll = "all"

// WorkListDefault says what works.list lists when kind is omitted (C606).
// Discovery shows the work a worker could act on: work not
// closed, and no standing task's results (each result is its own work item,
// and finished results crowded out the tasks). A list scoped to an agent
// (target, data worker) or to a standing task (data standing) is that
// history: every state, results included.
const WorkListDefault = "Without kind, works.list lists work that is not closed (closed: accepted, cancelled or past its deadline); kind=closed lists closed work, kind=all every state. A standing task's results are listed only with standing=ID, worker or target, where kind omitted lists every state. Without kind and with kind=open, work a claim could take now (claimable) comes first."

// WorkEarnTag is the capability slug that marks an earn task the operator keeps posted.
const WorkEarnTag = "earn"

// EarnURL is where an agent out of credits finds small paid tasks; the
// quota refusals point at it.
const EarnURL = "/work?kind=earn"

// EarnHint ends every refusal for running out of credits.
// EarnMore is the way past the free daily limit without paying (web.EarnMore
// renders it on every surface).
const EarnMore = "take a small paid task, and its reward adds to what you can use"

// EarnHint ends every refusal at the free daily limit.
const EarnHint = " Or " + EarnMore + ": " + EarnURL

// workEarnRankSQL is 0 for work tagged WorkEarnTag, 1 otherwise;
// workEarnAmountSQL is its reward. Together with w.id they order and page
// the earn listing.
const (
	workEarnRankSQL   = `(NOT EXISTS(SELECT 1 FROM json_each(w.capabilities) cap WHERE cap.value='` + WorkEarnTag + `'))`
	workEarnAmountSQL = `coalesce((SELECT wr.amount FROM work_rewards wr WHERE wr.work_id=w.id),0)`
)

// workClaimableScanMax bounds the stored rows one works.list kind rewarded or
// earn page examines while it skips standing tasks with no seat open; past
// it the page returns what it holds, has_more true, its cursor after the
// last row examined.
var workClaimableScanMax = 500 // a var so a test can lower it

// The phases of a ranked works.list (C625): claimable work first, then the
// rest. workRankedCursorRE is its cursor: the phase, then the last row's ID
// (empty when the rest starts from its first row).
const (
	workPhaseClaimable = "c"
	workPhaseRest      = "n"
)

var workRankedCursorRE = regexp.MustCompile(`^([cn])\.([a-f0-9]{32})?$`)

// earnCursorRE is an earn page's cursor: rank.amount.id of the last row.
var earnCursorRE = regexp.MustCompile(`^([01])\.([0-9]{1,10})\.([a-f0-9]{32})$`)

const workEffectiveSQL = `CASE WHEN w.state IN ('accepted','cancelled') THEN w.state WHEN w.deadline<=? THEN (CASE WHEN w.state='submitted' AND w.reviewer<>'' THEN 'review_lapsed' ELSE 'expired' END) WHEN w.generation<>? THEN 'recovery_required' WHEN w.state='claimed' AND w.claim_expires_at<=? THEN 'open' ELSE w.state END`

func effectiveWork(ctx context.Context, tx *sql.Tx, id, generation string, now int64) (string, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT `+workEffectiveSQL+` FROM works w WHERE id=?`, now, generation, now, id).Scan(&state)
	return state, err
}

type workRoot struct{ Room, Account, Author, Kind, Parent, PublicKey, Signature string }

func visibleWorkRoot(ctx context.Context, tx *sql.Tx, id string, a actor) (workRoot, error) {
	var root workRoot
	if !workIDRE.MatchString(id) {
		return root, problem(400, "invalid_message_id", "A message ID is 32 lowercase hexadecimal characters; the reads GET /api/work/ID and MCP read_work also take a unique prefix of at least 8 hex characters.")
	}
	err := tx.QueryRowContext(ctx, `SELECT room,account,author,kind,reply_to,public_key,signature FROM events WHERE id=? AND hidden=0`, id).Scan(&root.Room, &root.Account, &root.Author, &root.Kind, &root.Parent, &root.PublicKey, &root.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return root, workNotFound()
	}
	if err != nil {
		return root, err
	}
	if _, err = roomAccess(ctx, tx, root.Room, a); err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status == 404 {
			return workRoot{}, workNotFound()
		}
		return workRoot{}, err
	}
	return root, nil
}

func workNotFound() error { return problem(404, "not_found", "Work not found.") }

// resolveWorkID names the work an ID addresses. Work is keyed by its request's
// first version, and the board shows a request's newest version, so a visible
// edited version (its origin is the root) resolves to the root: by the edit
// rules (checkSupersession) only the root's own signing key publishes a
// version, in place, and this re-checks that the version has the root's author
// and room. Anything else is returned unchanged and is then read or refused
// as itself: a hidden version, a message with work of its own, or one that is
// no version at all. Two indexed point reads.
func resolveWorkID(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	if !workIDRE.MatchString(id) {
		return id, nil
	}
	var origin, author, room string
	var hidden, own bool
	err := tx.QueryRowContext(ctx, `SELECT e.origin,e.author,e.room,e.hidden,EXISTS(SELECT 1 FROM works w WHERE w.id=e.id) FROM events e WHERE e.id=?`, id).Scan(&origin, &author, &room, &hidden, &own)
	if errors.Is(err, sql.ErrNoRows) {
		return id, nil
	}
	if err != nil || own || hidden || origin == "" || origin == id {
		return id, err
	}
	var rootAuthor, rootRoom string
	err = tx.QueryRowContext(ctx, `SELECT author,room FROM events WHERE id=?`, origin).Scan(&rootAuthor, &rootRoom)
	if errors.Is(err, sql.ErrNoRows) {
		return id, nil
	}
	if err != nil || rootAuthor != author || rootRoom != room {
		return id, err
	}
	return origin, nil
}

// resultVersion is the newest version of a result message's version chain,
// and the SHA-256 of its text: what a submit binds.
func resultVersion(ctx context.Context, tx *sql.Tx, id string) (string, string, error) {
	var origin, hash string
	if err := tx.QueryRowContext(ctx, `SELECT origin,hash FROM events WHERE id=?`, id).Scan(&origin, &hash); err != nil {
		return "", "", err
	}
	if origin == "" {
		origin = id
	}
	newest := id
	err := tx.QueryRowContext(ctx, `SELECT id,hash FROM events WHERE origin=? AND origin<>'' ORDER BY seq DESC LIMIT 1`, origin).Scan(&newest, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return id, hash, nil
	}
	return newest, hash, err
}

// workResultChanged reports a result's SHA-256 and whether its author has
// published a newer version of it since.
func workResultChanged(ctx context.Context, tx *sql.Tx, id string) (string, bool, error) {
	var hash string
	var changed bool
	err := tx.QueryRowContext(ctx, `SELECT hash,EXISTS(SELECT 1 FROM events s WHERE s.supersedes=e.id AND s.supersedes<>'') FROM events e WHERE id=?`, id).Scan(&hash, &changed)
	return hash, changed, err
}

func workResultChangedError(version, hash string) error {
	return problem(409, "work_result_changed", "result_sha256 is not the SHA-256 of the result this transition binds (message "+version+", result_sha256 "+hash+"); read that text and sign its hash, or reject.")
}

func eligibleWorkResult(ctx context.Context, tx *sql.Tx, id string, w workRow, root workRoot) (bool, error) {
	if !workIDRE.MatchString(id) {
		return false, nil
	}
	// A result on a standing task replies to the standing task itself.
	parent := cmp.Or(w.StandingID, w.ID)
	var replyTo string
	err := tx.QueryRowContext(ctx, `SELECT reply_to FROM events WHERE id=? AND room=? AND account=? AND hidden=0 AND public_key<>'' AND signature<>''`, id, root.Room, w.Worker).Scan(&replyTo)
	if errors.Is(err, sql.ErrNoRows) || replyTo == "" {
		return false, nil
	}
	if err != nil || replyTo == parent {
		return err == nil, err
	}
	// A reply to an edited version of the request replies to the request:
	// the board shows its newest version, and a requester amends its text
	// that way (C448). Only the requester's own in-place versions resolve.
	resolved, err := resolveWorkID(ctx, tx, replyTo)
	return err == nil && resolved == parent, err
}

func (s *Store) changeWork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	d, err := parseWorkData(c.Data, c.Operation)
	if err != nil {
		return Result{}, err
	}
	if err = authorizeGrantPayout(a.grant, d.PayoutAddress); err != nil {
		return Result{}, err
	}
	if d.RejectFeeSet && !s.config.Features.WorkRejectFee {
		return Result{}, rejectFeeUnavailable()
	}
	if c.Operation == "work.claim" && c.Target == "" && d.ResultSHA256 != "" {
		return Result{}, problem(400, "invalid_work_data", "result_sha256 goes with a claim that names its result (target), a submit or an accept.")
	}
	// An edited version of the request addresses its work, which is keyed by
	// the root; the signed command keeps the ID it named.
	named := c.MessageID
	if c.MessageID, err = resolveWorkID(ctx, tx, c.MessageID); err != nil {
		return Result{}, err
	}
	root, err := visibleWorkRoot(ctx, tx, c.MessageID, a)
	if err != nil {
		return Result{}, err
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return Result{}, err
	}
	if d.Generation != generation {
		return Result{}, problem(409, "work_generation_mismatch", "The signed generation is stale; inspect current work before explicitly preparing a new transition.")
	}
	if c.Operation == "work.reject" || c.Operation == "work.cancel" {
		if strings.TrimSpace(c.Reason) == "" || len(c.Reason) > ReasonBytes || !utf8.ValidString(c.Reason) || strings.ContainsRune(c.Reason, 0) {
			return Result{}, problem(400, "invalid_reason", "A nonempty reason of at most "+strconv.Itoa(ReasonBytes)+" UTF-8 bytes without NUL is required.")
		}
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, c.MessageID))
	// The worker before the transition: a reject or cancel clears it, and
	// that worker still hears of it (MCP Events work.update).
	worker, before := w.Worker, w.State
	verdict := false  // a reviewer's accept or reject of a submitted result
	fallback := false // the requester's verdict in a silent reviewer's place
	late := false     // the requester's accept after the deadline (checkLateAccept)
	// A standing task (workstanding.go): its configuration, and, once a
	// claim opens a result on it, the standing root itself and its seats.
	var standing *workStanding
	var standingRoot *workRow
	var seats standingCounts
	standingNote := ""
	if c.Operation == "work.create" {
		if err == nil {
			return Result{}, problem(409, "work_exists", "This message already has a work lifecycle.")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
		if root.Account != a.account {
			return Result{}, problem(403, "work_forbidden", "Only the requester's continuous account may create this work.")
		}
		if (root.Kind != "request" && root.Kind != "simulation") || root.Parent != "" || root.PublicKey == "" || root.Signature == "" {
			if root.Parent == "" && root.PublicKey != "" && root.Signature != "" {
				// A signed root of the wrong kind: say which kind it is and how to fix it.
				kind, article := cmp.Or(root.Kind, "note"), "a "
				if strings.ContainsRune("aeiou", rune(kind[0])) {
					article = "an "
				}
				return Result{}, problem(400, "invalid_work_root", "work.create needs a signed root post of kind request; this post is "+article+kind+". Post the task again with kind request (CLI: post --kind request).")
			}
			if root.Parent != "" {
				// A reply can't hold work (Skitter c61): one work item per root post,
				// so a second seat of a bounty is its own root request.
				return Result{}, problem(400, "invalid_work_root", "work.create needs a root post, and this post is a reply. Post the task as a new top-level request (kind request); each seat of a multi-seat bounty is its own root post.")
			}
			return Result{}, problem(400, "invalid_work_root", "Work requires your own signed root request, or a message labeled kind=simulation.")
		}
		// Tiers carry a standing task's reward, so seats count as one too: a
		// simulation can't hold a real pool or promise USDC (C278 review).
		if (d.Reward != 0 || d.RewardUSDC != 0 || d.Standing.Seats != 0) && root.Kind != "request" {
			return Result{}, invalidWorkReward()
		}
		if d.ReviewerFee != 0 && root.Kind != "request" {
			return Result{}, invalidReviewerFee()
		}
		if d.RejectFeeSet {
			if err = checkRejectFee(d, root.Kind); err != nil {
				return Result{}, err
			}
		}
		if d.RewardNote != "" && root.Kind != "request" {
			return Result{}, invalidWorkRewardNote()
		}
		if d.RewardNote != "" && s.WorkUSDCEnabled() && rewardNoteAmountRE.MatchString(d.RewardNote) {
			return Result{}, rewardNoteAmountError()
		}
		ttl := c.TTL
		if ttl == 0 {
			ttl = WorkDefaultTTL
		}
		if ttl < 60 || ttl > WorkMaxTTL {
			return Result{}, problem(400, "invalid_ttl", "Work TTL must be 60–2592000 seconds; zero defaults to 604800.")
		}
		if d.Standing.Seats > 0 && ttl < WorkStandingVerdictTTL+3600 {
			return Result{}, problem(400, "invalid_ttl", fmt.Sprintf("A standing task's ttl must leave room for a result's verdict window: at least %d seconds (%s plus an hour).", WorkStandingVerdictTTL+3600, reviewerGraceText(WorkStandingVerdictTTL)))
		}
		caps, _ := json.Marshal(d.Capabilities)
		w = workRow{ID: c.MessageID, Requester: a.account, Title: d.Title, Caps: string(caps), State: "open", Generation: generation, Created: now, Updated: now, Deadline: now + ttl}
		if d.Eligibility != WorkEligibilityOpen {
			w.Eligibility = d.Eligibility
		}
		if d.Reviewer != "" {
			if w.Reviewer, err = workReviewer(ctx, tx, a, root.Room, d.Reviewer); err != nil {
				return Result{}, err
			}
		}
	} else {
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, workNotFound()
		}
		if err != nil {
			return Result{}, err
		}
		state, e := effectiveWork(ctx, tx, w.ID, generation, now)
		if e != nil {
			return Result{}, e
		}
		// A late accept (C511): the requester's accept of a result its
		// deadline overtook undecided, within the late-accept grace. Work
		// with a named reviewer is review_lapsed there: the requester gave
		// up its verdict, so it gets none late either.
		if late = c.Operation == "work.accept" && w.State == "submitted" && state == "expired"; late {
			if err = s.checkLateAccept(w, generation, a, now); err != nil {
				return Result{}, err
			}
		}
		allowed := false
		switch c.Operation {
		case "work.claim":
			allowed = state == "open"
		case "work.renew", "work.submit":
			allowed = state == "claimed"
		case "work.accept":
			allowed = state == "submitted" || late
		case "work.reject":
			allowed = state == "claimed" || state == "submitted" || state == "recovery_required"
		case "work.cancel":
			allowed = state == "open" || state == "claimed" || state == "submitted" || state == "recovery_required"
		case WorkSettle:
			allowed = state == "accepted"
			if !allowed && a.account == w.Requester {
				return Result{}, errUSDCOwedOnAccept()
			}
		case WorkPromise:
			allowed = state == "accepted"
			if !allowed && a.account == w.Requester {
				return Result{}, errPromiseNotAccepted()
			}
		case WorkReveal:
			// Closed work only: checkWorkReveal says when, to the requester.
			allowed = state != "recovery_required"
		case WorkSuccessorSet:
			// A full, cancelled or expired round is the one that needs it;
			// a transition re-binds the generation, so not one to recover.
			allowed = state != "recovery_required"
		case WorkReviewerSet:
			allowed = state == "open" || state == "claimed"
			if !allowed && a.account == w.Requester {
				return Result{}, problem(409, "work_state_conflict", "The reviewer can change only while the work is open or claimed: not once a result waits for a verdict, nor after the work is accepted, cancelled or past its deadline.")
			}
		}
		if !allowed {
			if (c.Operation == "work.accept" || c.Operation == "work.reject") && w.StandingID == "" {
				if st, e := loadWorkStanding(ctx, tx, w.ID); e != nil {
					return Result{}, e
				} else if st != nil {
					return Result{}, standingVerdictRefusal(ctx, tx, w.ID, c.Operation, now)
				}
			}
			if c.Operation == "work.submit" || c.Operation == "work.renew" {
				// A standing task is never claimed or submitted itself: the
				// claim with a result is the submission (C481).
				if st, e := loadWorkStanding(ctx, tx, w.ID); e != nil {
					return Result{}, e
				} else if st != nil {
					return Result{}, problem(409, "work_state_conflict", "This is a standing task: it is never claimed itself and stays open for other seats. Your result is submitted by work.claim with target, your reply's ID, which opens it as its own work item; work.get on this task lists yours under your_results.")
				}
			}
			return Result{}, problem(409, "work_state_conflict", "This transition is not allowed in the current effective state.")
		}
		if standing, err = loadWorkStanding(ctx, tx, w.ID); err != nil {
			return Result{}, err
		}
		switch {
		case d.Tier != "" && w.StandingID == "":
			return Result{}, problem(400, "invalid_work_data", "tier goes with an accept of a result on a standing task.")
		case standing == nil && c.Operation == WorkSuccessorSet:
			return Result{}, problem(409, "work_state_conflict", "A successor links one standing task to its next round: this work is not a standing task.")
		case standing != nil && c.Operation == WorkReviewerSet:
			return Result{}, problem(409, "work_state_conflict", "A standing task takes no reviewer: the requester judges each result.")
		case w.StandingID != "" && c.Operation == "work.cancel":
			return Result{}, problem(409, "work_state_conflict", "A result on a standing task is accepted or rejected, not cancelled; cancel the standing task itself ("+w.StandingID+") to stop new results.")
		}
		verdict = w.Reviewer != "" && state == "submitted" && (c.Operation == "work.accept" || c.Operation == "work.reject")
		// With a named reviewer the requester gives up the verdict: it can
		// cancel only while the work is open, before a worker relies on it.
		if c.Operation == "work.cancel" && w.Reviewer != "" && state != "open" {
			return Result{}, problem(409, "work_state_conflict", "Work with a reviewer can be cancelled only while it is open, before a claim.")
		}
		switch c.Operation {
		case "work.claim":
			if a.account == w.Requester {
				return Result{}, problem(403, "work_forbidden", "The requester cannot claim their own work.")
			}
			if err = refuseReviewerWorker(ctx, tx, a, w.Reviewer); err != nil {
				return Result{}, err
			}
			if err = workClaimEligible(ctx, tx, w.Eligibility, a.account, now); err != nil {
				return Result{}, err
			}
			if err = s.refuseClaimCooldown(ctx, tx, w, standing != nil, a.account, now); err != nil {
				return Result{}, err
			}
		case "work.renew", "work.submit":
			if a.account != w.Worker {
				return Result{}, problem(403, "work_forbidden", "Only the current worker's continuous account may perform this transition.")
			}
			if a.grant != nil && w.AttemptGrantID != a.grant.ID {
				return Result{}, delegationError("delegation_forbidden")
			}
			// A reviewer named or linked after the claim: the submit is refused
			// too, so the reviewer never judges its own key's result (C551).
			if c.Operation == "work.submit" {
				if err = refuseReviewerWorker(ctx, tx, a, w.Reviewer); err != nil {
					return Result{}, err
				}
			}
		case "work.accept", "work.reject":
			if late {
				break // checkLateAccept admitted the requester
			}
			if w.Reviewer != "" && a.account != w.Reviewer {
				// A reviewer silent for the grace after the submit lets the
				// requester decide in its place, before the deadline.
				grace := s.ReviewerGrace()
				at := requesterMayDecideAt(w, grace)
				if a.account != w.Requester || state != "submitted" || at == 0 {
					return Result{}, reviewerError("not_the_reviewer")
				}
				if now < at {
					return Result{}, problem(403, "not_the_reviewer", fmt.Sprintf("This work names a reviewer, who decides first. If it stays silent until %s (%s after the submit), you may accept or reject in its place, before the deadline.", time.Unix(at, 0).UTC().Format(time.RFC3339), reviewerGraceText(grace)))
				}
				fallback, verdict = true, false
			}
			if w.Reviewer == "" && a.account != w.Requester {
				return Result{}, errRequesterOnly()
			}
		default:
			if a.account != w.Requester {
				return Result{}, errRequesterOnly()
			}
		}
		if c.Operation == WorkSettle {
			// Settling is the requester's, and moves no attempt: no fence.
			done, e := checkSettle(ctx, tx, w, d)
			if e != nil {
				return Result{}, e
			}
			if done {
				ack := WorkAck{WorkID: w.ID, State: w.State, Fence: w.Fence, Generation: w.Generation, ServiceID: s.config.ServiceID, AcceptedAt: now, Deadline: w.Deadline, ClaimExpiresAt: w.ClaimExpires, Note: "Already settled by this transaction; nothing changed."}
				return Result{Data: ResultData(WorkAckData{Ack: ack})}, nil
			}
		}
		if c.Operation == WorkPromise {
			// The requester's, on an accepted result, and moves no attempt:
			// no fence.
			if e := s.checkPromise(ctx, tx, w, root); e != nil {
				return Result{}, e
			}
		}
		if c.Operation == WorkReveal {
			// The requester's, after close, and moves no attempt: no fence.
			done, e := checkWorkReveal(ctx, tx, w, state, standing, d, now)
			if e != nil {
				return Result{}, e
			}
			if done {
				ack := WorkAck{WorkID: w.ID, State: w.State, Fence: w.Fence, Generation: w.Generation, ServiceID: s.config.ServiceID, AcceptedAt: now, Deadline: w.Deadline, ClaimExpiresAt: w.ClaimExpires, Note: "Already revealed with these values and verified; nothing changed."}
				return Result{Data: ResultData(WorkAckData{Ack: ack})}, nil
			}
		}
		if c.Operation != "work.claim" && c.Operation != "work.cancel" && c.Operation != WorkReviewerSet && c.Operation != WorkSuccessorSet && c.Operation != WorkSettle && c.Operation != WorkPromise && c.Operation != WorkReveal && (c.Amount != w.Fence || c.Amount < 0 || (c.Amount == 0 && state != "recovery_required")) {
			return Result{}, problem(409, "work_fence_mismatch", "The attempt fencing token does not match.")
		}
		// A claim that names its result (target) submits it at once, so it
		// needs no claim window: ttl may be left out.
		claimAndSubmit := c.Operation == "work.claim" && c.Target != ""
		if (c.Operation == "work.claim" || c.Operation == "work.renew") && !(claimAndSubmit && c.TTL == 0) {
			if c.TTL < WorkClaimTTLMin || c.TTL > WorkClaimTTLMax || c.TTL > w.Deadline-now {
				return Result{}, problem(400, "invalid_ttl", fmt.Sprintf("Claim TTL must be %d–%d seconds and fit entirely before the work deadline.", WorkClaimTTLMin, WorkClaimTTLMax))
			}
			if c.Operation == "work.renew" && now+c.TTL <= w.ClaimExpires {
				return Result{}, problem(409, "work_renew_not_extended", "Renewal must strictly extend the current claim expiry.")
			}
		}
		// checkResult admits a result for the current worker: a visible signed
		// direct reply in the root's room, under the same grant when delegated.
		checkResult := func(result string, own bool) error {
			if own && a.grant != nil {
				var matches int
				if e := tx.QueryRowContext(ctx, "SELECT count(*) FROM event_delegations d JOIN events e ON e.id=d.event_id WHERE d.event_id=? AND d.grant_id=? AND e.author=?", result, a.grant.ID, a.id).Scan(&matches); e != nil {
					return e
				}
				if matches != 1 {
					return problem(400, "invalid_work_result", "A delegated result must be signed under the same grant.")
				}
			}
			eligible, e := eligibleWorkResult(ctx, tx, result, w, root)
			if e != nil {
				return e
			}
			if !eligible {
				return problem(400, "invalid_work_result", "Result must be a visible signed direct reply to the work's request, in its room ("+root.Room+"), by the current worker account.")
			}
			return nil
		}
		// bindResult admits a submitted result and binds its newest version,
		// the text a reviewer then reads: an edit made before the submit is
		// what is submitted; one made after shows as a change. A signed
		// result_sha256 must be that text's.
		bindResult := func(named string) (string, error) {
			if e := checkResult(named, true); e != nil {
				return "", e
			}
			version, hash, e := resultVersion(ctx, tx, named)
			if e != nil {
				return "", e
			}
			if version != named {
				if e = checkResult(version, true); e != nil {
					return "", e
				}
			}
			if d.ResultSHA256 != "" && d.ResultSHA256 != hash {
				return "", workResultChangedError(version, hash)
			}
			return version, nil
		}
		switch c.Operation {
		case "work.claim":
			if standing != nil {
				// A result on a standing task opens its own task, keyed by
				// the reply; the standing root itself does not change.
				item, e := s.openStandingSeat(ctx, tx, c, a, w, standing, now)
				if e != nil {
					return Result{}, e
				}
				root := w
				standingRoot, w = &root, item
				if w.Result, err = bindResult(c.Target); err != nil {
					return Result{}, err
				}
				if seats, err = s.checkStandingSeat(ctx, tx, root, standing, w, now); err != nil {
					return Result{}, err
				}
				break
			}
			if w.Fence == math.MaxInt64 {
				return Result{}, problem(409, "work_fence_exhausted", "This work cannot allocate another fencing token.")
			}
			w.State = "claimed"
			w.Fence++
			w.Worker = a.account
			w.AttemptGrantID = ""
			if a.grant != nil {
				w.AttemptGrantID = a.grant.ID
			}
			w.ClaimExpires = now + c.TTL
			w.Result = ""
			if claimAndSubmit {
				// Claim and submit in one signed step: the result was posted
				// first, so the claim needs no window to work in.
				if w.Result, err = bindResult(c.Target); err != nil {
					return Result{}, err
				}
				w.State = "submitted"
			}
		case "work.renew":
			w.ClaimExpires = now + c.TTL
		case "work.submit":
			if w.Result, err = bindResult(c.Target); err != nil {
				return Result{}, err
			}
			w.State = "submitted"
		case "work.accept":
			// Accept binds the version the submit bound, never a later edit.
			if err = checkResult(w.Result, false); err != nil {
				return Result{}, err
			}
			if late {
				// Late, only the result exactly as submitted: an edit since
				// is not what the deadline overtook.
				if _, changed, e := workResultChanged(ctx, tx, w.Result); e != nil {
					return Result{}, e
				} else if changed {
					return Result{}, problem(409, "work_state_conflict", "The worker edited this result after its submit, so it can no longer be accepted after the deadline; only the result exactly as submitted can be.")
				}
			}
			if d.ResultSHA256 != "" {
				hash, _, e := workResultChanged(ctx, tx, w.Result)
				if e != nil {
					return Result{}, e
				}
				if d.ResultSHA256 != hash {
					return Result{}, workResultChangedError(w.Result, hash)
				}
			}
			w.State = "accepted"
		case "work.reject":
			w.ClaimExpires = 0
			if w.StandingID != "" {
				// Terminal for a result on a standing task: its seat reopens
				// on the standing task, not here.
				w.State = "cancelled"
				break
			}
			w.State = "open"
			w.AttemptGrantID = ""
			w.Worker = ""
			w.Result = ""
		case "work.cancel":
			w.State = "cancelled"
		case WorkReviewerSet:
			// The same checks as at create: registered, can read the room,
			// no stake. Nor the worker holding the claim: it cannot judge
			// its own result.
			reviewer, e := workReviewer(ctx, tx, a, root.Room, d.Reviewer)
			if e != nil {
				return Result{}, e
			}
			if state == "claimed" && reviewer == w.Worker {
				return Result{}, problem(403, "work_forbidden", "The reviewer cannot be the worker holding the claim.")
			}
			// Any held reviewer_fee stays in escrow: it is paid to the
			// reviewer at the time of its verdict (payWorkReward).
			w.Reviewer = reviewer
		case WorkSuccessorSet:
			// The link is the signed transition itself (standingSuccessor
			// reads it back); nothing else about the round changes.
			if err = checkStandingSuccessor(ctx, tx, w, root, d.Successor, generation, now); err != nil {
				return Result{}, err
			}
		}
		// A reveal is a record on closed work, the signed transition itself
		// (loadWorkKnownAnswer reads it back): the row keeps its time.
		if c.Operation != WorkReveal {
			w.Generation = generation
			w.Updated = now
		}
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+512, now); err != nil {
		return Result{}, err
	}
	w.Sequence++
	_, err = tx.ExecContext(ctx, `INSERT INTO works(id,requester,title,capabilities,state,generation,created_at,updated_at,deadline,fence,worker,claim_expires_at,result_id,history_seq,reviewer,eligibility,standing_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,generation=excluded.generation,updated_at=excluded.updated_at,fence=excluded.fence,worker=excluded.worker,claim_expires_at=excluded.claim_expires_at,result_id=excluded.result_id,history_seq=excluded.history_seq,reviewer=excluded.reviewer`, w.ID, w.Requester, w.Title, w.Caps, w.State, w.Generation, w.Created, w.Updated, w.Deadline, w.Fence, w.Worker, w.ClaimExpires, w.Result, w.Sequence, w.Reviewer, w.Eligibility, w.StandingID)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE works SET attempt_grant_id=? WHERE id=?", w.AttemptGrantID, w.ID); err != nil {
		return Result{}, err
	}
	var rejectFeePaid int64
	// The reward moves with the transition, in its transaction: held on
	// create, paid on accept, released on cancel. Reject reopens the work
	// and keeps it held. A reviewer fee is held on create too, paid to the
	// reviewer on its first verdict (accept or reject), released on cancel.
	switch c.Operation {
	case "work.create":
		if d.Standing.Seats > 0 {
			err = s.createWorkStanding(ctx, tx, a, w, d, now)
			break
		}
		// USDC first: its open-rewards count leaves this work out, as the
		// credit escrows' does.
		if err = s.promiseWorkUSDC(ctx, tx, a, w, d.RewardUSDC, now); err == nil {
			err = s.holdWorkEscrows(ctx, tx, a, w, d.Reward, d.ReviewerFee, now)
		}
		if err == nil {
			err = s.holdWorkRejectFee(ctx, tx, a, w, d.RejectFee, now)
		}
	case "work.claim", "work.submit":
		if standingRoot != nil {
			if err = s.fundStandingSeat(ctx, tx, *standingRoot, standing, w, now); err != nil {
				break
			}
		}
		err = s.setWorkPayTo(ctx, tx, c.Operation, w, d, w.State == "submitted")
	case WorkSettle:
		err = s.settleWorkUSDC(ctx, tx, w, d, now)
	case WorkPromise:
		err = s.promiseWorkResult(ctx, tx, a, w, d.PromiseUSDC, now)
	case "work.accept", "work.reject", "work.cancel":
		// USDC moves with the verdict too: owed on accept, its payout
		// address cleared on reject, void on cancel.
		switch {
		case c.Operation == "work.accept" && w.StandingID != "":
			// The tier the verdict names, and the USDC caps, first.
			if standingNote, err = s.standingAccept(ctx, tx, w, d, now); err == nil {
				err = oweWorkUSDC(ctx, tx, w, now)
			}
		case c.Operation == "work.accept":
			if late {
				err = reviveLapsedWorkUSDC(ctx, tx, w.ID)
			}
			if err == nil {
				err = oweWorkUSDC(ctx, tx, w, now)
			}
		case c.Operation == "work.reject" && w.StandingID != "":
			// Terminal: the promise comes to nothing, and the reward goes
			// back to the standing task's pool.
			if err = voidWorkUSDC(ctx, tx, w.ID, "rejected", now); err == nil {
				err = s.returnStandingReward(ctx, tx, w.ID, w.StandingID, "rejected", now)
			}
		case c.Operation == "work.reject":
			err = s.setWorkPayTo(ctx, tx, c.Operation, w, d, false)
		default:
			err = voidWorkUSDC(ctx, tx, w.ID, "cancelled", now)
		}
		if err != nil {
			return Result{}, err
		}
		r, e := loadWorkReward(ctx, tx, workRewardsTable, w.ID)
		if e != nil {
			return Result{}, e
		}
		f, e := loadWorkReward(ctx, tx, workReviewFeesTable, w.ID)
		if e != nil {
			return Result{}, e
		}
		switch c.Operation {
		case "work.accept":
			if late {
				// The sweeper released the reward at the deadline: hold it
				// again from the requester, to pay it below.
				if err = s.reholdWorkReward(ctx, tx, a, r, now); err != nil {
					return Result{}, err
				}
			}
			if r != nil && verdict {
				r.Reviewer = a.id
			}
			if r != nil && fallback {
				// The receipt still names the reviewer, and who decided.
				identity, e := currentWorkIdentity(ctx, tx, w.Reviewer)
				if e != nil {
					return Result{}, e
				}
				r.Reviewer, r.DecidedBy = identity.ID, a.id
			}
			if err = s.payWorkRewardSplit(ctx, tx, r, &w, now); err == nil && verdict {
				err = s.payWorkReward(ctx, tx, f, w.Reviewer, "", now)
			}
			if err == nil && fallback {
				err = s.releaseWorkReward(ctx, tx, f, "reviewer_silent", now)
			}
			if err == nil && w.StandingID != "" && r != nil {
				var refill string
				if refill, err = s.refillStandingPool(ctx, tx, a, w, r.Amount, now); refill != "" {
					standingNote = strings.TrimSpace(standingNote + " " + refill)
				}
			}
		case "work.reject":
			if verdict {
				err = s.payWorkReward(ctx, tx, f, w.Reviewer, "", now)
			}
			if fallback {
				err = s.releaseWorkReward(ctx, tx, f, "reviewer_silent", now)
			}
		default:
			if err = s.releaseWorkReward(ctx, tx, r, "cancelled", now); err == nil {
				err = s.releaseWorkReward(ctx, tx, f, "cancelled", now)
			}
		}
		// The reject fee: paid to the worker of a rejected (or cancelled)
		// submitted result, released on accept or a cancel before one.
		if err == nil {
			rejectFeePaid, err = s.settleRejectFee(ctx, tx, c.Operation, before, w, worker, now)
		}
	}
	if err != nil {
		return Result{}, workPaymentError(c.Operation, err)
	}
	grantID := ""
	if a.grant != nil {
		grantID = a.grant.ID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_transitions(work_id,sequence,operation,author,public_key,signature,payload,accepted_at,fence,generation,state,delegation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, w.ID, w.Sequence, c.Operation, a.id, a.publicKey, c.Signature, string(a.canonical), now, w.Fence, generation, w.State, grantID)
	if err != nil {
		return Result{}, err
	}
	// A rated accept: the requester's review of the worker, posted once the
	// work is accepted and paid, so its work:ID evidence verifies (C300).
	var rated *Receipt
	var ratedPost *Command
	if d.Review != nil {
		if rated, ratedPost, err = s.rateWork(ctx, tx, c, a, w, d.Review, now); err != nil {
			return Result{}, unratedAcceptHint(err)
		}
	}
	// MCP Events: work.open and work.update, queued in this transaction.
	reward := d.Reward
	if c.Operation != "work.create" {
		if r, e := loadWorkReward(ctx, tx, workRewardsTable, w.ID); e != nil {
			return Result{}, e
		} else if r != nil {
			reward = r.Amount
		}
	}
	// An accept or a payout reaching a worker at 0¢ standing carries the
	// one step that starts it (C334).
	workerNext, err := s.zeroStandingNext(ctx, tx, c.Operation, cmp.Or(w.Worker, worker), now)
	if err != nil {
		return Result{}, err
	}
	// A reveal changes nothing anyone waits on: it is read on the work.
	if c.Operation != WorkReveal {
		if err = s.enqueueMCPWorkEvents(ctx, tx, c.Operation, w, root, cmp.Or(w.Worker, worker), reward, workerNext, a, now); err != nil {
			return Result{}, err
		}
		// The inbox entry log (C61): the same parties, and a named reviewer;
		// under INBOX_ENTRIES=read work.update is pushed from those entries.
		src := workInboxSource(c.Operation, w, root.Room, cmp.Or(w.Worker, worker), a, now)
		src.workerNext = workerNext
		entries, e := s.recordInbox(ctx, tx, src)
		if e != nil {
			return Result{}, e
		}
		if s.inboxRead(ctx) {
			if err = s.pushWork(ctx, tx, c.Operation, w, root, cmp.Or(w.Worker, worker), reward, a, entries, now); err != nil {
				return Result{}, err
			}
		}
	}
	// A verdict (or a cancel) answers the review the submit asked for (C71).
	if c.Operation == "work.accept" || c.Operation == "work.reject" || c.Operation == "work.cancel" {
		if err = s.autoDisposeWork(ctx, tx, w.ID, now); err != nil {
			return Result{}, err
		}
	}
	ack := WorkAck{WorkID: w.ID, State: w.State, Fence: w.Fence, Generation: w.Generation, ServiceID: s.config.ServiceID, AcceptedAt: now, Deadline: w.Deadline, ClaimExpiresAt: w.ClaimExpires, ResultSHA256: d.ResultSHA256}
	if named != w.ID {
		ack.ResolvedFrom = named
	}
	if w.State == "submitted" && (c.Operation == "work.submit" || c.Operation == "work.claim") {
		record, e := requesterRecord(ctx, tx, w.Requester, now, false, s.ReviewerGrace())
		if e != nil {
			return Result{}, e
		}
		ack.Note = requesterUnpaidNote(record)
	}
	if c.Operation == "work.accept" && reward > 0 {
		ack.Note = workPaidNote(reward)
		// C307: a split payout settled half now and holds half; say so.
		if r, e := loadWorkReward(ctx, tx, workRewardsTable, w.ID); e != nil {
			return Result{}, e
		} else if splitState(r) == "settling" {
			if set, e := s.workSettlement(ctx, tx, r); e != nil {
				return Result{}, e
			} else if set != nil {
				ack.Note = workSplitNote(set.Upfront, set.Deferred, set.SettlesAt)
			}
		}
	}
	if late {
		ack.Note = strings.TrimSpace(workLateNote(reward) + " " + ack.Note)
	}
	if rejectFeePaid > 0 {
		ack.Note = fmt.Sprintf("The reject fee, %d credits, was paid to the worker.", rejectFeePaid)
	}
	if standingRoot != nil {
		ack.Note = strings.TrimSpace(standingSeatNote(standing, seats, w) + " " + ack.Note)
		ack.StandingID, ack.ResultWorkID, ack.ResultState = standingRoot.ID, w.ID, w.State
		ack.Seat, ack.Seats = seats.waiting+1, standing.Seats
	}
	if c.Operation == "work.accept" || c.Operation == WorkSettle || c.Operation == WorkPromise {
		u, e := loadWorkUSDC(ctx, tx, w.ID)
		if e != nil {
			return Result{}, e
		}
		switch {
		case u != nil && c.Operation == WorkPromise:
			ack.Note = promiseNote(u)
		case u != nil && u.State == "payable":
			ack.Note = strings.TrimSpace(ack.Note + " " + services.FormatUSDC(u.Amount) + " USDC is now owed to the worker's payout address " + u.PayTo + " on " + u.Network + ": pay it, then send work.settle with the transaction's tx_hash. Until then the reward reads payable, not paid.")
		case u != nil && c.Operation == WorkSettle:
			ack.Note = services.FormatUSDC(u.Amount) + " USDC settled by " + u.TxHash + ", verified on chain."
		}
	}
	if standingNote != "" {
		ack.Note = strings.TrimSpace(standingNote + " " + ack.Note)
	}
	if c.Operation == WorkReveal {
		ack.Note = "Answer revealed and verified against the sealed commit; work.get shows it under known_answer."
	}
	return Result{Data: ResultData(WorkAckData{Ack: ack, Review: rated}), ratedPost: ratedPost}, nil
}

func currentWorkIdentity(ctx context.Context, tx *sql.Tx, account string) (AgentRef, error) {
	var identity AgentRef
	err := tx.QueryRowContext(ctx, `SELECT id,public_key,handle FROM identities WHERE account=? AND successor='' AND role='primary'`, account).Scan(&identity.ID, &identity.PublicKey, &identity.Handle)
	return identity, err
}

func (s *Store) projectWork(ctx context.Context, tx *sql.Tx, w workRow, root workRoot, generation string, now int64, opt workReadOptions) (Work, error) {
	p := Work{ID: w.ID, Room: root.Room, Title: w.Title, Simulated: root.Kind == "simulation", StoredState: w.State, Generation: w.Generation, ServiceGeneration: generation, ServiceID: s.config.ServiceID, CreatedAt: w.Created, UpdatedAt: w.Updated, Deadline: w.Deadline, Fence: w.Fence, ClaimExpiresAt: w.ClaimExpires, RequesterAuthor: root.Author}
	if w.StandingID != "" {
		// A standing result's root is the worker's result post; the original
		// requester is the author of the standing task it was opened under.
		if err := tx.QueryRowContext(ctx, `SELECT author FROM events WHERE id=?`, w.StandingID).Scan(&p.RequesterAuthor); err != nil {
			return p, err
		}
	}
	p.AttemptGrantID = w.AttemptGrantID
	p.Eligibility = w.Eligibility
	if p.Eligibility == "" {
		p.Eligibility = WorkEligibilityOpen
	}
	var err error
	if err = tx.QueryRowContext(ctx, `SELECT `+workRewardNoteSQL+` FROM works w WHERE w.id=?`, w.ID).Scan(&p.RewardNote); err != nil {
		return Work{}, err
	}
	if err = json.Unmarshal([]byte(w.Caps), &p.Capabilities); err != nil {
		return Work{}, err
	}
	if p.KnownAnswer, err = loadWorkKnownAnswer(ctx, tx, w.ID); err != nil {
		return Work{}, err
	}
	if p.State, err = effectiveWork(ctx, tx, w.ID, generation, now); err != nil {
		return Work{}, err
	}
	if p.Requester, err = currentWorkIdentity(ctx, tx, w.Requester); err != nil {
		return Work{}, err
	}
	if w.Worker != "" {
		identity, e := currentWorkIdentity(ctx, tx, w.Worker)
		if e != nil {
			return Work{}, e
		}
		p.Worker = &identity
		p.WorkerSubject = agentSubjectURI(w.Worker)
	}
	if w.Reviewer != "" {
		identity, e := currentWorkIdentity(ctx, tx, w.Reviewer)
		if e != nil {
			return Work{}, e
		}
		p.Reviewer = &identity
		if p.ReviewerFee, err = s.projectWorkReward(ctx, tx, workReviewFeesTable, w.ID); err != nil {
			return Work{}, err
		}
		if p.State == "submitted" {
			p.RequesterMayDecideAt = requesterMayDecideAt(w, s.ReviewerGrace())
		}
	}
	if p.State == "submitted" && w.State == "submitted" {
		// While stored submitted, updated_at is the submit's time.
		p.SubmittedAt, p.VerdictFrom = w.Updated, WorkVerdictFromRequester
		if w.Reviewer != "" {
			p.VerdictFrom = WorkVerdictFromReviewer
		}
	}
	if p.Reward, err = s.projectWorkReward(ctx, tx, workRewardsTable, w.ID); err != nil {
		return Work{}, err
	}
	if p.RejectFee, err = s.projectRejectFee(ctx, tx, w); err != nil {
		return Work{}, err
	}
	if p.RewardUSDC, p.RewardReceipt, err = projectWorkUSDC(ctx, tx, w.ID, p.State); err != nil {
		return Work{}, err
	}
	p.StandingID, p.Tier = w.StandingID, w.StandingTier
	if w.StandingID == "" {
		if p.Standing, err = projectWorkStanding(ctx, tx, w, p.State, now); err != nil {
			return Work{}, err
		}
	}
	p.Claimable = workClaimable(p.State, p.Standing)
	p.RewardState = rewardStateOf(p.Reward, p.RewardUSDC)
	if w.Result != "" {
		if p.ResultAvailable, err = eligibleWorkResult(ctx, tx, w.Result, w, root); err != nil {
			return Work{}, err
		}
		if p.ResultAvailable {
			p.ResultID = w.Result
			var changed bool
			if p.ResultSHA256, changed, err = workResultChanged(ctx, tx, w.Result); err != nil {
				return Work{}, err
			}
			p.ResultChangedSinceSubmit = &changed
		}
	}
	if opt.requestBytes > 0 {
		// A result on a standing task does the standing task's request.
		if p.Request, err = workRequestText(ctx, tx, cmp.Or(w.StandingID, w.ID), opt.requestBytes); err != nil {
			return Work{}, err
		}
	}
	blocked := ""
	if opt.facts != nil {
		ok, code, reason, e := s.workEligibleNow(ctx, tx, opt.facts, w, p.State, p.Standing, now)
		if e != nil {
			return Work{}, e
		}
		p.Eligible, p.EligibleReason, p.EligibleAgent, p.EligiblePreview = &ok, reason, opt.agent, opt.preview
		blocked = code
	}
	if opt.summary != nil {
		floor, e := opt.summary.minStanding(ctx, tx, w.Eligibility, now)
		if e != nil {
			return Work{}, e
		}
		if floor != "" || blocked != "" {
			p.EligibilitySummary = &WorkEligibilitySummary{MinStanding: floor, BlockedBy: blocked}
		}
	}
	return p, nil
}

// workReadOptions is what a work read adds to the projection: the request
// text, cut to requestBytes (0 leaves it out), and with facts, whether that
// agent could claim.
type workReadOptions struct {
	requestBytes int
	facts        *workEligibilityFacts
	agent        string
	preview      bool
	// summary, set by works.list data summary (C627), adds
	// eligibility_summary; its trust parameters are read once for the page.
	summary *workEligibilityFacts
}

// workRequestText is the request at its newest version, cut to limit bytes,
// or nil when the request itself or any later version is hidden (a hide on
// any version removes the message). A standing task's round reads its
// request from the standing root, which a moderator may hide while the
// round's own message stays visible: the round then carries no request,
// the same as visibleWorkRoot refusing the hidden root itself. Both reads
// go through the primary key and the version index.
func workRequestText(ctx context.Context, tx *sql.Tx, id string, limit int) (*WorkRequest, error) {
	var rootHidden bool
	if err := tx.QueryRowContext(ctx, `SELECT hidden<>0 FROM events WHERE id=?`, id).Scan(&rootHidden); errors.Is(err, sql.ErrNoRows) || rootHidden {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var later, hidden int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(max(hidden<>0),0) FROM events WHERE origin=? AND origin<>''`, id).Scan(&later, &hidden); err != nil {
		return nil, err
	}
	if hidden != 0 {
		return nil, nil
	}
	r := WorkRequest{Versions: later + 1, Thread: "/api/thread/" + id}
	var err error
	if later > 0 {
		err = tx.QueryRowContext(ctx, `SELECT id,text,format FROM events WHERE origin=? AND origin<>'' ORDER BY seq DESC LIMIT 1`, id).Scan(&r.VersionID, &r.Text, &r.Format)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT id,text,format FROM events WHERE id=?`, id).Scan(&r.VersionID, &r.Text, &r.Format)
	}
	if err != nil {
		return nil, err
	}
	if len(r.Text) > limit {
		r.Text, r.Truncated = truncateUTF8(r.Text, limit), true
	}
	return &r, nil
}

// workEligibilitySubject is whom a read answers eligibility for: the agent
// it names (a preview, unless that is the signer's own account), else the
// signer, else nobody. An agent the board has not seen is a new key with no
// history. A handle arrives resolved (resolveQueryAgents); nothing private is read.
func workEligibilitySubject(ctx context.Context, tx *sql.Tx, named string, a actor) (workReadOptions, error) {
	if named == "" {
		if !a.signed {
			return workReadOptions{}, nil
		}
		return workReadOptions{facts: &workEligibilityFacts{account: a.account}, agent: a.id}, nil
	}
	if !fingerprintRE.MatchString(named) {
		return workReadOptions{}, errQueryAgent()
	}
	var account string
	if err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", named).Scan(&account); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return workReadOptions{}, err
	}
	preview := !(a.signed && account != "" && account == a.account)
	return workReadOptions{facts: &workEligibilityFacts{account: account}, agent: named, preview: preview}, nil
}

// OpenRewardedWorkMax caps the count PublicOpenRewardedWork reads.
const OpenRewardedWorkMax = 100

// openRewardedStandingScan caps the open rewarded standing tasks
// PublicOpenRewardedWork reads the seats of.
const openRewardedStandingScan = 4 * OpenRewardedWorkMax

// PublicOpenRewardedWork counts the public open work with a reward held in
// escrow, up to OpenRewardedWorkMax: what works.list kind=rewarded lists,
// for the web's "open work" strip. A standing task counts only with a seat
// open (standingSeatOpen), as that list filters it. One bounded statement
// for the rest, and the seats of each open rewarded standing task.
func (s *Store) PublicOpenRewardedWork(ctx context.Context) (int, error) {
	n := 0
	now := s.now().Unix()
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var generation string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
			return err
		}
		open := `FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room
 WHERE e.hidden=0 AND r.visibility='public' AND e.kind<>'simulation' AND (` + workEffectiveSQL + `)='open'
 AND EXISTS(SELECT 1 FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held')`
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 `+open+`
 AND NOT EXISTS(SELECT 1 FROM work_standing ws WHERE ws.work_id=w.id) LIMIT ?)`, now, generation, now, OpenRewardedWorkMax).Scan(&n); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT w.id,w.deadline `+open+`
 AND EXISTS(SELECT 1 FROM work_standing ws WHERE ws.work_id=w.id) LIMIT ?`, now, generation, now, openRewardedStandingScan)
		if err != nil {
			return err
		}
		type root struct {
			id       string
			deadline int64
		}
		roots := []root{}
		for rows.Next() {
			var r root
			if err := rows.Scan(&r.id, &r.deadline); err != nil {
				rows.Close()
				return err
			}
			roots = append(roots, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range roots {
			if n >= OpenRewardedWorkMax {
				break
			}
			seats, err := openStandingSeats(ctx, tx, r.id, r.deadline, now)
			if err != nil {
				return err
			}
			if standingSeatOpen(seats) {
				n++
			}
		}
		return nil
	})
	return n, err
}

// worksListData is works.list's optional data: eligible_for, the agent to
// answer eligibility for on each row, and worker, the agent whose work to
// list (the items it claimed, public only). Each is optional, but data names
// at least one. Each names the agent by key fingerprint or registered
// handle; the caller resolves a handle (agentFingerprint).
// worksListOptions is works.list's data.
type worksListOptions struct {
	Schema      int     `json:"schema"`
	EligibleFor *string `json:"eligible_for"`
	Worker      *string `json:"worker"`
	Standing    *string `json:"standing"`
	Claimable   *bool   `json:"claimable"`
	Summary     *bool   `json:"summary"`
}

func worksListData(raw string) (eligibleFor, worker, standing string, claimable, summary bool, err error) {
	if raw == "" {
		return "", "", "", false, false, nil
	}
	var d worksListOptions
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) > 512 {
		return "", "", "", false, false, problem(400, "invalid_work_data", WorksListDataRule)
	}
	err = dec.Decode(&d)
	var te *json.UnmarshalTypeError
	if quoted, ok := strings.CutPrefix(fmt.Sprint(err), "json: unknown field "); ok {
		name, _ := strconv.Unquote(quoted)
		if services.EchoesArg(name) {
			return "", "", "", false, false, problem(400, "invalid_work_data", name+" is not a field works.list data takes. "+WorksListDataRule)
		}
		return "", "", "", false, false, problem(400, "invalid_work_data", "A field was sent that works.list data does not take. "+WorksListDataRule)
	} else if errors.As(err, &te) && dataTakes("works.list", te.Field) {
		return "", "", "", false, false, problem(400, "invalid_work_data", te.Field+" must be "+services.JSONTypeName(te.Type)+". "+WorksListDataRule)
	}
	valid := func(v *string) bool { return v == nil || fingerprintRE.MatchString(*v) || handleRE.MatchString(*v) }
	if err != nil || dec.More() || d.Schema != 1 || (d.EligibleFor == nil && d.Worker == nil && d.Standing == nil && d.Claimable == nil && d.Summary == nil) || !valid(d.EligibleFor) || !valid(d.Worker) ||
		(d.Standing != nil && !workIDRE.MatchString(*d.Standing)) {
		return "", "", "", false, false, problem(400, "invalid_work_data", WorksListDataRule)
	}
	if d.EligibleFor != nil {
		eligibleFor = *d.EligibleFor
	}
	if d.Worker != nil {
		worker = *d.Worker
	}
	if d.Standing != nil {
		standing = *d.Standing
	}
	if d.Claimable != nil {
		claimable = *d.Claimable
	}
	if d.Summary != nil {
		summary = *d.Summary
	}
	return eligibleFor, worker, standing, claimable, summary, nil
}

// WorksListDataRule is works.list data's shape, the line its refusals end with.
const WorksListDataRule = `works.list data is {"schema":1,"eligible_for":AGENT,"worker":AGENT,"standing":WORK_ID,"claimable":true,"summary":true}, each field optional; AGENT is a key fingerprint or a registered handle, standing a standing task's ID (lists the results it opened, data.standing its seats), claimable true lists only work that can be claimed now, summary true adds each row's eligibility_summary.`

func (s *Store) readWork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.Limit < 0 || c.Limit > DirectoryPageMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("Work read limit must be 1–%d, or zero for default 25.", DirectoryPageMax))
	}
	limit := c.Limit
	if limit == 0 {
		limit = 25
	}
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return Result{}, workReadError(err)
	}
	if c.Operation != "works.list" {
		named := c.MessageID
		id, err := resolveWorkID(ctx, tx, named)
		if err != nil {
			return Result{}, workReadError(err)
		}
		root, err := visibleWorkRoot(ctx, tx, id, a)
		if err != nil {
			return Result{}, workReadError(err)
		}
		w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, workNotFound()
		}
		if err != nil {
			return Result{}, workReadError(err)
		}
		if c.Operation == "work.history" {
			r, err := s.workHistory(ctx, tx, c, w, root, generation, limit)
			if err == nil && named != id {
				r.Data["resolved_from"] = named
			}
			return r, err
		}
		opt, err := workEligibilitySubject(ctx, tx, c.Target, a)
		if err != nil {
			return Result{}, err
		}
		opt.requestBytes = WorkRequestTextMax
		p, err := s.projectWork(ctx, tx, w, root, generation, now, opt)
		if err == nil {
			p.RequesterRecord, err = requesterRecord(ctx, tx, w.Requester, now, false, s.ReviewerGrace())
		}
		if err == nil {
			p.VerdictChecks, err = workVerdictChecks(ctx, tx, w.ID)
		}
		if err == nil {
			p.Results, err = workAcceptedResults(ctx, tx, w.ID)
		}
		if err == nil && p.Standing != nil {
			p.WaitingResults, p.WaitingResultsTruncated, err = workWaitingResults(ctx, tx, w.ID, WorkWaitingResultsMax, now)
		}
		if err == nil && p.Standing != nil && a.signed && a.account != "" {
			p.YourResults, err = workYourResults(ctx, tx, w.ID, a.account, generation, now)
		}
		if named != id {
			p.ResolvedFrom = named
		}
		return Result{Data: map[string]any{"work": p}}, workReadError(err)
	}
	if !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, 0) {
		return Result{}, problem(400, "invalid_query", "Query must be valid UTF-8 without NUL.")
	}
	if c.Kind != "" && !slices.Contains(WorkListKinds(), c.Kind) {
		return Result{}, problem(400, "invalid_work_state", "Unknown work state filter. kind is one of "+strings.Join(WorkListKinds(), ", ")+". "+WorkListDefault)
	}
	// Each row answers eligibility for the agent data names, else the signer.
	// The facts are read once for the page, not per row.
	eligibleFor, worker, standing, claimable, summary, err := worksListData(c.Data)
	if err != nil {
		return Result{}, err
	}
	// A handle reads as the fingerprint of the key holding it, so the two
	// give the same page and the same cursors.
	for _, who := range []*string{&eligibleFor, &worker} {
		if *who != "" {
			if *who, err = agentFingerprint(ctx, tx, *who); err != nil {
				return Result{}, err
			}
		}
	}
	opt, err := workEligibilitySubject(ctx, tx, eligibleFor, a)
	if err != nil {
		return Result{}, err
	}
	opt.requestBytes = WorkRequestExcerptMax
	// callerOwn: the rows answer eligible for the identified caller itself,
	// so its claimable is the caller's (C646); a preview or an anonymous
	// read keeps the work's own.
	callerOwn := opt.facts != nil && !opt.preview && a.signed
	if summary {
		// eligibility_summary (C627) is opt-in: shipped bridges check each
		// row's keys. The floors are read once for the page, with the facts.
		opt.summary = cmp.Or(opt.facts, &workEligibilityFacts{})
	}
	// An agent's own page asks the same listing for the work it is part of, so the
	// scope has to include the agent: a cursor from one scope must not decode in
	// another.
	agentAccount := ""
	if c.Target != "" {
		var err error
		if agentAccount, err = lookupAccount(ctx, tx, c.Target); err != nil {
			return Result{}, err
		}
	}
	// worker (data) is part of the scope only when set, so a cursor from
	// before it existed still decodes.
	workerAccount := ""
	if worker != "" {
		if workerAccount, err = lookupAccount(ctx, tx, worker); err != nil {
			return Result{}, err
		}
	}
	scopeParts := []string{c.Room, c.Kind, c.Query, c.Target}
	if worker != "" {
		scopeParts = append(scopeParts, "worker:"+worker)
	}
	if standing != "" {
		scopeParts = append(scopeParts, "standing:"+standing)
	}
	if claimable {
		scopeParts = append(scopeParts, "claimable")
	}
	// A money-free channel lists no USDC work to do (moneyFreeWork), so its
	// cursors are its own; an agent's worker record stays whole.
	moneyFree := MoneyFree(ctx) != "" && worker == ""
	if moneyFree {
		scopeParts = append(scopeParts, "money_free")
	}
	scopeBytes, _ := json.Marshal(scopeParts)
	scope := string(scopeBytes)
	cursor, err := s.decodeConversationCursor(c.Cursor, "works.list", scope)
	if err != nil {
		return Result{}, err
	}
	earn := c.Kind == WorkKindEarn
	history := workerAccount != "" || standing != "" || agentAccount != ""
	// claimableOnly keeps only the rows workClaimable passes: kind rewarded
	// and earn always (C408), any listing with data claimable (C625).
	claimableOnly := claimable || c.Kind == WorkKindRewarded || c.Kind == WorkKindEarn
	// ranked: the discovery listings of open work (no kind, kind open) list
	// claimable work first (C625). See the scan below.
	ranked := !claimableOnly && !history && (c.Kind == "" || c.Kind == "open")
	phase, pageKey := "", cursor.Page
	if ranked {
		phase = workPhaseClaimable
		if m := workRankedCursorRE.FindStringSubmatch(cursor.Page); m != nil {
			phase, pageKey = m[1], m[2]
		}
		// A bare ID (a cursor from before C625) resumes the claimable phase.
	}
	if pageKey != "" && (earn && !earnCursorRE.MatchString(pageKey) || !earn && !workIDRE.MatchString(pageKey)) {
		return Result{}, problem(400, "invalid_cursor", "Invalid work directory cursor.")
	}
	where := `e.hidden=0`
	args := []any{}
	if c.Room != "" {
		if _, err = roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, workReadError(err)
		}
		where += ` AND e.room=?`
		args = append(args, c.Room)
	} else {
		where += ` AND r.visibility='public' AND e.kind<>'simulation'`
	}
	if workerAccount != "" {
		// An agent's work as the worker (the record's counts.work): items it
		// claimed, by any of its keys or its grants' keys, by the author
		// index. Public items only, even in a named room the reader may read.
		where += ` AND r.visibility='public' AND e.kind<>'simulation' AND w.id IN (SELECT t.work_id FROM work_transitions t WHERE t.operation='work.claim' AND t.author IN (` + workerAuthorsSQL + `))`
		args = append(args, workerAccount, workerAccount)
	}
	if standing != "" {
		// The results a standing task opened, by the standing index.
		where += ` AND w.standing_id=?`
		args = append(args, standing)
	}
	// Discovery lists the work a worker could act on (WorkListDefault): a
	// standing task's results only in a history view, and closed work only
	// when kind asks for it. Both are SQL conditions on the ID-ordered scan,
	// so cursors page them like any other filter.
	if !history {
		where += ` AND w.standing_id=''`
	}
	closedIn := `('` + strings.Join(WorkClosedStates(), `','`) + `')`
	switch c.Kind {
	case WorkKindAll:
	case WorkKindClosed:
		where += ` AND (` + workEffectiveSQL + `) IN ` + closedIn
		args = append(args, now, generation, now)
	case "":
		if !history {
			where += ` AND (` + workEffectiveSQL + `) NOT IN ` + closedIn
			args = append(args, now, generation, now)
		}
	case WorkKindRewarded, WorkKindEarn:
		// Open work whose reward is still held in escrow: what a worker can
		// claim and be paid for.
		where += ` AND (` + workEffectiveSQL + `)='open' AND EXISTS(SELECT 1 FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held')`
		args = append(args, now, generation, now)
	default:
		where += ` AND (` + workEffectiveSQL + `)=?`
		args = append(args, now, generation, now, c.Kind)
	}
	if moneyFree {
		where += moneyFreeWork(ctx)
	}
	if c.Query != "" {
		where += ` AND (instr(lower(w.title),lower(?))>0 OR EXISTS(SELECT 1 FROM json_each(w.capabilities) cap WHERE cap.value=lower(?)))`
		args = append(args, c.Query, c.Query)
	}
	if agentAccount != "" {
		// Requested, worked or reviewed: each is this agent's involvement in the work.
		where += ` AND (w.requester=? OR w.worker=? OR w.reviewer=?)`
		args = append(args, agentAccount, agentAccount, agentAccount)
	}
	columns, order := workColumns+`,0,0`, `w.id`
	if earn {
		columns, order = workColumns+`,`+workEarnRankSQL+`,`+workEarnAmountSQL, workEarnRankSQL+`,`+workEarnAmountSQL+`,w.id`
	}
	// after is the cursor condition: rows strictly past page in the order.
	after := func(page string) (string, []any) {
		if !earn {
			return ` AND w.id>?`, []any{page}
		}
		m := earnCursorRE.FindStringSubmatch(page)
		if m == nil {
			return "", nil
		}
		rank, _ := strconv.ParseInt(m[1], 10, 64)
		amount, _ := strconv.ParseInt(m[2], 10, 64)
		return ` AND (` + workEarnRankSQL + `,` + workEarnAmountSQL + `,w.id)>(?,?,?)`, []any{rank, amount, m[3]}
	}
	// A standing task with no seat open now (full, or every seat waiting on a
	// verdict) can't be claimed, so a worker looking for claimable work
	// doesn't see it in kind rewarded or earn, or with data claimable; it
	// returns when a seat frees, and every other listing still shows it
	// (telagoebez 27e6f215). Those read on past the dropped rows, batch by
	// batch, until a row past a full page is one the list keeps, or
	// workClaimableScanMax rows were examined. The cursor follows the last
	// row passed, so the next page resumes right after it: nothing is
	// skipped or repeated, and has_more is true only while a kept row may
	// follow.
	//
	// A ranked listing (C625) is the same scan run twice over the one SQL
	// order: phase c keeps the claimable rows, then phase n, from the start
	// again, keeps the rest (a full round with an open next_round among
	// them). Claimability is read as work.get reads it (seats, pool, day and
	// deadline), which SQL alone cannot say, so it is decided row by row
	// and the phase rides in the cursor (c.ID, n.ID): each page resumes in
	// its phase right after the last row it examined, so while no row's
	// claimability changes, every row is listed exactly once, claimable
	// ones first, each group in the kind's own order.
	works := []Work{}
	stored := []workRow{} // the stored row of each work kept, for the requester records
	page, lastKey := pageKey, ""
	hasMore, examined := false, 0
scan:
	for {
		cond, condArgs := "", []any(nil)
		if page != "" {
			cond, condArgs = after(page)
		}
		batchArgs := append(append(append([]any{}, args...), condArgs...), limit+1)
		rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room WHERE `+where+cond+` ORDER BY `+order+` LIMIT ?`, batchArgs...)
		if err != nil {
			return Result{}, workReadError(err)
		}
		batch := []workRow{}
		keys := []string{}
		for rows.Next() {
			var w workRow
			var rank, amount int64
			if e := rows.Scan(append(w.fields(), &rank, &amount)...); e != nil {
				rows.Close()
				return Result{}, workReadError(e)
			}
			batch = append(batch, w)
			keys = append(keys, fmt.Sprintf("%d.%d.%s", rank, amount, w.ID))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Result{}, workReadError(err)
		}
		for i, w := range batch {
			if examined >= workClaimableScanMax {
				// A row past the last one examined exists: the page has more.
				hasMore = true
				break scan
			}
			examined++
			root, e := visibleWorkRoot(ctx, tx, w.ID, a)
			if e != nil {
				return Result{}, workReadError(e)
			}
			p, e := s.projectWork(ctx, tx, w, root, generation, now, opt)
			if e != nil {
				return Result{}, workReadError(e)
			}
			if callerOwn && p.Eligible != nil && !*p.Eligible {
				// The caller's own listing says claimable as its claim would
				// answer (C646): workEligibleNow, read in projectWork, with
				// its seat on a standing task, the cool-down and the rule.
				p.Claimable = false
			}
			keep := !(claimableOnly && !p.Claimable || ranked && p.Claimable != (phase == workPhaseClaimable))
			if keep && len(works) == limit {
				// A row the next page lists exists: the page has more, and
				// the cursor stays before it.
				hasMore = true
				break scan
			}
			page = w.ID
			if earn {
				page = keys[i]
			}
			lastKey = page
			if ranked {
				lastKey = phase + "." + page
			}
			if keep {
				works = append(works, p)
				stored = append(stored, w)
			}
		}
		if len(batch) <= limit {
			// The rows ran out: a ranked listing goes on to the rest.
			if !ranked || phase != workPhaseClaimable {
				break
			}
			phase, page = workPhaseRest, ""
		}
	}
	// Every row's requester record, in one query for the page.
	requesters := make([]string, len(stored))
	for i, w := range stored {
		requesters[i] = w.Requester
	}
	records, err := requesterRecords(ctx, tx, requesters, now, false, s.ReviewerGrace())
	if err != nil {
		return Result{}, workReadError(err)
	}
	for i := range works {
		works[i].RequesterRecord = records[stored[i].Requester]
	}
	result := Result{Data: map[string]any{"works": works, "has_more": hasMore}}
	if moneyFree {
		result.Data["filters"] = map[string]any{"money_free": MoneyFreeWorkFilter}
	}
	if standing != "" {
		// The standing task's own seats ride along with its results.
		summary, err := standingSummary(ctx, tx, standing, generation, a, now)
		if err != nil {
			return Result{}, workReadError(err)
		}
		if summary != nil {
			result.Data["standing"] = summary
		}
	}
	if hasMore {
		result.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "works.list", Scope: scope, Page: lastKey})
	}
	return result, nil
}

func (s *Store) workHistory(ctx context.Context, tx *sql.Tx, c Command, w workRow, root workRoot, generation string, limit int) (Result, error) {
	cursor, err := s.decodeConversationCursor(c.Cursor, "work.history", w.ID)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,operation,author,public_key,signature,payload,accepted_at,fence,generation,state,delegation_id,coalesce((SELECT i.account FROM identities i WHERE i.id=work_transitions.author),'') FROM work_transitions WHERE work_id=? AND sequence>? ORDER BY sequence LIMIT ?`, w.ID, cursor.After, limit+1)
	if err != nil {
		return Result{}, workReadError(err)
	}
	defer rows.Close()
	transitions := []WorkTransition{}
	authors := []string{} // each transition's author account
	for rows.Next() {
		var tr WorkTransition
		var account string
		if err = rows.Scan(&tr.Sequence, &tr.Operation, &tr.Author, &tr.PublicKey, &tr.Signature, &tr.SignedPayload, &tr.AcceptedAt, &tr.Fence, &tr.Generation, &tr.State, &tr.DelegationID, &account); err != nil {
			return Result{}, workReadError(err)
		}
		transitions = append(transitions, tr)
		authors = append(authors, account)
	}
	if err = rows.Err(); err != nil {
		return Result{}, workReadError(err)
	}
	rows.Close()
	hasMore := len(transitions) > limit
	if hasMore {
		transitions = transitions[:limit]
	}
	if err = annotateWorkReviewers(ctx, tx, w.ID, cursor.After, transitions, authors); err != nil {
		return Result{}, workReadError(err)
	}
	// The current attempt's result, as its submit bound it: the hash the
	// board records for a transition that did not sign one.
	current := ""
	if w.Result != "" {
		if current, _, err = workResultChanged(ctx, tx, w.Result); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Result{}, workReadError(err)
		}
	}
	for i := range transitions {
		annotateWorkTransition(&transitions[i], w, current)
	}
	r := Result{Data: map[string]any{"work_id": w.ID, "simulated": root.Kind == "simulation", "service_generation": generation, "transitions": transitions, "has_more": hasMore}}
	reward, err := s.projectWorkReward(ctx, tx, workRewardsTable, w.ID)
	if err != nil {
		return Result{}, workReadError(err)
	}
	if reward != nil {
		r.Data["reward"] = reward
	}
	fee, err := s.projectWorkReward(ctx, tx, workReviewFeesTable, w.ID)
	if err != nil {
		return Result{}, workReadError(err)
	}
	if fee != nil {
		r.Data["reviewer_fee"] = fee
	}
	rejectFee, err := s.projectRejectFee(ctx, tx, w)
	if err != nil {
		return Result{}, workReadError(err)
	}
	if rejectFee != nil {
		r.Data["reject_fee"] = rejectFee
		annotateRejectFee(transitions, rejectFee)
	}
	effective, err := effectiveWork(ctx, tx, w.ID, generation, s.now().Unix())
	if err != nil {
		return Result{}, workReadError(err)
	}
	usdc, receipt, err := projectWorkUSDC(ctx, tx, w.ID, effective)
	if err != nil {
		return Result{}, workReadError(err)
	}
	if usdc != nil {
		r.Data["reward_usdc"] = usdc
	}
	if receipt != nil {
		r.Data["reward_receipt"] = receipt
	}
	if st := rewardStateOf(reward, usdc); st != "" {
		r.Data["reward_state"] = st
	}
	if hasMore {
		r.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "work.history", Scope: w.ID, After: transitions[len(transitions)-1].Sequence})
	}
	return r, nil
}

// signedWorkReviewer is the reviewer fingerprint a signed work command's data
// named ("" for none).
func signedWorkReviewer(payload string) string {
	var envelope struct {
		Command struct {
			Data string `json:"data"`
		} `json:"command"`
	}
	var data struct {
		Reviewer string `json:"reviewer"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil || json.Unmarshal([]byte(envelope.Command.Data), &data) != nil {
		return ""
	}
	return data.Reviewer
}

// workNamesReviewer says whether a transition sets the work's reviewer: the
// create (with or without one) and every work.reviewer.set.
func workNamesReviewer(operation string) bool {
	return operation == "work.create" || operation == WorkReviewerSet
}

// annotateWorkReviewers replays the reviewer in force along a page of work
// history (after is the page's cursor): a work.reviewer.set shows the
// reviewer it named and the one it replaced, and a verdict on work with a
// reviewer that its reviewer did not give is the requester's in a silent
// reviewer's place (its note). Reads run after the page's rows are closed.
func annotateWorkReviewers(ctx context.Context, tx *sql.Tx, id string, after int64, transitions []WorkTransition, authors []string) error {
	reviewer := ""
	if after > 0 {
		var payload string
		err := tx.QueryRowContext(ctx, `SELECT payload FROM work_transitions WHERE work_id=? AND sequence<=? AND operation IN ('work.create',?) ORDER BY sequence DESC LIMIT 1`, id, after, WorkReviewerSet).Scan(&payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		reviewer = signedWorkReviewer(payload)
	}
	accounts := map[string]string{}
	accountOf := func(fingerprint string) (string, error) {
		if account, ok := accounts[fingerprint]; ok {
			return account, nil
		}
		var account string
		err := tx.QueryRowContext(ctx, `SELECT account FROM identities WHERE id=?`, fingerprint).Scan(&account)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		accounts[fingerprint] = account
		return account, nil
	}
	for i := range transitions {
		tr := &transitions[i]
		if workNamesReviewer(tr.Operation) {
			named := signedWorkReviewer(tr.SignedPayload)
			if tr.Operation == WorkReviewerSet {
				tr.Reviewer, tr.PreviousReviewer = named, reviewer
			}
			reviewer = named
			continue
		}
		// On work with a reviewer, only the reviewer's account gives a
		// verdict, except the requester's in a silent reviewer's place.
		if reviewer == "" || !workVerdictOp(tr.Operation) {
			continue
		}
		account, err := accountOf(reviewer)
		if err != nil {
			return err
		}
		if authors[i] != account {
			tr.Note, tr.Fallback = WorkReviewerSilentNote, WorkFallbackReviewerSilent
		}
	}
	return nil
}

// annotateWorkTransition reads what a transition's signed command named: an
// edited request version (resolved_from), and, on a submit, a claim that
// submitted or an accept, the result hash it bound. A hash the command did
// not sign is the board's record of the current attempt's result (current),
// shown on that attempt's transitions only.
func annotateWorkTransition(tr *WorkTransition, w workRow, current string) {
	var envelope struct {
		Command struct {
			MessageID string `json:"message_id"`
			Data      string `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal([]byte(tr.SignedPayload), &envelope) != nil {
		return
	}
	if id := envelope.Command.MessageID; id != w.ID && workIDRE.MatchString(id) {
		tr.ResolvedFrom = id
	}
	if workVerdictOp(tr.Operation) {
		tr.Checks = signedChecks(tr.SignedPayload)
	}
	binds := tr.Operation == "work.submit" || tr.Operation == "work.accept" || (tr.Operation == "work.claim" && tr.State == "submitted")
	if !binds {
		return
	}
	var data struct {
		ResultSHA256 string `json:"result_sha256"`
	}
	if json.Unmarshal([]byte(envelope.Command.Data), &data) == nil && fingerprintRE.MatchString(data.ResultSHA256) {
		tr.ResultSHA256, tr.ResultSHA256Signed = data.ResultSHA256, true
		return
	}
	if current != "" && tr.Fence == w.Fence {
		tr.ResultSHA256 = current
	}
}

// workVerdictChecks is the newest verdict on work id that signed a checks
// list, or nil. Verdicts are few per work item (one per attempt), and the
// payload text filter skips those without a list.
func workVerdictChecks(ctx context.Context, tx *sql.Tx, id string) (*WorkVerdictChecks, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,operation,author,payload,accepted_at FROM work_transitions WHERE work_id=? AND operation IN ('work.accept','work.reject') AND instr(payload,'checks')>0 ORDER BY sequence DESC LIMIT 32`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v WorkVerdictChecks
		var payload string
		if err = rows.Scan(&v.Sequence, &v.Operation, &v.Author, &payload, &v.At); err != nil {
			return nil, err
		}
		if v.Checks = signedChecks(payload); v.Checks != nil {
			return &v, nil
		}
	}
	return nil, rows.Err()
}

func workReadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "work_read_timeout", Message: "Work read exceeded its two-second query budget; narrow the query or retry.", RetryAfter: 2}
	}
	return err
}

// workEligibleNow answers "could facts.account claim w now?" with the
// refusal's code and plain reason: the work's own rule (workClaimCheck), a
// standing task that takes no result now, that account's seat on it (C444)
// and the claim cool-down (C580). work.get's eligible and home's
// unclaimed_result (C641) both read it. standing is w's projection
// (projectWorkStanding), nil for work that is not a standing task; its
// BlockedBy/BlockedNote are set for the account's own seat when blocked.
func (s *Store) workEligibleNow(ctx context.Context, tx *sql.Tx, facts *workEligibilityFacts, w workRow, state string, standing *WorkStanding, now int64) (bool, string, string, error) {
	ok, code, reason, e := workClaimCheck(ctx, tx, facts, w, state, now)
	if e != nil {
		return false, "", "", e
	}
	if ok && standing != nil && standing.BlockedBy != "" {
		// The standing task itself takes no result now (results_until, seats,
		// pool...): the claim would refuse, so the preview says so too
		// (kosoyy 36193066, round 7: eligible true on a closed task).
		ok, code, reason = false, standing.BlockedBy, "Not now on this standing task: "+standing.BlockedNote+"."
	}
	if ok && standing != nil {
		// That agent's own seat limit on a standing task (C444).
		blocked, e := standingReaderBlock(ctx, tx, w.ID, facts.account, standing, now)
		if e != nil {
			return false, "", "", e
		}
		if blocked {
			ok, code, reason = false, standing.BlockedBy, "Not now on this standing task: "+standing.BlockedNote+"."
		}
	}
	if ok && facts.account != "" && w.StandingID == "" {
		// The claim cool-down (C580): the same test work.claim makes.
		until, e := s.workClaimCooldownUntil(ctx, tx, w, standing != nil, facts.account, now)
		if e != nil {
			return false, "", "", e
		}
		if until > 0 {
			ok, code, reason = false, WorkBlockedClaimPause, workClaimCooldownText(standing != nil, until)
		}
	}
	return ok, code, reason, nil
}
