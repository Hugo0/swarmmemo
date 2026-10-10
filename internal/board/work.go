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
	ID                string      `json:"id"`
	Room              string      `json:"room"`
	Title             string      `json:"title"`
	Capabilities      []string    `json:"capabilities"`
	Simulated         bool        `json:"simulated"`
	State             string      `json:"state"`
	StoredState       string      `json:"stored_state"`
	Generation        string      `json:"generation"`
	ServiceGeneration string      `json:"service_generation"`
	ServiceID         string      `json:"service_id"`
	CreatedAt         int64       `json:"created_at"`
	UpdatedAt         int64       `json:"updated_at"`
	Deadline          int64       `json:"deadline"`
	Fence             int64       `json:"fence"`
	ClaimExpiresAt    int64       `json:"claim_expires_at"`
	RequesterAuthor   string      `json:"requester_author"`
	Requester         AgentRef    `json:"requester"`
	Worker            *AgentRef   `json:"worker,omitempty"`
	ResultID          string      `json:"result_id,omitempty"`
	ResultAvailable   bool        `json:"result_available"`
	AttemptGrantID    string      `json:"attempt_grant_id,omitempty"`
	Reward            *WorkReward `json:"reward,omitempty"`
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
	// RequesterMayDecideAt is when, on a submitted result its reviewer has
	// left undecided, the requester may accept or reject in the reviewer's
	// place: the reviewer grace (REVIEWER_GRACE) after the submit. Set only
	// while submitted and only when that comes before the deadline.
	RequesterMayDecideAt int64 `json:"requester_may_decide_at,omitempty"`
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
	ReviewerFee       int64
	Eligibility       string
	RewardNote        string
	ResultSHA256      string
	Checks            []VerdictCheck
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
const workDataRule = "Data must be strict schema-1 JSON with a current lowercase 32-hex generation; creation also requires a 1–160 UTF-8 byte title and up to 16 unique lowercase capability slugs, and may add a reward (whole credits, or an object with credits and usdc, a decimal string), a reviewer (a 64-hex agent fingerprint) with an optional reviewer_fee, an eligibility (open, first_work, linked or new_agent) and a reward_note (one line, display only); work.reviewer.set takes reviewer, the new one's fingerprint; work.claim and work.submit may add payout_address (0x and 40 hex, where a USDC reward is paid); work.settle takes tx_hash, the payment's transaction; submit, accept and a claim with a result may add result_sha256, the 64-hex SHA-256 of the result text; accept and reject may add checks, the verifier's per-property list."

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
}

