package httpapi

import "testing"

// Independent HTTP/A2A request-parameter regression, entirely in memory.
// An A2A caller setting historyLength=-1 has not requested unbounded history;
// a negative maximum must not silently act like the field was omitted.
func TestA2ANegativeHistoryLengthRejected(t *testing.T) {
	rig := newA2ARig(t, 1000)
	addr := "192.0.2.220"
	created, err := rig.rpc(addr, "SendMessage", textMessage("history-seed-one", "public testing message", nil))
	if err != nil {
		t.Fatalf("set up isolated task: %v", err)
	}
	task := created["task"].(map[string]any)
	id := task["id"].(string)
	for _, method := range []string{"GetTask", "tasks/get"} {
		got, e := rig.rpc(addr, method, map[string]any{"id": id, "historyLength": -1})
		if e == nil || e["code"] != float64(a2aInvalidParams) {
			t.Errorf("%s must reject negative historyLength with invalid params, got result=%v error=%v", method, got, e)
		}
	}
	for _, method := range []string{"SendMessage", "message/send"} {
		got, e := rig.rpc(addr, method, map[string]any{
			"message": map[string]any{
				"messageId": "neg-history-" + method,
				"role":      "user",
				"parts":     []any{map[string]any{"text": "negative should not post"}},
			},
			"configuration": map[string]any{"historyLength": -1},
		})
		if e == nil || e["code"] != float64(a2aInvalidParams) {
			t.Errorf("%s must reject negative historyLength before any new post, got result=%v error=%v", method, got, e)
		}
	}
	valid, e := rig.rpc(addr, "GetTask", map[string]any{"id": id, "historyLength": 0})
	if e != nil || len(taskHistory(valid)) != 0 {
		t.Errorf("historyLength=0 must still show empty history: %#v %#v", valid, e)
	}
	valid, e = rig.rpc(addr, "GetTask", map[string]any{"id": id, "historyLength": 1})
	if e != nil || len(taskHistory(valid)) != 1 {
		t.Errorf("historyLength=1 must show one message: %#v %#v", valid, e)
	}
}
