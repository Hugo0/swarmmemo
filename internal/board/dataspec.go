package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"swarmmemo/internal/gate"
	"swarmmemo/internal/jsonstrict"
)

// The data spec (C377) is the one statement of what each operation's data
// carries, the companion of the operations table: every field's name, JSON
// type and whether it is required, with the contract's enum, pattern and
// limit keys where one applies. It is the enforcement, not a description of
// it: the token-loop and allowed-list parsers ask dataTakes whether a name
// is a field, and where a parser decodes into a struct the spec is read from
// that struct (structSpec), so the two cannot differ. clients/contract.json
// carries it as operations[op].data, and TestDataSpecIsTheEnforcement holds
// each parser to it: every declared field passes the name check, any other
// name is refused.
//
// To add a data field: add it here (or to the struct the spec reads), then
// handle it in the parser; a removed field stays with RemovedIn, an alias
// with AliasOf.

// The JSON types a data field takes. A field that takes either of two types
// is written "int|object".
const (
	DataString = "string"
	DataInt    = "int"
	DataNumber = "number"
	DataBool   = "bool"
	DataObject = "object"
	DataArray  = "array"
)

// The non-object data shapes (DataSpec.Raw): the string itself is the data.
const (
	// DataRawBase64URL is unpadded base64url bytes (a file, an invite secret).
	DataRawBase64URL = "base64url"
	// DataRawViewer is the literal string "viewer" (trust.get).
	DataRawViewer = "viewer"
	// DataRawService is a service method's arguments, a JSON object whose
	// fields each method defines (services.list, /capabilities).
	DataRawService = "service"
)

// DataField is one field of an operation's data object.
type DataField struct {
	Name string
	// Type is one of the Data* types, or two joined by "|".
	Type     string
	Required bool
	// Enum, Pattern and Limit name the clients/contract.json enums,
	// patterns and limits entry the value is checked against, if any.
	Enum, Pattern, Limit string
	// AliasOf is the field this name is another spelling of.
	AliasOf string
	// RemovedIn is the release that stopped taking this field; it is
	// refused like any unknown name.
	RemovedIn string
	// Items is an array field's entry, when each is an object of its own
	// fields (a checks list's check, C385).
	Items *DataSpec
}

// DataSpec is what an operation's data holds: a JSON object of Fields, or,
// with Raw set, a string of that shape.
type DataSpec struct {
	Raw    string
	Fields []DataField
	// Lax marks a parser that ignores names it does not know rather than
	// refusing them.
	Lax bool
}

// Field returns the named field, removed ones included.
func (d DataSpec) Field(name string) (DataField, bool) {
	for _, f := range d.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return DataField{}, false
}

// Takes reports whether name is a field the data takes now: declared, as
// itself or an alias, and not removed.
func (d DataSpec) Takes(name string) bool {
	f, ok := d.Field(name)
	return ok && f.RemovedIn == ""
}

// Names lists the fields taken now, in declared order; required ones only
// with required.
func (d DataSpec) Names(requiredOnly bool) []string {
	var out []string
	for _, f := range d.Fields {
		if f.RemovedIn == "" && f.AliasOf == "" && (f.Required || !requiredOnly) {
			out = append(out, f.Name)
		}
	}
	return out
}

// Optional lists the optional fields taken now, in declared order.
func (d DataSpec) Optional() []string {
	var out []string
	for _, f := range d.Fields {
		if f.RemovedIn == "" && f.AliasOf == "" && !f.Required {
			out = append(out, f.Name)
		}
	}
	return out
}

// marked lists the fields taken now, an optional one with a "?" suffix:
// the form strictDelegationObject reads.
func (d DataSpec) marked() []string {
	var out []string
	for _, f := range d.Fields {
		if f.RemovedIn != "" || f.AliasOf != "" {
			continue
		}
		if f.Required {
			out = append(out, f.Name)
		} else {
			out = append(out, f.Name+"?")
		}
	}
	return out
}

// DataSpecOf returns operation's data spec, and whether it takes data.
func DataSpecOf(operation string) (DataSpec, bool) {
	d, ok := dataSpecs[operation]
	return d, ok
}

// Data returns the operation's data spec, and whether it takes data.
func (op Operation) Data() (DataSpec, bool) { return DataSpecOf(op.Name) }

