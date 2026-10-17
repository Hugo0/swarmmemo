package board

import (
	"reflect"
	"slices"
	"strings"
)

// Result data shapes (C398). A command's acknowledgement is a named struct
// here, built by the code that answers the command (ResultData) and read by
// internal/clientcontract into clients/contract.json's structs, so a client
// derives the keys it accepts from the server's types instead of keeping a
// hand-written set. An optional field tagged intent:"name" is present only
// when the signed command's data carried that field.

// WorkAckData is a work transition's result data: its acknowledgement and,
// on a rated accept, the requester's review receipt.
type WorkAckData struct {
	Ack    WorkAck  `json:"ack"`
	Review *Receipt `json:"review,omitempty" intent:"review"`
}

// DelegationAckData is delegation.create's and delegation.revoke's result data.
type DelegationAckData struct {
	Ack DelegationAck `json:"ack"`
}

// WireFields is a wire object's field set read from its json tags: required
// (no omitempty), optional, and each optional field's intent tag. An
// untagged embedded struct's fields are flattened, as encoding/json writes
// them.
func WireFields(value any) (required, optional []string, intent map[string]string) {
	required, optional, intent = []string{}, []string{}, map[string]string{}
	for _, field := range reflect.VisibleFields(reflect.TypeOf(value)) {
		name, omitempty, ok := wireName(field)
		if !ok {
			continue
		}
		if !omitempty {
			required = append(required, name)
			continue
		}
		optional = append(optional, name)
		if from := field.Tag.Get("intent"); from != "" {
			intent[name] = from
		}
	}
	slices.Sort(required)
	slices.Sort(optional)
	return required, optional, intent
}

// ResultData is a result data struct (WorkAckData, DelegationAckData) as
// Result.Data: each field under its json name, holding its value as is, an
// omitempty field only when it is set.
func ResultData(value any) map[string]any {
	v := reflect.ValueOf(value)
	out := map[string]any{}
	for _, field := range reflect.VisibleFields(v.Type()) {
		name, omitempty, ok := wireName(field)
		if !ok {
			continue
		}
		f := v.FieldByIndex(field.Index)
		if omitempty && f.IsZero() {
			continue
		}
		out[name] = f.Interface()
	}
	return out
}

// wireName is field's json name and whether it is omitempty; ok is false for
// a field encoding/json leaves out (or flattens: an untagged embedded struct).
func wireName(field reflect.StructField) (name string, omitempty, ok bool) {
	name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
	if !field.IsExported() || name == "" || name == "-" {
		return "", false, false
	}
	return name, strings.Contains(","+opts+",", ",omitempty,"), true
}
