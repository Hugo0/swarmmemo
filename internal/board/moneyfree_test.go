package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// A money-free channel (moneyfree.go): its refusals, its no_share way
// forward without buying, and a climb that never routes through paying.
func TestMoneyFreeChannel(t *testing.T) {
	free := WithoutMoney(context.Background(), "/mcp/core")
	if MoneyFree(free) != "/mcp/core" || MoneyFree(context.Background()) != "" {
		t.Fatal("MoneyFree does not read the mark")
	}
	for _, c := range []Command{{Operation: "credits.topup"}, {Operation: WorkSettle}, {Operation: "standing.challenge", Data: `{"schema":1,"kind":"bond"}`}} {
		var be *Error
		if err := moneyPermitted(free, c); !errors.As(err, &be) || be.Code != "unsupported_operation" || !strings.Contains(be.Message, MoneyElsewhere) {
			t.Errorf("%s on a money-free channel: %v", c.Operation, err)
		}
		if err := moneyPermitted(context.Background(), c); err != nil {
			t.Errorf("%s elsewhere: %v", c.Operation, err)
		}
	}
	if err := moneyPermitted(free, Command{Operation: "standing.challenge", Data: `{"schema":1,"kind":"wallet","value":"0x0"}`}); err != nil {
		t.Errorf("a wallet link is not a payment: %v", err)
	}

	noShare := fromAllowance(&allowance.Err{Code: "no_share", Resource: allowance.Credit, Band: allowance.BandAnonymous})
	if !strings.Contains(noShare.Error(), noShareBuyCredit) {
		t.Fatalf("no_share no longer offers buying credit: the strip is stale: %v", noShare)
	}
	var be *Error
	if err := moneyFreeError(free, noShare); !errors.As(err, &be) || be.Code != "no_share" || strings.Contains(be.Message, "credits.topup") || !strings.Contains(be.Message, "receive a credit transfer") {
		t.Errorf("no_share on a money-free channel: %v", err)
	}
	if moneyFreeError(context.Background(), noShare) != noShare {
		t.Error("no_share elsewhere changed")
	}

	p := trust.DefaultParams()
	ways := []StandingWay{
		{Kind: "pow", Label: "Proof of work", Priced: true, State: "none", Status: "enabled", AddsCents: &StandingRange{Max: 50}, Action: "pow", EffortMinutes: 30},
		{Kind: "bond", Label: "Post a bond", Priced: true, State: "none", Status: "enabled", AddsCents: &StandingRange{Min: 100, Max: 500}, Needs: []string{"usdc"}, EffortMinutes: 5, Action: "bond"},
		{Kind: "money", Label: "Pay SwarmMemo", Priced: true, State: "none", AddsCents: &StandingRange{Max: 750}, Action: "topup", Needs: []string{"usdc"}, EffortMinutes: 2},
		{Kind: "earned", Label: "Earned standing", Priced: true, State: "none", EffortMinutes: earnedDayMinutes},
	}
	holds := climbHolds{Needs: map[string]bool{"usdc": true}}
	if c := climbOf(p, 0, climbWays(context.Background(), ways), holds); c.PayCents == 0 {
		t.Fatalf("the climb elsewhere has no pay route: %+v", c)
	}
	c := climbOf(p, 0, climbWays(free, ways), holds)
	if c.PayCents != 0 {
		t.Errorf("a money-free climb pays %d", c.PayCents)
	}
	for _, st := range c.Route {
		if st.Kind == "money" || st.Kind == "bond" || st.CostCents != 0 {
			t.Errorf("a money-free route pays: %+v", c.Route)
		}
	}
	shown := moneyFreeWays(free, append([]StandingWay(nil), ways...))
	if shown[0].Action != "pow" || shown[1].Action != MoneyWayAction || shown[2].Action != MoneyWayAction || len(shown) != len(ways) {
		t.Errorf("money-free ways: %+v", shown)
	}
}