// dataTakes reports whether operation's data takes the field name: the
// question every token-loop and allowed-list parser asks.
func dataTakes(operation, name string) bool {
	return dataSpecs[operation].Takes(name)
}

// objectSpec is a data object of the fields listed, each written
// "name[!] [TYPE] [enum=KEY] [pattern=KEY] [limit=KEY] [alias=NAME] [removed=RELEASE]";
// "!" marks a required field, and TYPE is required here.
func objectSpec(fields ...string) DataSpec {
	d := DataSpec{Fields: []DataField{}}
	for _, line := range fields {
		f := parseDataField(line, "")
		if f.Type == "" {
			panic("dataspec: " + line + " names no type")
		}
		d.Fields = append(d.Fields, f)
	}
	return d.checked()
}

// structSpec is a data object of v's json fields as encoding/json decodes
// them (an untagged embedded struct's fields promoted, a shallower name
// winning), each typed by reflection. notes annotate fields by name in
// objectSpec's form; a type in a note overrides the reflected one, and a
// json.RawMessage field needs one.
func structSpec(v any, notes ...string) DataSpec {
	t := reflect.TypeOf(v)
	type found struct {
		f     DataField
		depth int
	}
	var fields []found
	for _, sf := range reflect.VisibleFields(t) {
		name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if !sf.IsExported() || name == "-" || (name == "" && sf.Anonymous) {
			continue
		}
		if name == "" {
			continue // an untagged field is not this spec's to read
		}
		i := slices.IndexFunc(fields, func(x found) bool { return x.f.Name == name })
		if i >= 0 {
			if fields[i].depth <= len(sf.Index) {
				continue
			}
			fields = slices.Delete(fields, i, i+1)
		}
		fields = append(fields, found{DataField{Name: name, Type: reflectDataType(sf.Type)}, len(sf.Index)})
	}
	d := DataSpec{Fields: []DataField{}}
	for _, x := range fields {
		d.Fields = append(d.Fields, x.f)
	}
	for _, line := range notes {
		note := parseDataField(line, "")
		i := slices.IndexFunc(d.Fields, func(f DataField) bool { return f.Name == note.Name })
		if i < 0 {
			panic(fmt.Sprintf("dataspec: %T has no field %s", v, note.Name))
		}
		f := &d.Fields[i]
		f.Required = note.Required
		if note.Type != "" {
			f.Type = note.Type
		}
		f.Enum, f.Pattern, f.Limit, f.AliasOf, f.RemovedIn = note.Enum, note.Pattern, note.Limit, note.AliasOf, note.RemovedIn
	}
	for _, f := range d.Fields {
		if f.Type == "" {
			panic(fmt.Sprintf("dataspec: %T field %s needs a type note", v, f.Name))
		}
	}
	return d.checked()
}

// only keeps the named fields, in declared order: a struct shared by
// several operations, each of which reads some of it.
func (d DataSpec) only(names ...string) DataSpec {
	out := DataSpec{Raw: d.Raw, Lax: d.Lax, Fields: []DataField{}}
	for _, name := range names {
		if _, ok := d.Field(name); !ok {
			panic("dataspec: only names unknown field " + name)
		}
	}
	for _, f := range d.Fields {
		if slices.Contains(names, f.Name) {
			out.Fields = append(out.Fields, f)
		}
	}
	return out
}

// with appends fields in objectSpec's form.
func (d DataSpec) with(fields ...string) DataSpec {
	more := objectSpec(fields...)
	d.Fields = append(slices.Clone(d.Fields), more.Fields...)
	return d.checked()
}

// withItems sets array field name's entry spec.
func (d DataSpec) withItems(name string, items DataSpec) DataSpec {
	d.Fields = slices.Clone(d.Fields)
	i := slices.IndexFunc(d.Fields, func(f DataField) bool { return f.Name == name })
	if i < 0 || d.Fields[i].Type != DataArray {
		panic("dataspec: withItems names no array field " + name)
	}
	d.Fields[i].Items = &items
	return d
}

// rawSpec is data that is a string of shape, not a JSON object.
func rawSpec(shape string) DataSpec { return DataSpec{Raw: shape} }

var dataTypes = []string{DataString, DataInt, DataNumber, DataBool, DataObject, DataArray}

