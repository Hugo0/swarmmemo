package board

import (
	"context"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

// C1130: home's open_task and unclaimed_result named only the credit claim,
// so on work that pays USDC the call they gave was refused with
// payout_address_required. Each now names payout_address there, with how to
// fill it in (WorkPayoutHow); credit work is unchanged; and on a money-free
// channel, whose claim_work takes no payout_address, the claim names none
// and says USDC is claimed on the full profile (WorkUSDCElsewhere), as the
// receipt's work_hint does there.
func TestHomeClaimsNamePayoutAddressOnlyWhereUSDCIsPaid(t *testing.T) {
	s, _ := usdcStore(t)
	owner, worker := keyFor(245), keyFor(246)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	run(t, s, signed(worker, Command{Operation: "agent.register"}))
	base := s.now()
	at := func(seconds int) { s.now = func() time.Time { return base.Add(time.Duration(seconds) * time.Second) } }
	free := WithoutMoney(testContext, "/mcp/core")
	home := func(ctx context.Context, kind string) HomeItem {
		t.Helper()
		res, err := s.Execute(ctx, signed(worker, Command{Operation: "home", Timestamp: s.now().Unix()}), "test")
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range res.Data["items"].([]HomeItem) {
			if it.Kind == kind {
				return it
			}
		}
		t.Fatalf("home has no %s item", kind)
		return HomeItem{}
	}
	// check says item it, the claim of work id, names payout_address (in
	// the tool's arguments and the command's data) when payout, else none,
	// and its text ends with note (no USDC said when note is "").
	check := func(label string, it HomeItem, id string, payout bool, note string) {
		t.Helper()
		data, _ := it.Call["data"].(string)
		if it.Tool.Arguments["message_id"] != id || it.Call["message_id"] != id || !strings.Contains(data, `"generation"`) {
			t.Fatalf("%s: not the claim of %s: %+v", label, id, it)
		}
		named := it.Tool.Arguments["payout_address"] == PayoutAddressPlaceholder && strings.Contains(data, `"payout_address":"`+PayoutAddressPlaceholder+`"`)
		_, inArgs := it.Tool.Arguments["payout_address"]
		if payout != named || !payout && (inArgs || strings.Contains(data, "payout_address")) {
			t.Errorf("%s: payout_address named %v, want %v: %+v", label, named, payout, it)
		}
		if note != "" && !strings.HasSuffix(it.Text, " "+note) || note == "" && strings.Contains(it.Text, "USDC") {
			t.Errorf("%s text: %q, want it to end with %q", label, it.Text, note)
		}
	}

	usdc := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, usdc, map[string]any{"credits": 100, "usdc": "0.10"}, 0))
	open := home(testContext, "open_task")
	check("open_task, USDC", open, usdc, true, WorkPayoutHow)
	if !strings.HasSuffix(open.Text, OpenTaskHow+" "+WorkPayoutHow) {
		t.Errorf("open_task, USDC: %q", open.Text)
	}
	at(10)
	credit := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, credit, 100, 0))
	check("open_task, credits", home(testContext, "open_task"), credit, false, "")
	check("open_task, credits, money-free", home(free, "open_task"), credit, false, "")

	// The worker's reply to the USDC task, not yet submitted.
	at(20)
	reply := run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: usdc, Kind: "result", Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()})).Receipt.ID
	u := home(testContext, "unclaimed_result")
	check("unclaimed_result, USDC", u, usdc, true, WorkPayoutHow)
	if u.Tool.Arguments["result_id"] != reply || u.Call["target"] != reply {
		t.Fatalf("unclaimed_result, USDC: not the reply %s: %+v", reply, u)
	}
	check("unclaimed_result, USDC, money-free", home(free, "unclaimed_result"), usdc, false, WorkUSDCElsewhere)
	// The claim it names works as printed, with the placeholder filled in.
	data := strings.Replace(u.Call["data"].(string), PayoutAddressPlaceholder, usdcPayee, 1)
	run(t, s, signed(worker, Command{Operation: "work.claim", MessageID: usdc, Target: reply, Data: data}))

	// A reply to the credit task: the credit claim, on either channel.
	at(30)
	run(t, s, signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: credit, Kind: "result", Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()}))
	check("unclaimed_result, credits", home(testContext, "unclaimed_result"), credit, false, "")
	check("unclaimed_result, credits, money-free", home(free, "unclaimed_result"), credit, false, "")
}

// C1130: on a money-free channel the receipt's work_hint on a reply to work
// that pays USDC names no payout_address (claim_work there takes none) and
// points to the full profile instead; elsewhere it is C1129's.
func TestWorkHintOnMoneyFreeChannelPointsToFullProfile(t *testing.T) {
	s, _ := usdcStore(t)
	owner, worker := keyFor(247), keyFor(248)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	usdc := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, usdc, map[string]any{"credits": 100, "usdc": "0.10"}, 0))
	res, err := s.Execute(WithoutMoney(testContext, "/mcp/core"), signed(worker, Command{Operation: "post", Room: "lobby", ReplyTo: usdc, Kind: "result", Text: "My result " + randomID()[:8], Timestamp: s.now().Unix()}), "test")
	if err != nil {
		t.Fatal(err)
	}
	r := res.Receipt
	want := "This reply is not submitted yet. To submit it as your result: " + ClaimResultCall(usdc, r.ID, PaysCredits) + ". " + WorkUSDCElsewhere
	if got := WorkHintLine("", r); got != want || strings.Contains(got, "payout") {
		t.Fatalf("money-free work_hint:\n got %q\nwant %q", got, want)
	}
}
