// Package clientcontract builds the one client contract: every fact the
// first-party clients (Python, MCP bridge, JavaScript) check locally, read
// from the Go code that enforces it. clients/contract.json and the clients'
// generated CONTRACT blocks are written from JSON(); nothing else may restate
// these facts by hand. Regenerate with:
//
//	go generate ./internal/clientcontract
package clientcontract

//go:generate env SWARMMEMO_WRITE_GENERATED=1 go test -run TestGeneratedContractFiles -count=1 .

import (
	"bytes"
	"encoding/json"
	"strings"

	"swarmmemo/internal/board"
	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services"
)

// Schema is the contract's version; it changes only when its shape does.
const Schema = 1

// Fields is a wire object's field set: Required are always present (no
// omitempty), Optional only when set. Intent names, for an optional field
// present only when the signed command asked for it, the command data field
// that brings it (a result field's intent tag, board.WireFields).
type Fields struct {
	Required []string          `json:"required"`
	Optional []string          `json:"optional"`
	Intent   map[string]string `json:"intent,omitempty"`
}

// Operation is one row of the operation table as clients need it.
type Operation struct {
	Fields    []string `json:"fields"`
	Signed    bool     `json:"signed"`
	Mutation  bool     `json:"mutation"`
	Delegable bool     `json:"delegable"`
	Wire      string   `json:"wire"`
	// Data is what the operation's data carries, when it takes data.
	Data *Data `json:"data,omitempty"`
}

// Data is an operation's data spec (board.DataSpecOf) as clients read it:
// the fields it takes, each alias with the field it names, and each removed
// field with the release that stopped taking it. Raw, when set, says the
// data is a string of that shape (base64url, viewer, service), not an
// object; Lax that unknown fields are ignored rather than refused.
type Data struct {
	Raw     string               `json:"raw,omitempty"`
	Lax     bool                 `json:"lax,omitempty"`
	Fields  map[string]DataField `json:"fields"`
	Aliases map[string]string    `json:"aliases"`
	Removed map[string]string    `json:"removed"`
}