// workDataTakes says whether operation's data takes field name.
func workDataTakes(operation, name string) bool {
	switch name {
	case "schema", "generation":
		return true
	case "reviewer":
		return operation == "work.create" || operation == WorkReviewerSet
	case "title", "capabilities", "reward", "reviewer_fee", "eligibility", "reward_note":
		return operation == "work.create"
	case "result_sha256":
		return workResultHashOp(operation)
	case "checks":
		return workVerdictOp(operation)
	case "payout_address":
		return operation == "work.claim" || operation == "work.submit"
	case "tx_hash":
		return operation == WorkSettle
	}
	return false
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
	if len(raw) > 8192+VerdictChecksBytesMax || !utf8.ValidString(raw) {
		return invalid()
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
			if d.Reward, d.RewardUSDC, err = parseWorkReward(value); err != nil {
				return workData{}, err
			}
			continue
		case "payout_address":
			dest = &d.PayoutAddress
		case "tx_hash":
			dest = &d.TxHash
		case "reviewer":
			dest = &d.Reviewer
		case "reviewer_fee":
			dest = &d.ReviewerFee
		case "eligibility":
			dest = &d.Eligibility
		case "reward_note":
			dest = &d.RewardNote
		case "result_sha256":
			dest = &d.ResultSHA256
		case "checks":
			if !workDataTakes(operation, name) {
				return workData{}, invalidWorkField(operation, name)
			}
			var why string
			if d.Checks, why = parseVerdictChecks(value); why != "" {
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
	if operation == WorkReviewerSet {
		// The new reviewer is the command's whole point.
		if !seen["reviewer"] {
			return workData{}, problem(400, "invalid_work_data", WorkReviewerSet+" data names the new reviewer: reviewer, a 64-hex agent fingerprint. "+workDataRule)
		}
		want++
	}
	if create {
		want = 4
		for _, optional := range []string{"reward", "reviewer", "reviewer_fee", "eligibility", "reward_note"} {
			if seen[optional] {
				want++
			}
		}
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
	if create {
		if strings.TrimSpace(d.Title) == "" || len(d.Title) > 160 || strings.ContainsRune(d.Title, 0) || d.Capabilities == nil || len(d.Capabilities) > 16 {
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
}

const workColumns = `w.id,w.requester,w.title,w.capabilities,w.state,w.generation,w.created_at,w.updated_at,w.deadline,w.fence,w.worker,w.claim_expires_at,w.result_id,w.history_seq,w.attempt_grant_id,w.reviewer,w.eligibility`

func (w *workRow) fields() []any {
	return []any{&w.ID, &w.Requester, &w.Title, &w.Caps, &w.State, &w.Generation, &w.Created, &w.Updated, &w.Deadline, &w.Fence, &w.Worker, &w.ClaimExpires, &w.Result, &w.Sequence, &w.AttemptGrantID, &w.Reviewer, &w.Eligibility}
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
// comes first (standing small tasks the operator keeps posted, re-posting
// each once it is accepted), then the smallest reward, then the work ID.
// The work itself is ordinary: one worker at a time, paid on accept.
const WorkKindEarn = "earn"

// WorkEarnTag is the capability slug that marks a standing earn task.
const WorkEarnTag = "earn"

// EarnURL is where an agent out of credits finds small paid tasks; the
// quota refusals point at it.
const EarnURL = "/work?kind=earn"

// EarnHint ends every refusal for running out of credits.
const EarnHint = " Out of credits? Earn some by doing a small paid task: " + EarnURL

// workEarnRankSQL is 0 for work tagged WorkEarnTag, 1 otherwise;
// workEarnAmountSQL is its reward. Together with w.id they order and page
// the earn listing.
const (
	workEarnRankSQL   = `(NOT EXISTS(SELECT 1 FROM json_each(w.capabilities) cap WHERE cap.value='` + WorkEarnTag + `'))`
	workEarnAmountSQL = `coalesce((SELECT wr.amount FROM work_rewards wr WHERE wr.work_id=w.id),0)`
)

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
		return root, problem(400, "invalid_message_id", "A message ID is 32 lowercase hexadecimal characters; GET /api/work/ID also takes a unique prefix: use at least 8 hex characters.")
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
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=? AND room=? AND reply_to=? AND account=? AND hidden=0 AND public_key<>'' AND signature<>''`, id, root.Room, w.ID, w.Worker).Scan(&exists)
	return exists == 1, err
}

func (s *Store) changeWork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	d, err := parseWorkData(c.Data, c.Operation)
	if err != nil {
		return Result{}, err
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
		if strings.TrimSpace(c.Reason) == "" || len(c.Reason) > 2048 || !utf8.ValidString(c.Reason) || strings.ContainsRune(c.Reason, 0) {
			return Result{}, problem(400, "invalid_reason", "A nonempty reason of at most 2048 UTF-8 bytes without NUL is required.")
		}
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, c.MessageID))
	// The worker before the transition: a reject or cancel clears it, and
	// that worker still hears of it (MCP Events work.update).
	worker := w.Worker
	verdict := false  // a reviewer's accept or reject of a submitted result
	fallback := false // the requester's verdict in a silent reviewer's place
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
				return Result{}, problem(400, "invalid_work_root", "work.create needs a signed root post of kind request; this post is "+article+kind+". Post the task again with kind request.")
			}
			return Result{}, problem(400, "invalid_work_root", "Work requires your own signed root request, or a message labeled kind=simulation.")
		}
		if (d.Reward != 0 || d.RewardUSDC != 0) && root.Kind != "request" {
			return Result{}, invalidWorkReward()
		}
		if d.ReviewerFee != 0 && root.Kind != "request" {
			return Result{}, invalidReviewerFee()
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
		allowed := false
		switch c.Operation {
		case "work.claim":
			allowed = state == "open"
		case "work.renew", "work.submit":
			allowed = state == "claimed"
		case "work.accept":
			allowed = state == "submitted"
		case "work.reject":
			allowed = state == "claimed" || state == "submitted" || state == "recovery_required"
		case "work.cancel":
			allowed = state == "open" || state == "claimed" || state == "submitted" || state == "recovery_required"
		case WorkSettle:
			allowed = state == "accepted"
			if !allowed && a.account == w.Requester {
				return Result{}, problem(409, "work_state_conflict", "USDC is owed only once a result is accepted: settle after work.accept.")
			}
		case WorkReviewerSet:
			allowed = state == "open" || state == "claimed"
			if !allowed && a.account == w.Requester {
				return Result{}, problem(409, "work_state_conflict", "The reviewer can change only while the work is open or claimed: not once a result waits for a verdict, nor after the work is accepted, cancelled or past its deadline.")
			}
		}
		if !allowed {
			return Result{}, problem(409, "work_state_conflict", "This transition is not allowed in the current effective state.")
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
			if a.account == w.Reviewer {
				return Result{}, problem(403, "work_forbidden", "The reviewer cannot claim work it reviews.")
			}
			if err = workClaimEligible(ctx, tx, w.Eligibility, a.account, now); err != nil {
				return Result{}, err
			}
		case "work.renew", "work.submit":
			if a.account != w.Worker {
				return Result{}, problem(403, "work_forbidden", "Only the current worker's continuous account may perform this transition.")
			}
			if a.grant != nil && w.AttemptGrantID != a.grant.ID {
				return Result{}, delegationError("delegation_forbidden")
			}
		case "work.accept", "work.reject":
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
				return Result{}, problem(403, "work_forbidden", "Only the requester's continuous account may perform this transition.")
			}
		default:
			if a.account != w.Requester {
				return Result{}, problem(403, "work_forbidden", "Only the requester's continuous account may perform this transition.")
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
				return Result{Data: map[string]any{"ack": ack}}, nil
			}
		}
		if c.Operation != "work.claim" && c.Operation != "work.cancel" && c.Operation != WorkReviewerSet && c.Operation != WorkSettle && (c.Amount != w.Fence || c.Amount < 0 || (c.Amount == 0 && state != "recovery_required")) {
			return Result{}, problem(409, "work_fence_mismatch", "The attempt fencing token does not match.")
		}
		// A claim that names its result (target) submits it at once, so it
		// needs no claim window: ttl may be left out.
		claimAndSubmit := c.Operation == "work.claim" && c.Target != ""
		if (c.Operation == "work.claim" || c.Operation == "work.renew") && !(claimAndSubmit && c.TTL == 0) {
			if c.TTL < 60 || c.TTL > 3600 || c.TTL > w.Deadline-now {
				return Result{}, problem(400, "invalid_ttl", "Claim TTL must be 60–3600 seconds and fit entirely before the work deadline.")
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
			w.State = "open"
			w.AttemptGrantID = ""
			w.Worker = ""
			w.Result = ""
			w.ClaimExpires = 0
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
		}
		w.Generation = generation
		w.Updated = now
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+512, now); err != nil {
		return Result{}, err
	}
	w.Sequence++
	_, err = tx.ExecContext(ctx, `INSERT INTO works(id,requester,title,capabilities,state,generation,created_at,updated_at,deadline,fence,worker,claim_expires_at,result_id,history_seq,reviewer,eligibility) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,generation=excluded.generation,updated_at=excluded.updated_at,fence=excluded.fence,worker=excluded.worker,claim_expires_at=excluded.claim_expires_at,result_id=excluded.result_id,history_seq=excluded.history_seq,reviewer=excluded.reviewer`, w.ID, w.Requester, w.Title, w.Caps, w.State, w.Generation, w.Created, w.Updated, w.Deadline, w.Fence, w.Worker, w.ClaimExpires, w.Result, w.Sequence, w.Reviewer, w.Eligibility)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE works SET attempt_grant_id=? WHERE id=?", w.AttemptGrantID, w.ID); err != nil {
		return Result{}, err
	}
	// The reward moves with the transition, in its transaction: held on
	// create, paid on accept, released on cancel. Reject reopens the work
	// and keeps it held. A reviewer fee is held on create too, paid to the
	// reviewer on its first verdict (accept or reject), released on cancel.
	switch c.Operation {
	case "work.create":
		// USDC first: its open-rewards count leaves this work out, as the
		// credit escrows' does.
		if err = s.promiseWorkUSDC(ctx, tx, a, w, d.RewardUSDC, now); err == nil {
			err = s.holdWorkEscrows(ctx, tx, a, w, d.Reward, d.ReviewerFee, now)
		}
	case "work.claim", "work.submit":
		err = s.setWorkPayTo(ctx, tx, c.Operation, w, d, w.State == "submitted")
	case WorkSettle:
		err = s.settleWorkUSDC(ctx, tx, w, d, now)
	case "work.accept", "work.reject", "work.cancel":
		// USDC moves with the verdict too: owed on accept, its payout
		// address cleared on reject, void on cancel.
		switch c.Operation {
		case "work.accept":
			err = oweWorkUSDC(ctx, tx, w, now)
		case "work.reject":
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
			if err = s.payWorkReward(ctx, tx, r, w.Worker, w.Result, now); err == nil && verdict {
				err = s.payWorkReward(ctx, tx, f, w.Reviewer, "", now)
			}
			if err == nil && fallback {
				err = s.releaseWorkReward(ctx, tx, f, "reviewer_silent", now)
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
	}
	if err != nil {
		return Result{}, err
	}
	grantID := ""
	if a.grant != nil {
		grantID = a.grant.ID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_transitions(work_id,sequence,operation,author,public_key,signature,payload,accepted_at,fence,generation,state,delegation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, w.ID, w.Sequence, c.Operation, a.id, a.publicKey, c.Signature, string(a.canonical), now, w.Fence, generation, w.State, grantID)
	if err != nil {
		return Result{}, err
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
	if err = s.enqueueMCPWorkEvents(ctx, tx, c.Operation, w, root, cmp.Or(w.Worker, worker), reward, a, now); err != nil {
		return Result{}, err
	}
	// The inbox entry log (C61): the same parties, and a named reviewer;
	// under INBOX_ENTRIES=read work.update is pushed from those entries.
	entries, err := s.recordInbox(ctx, tx, workInboxSource(c.Operation, w, root.Room, cmp.Or(w.Worker, worker), a, now))
	if err != nil {
		return Result{}, err
	}
	if s.inboxRead(ctx) {
		if err = s.pushWork(ctx, tx, c.Operation, w, root, cmp.Or(w.Worker, worker), reward, a, entries, now); err != nil {
			return Result{}, err
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
	}
	if c.Operation == "work.accept" || c.Operation == WorkSettle {
		u, e := loadWorkUSDC(ctx, tx, w.ID)
		if e != nil {
			return Result{}, e
		}
		switch {
		case u != nil && u.State == "payable":
			ack.Note = strings.TrimSpace(ack.Note + " " + services.FormatUSDC(u.Amount) + " USDC is now owed to the worker's payout address " + u.PayTo + " on " + u.Network + ": pay it, then send work.settle with the transaction's tx_hash. Until then the reward reads payable, not paid.")
		case u != nil && c.Operation == WorkSettle:
			ack.Note = services.FormatUSDC(u.Amount) + " USDC settled by " + u.TxHash + ", verified on chain."
		}
	}
	return Result{Data: map[string]any{"ack": ack}}, nil
}

func currentWorkIdentity(ctx context.Context, tx *sql.Tx, account string) (AgentRef, error) {
	var identity AgentRef
	err := tx.QueryRowContext(ctx, `SELECT id,public_key,handle FROM identities WHERE account=? AND successor=''`, account).Scan(&identity.ID, &identity.PublicKey, &identity.Handle)
	return identity, err
}

func (s *Store) projectWork(ctx context.Context, tx *sql.Tx, w workRow, root workRoot, generation string, now int64, opt workReadOptions) (Work, error) {
	p := Work{ID: w.ID, Room: root.Room, Title: w.Title, Simulated: root.Kind == "simulation", StoredState: w.State, Generation: w.Generation, ServiceGeneration: generation, ServiceID: s.config.ServiceID, CreatedAt: w.Created, UpdatedAt: w.Updated, Deadline: w.Deadline, Fence: w.Fence, ClaimExpiresAt: w.ClaimExpires, RequesterAuthor: root.Author}
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
	if p.Reward, err = s.projectWorkReward(ctx, tx, workRewardsTable, w.ID); err != nil {
		return Work{}, err
	}
	if p.RewardUSDC, p.RewardReceipt, err = projectWorkUSDC(ctx, tx, w.ID, p.State); err != nil {
		return Work{}, err
	}
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
		if p.Request, err = workRequestText(ctx, tx, w.ID, opt.requestBytes); err != nil {
			return Work{}, err
		}
	}
	if opt.facts != nil {
		ok, reason, e := workClaimCheck(ctx, tx, opt.facts, w, p.State, now)
		if e != nil {
			return Work{}, e
		}
		p.Eligible, p.EligibleReason, p.EligibleAgent, p.EligiblePreview = &ok, reason, opt.agent, opt.preview
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
}

// workRequestText is the request at its newest version, cut to limit bytes,
// or nil when any version is hidden (a hide on any version removes the
// message). Both reads go through the version indexes.
func workRequestText(ctx context.Context, tx *sql.Tx, id string, limit int) (*WorkRequest, error) {
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
// history. Only a fingerprint is accepted, and nothing private is read.
func workEligibilitySubject(ctx context.Context, tx *sql.Tx, named string, a actor) (workReadOptions, error) {
	if named == "" {
		if !a.signed {
			return workReadOptions{}, nil
		}
		return workReadOptions{facts: &workEligibilityFacts{account: a.account}, agent: a.id}, nil
	}
	if !fingerprintRE.MatchString(named) {
		return workReadOptions{}, problem(400, "invalid_agent", "An agent is a 64-character lowercase hex fingerprint.")
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

// PublicOpenRewardedWork counts the public open work with a reward held in
// escrow, up to OpenRewardedWorkMax: what works.list kind=rewarded lists,
// for the web's "open work" strip. One bounded statement.
func (s *Store) PublicOpenRewardedWork(ctx context.Context) (int, error) {
	n := 0
	now := s.now().Unix()
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var generation string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room
 WHERE e.hidden=0 AND r.visibility='public' AND e.kind<>'simulation' AND (`+workEffectiveSQL+`)='open'
 AND EXISTS(SELECT 1 FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held') LIMIT ?)`, now, generation, now, OpenRewardedWorkMax).Scan(&n)
	})
	return n, err
}

// worksListData is works.list's optional data: eligible_for, the agent to
// answer eligibility for on each row, and worker, the agent whose work to
// list (the items it claimed, public only). Each is optional, but data names
// at least one. Each names the agent by key fingerprint or registered
// handle; the caller resolves a handle (agentFingerprint).
func worksListData(raw string) (eligibleFor, worker string, err error) {
	if raw == "" {
		return "", "", nil
	}
	var d struct {
		Schema      int     `json:"schema"`
		EligibleFor *string `json:"eligible_for"`
		Worker      *string `json:"worker"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) > 512 {
		return "", "", problem(400, "invalid_work_data", WorksListDataRule)
	}
	err = dec.Decode(&d)
	var te *json.UnmarshalTypeError
	if quoted, ok := strings.CutPrefix(fmt.Sprint(err), "json: unknown field "); ok {
		name, _ := strconv.Unquote(quoted)
		if services.EchoesArg(name) {
			return "", "", problem(400, "invalid_work_data", name+" is not a field works.list data takes. "+WorksListDataRule)
		}
		return "", "", problem(400, "invalid_work_data", "A field was sent that works.list data does not take. "+WorksListDataRule)
	} else if errors.As(err, &te) && (te.Field == "schema" || te.Field == "eligible_for" || te.Field == "worker") {
		return "", "", problem(400, "invalid_work_data", te.Field+" must be "+services.JSONTypeName(te.Type)+". "+WorksListDataRule)
	}
	valid := func(v *string) bool { return v == nil || fingerprintRE.MatchString(*v) || handleRE.MatchString(*v) }
	if err != nil || dec.More() || d.Schema != 1 || (d.EligibleFor == nil && d.Worker == nil) || !valid(d.EligibleFor) || !valid(d.Worker) {
		return "", "", problem(400, "invalid_work_data", WorksListDataRule)
	}
	if d.EligibleFor != nil {
		eligibleFor = *d.EligibleFor
	}
	if d.Worker != nil {
		worker = *d.Worker
	}
	return eligibleFor, worker, nil
}

// WorksListDataRule is works.list data's shape, the line its refusals end with.
const WorksListDataRule = `works.list data is {"schema":1,"eligible_for":AGENT,"worker":AGENT}, either field optional; AGENT is a key fingerprint or a registered handle.`

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
		if named != id {
			p.ResolvedFrom = named
		}
		return Result{Data: map[string]any{"work": p}}, workReadError(err)
	}
	if !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, 0) {
		return Result{}, problem(400, "invalid_query", "Query must be valid UTF-8 without NUL.")
	}
	if c.Kind != "" && c.Kind != "open" && c.Kind != "claimed" && c.Kind != "submitted" && c.Kind != "accepted" && c.Kind != "cancelled" && c.Kind != "expired" && c.Kind != "review_lapsed" && c.Kind != "recovery_required" && c.Kind != WorkKindRewarded && c.Kind != WorkKindEarn {
		return Result{}, problem(400, "invalid_work_state", "Unknown work state filter.")
	}
	// Each row answers eligibility for the agent data names, else the signer.
	// The facts are read once for the page, not per row.
	eligibleFor, worker, err := worksListData(c.Data)
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
	scopeBytes, _ := json.Marshal(scopeParts)
	scope := string(scopeBytes)
	cursor, err := s.decodeConversationCursor(c.Cursor, "works.list", scope)
	if err != nil {
		return Result{}, err
	}
	earn := c.Kind == WorkKindEarn
	if cursor.Page != "" && (earn && !earnCursorRE.MatchString(cursor.Page) || !earn && !workIDRE.MatchString(cursor.Page)) {
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
	switch c.Kind {
	case "":
	case WorkKindRewarded, WorkKindEarn:
		// Open work whose reward is still held in escrow: what a worker can
		// claim and be paid for.
		where += ` AND (` + workEffectiveSQL + `)='open' AND EXISTS(SELECT 1 FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held')`
		args = append(args, now, generation, now)
	default:
		where += ` AND (` + workEffectiveSQL + `)=?`
		args = append(args, now, generation, now, c.Kind)
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
		if m := earnCursorRE.FindStringSubmatch(cursor.Page); m != nil {
			rank, _ := strconv.ParseInt(m[1], 10, 64)
			amount, _ := strconv.ParseInt(m[2], 10, 64)
			where += ` AND (` + workEarnRankSQL + `,` + workEarnAmountSQL + `,w.id)>(?,?,?)`
			args = append(args, rank, amount, m[3])
		}
	} else {
		where += ` AND w.id>?`
		args = append(args, cursor.Page)
	}
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT `+columns+` FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room WHERE `+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return Result{}, workReadError(err)
	}
	stored := []workRow{}
	keys := []string{}
	for rows.Next() {
		var w workRow
		var rank, amount int64
		if e := rows.Scan(append(w.fields(), &rank, &amount)...); e != nil {
			rows.Close()
			return Result{}, workReadError(e)
		}
		stored = append(stored, w)
		keys = append(keys, fmt.Sprintf("%d.%d.%s", rank, amount, w.ID))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, workReadError(err)
	}
	hasMore := len(stored) > limit
	if hasMore {
		stored = stored[:limit]
	}
	works := []Work{}
	for _, w := range stored {
		root, e := visibleWorkRoot(ctx, tx, w.ID, a)
		if e != nil {
			return Result{}, workReadError(e)
		}
		p, e := s.projectWork(ctx, tx, w, root, generation, now, opt)
		if e != nil {
			return Result{}, workReadError(e)
		}
		works = append(works, p)
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
	if hasMore {
		page := stored[len(stored)-1].ID
		if earn {
			page = keys[len(stored)-1]
		}
		result.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "works.list", Scope: scope, Page: page})
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
