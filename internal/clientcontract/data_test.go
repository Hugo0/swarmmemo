package clientcontract

import (
	"slices"
	"testing"

	"swarmmemo/internal/board"
)

// Every enum, pattern and limit a data field names is an entry of this
// contract, and every operation that takes data carries its spec.
func TestDataSpecReferences(t *testing.T) {
	c := Build()
	enums := c["enums"].(map[string][]string)
	patterns := c["patterns"].(map[string]string)
	limits := c["limits"].(map[string]int64)
	operations := c["operations"].(map[string]Operation)
	fields := 0
	for name, op := range operations {
		if slices.Contains(op.Fields, "data") != (op.Data != nil) {
			t.Errorf("%s: fields name data %t, data spec %t", name, slices.Contains(op.Fields, "data"), op.Data != nil)
		}
		if op.Data == nil {
			continue
		}
		all := map[string]DataField{}
		for field, f := range op.Data.Fields {
			all[field] = f
			if f.Items != nil {
				for item, g := range f.Items.Fields {
					all[field+"[]."+item] = g
				}
			}
		}
		for field, f := range all {
			fields++
			if _, ok := enums[f.Enum]; f.Enum != "" && !ok {
				t.Errorf("%s data.%s: enum %q is not in the contract", name, field, f.Enum)
			}
			if _, ok := patterns[f.Pattern]; f.Pattern != "" && !ok {
				t.Errorf("%s data.%s: pattern %q is not in the contract", name, field, f.Pattern)
			}
			if _, ok := limits[f.Limit]; f.Limit != "" && !ok {
				t.Errorf("%s data.%s: limit %q is not in the contract", name, field, f.Limit)
			}
		}
	}
	if fields == 0 {
		t.Fatal("no data fields in the contract")
	}
	// work_create_data stays, the spec's work.create fields.
	want := []string{}
	for field := range operations["work.create"].Data.Fields {
		want = append(want, field)
	}
	slices.Sort(want)
	if got := c["work_create_data"].([]string); !slices.Equal(got, want) {
		t.Errorf("work_create_data %v, spec %v", got, want)
	}
	if _, ok := board.DataSpecOf("ledger.list"); ok {
		t.Error("ledger.list has a data spec")
	}
}

// The command results clients validate are in the contract (C398), and an
// intent-gated result field names an optional field of the struct and a data
// field the command it answers takes. internal/board TestResultShapes checks
// the store's answers against the same structs.
func TestResultStructs(t *testing.T) {
	c := Build()
	structs := c["structs"].(map[string]Fields)
	operations := c["operations"].(map[string]Operation)
	answers := map[string]string{"work_ack_data": "work.accept", "work_ack": "work.claim", "delegation_ack_data": "delegation.create", "delegation_ack": "delegation.create", "delegation_status": "delegation.get"}
	for name, op := range answers {
		s, ok := structs[name]
		if !ok {
			t.Fatalf("structs.%s is not in the contract", name)
		}
		for field, from := range s.Intent {
			if !slices.Contains(s.Optional, field) {
				t.Errorf("structs.%s: intent field %q is not optional", name, field)
			}
			if data := operations[op].Data; data == nil || data.Fields[from].Type == "" {
				t.Errorf("structs.%s.%s: %s data takes no %q", name, field, op, from)
			}
		}
	}
	if structs["work_ack_data"].Intent["review"] != "review" || structs["delegation_ack"].Intent["spend_limit"] != "spend_limit" {
		t.Errorf("intent-gated fields: %+v, %+v", structs["work_ack_data"], structs["delegation_ack"])
	}
	if !slices.Equal(structs["work_ack_data"].Required, []string{"ack"}) || !slices.Equal(structs["delegation_ack_data"].Required, []string{"ack"}) {
		t.Error("a result's data must carry its ack")
	}
}
