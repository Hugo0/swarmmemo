package board

import (
	"crypto/ed25519"
	"testing"

	"swarmmemo/internal/allowance"
)

// C627: works.list data summary adds each row's eligibility_summary, from
// the read's own claim test: a standing rule's floor for any reader, and for
// a signed reader that cannot claim the code of its eligible_reason. Without
// summary no row carries it (shipped bridges check row keys).
func TestWorksListEligibilitySummary(t *testing.T) {
	s := rewardStore(t)
	owner, low, worker := keyFor(230), keyFor(231), keyFor(232)
	registerAll(t, s, owner, low, worker)
	mintCredit(t, s, keyID(owner), allowance.Paid, 30000)
	setStanding(t, s, map[string]int64{keyID(low): 199})
	proven := eligibleCreate(t, s, owner, "standing_at_least:proven")
	open := eligibleCreate(t, s, owner, "")
	first := eligibleCreate(t, s, owner, "first_work")
	seat := rewardRequest(t, s, owner, "lobby")
	run(t, s, standingCreate(s, owner, seat, map[string]any{"seats": 2, "reward": 100}, 10*86400))
	_, c := standingClaim(t, s, worker, seat, nil)
	run(t, s, c)

	list := func(c Command) map[string]Work {
		t.Helper()
		rows := map[string]Work{}
		for _, w := range run(t, s, c).Data["works"].([]Work) {
			rows[w.ID] = w
		}
		return rows
	}
	signedList := func(key ed25519.PrivateKey, data string) map[string]Work {
		return list(signed(key, Command{Operation: "works.list", Data: data, Timestamp: s.now().Unix()}))
	}
	summary := `{"schema":1,"summary":true}`
	check := func(who string, rows map[string]Work, want map[string]WorkEligibilitySummary) {
		t.Helper()
		for id, w := range want {
			if _, ok := rows[id]; !ok {
				t.Errorf("%s: %s not listed", who, id)
				continue
			}
			got := rows[id].EligibilitySummary
			if w == (WorkEligibilitySummary{}) {
				if got != nil {
					t.Errorf("%s: %s summary %+v, want none", who, id, *got)
				}
				continue
			}
			if got == nil || *got != w {
				t.Errorf("%s: %s summary %+v, want %+v", who, id, got, w)
				continue
			}
			if got.BlockedBy != "" && (rows[id].Eligible == nil || *rows[id].Eligible) {
				t.Errorf("%s: %s blocked_by %q beside eligible %v", who, id, got.BlockedBy, rows[id].Eligible)
			}
		}
	}

	for _, rows := range []map[string]Work{list(Command{Operation: "works.list"}), signedList(low, "")} {
		for id, w := range rows {
			if w.EligibilitySummary != nil {
				t.Fatalf("%s carries eligibility_summary without summary: %+v", id, *w.EligibilitySummary)
			}
		}
	}
	check("anonymous", list(Command{Operation: "works.list", Data: summary}), map[string]WorkEligibilitySummary{
		proven: {MinStanding: "$2.00"}, open: {}, first: {}, seat: {},
	})
	check("low", signedList(low, summary), map[string]WorkEligibilitySummary{
		proven: {MinStanding: "$2.00", BlockedBy: WorkBlockedEligibility}, open: {}, first: {}, seat: {},
	})
	check("owner", signedList(owner, summary), map[string]WorkEligibilitySummary{
		proven: {MinStanding: "$2.00", BlockedBy: WorkBlockedRequester}, open: {BlockedBy: WorkBlockedRequester},
	})
	// The worker's result waits on the standing task, and that submit spent its first_work.
	check("worker", signedList(worker, summary), map[string]WorkEligibilitySummary{
		open: {}, first: {BlockedBy: WorkBlockedEligibility}, seat: {BlockedBy: StandingBlockedOwnResult},
	})
	// A preview (eligible_for) answers for the agent it names.
	check("preview", list(Command{Operation: "works.list", Data: `{"schema":1,"summary":true,"eligible_for":"` + keyID(low) + `"}`}), map[string]WorkEligibilitySummary{
		proven: {MinStanding: "$2.00", BlockedBy: WorkBlockedEligibility},
	})
}
