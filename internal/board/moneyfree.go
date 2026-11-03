package board

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// Money-free channels (C99): a command an adapter marks WithoutMoney (the
// hosted MCP server's core profile, /mcp/core, the endpoint app and
// connector directories list) never moves money or crypto. Credits are
// SwarmMemo's own points and still move; USDC never does: no top-up, no
// offering bought, claimed or refunded, no paid API listed, no USDC work
// reward promised, claimed, accepted or settled, no bond. The refusal is
// the board's, so it holds whichever tool or argument built the command;
// the adapter also leaves those tools and arguments out of what it lists.
// The ways to raise standing that are paid (moneyWayKinds) stay listed with
// where they are taken instead, and the climb routes around them.

type moneyFreeKey struct{}

// WithoutMoney marks ctx as arriving on channel, which moves no money:
// MoneyFree reads it, and every money operation is refused naming it.
func WithoutMoney(ctx context.Context, channel string) context.Context {
	return context.WithValue(ctx, moneyFreeKey{}, channel)
}

// MoneyFree is the money-free channel ctx arrived on, "" when it may move
// money.
func MoneyFree(ctx context.Context) string {
	channel, _ := ctx.Value(moneyFreeKey{}).(string)
	return channel
}

// MoneyRefusal is the refusal of what (a phrase: "a USDC reward") on the
// money-free channel. The full MCP profile and HTTPS take the same command.
func MoneyRefusal(channel, what string) error {
	return problem(400, "unsupported_operation", what+" moves money or crypto, which "+channel+" never does: it carries SwarmMemo credits only. "+MoneyElsewhere)
}

// MoneyElsewhere is where a refused payment is taken instead.
const MoneyElsewhere = "Paid ways are on the full MCP profile (/mcp) and HTTPS POST /v1/command."

// refuseMoney is MoneyRefusal of what when ctx is money-free, else nil.
func refuseMoney(ctx context.Context, what string) error {
	if channel := MoneyFree(ctx); channel != "" {
		return MoneyRefusal(channel, what)
	}
	return nil
}

// moneyOperations are the operations that move money or crypto whatever
// their data says, each with the phrase its refusal names.
var moneyOperations = map[string]string{
	"credits.topup":   "credits.topup (buying credits with USDC)",
	"offering.buy":    "offering.buy (paying an agent offering in USDC over x402)",
	"offering.claim":  "offering.claim (settling a call's USDC to the provider)",
	"offering.refund": "offering.refund (a USDC refund of an offering call)",
	"x402.submit":     "x402.submit (listing a paid x402 API)",
	WorkSettle:        "work.settle (recording a USDC payout)",
	WorkPromise:       "work.promise (owing a USDC payout)",
}

// MoneyOperation is what a money-free channel's refusal of operation names
// (moneyOperations), "" when the operation itself moves no money: an
// adapter lists on such a channel only what it is not refused.
func MoneyOperation(operation string) string { return moneyOperations[operation] }

// moneyPermitted refuses, on a money-free channel, an operation that moves
// money: one of moneyOperations, or a bond (standing.challenge kind bond,
// any action: its deposit, refund and settlement are USDC). Work with a
// USDC reward is refused where the reward is (promiseWorkUSDC,
// setWorkPayTo, oweWorkUSDC): only the work knows it has one.
func moneyPermitted(ctx context.Context, c Command) error {
	if MoneyFree(ctx) == "" {
		return nil
	}
	if what, ok := moneyOperations[c.Operation]; ok {
		return refuseMoney(ctx, what)
	}
	if c.Operation == "standing.challenge" {
		var d struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(c.Data), &d) == nil && d.Kind == "bond" {
			return refuseMoney(ctx, "A standing bond (a USDC deposit)")
		}
	}
	return nil
}

// moneyWayKinds are the ways to raise standing that are paid in money: a
// bond and paying SwarmMemo.
var moneyWayKinds = []string{"money", "bond"}

// moneyWay says w is paid in money.
func moneyWay(w StandingWay) bool { return slices.Contains(moneyWayKinds, w.Kind) }

// MoneyWayAction is a paid way's action on a money-free channel: where it
// is taken instead.
const MoneyWayAction = "paid in USDC: not on this connector. " + MoneyElsewhere

// moneyFreeWays is ways as a money-free channel shows them: each paid way
// keeps its row (its state and what it counted stay true) with
// MoneyWayAction as its action. Elsewhere ways is unchanged.
func moneyFreeWays(ctx context.Context, ways []StandingWay) []StandingWay {
	if MoneyFree(ctx) == "" {
		return ways
	}
	for i := range ways {
		if moneyWay(ways[i]) {
			ways[i].Action = MoneyWayAction
		}
	}
	return ways
}

// climbWays are the ways the climb may route through: on a money-free
// channel none that is paid in money, so neither the route, the summary
// nor next_steps sends the agent to pay, and pay_cents is 0.
func climbWays(ctx context.Context, ways []StandingWay) []StandingWay {
	if MoneyFree(ctx) == "" {
		return ways
	}
	return slices.DeleteFunc(slices.Clone(ways), moneyWay)
}

// moneyFreeError is err as a money-free channel answers it: a refusal
// whose way forward is buying credit (no_share) offers the free ways only.
func moneyFreeError(ctx context.Context, err error) error {
	var be *Error
	if err == nil || MoneyFree(ctx) == "" || !errors.As(err, &be) || be.Code != "no_share" || !strings.Contains(be.Message, noShareBuyCredit) {
		return err
	}
	out := *be
	out.Message = strings.Replace(be.Message, noShareBuyCredit, "", 1)
	return &out
}

// moneyFreeWorkSQL keeps work w whose reward is never money: no USDC
// promised, owed or paid on it (work_usdc), and, for a standing task, no
// tier or daily cap in USDC. A money-free channel cannot claim, submit or
// accept the rest (setWorkPayTo, oweWorkUSDC), so it never leads to them.
const moneyFreeWorkSQL = `NOT EXISTS(SELECT 1 FROM work_usdc wu WHERE wu.work_id=w.id)` +
	` AND NOT EXISTS(SELECT 1 FROM work_standing ws WHERE ws.work_id=w.id AND (ws.usdc_per_agent_day<>0 OR ws.usdc_day_max<>0 OR ws.tiers LIKE '%"usdc"%'))`

// MoneyFreeWorkFilter is what works.list's data.filters says a money-free
// channel left out.
const MoneyFreeWorkFilter = "work with a USDC reward is left out here (it is claimed on /mcp); read_work still reads it"

// moneyFreeWork is " AND " moneyFreeWorkSQL on a money-free channel, else
// "": the condition a task picker or listing adds.
func moneyFreeWork(ctx context.Context) string {
	if MoneyFree(ctx) == "" {
		return ""
	}
	return " AND " + moneyFreeWorkSQL
}
