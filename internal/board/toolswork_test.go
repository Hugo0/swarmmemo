package board

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// The /tools/work page's commands, as published, on the real store, ledger
// and notary: each `python3 swarmmemo.py --key FILE command 'JSON'` line,
// its placeholders filled in, is the command the client signs and sends.
// The rewarded flow holds, survives a reject, pays once with a receipt; the
// reviewed flow pays the reviewer its fee and names it on the receipt; the
// rewarded filter lists exactly the open work with a held reward.

var toolsWorkLine = regexp.MustCompile(`(?m)^python3 swarmmemo\.py --key ([a-z]+\.json) command '(.+)'$`)

type toolsWorkCommand struct{ key, json string }

func toolsWorkCommands(t *testing.T) []toolsWorkCommand {
	t.Helper()
	raw, err := os.ReadFile("../../docs/TOOLS_WORK.md")
	if err != nil {
		t.Fatal(err)
	}
	var out []toolsWorkCommand
	for _, m := range toolsWorkLine.FindAllStringSubmatch(string(raw), -1) {
		out = append(out, toolsWorkCommand{m[1], m[2]})
	}
	return out
}

func TestToolsWorkPageCommandsRun(t *testing.T) {
	s := rewardStore(t)
	requester, worker, judge := keyFor(150), keyFor(151), keyFor(152)
	keys := map[string]ed25519.PrivateKey{"agent.json": requester, "worker.json": worker}
	register(t, s, judge)
	mintCredit(t, s, keyID(requester), allowance.Paid, 5000)

	byOp := map[string][]toolsWorkCommand{}
	for _, c := range toolsWorkCommands(t) {
		var probe struct {
			Operation string `json:"operation"`
		}
		if err := json.Unmarshal([]byte(regexp.MustCompile(`:FENCE\b`).ReplaceAllString(c.json, ":1")), &probe); err != nil {
			t.Fatalf("%s: %v", c.json, err)
		}
		byOp[probe.Operation] = append(byOp[probe.Operation], c)
	}
	for op, n := range map[string]int{"post": 2, "work.create": 2, "works.list": 1, "work.claim": 2, "work.submit": 1, "work.accept": 1, "work.reject": 1} {
		if len(byOp[op]) != n {
			t.Fatalf("the page shows %d %s commands, want %d", len(byOp[op]), op, n)
		}
	}
	values := map[string]string{"GENERATION": s.generation, "REVIEWER_FINGERPRINT": keyID(judge)}
	// do fills in the placeholders and runs the command as the client would:
	// strict fields, signed with key (the page's, unless as is given).
	do := func(c toolsWorkCommand, as ed25519.PrivateKey) Result {
		t.Helper()
		text := c.json
		for name, value := range values {
			if name == "FENCE" {
				text = regexp.MustCompile(`:FENCE\b`).ReplaceAllString(text, ":"+value)
				continue
			}
			text = strings.ReplaceAll(text, name, value)
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(text)))
		dec.DisallowUnknownFields()
		var cmd Command
		if err := dec.Decode(&cmd); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		key := keys[c.key]
		if as != nil {
			key = as
		}
		if key == nil {
			t.Fatalf("no key for %s", c.key)
		}
		cmd.Timestamp = s.now().Unix()
		return run(t, s, signed(key, cmd))
	}
	rewarded := func() []string {
		var ids []string
		for _, w := range run(t, s, Command{Operation: "works.list", Kind: WorkKindRewarded}).Data["works"].([]Work) {
			ids = append(ids, w.ID)
		}
		return ids
	}
	// attempt runs the worker's page commands: the result posted first and
	// claimed with it in one step, or (held) a claim with a window, the
	// reply, then the submit. The reply names no room; it lands in the
	// request's.
	attempt := func(held bool) {
		t.Helper()
		if held {
			values["FENCE"] = strconv.FormatInt(do(byOp["work.claim"][1], nil).Data["ack"].(WorkAck).Fence, 10)
			values["RESULT_ID"] = do(byOp["post"][1], nil).Receipt.ID
			do(byOp["work.submit"][0], nil)
			return
		}
		values["RESULT_ID"] = do(byOp["post"][1], nil).Receipt.ID
		ack := do(byOp["work.claim"][0], nil).Data["ack"].(WorkAck)
		if ack.State != "submitted" {
			t.Fatalf("claim with its result: state %s, want submitted", ack.State)
		}
		values["FENCE"] = strconv.FormatInt(ack.Fence, 10)
	}

	// Rewarded, decided by the requester: a reject keeps the hold, the
	// accept pays it once, with a notary receipt.
	unpaid := run(t, s, workCommand(s, requester, Command{Operation: "work.create", MessageID: rewardRequest(t, s, requester, "lobby")})).Data["ack"].(WorkAck).WorkID
	values["MESSAGE_ID"] = do(byOp["post"][0], nil).Receipt.ID
	first := values["MESSAGE_ID"]
	do(byOp["work.create"][0], nil)
	if w := getTestWork(t, s, first); w.Reward == nil || w.Reward.Amount != 500 || w.Reward.State != "held" {
		t.Fatalf("held reward: %+v", w.Reward)
	}
	if ids := rewarded(); len(ids) != 1 || ids[0] != first {
		t.Fatalf("rewarded work %v, want only %s (not the unpaid %s)", ids, first, unpaid)
	}
	// The worker's signed directory read says it may claim, with the task.
	listed := do(byOp["works.list"][0], nil).Data["works"].([]Work)
	if len(listed) != 1 || listed[0].Eligible == nil || !*listed[0].Eligible || listed[0].EligiblePreview || listed[0].Request == nil || listed[0].Request.Text == "" {
		t.Fatalf("signed rewarded list: %+v", listed)
	}
	attempt(false)
	if ids := rewarded(); len(ids) != 0 {
		t.Fatalf("submitted work is still listed as rewarded: %v", ids)
	}
	do(byOp["work.reject"][0], nil)
	if w := getTestWork(t, s, first); w.State != "open" || w.Reward.State != "held" {
		t.Fatalf("after reject: %s %+v", w.State, w.Reward)
	}
	attempt(true)
	do(byOp["work.accept"][0], nil)
	w := getTestWork(t, s, first)
	if w.State != "accepted" || w.Reward.State != "paid" || w.Reward.Receipt == nil || creditIn(t, s, keyID(worker), "remaining") != 500 {
		t.Fatalf("after accept: %s %+v", w.State, w.Reward)
	}
	if !strings.Contains(w.Reward.Receipt.Statement, `"worker":"`+keyID(worker)+`"`) {
		t.Fatalf("receipt statement: %s", w.Reward.Receipt.Statement)
	}

	// Reviewed: only the named reviewer decides, and is paid its fee.
	values["MESSAGE_ID"] = do(byOp["post"][0], nil).Receipt.ID
	second := values["MESSAGE_ID"]
	do(byOp["work.create"][1], nil)
	attempt(false)
	fails(t, s, signed(requester, Command{Operation: "work.accept", MessageID: second, Amount: mustAtoi(t, values["FENCE"]), Data: `{"schema":1,"generation":"` + s.generation + `"}`, Timestamp: s.now().Unix()}), "not_the_reviewer")
	do(byOp["work.reject"][0], judge)
	attempt(true)
	do(byOp["work.accept"][0], judge)
	w = getTestWork(t, s, second)
	if w.State != "accepted" || w.Reward.State != "paid" || w.ReviewerFee == nil || w.ReviewerFee.State != "paid" || creditIn(t, s, keyID(judge), "remaining") != 50 {
		t.Fatalf("reviewed work: %s reward %+v fee %+v", w.State, w.Reward, w.ReviewerFee)
	}
	if !strings.Contains(w.Reward.Receipt.Statement, `"reviewer":"`+keyID(judge)+`"`) {
		t.Fatalf("receipt names no reviewer: %s", w.Reward.Receipt.Statement)
	}
	fails(t, s, Command{Operation: "works.list", Kind: "rewarded-ish"}, "invalid_work_state")
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
