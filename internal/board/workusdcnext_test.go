package board

import (
	"reflect"
	"testing"

	"swarmmemo/internal/allowance"
)

// C1047: once a USDC round's day_max is taken, the claimable lists (kind
// rewarded and earn, MCP find_work's default list too) leave it out but say
// in data.usdc_next when its next seat opens, so the front door never reads
// as "no USDC work".
func TestWorksListUSDCNext(t *testing.T) {
	s, _ := usdcStore(t)
	owner := keyFor(170)
	mintCredit(t, s, keyID(owner), allowance.Paid, 5000)
	id := rewardRequest(t, s, owner, "lobby")
	tiers := []any{map[string]any{"name": "bug", "credits": 100, "usdc": "0.10"}, map[string]any{"name": "big", "credits": 200, "usdc": "0.30"}}
	run(t, s, standingCreate(s, owner, id, map[string]any{"seats": 3, "day_max": 1, "results_max": WorkStandingResultsMax, "tiers": tiers}, 10*86400))
	credits := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, credits, map[string]any{"seats": 3, "day_max": 1, "results_max": WorkStandingResultsMax, "reward": 50}, 10*86400))
	for _, kind := range []string{WorkKindRewarded, WorkKindEarn, ""} {
		if _, ok := run(t, s, Command{Operation: "works.list", Kind: kind}).Data["usdc_next"]; ok {
			t.Fatalf("kind %q: usdc_next while the USDC seat is open", kind)
		}
	}
	_, c := standingClaim(t, s, keyFor(171), id, map[string]any{"payout_address": usdcPayee})
	run(t, s, c)
	_, c = standingClaim(t, s, keyFor(172), credits, nil)
	run(t, s, c)
	midnight := testTime - testTime%86400 + 86400
	want := []WorkUSDCNext{{ID: id, Title: getTestWork(t, s, id).Title, USDCPerResult: "0.3", NextOpenAt: midnight}}
	for _, kind := range []string{WorkKindRewarded, WorkKindEarn, ""} {
		data := run(t, s, Command{Operation: "works.list", Kind: kind}).Data
		if got, _ := data["usdc_next"].([]WorkUSDCNext); !reflect.DeepEqual(got, want) {
			t.Fatalf("kind %q: usdc_next %+v, want %+v", kind, data["usdc_next"], want)
		}
	}
	// A history view never carries it.
	if _, ok := run(t, s, Command{Operation: "works.list", Data: `{"schema":1,"standing":"` + id + `"}`}).Data["usdc_next"]; ok {
		t.Fatal("usdc_next on a standing history view")
	}
}
