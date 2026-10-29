package board

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/find"
	"swarmmemo/internal/gate"
	"swarmmemo/internal/services"
)

// Every operation whose command fields name data has a data spec, and only
// those do.
func TestDataSpecCoversEveryDataOperation(t *testing.T) {
	for _, op := range Operations() {
		_, spec := DataSpecOf(op.Name)
		if takes := slices.Contains(strings.Fields(op.Fields), "data"); takes != spec {
			t.Errorf("%s: takes data %t, has a data spec %t", op.Name, takes, spec)
		}
	}
	for name, d := range dataSpecs {
		if !KnownOperation(name) {
			t.Errorf("data spec for unknown operation %s", name)
		}
		if (d.Raw != "") == (len(d.Fields) > 0) {
			t.Errorf("%s: a data spec is raw or has fields, not both or neither", name)
		}
		for _, f := range d.Fields {
			if !validDataType(f.Type) {
				t.Errorf("%s.%s: type %q", name, f.Name, f.Type)
			}
		}
	}
}

// strictProbe is the name check of a parser that decodes into T with
// services.StrictObject.
func strictProbe[T any]() func(string) error {
	return func(raw string) error {
		var v T
		return services.StrictObject([]byte(raw), &v)
	}
}

// dataProbes run each operation's real data parser (or, where it decodes
// into a struct inside a Store method, the same strict decode into that
// struct) on raw data.
var dataProbes = map[string]func(string) error{
	"post":             func(raw string) error { _, err := parsePostData(raw); return err },
	"messages.list":    func(raw string) error { _, err := parseListOptions(raw); return err },
	"feed.get":         func(raw string) error { _, err := parseFeedRequest(raw); return err },
	"feed.profile.get": func(raw string) error { _, err := feedData("feed.profile.get", raw); return err },
	"feed.profile.put": func(raw string) error { _, err := feedData("feed.profile.put", raw); return err },
	"feed.profile.fork": func(raw string) error {
		_, err := feedData("feed.profile.fork", raw)
		return err
	},
	"room.subscribe":    func(raw string) error { _, err := feedData("room.subscribe", raw); return err },
	"updates.get":       func(raw string) error { _, err := parseUpdatesOptions(raw); return err },
	"updates.dispose":   strictProbe[disposeData](),
	"updates.mark_read": strictProbe[markReadData](),
	"vote":              func(raw string) error { _, err := parseVoteData(raw); return err },
	"room.policy.set":   func(raw string) error { _, err := decodePolicyData(raw); return err },
	"room.style.set":    func(raw string) error { _, err := parseStyle(raw); return err },
	"room.style.check":  func(raw string) error { _, err := parseStyle(raw); return err },
	"agent.profile.publish": func(raw string) error {
		_, err := parsePeerData(raw)
		return err
	},
	"key.backup.put":     func(raw string) error { _, err := parseKeyBackup(raw, false); return err },
	"device.request":     func(raw string) error { _, err := parseDeviceData("device.request", raw); return err },
	"device.request.get": func(raw string) error { _, err := parseDeviceData("device.request.get", raw); return err },
	"device.add":         func(raw string) error { _, err := parseDeviceData("device.add", raw); return err },
	"key.backup.get":     func(raw string) error { _, err := parseKeyBackup(raw, true); return err },
	"identity.link":      func(raw string) error { _, err := parseLinkData(raw, true); return err },
	"identity.unlink":    func(raw string) error { _, err := parseLinkData(raw, false); return err },
	"identity.witness":   func(raw string) error { _, err := parseWitnessData(raw); return err },
	"verify.publish":     func(raw string) error { _, err := parseVerifyData(raw); return err },
	"standing.challenge": func(raw string) error {
		_, _, err := parseStandingData(raw, dataSpecs["standing.challenge"].Names(false)...)
		return err
	},
	"standing.work": func(raw string) error {
		_, _, err := parseStandingData(raw, dataSpecs["standing.work"].Names(false)...)
		return err
	},
	"gate.challenge": func(raw string) error {
		_, err := gate.ParseChallengeRequest([]byte(raw), ValidWebOrigin)
		return err
	},
	"gate.verify": func(raw string) error { _, err := gate.ParseVerifyRequest([]byte(raw), false); return err },
	"vouch":       func(raw string) error { _, _, _, err := parseVouchData(raw); return err },
	"works.list":  func(raw string) error { _, _, _, _, _, err := worksListData(raw); return err },
	"delegation.create": func(raw string) error {
		_, _, _, err := parseDelegationData(raw, true)
		return err
	},
	"delegation.revoke": func(raw string) error {
		_, _, _, err := parseDelegationData(raw, false)
		return err
	},
	"private_read.create": func(raw string) error { _, _, err := parsePrivateReadData(raw, true); return err },
	"private_read.revoke": func(raw string) error { _, _, err := parsePrivateReadData(raw, false); return err },
	"webhook.create":      func(raw string) error { _, _, err := parseWebhookData(raw, true); return err },
	"spend_limit.set":     strictProbe[spendLimitSetData](),
	"allowance.get":       strictProbe[schemaOnlyData](),
	"allowance.transfer":  strictProbe[allowanceTransferData](),
	"credits.topup":       func(raw string) error { _, err := topupData(raw); return err },
	"offering.publish":    strictProbe[offeringData](),
	"offering.buy":        func(raw string) error { _, _, _, err := parseBuy(raw, offeringRow{}); return err },
	"offering.answer":     func(raw string) error { _, err := parseAnswer(raw); return err },
	"offering.refund":     func(raw string) error { _, err := parseRefund(raw); return err },
	"x402.submit":         strictProbe[x402SubmitData](),
	"subjects.list":       func(raw string) error { _, err := parseSubjectsListData(raw); return err },
	"find": func(raw string) error {
		_, err := FindRequest(Command{Operation: "find", Data: raw})
		return err
	},
	"conversation.open":    strictProbe[openData](),
	"conversation.get":     strictProbe[getData](),
	"conversation.respond": strictProbe[respondData](),
	"conversation.seal":    strictProbe[sealRotation](),
	"messaging.policy.set": strictProbe[messagingData](),
	"hosted.recover":       strictProbe[hostedData](),
	"hosted.token":         strictProbe[hostedData](),
	"hosted.claim":         strictProbe[hostedData](),
}