func validDataType(t string) bool {
	for _, part := range strings.Split(t, "|") {
		if !slices.Contains(dataTypes, part) {
			return false
		}
	}
	return t != ""
}

func parseDataField(line, defaultType string) DataField {
	words := strings.Fields(line)
	if len(words) == 0 {
		panic("dataspec: empty field")
	}
	f := DataField{Type: defaultType}
	f.Name, f.Required = strings.CutSuffix(words[0], "!")
	for _, w := range words[1:] {
		key, value, ok := strings.Cut(w, "=")
		switch {
		case !ok && validDataType(w):
			f.Type = w
		case key == "enum":
			f.Enum = value
		case key == "pattern":
			f.Pattern = value
		case key == "limit":
			f.Limit = value
		case key == "alias":
			f.AliasOf = value
		case key == "removed":
			f.RemovedIn = value
		default:
			panic("dataspec: " + line + ": cannot read " + w)
		}
	}
	return f
}

func (d DataSpec) checked() DataSpec {
	seen := map[string]bool{}
	for _, f := range d.Fields {
		if seen[f.Name] || f.Name == "" {
			panic("dataspec: duplicate or empty field " + f.Name)
		}
		seen[f.Name] = true
	}
	for _, f := range d.Fields {
		if f.AliasOf != "" && !seen[f.AliasOf] {
			panic("dataspec: " + f.Name + " is an alias of an unknown field")
		}
	}
	return d
}

var (
	rawMessageType = reflect.TypeOf(json.RawMessage(nil))
	numberType     = reflect.TypeOf(json.Number(""))
)

// reflectDataType is the JSON type encoding/json decodes into t, or "" for
// a json.RawMessage, which takes anything.
func reflectDataType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == rawMessageType:
		return ""
	case t == numberType:
		return DataNumber
	}
	switch t.Kind() {
	case reflect.String:
		return DataString
	case reflect.Bool:
		return DataBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return DataInt
	case reflect.Float32, reflect.Float64:
		return DataNumber
	case reflect.Slice, reflect.Array:
		return DataArray
	case reflect.Map, reflect.Struct:
		return DataObject
	}
	panic("dataspec: no JSON type for " + t.String())
}

// The work.* data every operation takes, then each one's own.
var workCommonData = []string{"schema! int", "generation! string pattern=work_id"}

func workSpec(fields ...string) DataSpec {
	return objectSpec(append(slices.Clone(workCommonData), fields...)...)
}

// A checks list's entry (C97, checks.go): verdictCheckItems where a verdict
// carries one, verificationCheckItems in verify.publish, whose state may be
// flaky with runs, passes and failing_step (C385). parseVerdictChecks reads
// the names and the state enum from them.
var (
	verdictCheckFields = func(states string) []string {
		return []string{"property! string pattern=verdict_check_property", "state! string enum=" + states, "subject_sha256 string pattern=fingerprint",
			"tool string limit=verdict_check_tool_chars", "evidence string limit=verdict_check_evidence_chars", "note string limit=verdict_check_note_chars"}
	}
	verdictCheckItems      = objectSpec(verdictCheckFields("verdict_check_states")...)
	verificationCheckItems = objectSpec(append(verdictCheckFields("verification_check_states"),
		"runs int limit=verification_check_runs_maximum", "passes int", "failing_step string limit=verification_check_failing_step_chars")...)
)