// A money-free channel never leads an agent to work it could not claim
// there: home's open_task, first_work and works.list skip any task with a
// USDC reward (an ordinary one, or a standing task whose tiers pay USDC);
// elsewhere the same reads still offer it, newest first.
func TestMoneyFreeLeadsToNoUSDCWork(t *testing.T) {
	s, _ := usdcStore(t)
	owner, worker := keyFor(190), keyFor(191)
	mintCredit(t, s, keyID(owner), allowance.Paid, 20000)
	run(t, s, signed(worker, Command{Operation: "agent.register"}))
	base := s.now()
	at := func(seconds int) { s.now = func() time.Time { return base.Add(time.Duration(seconds) * time.Second) } }
	credits := rewardRequest(t, s, owner, "lobby")
	run(t, s, earnCreate(s, owner, credits, "Credit task", []string{"review"}, 50))
	at(10)
	usdc := rewardRequest(t, s, owner, "lobby")
	run(t, s, rewardCreate(s, owner, usdc, map[string]any{"credits": 60, "usdc": "0.10"}, 0))
	at(20)
	standing := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, standing, map[string]any{"seats": 2, "tiers": []any{map[string]any{"name": "bug", "credits": 100, "usdc": "0.20"}}}, 10*86400))
	at(30)
	free := WithoutMoney(testContext, "/mcp/core")

	openTask := func(ctx context.Context) string {
		t.Helper()
		res, err := s.Execute(ctx, signed(worker, Command{Operation: "home", Timestamp: s.now().Unix()}), "test")
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range res.Data["items"].([]HomeItem) {
			if it.Kind == "open_task" {
				return fmt.Sprint(it.Tool.Arguments["message_id"])
			}
		}
		return ""
	}
	picked := func(ctx context.Context) string {
		task, _ := s.claimableTask(ctx, keyID(worker))
		return task.id
	}
	if got := openTask(testContext); got != usdc {
		t.Fatalf("home open_task elsewhere: %q, want the newest, the USDC task %s", got, usdc)
	}
	if got := openTask(free); got != credits {
		t.Fatalf("home open_task on a money-free channel: %q, want the credit task %s", got, credits)
	}
	if picked(free) != credits || picked(testContext) != usdc {
		t.Fatalf("first_work's task: money-free %q, elsewhere %q", picked(free), picked(testContext))
	}
	// The first_work tip itself, on each channel.
	tip := func(ctx context.Context) string {
		t.Helper()
		s.nudgeSubjectFor(testContext, keyID(worker), keyID(worker), s.now().Unix()).seen = []nudgeSeen{{"replies_delivered", s.now().Unix()}}
		c := signed(worker, Command{Operation: "post", Text: "what next" + testPostSuffix(), RequestID: randomID(), Timestamp: s.now().Unix()})
		res, err := s.Execute(ctx, c, "198.51.100.42")
		if err != nil {
			t.Fatal(err)
		}
		s.Nudge(ctx, c, &res, NudgeEnv{Base: "https://swarmmemo.com", Surface: "receipt", Source: "198.51.100.42"})
		if nudgeOf(res) != "first_work" {
			return ""
		}
		return res.Next.Nudge.How + " " + res.Next.Nudge.Text
	}
	if got := tip(free); !strings.Contains(got, credits) || strings.Contains(got, usdc) || strings.Contains(got, "USDC") {
		t.Fatalf("first_work on a money-free channel: %q", got)
	}
	if got := tip(testContext); !strings.Contains(got, usdc) {
		t.Fatalf("first_work elsewhere: %q", got)
	}

	ids := func(ctx context.Context, kind string) ([]string, any) {
		t.Helper()
		res, err := s.Execute(ctx, Command{Operation: "works.list", Room: "lobby", Kind: kind}, "test")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, w := range res.Data["works"].([]Work) {
			out = append(out, w.ID)
		}
		return out, res.Data["filters"]
	}
	for _, kind := range []string{"", "open", WorkKindRewarded, WorkKindEarn} {
		got, filters := ids(free, kind)
		if slices.Contains(got, usdc) || slices.Contains(got, standing) || !slices.Contains(got, credits) || fmt.Sprint(filters) != fmt.Sprint(map[string]any{"money_free": MoneyFreeWorkFilter}) {
			t.Errorf("works.list kind %q on a money-free channel: %v, filters %v", kind, got, filters)
		}
		got, filters = ids(testContext, kind)
		if !slices.Contains(got, usdc) || !slices.Contains(got, credits) || filters != nil {
			t.Errorf("works.list kind %q elsewhere: %v, filters %v", kind, got, filters)
		}
	}
	// work.get still reads a USDC task on a money-free channel.
	if res, err := s.Execute(free, Command{Operation: "work.get", MessageID: usdc}, "test"); err != nil || res.Data["work"].(Work).RewardUSDC == nil {
		t.Fatalf("work.get of USDC work on a money-free channel: %v", err)
	}

	// With the credit task taken, nothing is left to lead to on the
	// money-free channel: the standing task pays USDC too.
	run(t, s, claimCommand(s, worker, credits))
	if got := picked(free); got != "" {
		t.Fatalf("money-free picker with only USDC work open: %q", got)
	}
	if got := picked(testContext); got != usdc {
		t.Fatalf("picker elsewhere: %q", got)
	}
}
