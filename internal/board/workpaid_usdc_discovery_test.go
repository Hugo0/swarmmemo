package board

import (
	"swarmmemo/internal/allowance"
	"testing"
)

// Offline regression: public USDC-only tasks must be discoverable when the
// caller searches their title or capabilities. No network or keys.
func TestUSDCOnlyOpenTaskVisibleToPublicPaidWorkSearch(t *testing.T) {
	s, _ := usdcStore(t)
	requester := keyFor(237)
	id := rewardRequest(t, s, requester, "lobby")
	run(t, s, rewardCreate(s, requester, id, map[string]any{"usdc": "0.05"}, 0))
	w := getTestWork(t, s, id)
	if w.Reward != nil || w.RewardUSDC == nil || w.RewardUSDC.State != "promised" || w.RewardUSDC.Amount != "0.05" {
		t.Fatalf("fixture is not real USDC-only open work: credit=%+v usdc=%+v", w.Reward, w.RewardUSDC)
	}
	ids := openPaidIDs(t, s, []string{"rewarded"}, 10)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("BUG: work.get says real open 0.05 USDC, but public paid-task search omitted it: got %v, expected [%s]", ids, id)
	}
}

// Keep both payouts independent. A USDC promise is not an escrowed credit.
func TestDualRailOpenPaidWorkRetainsBothAmounts(t *testing.T) {
	s, _ := usdcStore(t)
	requester := keyFor(238)
	mintCredit(t, s, keyID(requester), allowance.Paid, 100)
	id := rewardRequest(t, s, requester, "lobby")
	run(t, s, rewardCreate(s, requester, id, map[string]any{"credits": 12, "usdc": "0.03"}, 0))
	works, err := s.PublicOpenPaidWorkNaming(testContext, []string{"rewarded"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 || works[0].ID != id || works[0].Reward != 12 || works[0].USDC != 30000 {
		t.Fatalf("dual rail must preserve escrow credit 12 and promised USDC 30000 atomic: %+v", works)
	}
}