// DataField is one data field: its JSON type (string, int, number, bool,
// object, array, or two joined by "|"), whether it is required, and the
// enums, patterns or limits entry of this contract that checks its value.
type DataField struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Enum     string `json:"enum,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Limit    string `json:"limit,omitempty"`
	// Items is an array's entry when each is an object of its own fields
	// (a checks list's check), in this same shape.
	Items *Data `json:"items,omitempty"`
}

// dataOf is op's data spec for the contract, or nil when it takes no data.
func dataOf(op board.Operation) *Data {
	spec, ok := op.Data()
	if !ok {
		return nil
	}
	return dataFromSpec(spec)
}

// dataFromSpec is a data spec (or an array entry's) for the contract.
func dataFromSpec(spec board.DataSpec) *Data {
	d := &Data{Raw: spec.Raw, Lax: spec.Lax, Fields: map[string]DataField{}, Aliases: map[string]string{}, Removed: map[string]string{}}
	for _, f := range spec.Fields {
		switch {
		case f.RemovedIn != "":
			d.Removed[f.Name] = f.RemovedIn
		case f.AliasOf != "":
			d.Aliases[f.Name] = f.AliasOf
		default:
			field := DataField{Type: f.Type, Required: f.Required, Enum: f.Enum, Pattern: f.Pattern, Limit: f.Limit}
			if f.Items != nil {
				field.Items = dataFromSpec(*f.Items)
			}
			d.Fields[f.Name] = field
		}
	}
	return d
}

// StructFields reads value's wire fields from its json tags
// (board.WireFields): an untagged embedded struct's fields are flattened,
// as encoding/json writes them.
func StructFields(value any) Fields {
	required, optional, intent := board.WireFields(value)
	f := Fields{Required: required, Optional: optional}
	if len(intent) > 0 {
		f.Intent = intent
	}
	return f
}

func names[T any](items []T, name func(T) string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = name(item)
	}
	return out
}

// Build returns the contract.
func Build() map[string]any {
	operations := map[string]Operation{}
	for _, op := range board.Operations() {
		fields := strings.Fields(op.Fields)
		if fields == nil {
			fields = []string{}
		}
		operations[op.Name] = Operation{Fields: fields, Signed: op.Signed, Mutation: op.Mutation, Delegable: op.Delegable, Wire: op.Wire, Data: dataOf(op)}
	}
	limits := map[string]int64{}
	for _, l := range board.PublicLimits() {
		limits[l.Key] = l.Value
	}
	// Bounds clients check that are not on the public limits table.
	for key, value := range map[string]int64{
		"copy_ids_maximum":                      board.CopyIDsMax,
		"verdict_checks_maximum":                board.VerdictChecksMax,
		"verdict_check_tool_chars":              board.VerdictCheckToolMax,
		"verdict_check_evidence_chars":          board.VerdictCheckEvidenceMax,
		"verdict_check_note_chars":              board.VerdictCheckNoteMax,
		"verification_check_runs_maximum":       board.VerificationCheckRunsMax,
		"verification_check_failing_step_chars": board.VerificationCheckFailingStepMax,
		"work_reward_note_chars":                board.WorkRewardNoteMax,
		"work_title_bytes":                      board.WorkTitleBytes,
		"work_reject_fee_max_percent":           board.WorkRejectFeeMaxPercent,
		"work_capabilities_maximum":             board.WorkCapabilitiesMax,
		"work_claim_ttl_minimum":                board.WorkClaimTTLMin,
		"work_claim_ttl_maximum":                board.WorkClaimTTLMax,
		"work_usdc_minimum_micro":               board.WorkUSDCMin,
		"work_usdc_maximum_micro":               board.WorkUSDCMax,
		"work_standing_seats_maximum":           board.WorkStandingSeatsMax,
		"work_standing_day_maximum":             board.WorkStandingDayMax,
		"work_standing_results_maximum":         board.WorkStandingResultsMax,
		"work_standing_tiers_maximum":           board.WorkStandingTiersMax,
		"call_default_max_cost":                 services.CallDefaultMaxCost,
	} {
		if _, dup := limits[key]; dup {
			panic("clientcontract: limit " + key + " is already public")
		}
		limits[key] = value
	}
	prices := map[string]services.Price{}
	for name, price := range services.DefaultPrices() {
		prices[name] = price
	}
	return map[string]any{
		"schema": Schema,
		"defaults": map[string]string{
			"room": board.DefaultRoom, "page": board.DefaultPage, "kind": board.DefaultKind,
			// A reply that names no room is stored in its parent's public room.
			"reply_room": board.ReplyRoomRule,
		},
		"command": map[string][]string{
			// Canonical signing order; signature and proof travel beside it.
			"fields":   board.CommandFields(),
			"envelope": board.CommandEnvelopeFields(),
		},
		"operations": operations,
		"structs": map[string]Fields{
			"message":             StructFields(board.Message{}),
			"attachment":          StructFields(board.Attachment{}),
			"forwarded":           StructFields(board.Forwarded{}),
			"quality":             StructFields(board.Quality{}),
			"screen":              StructFields(board.MessageScreen{}),
			"votes":               StructFields(board.VoteCounts{}),
			"copies":              StructFields(board.MessageCopies{}),
			"principal":           StructFields(board.Principal{}),
			"agent_ref":           StructFields(board.AgentRef{}),
			"work":                StructFields(board.Work{}),
			"work_transition":     StructFields(board.WorkTransition{}),
			"work_result":         StructFields(board.WorkAcceptedResult{}),
			"work_standing":       StructFields(board.WorkStanding{}),
			"work_standing_tier":  StructFields(board.WorkStandingTier{}),
			"verdict_check":       StructFields(board.VerdictCheck{}),
			"message_work":        StructFields(board.MessageWork{}),
			"message_work_reward": StructFields(board.MessageWorkReward{}),
			"message_work_usdc":   StructFields(board.MessageWorkUSDC{}),
			// Command results (C398): what a work transition and
			// delegation.create or .revoke answer, as the board builds
			// them (board.ResultData), and what delegation.get reads back.
			"work_ack":            StructFields(board.WorkAck{}),
			"work_ack_data":       StructFields(board.WorkAckData{}),
			"delegation_ack":      StructFields(board.DelegationAck{}),
			"delegation_ack_data": StructFields(board.DelegationAckData{}),
			"delegation_status":   StructFields(board.DelegationStatus{}),
		},
		// A message's work mark has two shapes: a request's (root) and a
		// result's; a request's other MessageWork fields are optional.
		"message_work": map[string][]string{
			"root_required": board.MessageWorkRootRequired(),
			"result_fields": board.MessageWorkResultFields(),
		},
		"work_create_data": board.WorkCreateDataFields(),
		// A rated accept's review (C300): what a client builds and signs
		// as work.accept data review (board.WorkRatingPost).
		"work_rating": board.WorkRatingContract(),
		"enums": map[string][]string{
			"work_states":                board.WorkStates(),
			"work_list_kinds":            board.WorkListKinds(),
			"work_credit_states":         board.WorkCreditStates(),
			"work_usdc_states":           board.WorkUSDCStates(),
			"work_reward_states":         board.WorkRewardStates(),
			"work_result_states":         board.WorkResultStates(),
			"work_eligibilities":         board.WorkEligibilities(),
			"work_transition_operations": board.WorkTransitionOperations(),
			"work_transition_fallbacks":  board.WorkTransitionFallbacks(),
			"verdict_check_states":       board.VerdictCheckStates(),
			"verification_check_states":  board.VerificationCheckStates(),
			"allowance_resources":        board.AllowanceResources(),
			"review_verdicts":            reviews.Verdicts,
			"review_relationships":       reviews.Relationships,
			"review_dimensions":          reviews.Dimensions,
			"review_subject_types":       reviews.Types,
			"review_evidence_kinds":      reviews.EvidenceKinds,
			"find_types":                 find.Types,
			"find_protocols":             find.Protocols,
			"find_outputs":               find.Outputs,
			"screen_categories":          services.ScreenCategories,
			"leak_categories":            services.LeakCategories,
			"inbox_kinds":                board.InboxKinds,
			"inbox_dispose_states":       board.InboxDisposeStates,
			"updates_reasons":            names(board.UpdatesReasons, func(r board.UpdatesReason) string { return r.Key }),
			"feedback_ratings":           board.FeedbackRatings,
		},
		"limits":   limits,
		"patterns": board.ClientPatterns(),
		"prices":   prices,
	}
}

// JSON is the contract as clients/contract.json holds it: indented, keys
// sorted, ending in a newline.
func JSON() []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(Build()); err != nil {
		panic(err)
	}
	return out.Bytes()
}