// dataSpecs is every operation's data, keyed by operation name. An
// operation whose Fields name data has an entry here, and only those
// (TestDataSpecCoversEveryDataOperation).
var dataSpecs = map[string]DataSpec{
	// Messages and feeds.
	"post":          objectSpec("schema! int", "format string", "supersedes string pattern=message_id", "review object"),
	"messages.list": structSpec(ListOptions{}, "schema int"),
	"feed.get":      objectSpec("profile string", "override object", "offset int", "explain bool"),
	// feedData takes schema (1) beside each write's own fields.
	"feed.profile.get":  objectSpec("schema int"),
	"feed.profile.put":  objectSpec("schema int", "profile! object", "visibility string", "if_revision int"),
	"feed.profile.fork": objectSpec("schema int", "hash string", "visibility string"),
	"room.subscribe":    objectSpec("schema int", "weight number"),
	"updates.get":       structSpec(updatesOptions{}, "schema!"),
	"updates.dispose":   structSpec(disposeData{}, "schema!", "ids!", "state! enum=inbox_dispose_states"),
	"updates.mark_read": structSpec(markReadData{}, "schema!"),
	"vote":              structSpec(voteData{}, "value!"),

	// Rooms.
	"room.invite.accept": rawSpec(DataRawBase64URL),
	"room.policy.set":    structSpec(roomPolicyData{}, "write_via array", "front_page bool", "rules limit=room_rules_bytes"),
	"room.style.set":     structSpec(roomStyleData{}, "css! limit=room_style_bytes"),
	"room.style.check":   structSpec(roomStyleData{}, "css! limit=room_style_bytes"),
	"blob.put":           rawSpec(DataRawBase64URL),

	// Identity.
	"agent.profile.publish": objectSpec("schema! int", "description! string limit=profile_description_bytes", "capabilities! array limit=profile_capabilities",
		"availability! string", "avatar object", "ucp object"),
	"key.backup.put":     structSpec(keyBackupData{}, "schema!", "scheme!", "credential_id!", "salt!", "iv!", "ciphertext!"),
	"key.backup.get":     structSpec(keyBackupData{}, "schema!", "credential_id!").only("schema", "credential_id"),
	"device.request":     structSpec(deviceData{}, "schema!", "label limit=device_label_chars").only("schema", "label"),
	"device.request.get": structSpec(deviceData{}, "schema!", "code!").only("schema", "code"),
	"device.add":         structSpec(deviceData{}, "schema!", "label limit=device_label_chars", "code_sha256 pattern=fingerprint").only("schema", "label", "code_sha256"),
	"identity.link": objectSpec("schema! int", "kind! string", "value! string", "proof string", "nonce string", "observed_at string",
		"nonce_log string", "nonce_log_size int", "observed_height int", "observed_time int"),
	"identity.unlink": objectSpec("schema! int", "kind! string", "value! string"),
	"identity.witness": objectSpec("schema! int", "agent! string pattern=fingerprint", "kind! string", "value! string", "nonce! string",
		"verdict! string", "checks array limit=verdict_checks_maximum").withItems("checks", verdictCheckItems),
	"verify.publish": objectSpec("schema! int", "repo string", "commit string", "file string", "file_sha256 string pattern=fingerprint",
		"command! string", "environment! string", "output_sha256! string pattern=fingerprint", "output_normaliser string limit=verification_output_normaliser_bytes", "stdio_encoding string limit=verification_stdio_encoding_bytes", "output string",
		"limits! string", "supersedes string",
		"checks! array limit=verdict_checks_maximum").withItems("checks", verificationCheckItems),
	"standing.challenge": objectSpec("schema! int", "kind! string", "value string", "bits int", "action string", "usdc string",
		"payment string", "id string", "tx_hash string pattern=tx_hash"),
	"standing.work":  objectSpec("schema! int", "nonce! string", "solution! string"),
	"gate.challenge": structSpec(gate.ChallengeRequest{}, "schema!"),
	"gate.verify":    structSpec(gate.VerifyRequest{}, "schema!", "challenge!"),
	"vouch":          structSpec(vouchData{}, "schema!", "value!"),
	"trust.get":      rawSpec(DataRawViewer),

	// Work.
	"work.create": workSpec("title! string limit=work_title_bytes", "capabilities! array limit=work_capabilities_maximum",
		"reward int|object limit=work_reward_maximum", "reviewer string pattern=fingerprint", "reviewer_fee int limit=work_reward_maximum",
		"reject_fee int limit=work_reject_fee_max_percent", "eligibility string enum=work_eligibilities",
		"reward_note string limit=work_reward_note_chars", "seats int limit=work_standing_seats_maximum", "per_agent_day int",
		"day_max int limit=work_standing_day_maximum", "results_max int limit=work_standing_results_maximum",
		"tiers array limit=work_standing_tiers_maximum", "usdc_per_agent_day string", "usdc_day_max string",
		"answer_commit string pattern=fingerprint", "answer_commit_note string limit=work_answer_note_chars"),
	"work.claim":         workSpec("result_sha256 string pattern=fingerprint", "payout_address string"),
	"work.renew":         workSpec(),
	"work.submit":        workSpec("result_sha256 string pattern=fingerprint", "payout_address string"),
	"work.accept":        workSpec("result_sha256 string pattern=fingerprint", "checks array limit=verdict_checks_maximum", "tier string pattern=standing_tier", "review object").withItems("checks", verdictCheckItems),
	"work.reject":        workSpec("checks array limit=verdict_checks_maximum").withItems("checks", verdictCheckItems),
	"work.cancel":        workSpec(),
	"work.settle":        workSpec("tx_hash! string pattern=tx_hash"),
	"work.promise":       workSpec("amount_usdc! string", "note string limit=work_reward_note_chars"),
	"work.reviewer.set":  workSpec("reviewer! string pattern=fingerprint"),
	"work.successor.set": workSpec("successor! string pattern=work_id"),
	"work.reveal":        workSpec("answer! string limit=work_answer_bytes", "salt! string limit=work_answer_salt_bytes"),
	"works.list":         structSpec(worksListOptions{}, "schema!", "standing pattern=work_id"),

	// Grants and push.
	"delegation.create": objectSpec("schema! int", "generation! string pattern=work_id", "operations! array", "disclosure! string",
		"spend_limit object", "origin string", "payout_address string"),
	"delegation.revoke":   objectSpec("schema! int", "generation! string pattern=work_id"),
	"private_read.create": objectSpec("schema! int", "generation! string pattern=work_id", "access_epoch! string pattern=work_id", "disclosure! string"),
	"private_read.revoke": objectSpec("schema! int", "generation! string pattern=work_id"),
	"webhook.create":      objectSpec("schema! int", "url! string limit=webhook_url_bytes", "kinds array"),
	"push.subscribe":      structSpec(pushData{}, "schema!", "endpoint! limit=push_endpoint_bytes", "p256dh!", "auth!", "label limit=push_label_chars"),
	"spend_limit.set":     structSpec(spendLimitSetData{}, "schema!"),

	// Allowance, credits, offerings and services.
	"allowance.get":      structSpec(schemaOnlyData{}, "schema!"),
	"allowance.transfer": structSpec(allowanceTransferData{}, "schema!", "resource enum=allowance_resources"),
	"credits.topup":      structSpec(topupRequest{}, "schema! int"),
	"offering.publish":   structSpec(offeringData{}, "schema! int", "name!", "title!", "price!", "pay_to!", "input object"),
	"offering.buy":       structSpec(buyData{}, "schema! int", "input object"),
	"offering.answer":    structSpec(answerData{}, "schema! int", "body!"),
	"offering.refund":    structSpec(refundData{}, "schema! int", "tx_hash!"),
	"x402.submit":        structSpec(x402SubmitData{}, "schema!"),
	"service.call":       rawSpec(DataRawService),
	"service.read":       rawSpec(DataRawService),

	// Subjects, find and trust.
	"subjects.list": structSpec(subjectsListData{}, "schema!", "sort!"),
	// language is read from the query ("agents that speak ...") only.
	"find": structSpec(findData{}, "schema!", "type string|array enum=find_types", "output enum=find_outputs", "protocol enum=find_protocols").
		only("schema", "type", "category", "max_price", "output", "network", "protocol", "door", "parse", "explain"),

	// Conversations and hosted identities.
	"conversation.open":     structSpec(openData{}, "schema!", "kind!"),
	"conversation.get":      structSpec(getData{}, "schema!"),
	"conversation.respond":  structSpec(respondData{}, "schema!", "action!"),
	"conversation.seal":     structSpec(sealRotation{}, "schema!", "member_epoch!", "epoch!", "wraps!"),
	"messaging.policy.set":  structSpec(messagingData{}, "schema!", "inbound_policy object", "inbound object", "outbound object"),
	"hosted.recover":        structSpec(hostedData{}, "schema!", "recovery_code!").only("schema", "recovery_code"),
	"hosted.token":          structSpec(hostedData{}, "schema!", "action!").only("schema", "action", "target", "label", "spend_limit"),
	"hosted.claim":          structSpec(hostedData{}, "schema!", "new_public_key!", "proof!", "recovery_code!").only("schema", "new_public_key", "proof", "recovery_code"),
	"app.register":          structSpec(appData{}, "schema!", "name!", "homepage!", "owner pattern=fingerprint"),
	"oauth.client.register": structSpec(oauthClientData{}, "schema!", "name!", "redirect_uris!", "scopes!"),
	"agents.list":           structSpec(agentsListData{}, "schema!"),
}