// Every work.* operation reads its data with parseWorkData.
func init() {
	for name := range dataSpecs {
		if strings.HasPrefix(name, "work.") {
			op := name
			dataProbes[op] = func(raw string) error { _, err := parseWorkData(raw, op); return err }
		}
	}
}

const (
	hex32 = "0123456789abcdef0123456789abcdef"
	hex64 = hex32 + hex32
	check = `[{"property":"build","state":"pass"}]`
)

// dataSampleValues are values a parser takes where the type's plain sample
// would fail a check that shares its refusal with an unknown field.
var dataSampleValues = map[string]map[string]string{
	"messages.list":         {"sort": `"hot"`, "scope": `"front"`},
	"updates.get":           {"wait": `1`},
	"vote":                  {"value": `1`},
	"work.promise":          {"amount_usdc": `"0.10"`},
	"room.style.set":        {"css": `"a{}"`},
	"room.style.check":      {"css": `"a{}"`},
	"post":                  {"format": `"markdown"`},
	"agent.profile.publish": {"capabilities": `["go"]`, "availability": `"available"`, "avatar": `{"kind":"sigil","seed":1}`, "ucp": `{"services":{"dev.ucp.shopping":[{"version":"2026-01-01"}]}}`},
	"identity.link": {"kind": `"url"`, "value": `"https://example.com"`, "nonce": `"abcdefghijklmnop"`, "nonce_log_size": `1`,
		"observed_height": `1`, "observed_time": `1700000000`},
	"identity.unlink":  {"kind": `"url"`},
	"identity.witness": {"kind": `"url"`, "nonce": `"abcdefghijklmnop"`, "verdict": `"verified"`, "checks": check},
	"verify.publish": {"repo": `"https://github.com/example/repo"`, "commit": `"` + hex32 + `01234567"`, "supersedes": `"0123456789abcdef"`,
		"checks": check},
	"gate.challenge":      {"origin": `"https://example.com"`, "nonce": `"abcdefgh"`, "mode": `"pow"`, "bits": fmt.Sprint(gate.BitsMax)},
	"gate.verify":         {"challenge": `"` + hex32 + `"`, "answer": `"42"`, "wallet": `"0x` + hex32 + `01234567"`, "signature": `"0x` + hex64 + hex64 + `01"`},
	"offering.refund":     {"tx_hash": `"0x` + hex64 + `"`},
	"work.accept":         {"checks": check},
	"work.reject":         {"checks": check},
	"work.create":         {"tiers": `[{"name":"bug","credits":1}]`},
	"vouch":               {"value": `1`},
	"delegation.create":   {"operations": `["post"]`, "disclosure": `"public"`, "origin": `"https://example.com"`},
	"private_read.create": {"disclosure": `"private"`},
	"webhook.create":      {"url": `"https://example.com/hook"`, "kinds": `["` + inboxReply + `"]`},
	"offering.answer":     {"content_type": `"` + OfferingAnswerTypes[0] + `"`},
	"subjects.list":       {"sort": `"recent"`},
	"find":                {"type": `"` + find.Types[0] + `"`, "output": `"` + find.Outputs[0] + `"`, "protocol": `"` + find.Protocols[0] + `"`, "parse": `"auto"`, "door": `"agent"`},
	"conversation.open":   {"kind": `"dm"`},
}

