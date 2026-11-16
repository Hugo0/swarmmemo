package httpapi

import (
	"encoding/json"
	"reflect"
	"testing"

	"swarmmemo/internal/board"
)

// C119: work_coordination.reject_fee appears only with WORK_REJECT_FEE on;
// off, /capabilities is exactly as before.
func TestCapabilitiesRejectFeeFollowsTheFlag(t *testing.T) {
	caps := func(features board.Features) map[string]any {
		t.Helper()
		var c map[string]any
		if err := json.Unmarshal(makeRequest(New(&fakeService{}, nil, Config{Features: features}), "GET", capabilitiesFullPath, "", "").Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	off, on := caps(board.Features{}), caps(board.Features{WorkRejectFee: true})
	if _, ok := off["work_coordination"].(map[string]any)["reject_fee"]; ok {
		t.Fatal("reject_fee advertised while off")
	}
	fee, ok := on["work_coordination"].(map[string]any)["reject_fee"].(map[string]any)
	if !ok || fee["field"] != "work.create data reject_fee" || fee["unit"] != "credit" || fee["flag"] != "WORK_REJECT_FEE" || fee["cap"] == nil || fee["paid"] == nil || fee["returned"] == nil || fee["instructions"] != "/protocol.md#work-reject-fee" {
		t.Fatalf("reject_fee capability: %v", fee)
	}
	delete(on["work_coordination"].(map[string]any), "reject_fee")
	if !reflect.DeepEqual(off, on) {
		t.Fatal("the flag changed /capabilities beyond work_coordination.reject_fee")
	}
}