// dataRefusals is the code each operation's own parser refuses its data
// with, so the strict check below answers as that parser would; an
// operation named by its family ("work.") takes the family's, and any other
// invalid_request. TestDataRefusalsAreTheParsers holds it to the parsers.
var dataRefusals = map[string]string{
	"post":                  "invalid_post_data",
	"messages.list":         "invalid_list_options",
	"agents.list":           "invalid_list_options",
	"feed.":                 "invalid_feed_profile",
	"room.subscribe":        "invalid_feed_profile",
	"vote":                  "invalid_vote",
	"room.policy.set":       "invalid_policy",
	"room.style.":           "invalid_style",
	"agent.profile.publish": "invalid_profile",
	"key.backup.":           "invalid_key_backup",
	"device.":               "invalid_device_data",
	"identity.link":         "invalid_link",
	"identity.unlink":       "invalid_link",
	"identity.witness":      "invalid_witness",
	"verify.publish":        "invalid_verification",
	"standing.":             "invalid_standing",
	"gate.":                 "invalid_gate_data",
	"vouch":                 "invalid_vouch",
	"work.":                 "invalid_work_data",
	"works.list":            "invalid_work_data",
	"delegation.":           "invalid_delegation_data",
	"private_read.":         "invalid_private_read_data",
	"webhook.":              "invalid_webhook",
	"push.":                 "invalid_push_subscription",
	"spend_limit.set":       "invalid_spend_limit",
	"offering.publish":      "invalid_offering",
	"offering.answer":       "invalid_offering_answer",
	"offering.refund":       "invalid_refund",
	"x402.submit":           "x402_listing_data",
	"find":                  "invalid_find",
	"conversation.":         "invalid_conversation",
	"conversation.seal":     "invalid_seal",
	"messaging.policy.set":  "invalid_messaging_policy",
	"hosted.":               "invalid_hosted_data",
	"app.":                  "invalid_app_data",
	"oauth.":                "invalid_client_data",
}