// dataSampleSets split an operation's fields into objects each of which the
// parser takes, where one object with every field could not pass (one
// proof of several); by default one object holds every field.
var dataSampleSets = map[string][][]string{
	"gate.verify": {{"schema", "challenge", "solution", "wallet", "signature"}, {"schema", "challenge", "answer"}},
}

// sampleValue is a plain value of f's type and pattern.
func sampleValue(f DataField) string {
	switch {
	case f.Name == "schema":
		return "1"
	case f.Pattern == "fingerprint":
		return `"` + hex64 + `"`
	case f.Pattern == "message_id" || f.Pattern == "work_id":
		return `"` + hex32 + `"`
	case f.Pattern == "tx_hash":
		return `"0x` + hex64 + `"`
	case f.Pattern == "standing_tier":
		return `"gold"`
	}
	switch strings.Split(f.Type, "|")[0] {
	case DataString:
		return `"x"`
	case DataInt, DataNumber:
		return "1"
	case DataBool:
		return "true"
	case DataObject:
		return "{}"
	case DataArray:
		return "[]"
	}
	panic(f.Type)
}

// sampleObject writes fields in order, then extra as the last field.
func sampleObject(op string, d DataSpec, names []string, extra string) string {
	var parts []string
	for _, name := range names {
		f, _ := d.Field(name)
		value, ok := dataSampleValues[op][name]
		if !ok {
			value = sampleValue(f)
		}
		key, _ := json.Marshal(name)
		parts = append(parts, string(key)+":"+value)
	}
	if extra != "" {
		parts = append(parts, `"`+extra+`":1`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// The spec is the enforcement: every operation's parser takes every field
// its spec declares, with a value of the declared type, and refuses a name
// the spec does not declare. A field the parser did not know would fail
// exactly as the unknown name placed after it does; a declared field that
// parses never fails that way.
func TestDataSpecIsTheEnforcement(t *testing.T) {
	const unknown = "zz_unknown"
	ops, fields := 0, 0
	for name, d := range dataSpecs {
		if d.Raw != "" {
			continue
		}
		ops++
		fields += len(d.Fields)
		probe := dataProbes[name]
		if probe == nil {
			t.Errorf("%s: no probe; add its parser to dataProbes", name)
			continue
		}
		sets := dataSampleSets[name]
		if sets == nil {
			sets = [][]string{d.Names(false)}
		}
		covered := map[string]bool{}
		for _, set := range sets {
			for _, required := range d.Names(true) {
				if !slices.Contains(set, required) {
					t.Errorf("%s: sample %v leaves out required %s", name, set, required)
				}
			}
			for _, field := range set {
				if !d.Takes(field) {
					t.Errorf("%s: sample names %s, which the spec does not take", name, field)
				}
				covered[field] = true
			}
			raw := sampleObject(name, d, set, "")
			got := probe(raw)
			refused := probe(sampleObject(name, d, set, unknown))
			if d.Lax {
				continue
			}
			if refused == nil {
				t.Errorf("%s: %s with an unknown field was taken", name, raw)
				continue
			}
			if got != nil && got.Error() == refused.Error() {
				t.Errorf("%s: %s fails as an unknown field does (a declared field the parser does not take, or a value of the wrong type): %v", name, raw, got)
			}
		}
		for _, field := range d.Names(false) {
			if !covered[field] {
				t.Errorf("%s: no sample sends %s", name, field)
			}
		}
	}
	t.Logf("%d operations with data objects, %d fields", ops, fields)
}

// work.create's data and the contract's work_create_data are one list.
func TestWorkCreateDataIsTheSpec(t *testing.T) {
	want := dataSpecs["work.create"].Names(false)
	slices.Sort(want)
	if got := WorkCreateDataFields(); !slices.Equal(got, want) {
		t.Fatalf("WorkCreateDataFields = %v, spec %v", got, want)
	}
}

// The normalizations of C377: vote and messages.list data may name schema
// 1 (and need not), subjects.list refuses an unknown field, and ledger.list
// takes no data.
func TestDataNormalizations(t *testing.T) {
	for _, raw := range []string{`{"value":1}`, `{"schema":1,"value":-1}`} {
		if _, err := parseVoteData(raw); err != nil {
			t.Errorf("vote %s: %v", raw, err)
		}
	}
	if _, err := parseVoteData(`{"schema":2,"value":1}`); err == nil {
		t.Error("vote took schema 2")
	}
	for _, raw := range []string{`{"sort":"hot"}`, `{"schema":1,"sort":"hot"}`} {
		if _, err := parseListOptions(raw); err != nil {
			t.Errorf("messages.list %s: %v", raw, err)
		}
	}
	if _, err := parseListOptions(`{"schema":2}`); err == nil {
		t.Error("messages.list took schema 2")
	}
	if _, err := parseSubjectsListData(`{"schema":1,"sort":"recent","extra":1}`); err == nil {
		t.Error("subjects.list took an unknown field")
	}
	if op, _ := LookupOperation("ledger.list"); slices.Contains(strings.Fields(op.Fields), "data") {
		t.Error("ledger.list declares data it never reads")
	}
}

// dataRawProtocol is how docs/PROTOCOL.md names each raw data shape.
var dataRawProtocol = map[string]string{
	DataRawBase64URL: "a string: unpadded base64url bytes",
	DataRawViewer:    "the string `viewer`",
	DataRawService:   "a method's call, each service's own: `schema`, `method`, `args` and a call's `max_cost`",
}

// dataFieldsProtocolSection is docs/PROTOCOL.md's data-fields table: each
// operation's data fields from its spec (C377), in the operations table's
// order.
func dataFieldsProtocolSection() string {
	code := func(names []string) string {
		out := make([]string, len(names))
		for i, n := range names {
			out[i] = "`" + n + "`"
		}
		return strings.Join(out, " ")
	}
	var b strings.Builder
	b.WriteString("| Operation | Required | Optional |\n|---|---|---|\n")
	for _, op := range Operations() {
		spec, ok := op.Data()
		if !ok {
			continue
		}
		link := fmt.Sprintf("[`%s`](#%s)", op.Name, op.Section)
		if spec.Raw != "" {
			raw, ok := dataRawProtocol[spec.Raw]
			if !ok {
				panic("dataFieldsProtocolSection: no words for raw data " + spec.Raw)
			}
			fmt.Fprintf(&b, "| %s | %s | |\n", link, raw)
			continue
		}
		optional := code(spec.Optional())
		var notes []string
		for _, f := range spec.Fields {
			if f.Items != nil {
				notes = append(notes, fmt.Sprintf("each `%s` entry: %s; optional %s", f.Name, code(f.Items.Names(true)), code(f.Items.Optional())))
			}
		}
		for _, f := range spec.Fields {
			switch {
			case f.RemovedIn != "":
				notes = append(notes, fmt.Sprintf("`%s` removed in %s", f.Name, f.RemovedIn))
			case f.AliasOf != "":
				notes = append(notes, fmt.Sprintf("`%s` for `%s`", f.Name, f.AliasOf))
			}
		}
		if spec.Lax {
			notes = append(notes, "other names ignored")
		}
		if len(notes) > 0 {
			optional = strings.TrimSpace(optional + " (" + strings.Join(notes, "; ") + ")")
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", link, code(spec.Names(true)), optional)
	}
	return b.String()
}
