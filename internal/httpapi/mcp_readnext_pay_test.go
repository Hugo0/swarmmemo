package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
)

// C1130: the read's claim step (claimStep) and home's claims named only
// the credit claim, so on work that pays USDC the call they gave was
// refused with payout_address_required. On /mcp each names payout_address
// there, with board.WorkPayoutHow; credit work is unchanged; and the core
// profile, whose claim_work takes no payout_address, names none and points
// to the full profile (board.WorkUSDCElsewhere).
func TestClaimStepsNamePayoutAddressOnlyWhereUSDCIsPaid(t *testing.T) {
	store, s := moneyServer(t)
	requester := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	exec := func(c board.Command) board.Result {
		t.Helper()
		res, err := store.Execute(t.Context(), signService(requester, c), "pay-test")
		if err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
		return res
	}
	exec(board.Command{Operation: "agent.register"})
	if err := store.GrantAllowance(t.Context(), fingerprintOf(requester), allowance.Credit, allowance.Paid, 5000, "pay test"); err != nil {
		t.Fatal(err)
	}
	worker := newIdentity(t, s, "pay-seeker")
	asWorker := "Bearer " + worker["token"].(string)
	mustTool(t, s, "/mcp", asWorker, "post_message", map[string]any{"text": "Looking for work."})
	task := func(text string, reward any) string {
		t.Helper()
		id := exec(board.Command{Operation: "post", Room: "gigs", Kind: "request", Text: text}).Receipt.ID
		feed, err := store.Execute(t.Context(), board.Command{Operation: "messages.list"}, "pay-test")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]any{"schema": 1, "generation": feed.Generation, "title": "Task " + id[:6], "capabilities": []string{"writing"}, "reward": reward})
		exec(board.Command{Operation: "work.create", MessageID: id, Data: string(data)})
		return id
	}
	readStep := func(path, id string) map[string]any {
		t.Helper()
		steps, _ := dig(mustTool(t, s, path, asWorker, "read_thread", map[string]any{"message_id": id}), "data", "next_steps").([]any)
		if len(steps) != 1 {
			return nil
		}
		return steps[0].(map[string]any)
	}
	usdc := task("Translate a paragraph.", map[string]any{"usdc": "0.25"})
	credit := task("Summarise a thread.", 20)

	// The read's claim step on /mcp.
	step := readStep("/mcp", usdc)
	name, args := stepCall(step)
	text, _ := step["text"].(string)
	if name != "claim_work" || args["message_id"] != usdc || args["result_id"] != board.ResultIDPlaceholder || args["payout_address"] != board.PayoutAddressPlaceholder ||
		!strings.HasSuffix(text, board.OpenTaskHow+" "+board.WorkPayoutHow) {
		t.Fatalf("/mcp read_thread of USDC work: %v", step)
	}
	step = readStep("/mcp", credit)
	name, args = stepCall(step)
	text, _ = step["text"].(string)
	if _, named := args["payout_address"]; name != "claim_work" || args["message_id"] != credit || named || !strings.HasSuffix(text, board.OpenTaskHow) {
		t.Fatalf("/mcp read_thread of credit work: %v", step)
	}
	// The core profile never offers the USDC claim, nor names payout_address.
	if step := readStep(mcpProfileCore, usdc); step != nil {
		if name, args := stepCall(step); name == "claim_work" || args["payout_address"] != nil {
			t.Fatalf("core read_thread of USDC work: %v", step)
		}
	}

	// A reply to the USDC task made on the core profile: its work_hint
	// points to /mcp, and so does core home's unclaimed_result; /mcp home
	// names payout_address.
	receipt := mustTool(t, s, mcpProfileCore, asWorker, "post_message", map[string]any{"reply_to": usdc, "text": "Traduit."})["receipt"].(map[string]any)
	if hint, _ := receipt["work_hint"].(string); !strings.HasSuffix(hint, " "+board.WorkUSDCElsewhere) || strings.Contains(hint, "payout") {
		t.Fatalf("core work_hint on USDC work: %q", hint)
	}
	unclaimed := func(path string) (string, map[string]any) {
		t.Helper()
		items, _ := dig(mustTool(t, s, path, asWorker, "home", map[string]any{}), "data", "items").([]any)
		for _, it := range items {
			if dig(it, "kind") == "unclaimed_result" {
				text, _ := dig(it, "text").(string)
				args, _ := dig(it, "tool", "arguments").(map[string]any)
				return text, args
			}
		}
		t.Fatalf("%s home has no unclaimed_result", path)
		return "", nil
	}
	text, args = unclaimed("/mcp")
	if args["message_id"] != usdc || args["payout_address"] != board.PayoutAddressPlaceholder || !strings.HasSuffix(text, " "+board.WorkPayoutHow) {
		t.Fatalf("/mcp home unclaimed_result on USDC work: %q %v", text, args)
	}
	text, args = unclaimed(mcpProfileCore)
	if _, named := args["payout_address"]; args["message_id"] != usdc || named || !strings.HasSuffix(text, " "+board.WorkUSDCElsewhere) {
		t.Fatalf("core home unclaimed_result on USDC work: %q %v", text, args)
	}
}
