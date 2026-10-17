package board

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"swarmmemo/internal/allowance"
)

// Command results keep the shapes clients/contract.json names (C398): each
// acknowledgement the store answers, as it goes on the wire, carries every
// required field of its struct, no key the struct does not name, and an
// intent-tagged field only when the signed data asked for it. A key added
// outside the struct fails here; one added to the struct fails
// internal/clientcontract until the contract (and so every client) has it.
func TestResultShapes(t *testing.T) {
	s := reputationStore(t)
	owner, worker, parent, child, plain := keyFor(230), keyFor(231), keyFor(232), keyFor(233), keyFor(234)
	mintCredit(t, s, keyID(owner), allowance.Paid, 1_000_000)
	checked := map[string]bool{}
	check := func(name string, c Command, res Result, data, ack any) {
		t.Helper()
		var intent map[string]any
		_ = json.Unmarshal([]byte(c.Data), &intent)
		raw, err := json.Marshal(res.Data)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err = json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		checkWireShape(t, name+" data", wire, data, intent)
		var inner map[string]json.RawMessage
		if err = json.Unmarshal(wire["ack"], &inner); err != nil {
			t.Fatalf("%s: ack %s", name, wire["ack"])
		}
		checkWireShape(t, name+" ack", inner, ack, intent)
		checked[name] = true
	}
	exec := func(c Command, data, ack any) Result {
		t.Helper()
		res := run(t, s, c)
		check(c.Operation, c, res, data, ack)
		return res
	}

	// Work: create with a reward, claim, submit, a rated accept (review
	// and the paid note); then claim and reject on a second task.
	id := rewardRequest(t, s, owner, "lobby")
	exec(rewardCreate(s, owner, id, 12_000, 0), WorkAckData{}, WorkAck{})
	claim := exec(workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600}), WorkAckData{}, WorkAck{}).Data["ack"].(WorkAck)
	result := workResult(t, s, worker, id, "lobby")
	exec(workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: claim.Fence, Target: result}), WorkAckData{}, WorkAck{})
	accepted := exec(ratedAccept(t, s, owner, id, claim.Fence, keyID(worker), 80, "Good."), WorkAckData{}, WorkAck{})
	if accepted.Data["review"] == nil {
		t.Fatalf("rated accept without its review: %+v", accepted.Data)
	}
	second := rewardRequest(t, s, owner, "lobby")
	exec(workCommand(s, owner, Command{Operation: "work.create", MessageID: second}), WorkAckData{}, WorkAck{})
	fence := claimAndSubmit(t, s, worker, second, "lobby")
	exec(workCommand(s, owner, Command{Operation: "work.reject", MessageID: second, Amount: fence, Reason: "Not yet."}), WorkAckData{}, WorkAck{})

	// Delegation: a grant with a spend limit, one without, and a revoke.
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "shape-room", Visibility: "public"}))
	limited := exec(limitedGrant(s, parent, child, "shape-room", map[string]any{"credit_per_day": 100}), DelegationAckData{}, DelegationAck{})
	if limited.Data["ack"].(DelegationAck).SpendLimit == nil {
		t.Fatalf("limited grant without its spend_limit: %+v", limited.Data)
	}
	exec(grantCommand(s, parent, plain, "shape-room", 3600, 1<<20, []string{"post"}), DelegationAckData{}, DelegationAck{})
	exec(revokeCommand(s, parent, keyID(child)), DelegationAckData{}, DelegationAck{})
	for _, op := range []string{"work.create", "work.claim", "work.submit", "work.accept", "work.reject", "delegation.create", "delegation.revoke"} {
		if !checked[op] {
			t.Errorf("%s's result was not checked", op)
		}
	}
}

// checkWireShape fails unless wire's keys are value's wire fields: every
// required one, no unnamed one, and an intent-tagged optional one only when
// the signed data carried its field.
func checkWireShape(t *testing.T, name string, wire map[string]json.RawMessage, value any, intent map[string]any) {
	t.Helper()
	required, optional, gated := WireFields(value)
	for _, key := range required {
		if _, ok := wire[key]; !ok {
			t.Errorf("%s: required %q missing from %v", name, key, slices.Sorted(maps.Keys(wire)))
		}
	}
	for key := range wire {
		if !slices.Contains(required, key) && !slices.Contains(optional, key) {
			t.Errorf("%s: %q is not a field of %T; add it to the struct (and so the contract)", name, key, value)
		}
		if from, ok := gated[key]; ok {
			if _, asked := intent[from]; !asked {
				t.Errorf("%s: %q present though the signed data has no %q", name, key, from)
			}
		}
	}
}