// dataRefusal is operation's entry in dataRefusals.
func dataRefusal(operation string) string {
	if code, ok := dataRefusals[operation]; ok {
		return code
	}
	for family := operation; ; {
		i := strings.LastIndex(family, ".")
		if i < 0 {
			return "invalid_request"
		}
		family = family[:i]
		if code, ok := dataRefusals[family+"."]; ok {
			return code
		}
	}
}

// strictCommandData is the one choke point for command data (C493), run
// before any operation reads its data or anything is written: data that
// parses as JSON but that two parsers could read differently is refused
// (invalid UTF-8, a key repeated in any object at any depth, keys that
// differ only in case included, nesting deeper than jsonstrict.DepthMax,
// anything after the value), so what is signed and stored means one thing
// to every reader. Malformed JSON goes on to the operation's parser, which
// refuses it in its own words. Raw data (DataSpec.Raw) is skipped:
// base64url bytes and "viewer" are not JSON, and a service method's data is
// held to the same rule, at its own depth bound and as invalid_service_data,
// by services.ParseData before the engine reads it.
func strictCommandData(c Command) error {
	if c.Data == "" {
		return nil
	}
	if spec, ok := DataSpecOf(c.Operation); !ok || spec.Raw != "" {
		return nil
	}
	err := jsonstrict.Value([]byte(c.Data), jsonstrict.DepthMax)
	if !jsonstrict.Ambiguous(err) {
		return nil
	}
	code := dataRefusal(c.Operation)
	if c.PrivateRead != nil || code == "invalid_private_read_data" {
		return privateReadError("invalid_private_read_data") // the private plane names nothing
	}
	msg := "data must be valid UTF-8."
	switch {
	case errors.Is(err, jsonstrict.ErrDuplicate):
		msg = "data repeats a field in one object (names that differ only in case are the same field); give each once."
	case errors.Is(err, jsonstrict.ErrDepth):
		msg = "data nests deeper than " + strconv.Itoa(jsonstrict.DepthMax) + " levels."
	case errors.Is(err, jsonstrict.ErrTrailing):
		msg = "data must be one JSON value with nothing after it."
	}
	return problem(400, code, msg)
}
