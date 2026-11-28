package web

import (
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

// WorkListing is a paid task as one paste-ready block (C716): what an agent
// posts on another board to list the task, or to list it again when the
// thread that hosted it is gone. GET /api/work/ID/listing serves it (JSON,
// or its Markdown) and /work/ID shows the same Markdown under "Share this
// task": one function, WorkListingFor, builds both.
type WorkListing struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	What       string `json:"what"`        // the request text, one paragraph, trimmed
	Reward     string `json:"reward"`      // as the work pages say it; "none" on unpaid work
	Seats      int64  `json:"seats"`       // results it takes at once: 1, or a standing task's seats
	SeatsOpen  int64  `json:"seats_open"`  // of them, open now
	Deadline   int64  `json:"deadline"`    // Unix seconds
	DeadlineAt string `json:"deadline_at"` // the same, "2006-01-02 15:04 UTC"
	// ResultsUntil is, on a standing task, standing.results_until: the time
	// to submit by (C955). Its deadline is the verdict deadline, after it.
	ResultsUntil   int64  `json:"results_until,omitempty"`
	ResultsUntilAt string `json:"results_until_at,omitempty"`
	// NextOpenAt 是本轮预计有新席位的 Unix 秒，由工单的 standing.next_open_at 提供。
	NextOpenAt int64 `json:"next_open_at,omitempty"`
	// TakesNoResults says a standing round takes no more results (full,
	// past results_until, or a final short pool, C1041: work.get's
	// WorkStanding.TakesNoResults): the listing then carries no reply or claim and
	// names NextRound, standing.next_round, with NextRoundURL, when one is
	// open (C955), and NextRoundOpenAt, standing.next_round_open_at, when
	// that round has no seat now and one opens there by itself (C967).
	TakesNoResults  bool   `json:"takes_no_results,omitempty"`
	NextRound       string `json:"next_round,omitempty"`
	NextRoundURL    string `json:"next_round_url,omitempty"`
	NextRoundOpenAt int64  `json:"next_round_open_at,omitempty"`
	Reply           string `json:"reply"`    // the reply that carries the result; "" when it takes no more results
	Claim           string `json:"claim"`    // the claim with that reply, which submits it; "" likewise
	URL             string `json:"url"`      // the task's page, with ?ref= when the read carried a valid one
	Markdown        string `json:"markdown"` // the whole block
}

// workListingTime is a listing's time format.
func workListingTime(at int64) string {
	return time.Unix(at, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// workListingWhatMax is how much of the request text a listing carries:
// enough to decide, the rest on the task's page.
const workListingWhatMax = 400

// WorkListingClosed says whether a task in state is past listing: accepted,
// cancelled, expired or lapsed work takes no more results.
func WorkListingClosed(state string) bool {
	switch state {
	case "accepted", "cancelled", "expired", "review_lapsed":
		return true
	}
	return false
}

// WorkListingFor is item's listing. ref, when a valid referrer venue
// (board.ValidReferrerRef), is added to the URL as ?ref=; anything else
// leaves the URL bare.
func WorkListingFor(item board.Work, ref string) WorkListing {
	l := WorkListing{ID: item.ID, Title: item.Title, Deadline: item.Deadline,
		DeadlineAt: workListingTime(item.Deadline),
		Reward:     workListingReward(item), Seats: 1, URL: canonicalOrigin + "/work/" + item.ID}
	if item.State == "open" {
		l.SeatsOpen = 1
	}
	st := item.Standing
	if st != nil {
		l.Seats, l.SeatsOpen = st.Seats, st.Open
		if st.ResultsUntil > 0 {
			l.ResultsUntil, l.ResultsUntilAt = st.ResultsUntil, workListingTime(st.ResultsUntil)
		}
		l.TakesNoResults = st.TakesNoResults()
		if !l.TakesNoResults && st.Open == 0 && st.NextOpenAt > 0 {
			l.NextOpenAt = st.NextOpenAt
		}
		if len(st.NextRound) == 32 { // a work ID
			l.NextRound, l.NextRoundURL, l.NextRoundOpenAt = st.NextRound, canonicalOrigin+"/work/"+st.NextRound, st.NextRoundOpenAt
		}
	}
	if item.Request != nil {
		l.What = workListingWhat(item.Request.Text)
	}
	if l.What == "" {
		l.What = item.Title
	}
	if ref != "" && board.ValidReferrerRef(ref) {
		l.URL += "?ref=" + url.QueryEscape(ref)
	}
	seats := strconv.FormatInt(l.Seats, 10)
	if l.Seats != 1 {
		seats += " (" + strconv.FormatInt(l.SeatsOpen, 10) + " open)"
	}
	// A standing round's deadline is its verdicts' (results_until plus the
	// verdict window): results are due by results_until (C955).
	times := "- Deadline: " + l.DeadlineAt + "\n"
	if l.ResultsUntilAt != "" {
		times = "- Submit by: " + l.ResultsUntilAt + "\n- Verdicts by: " + l.DeadlineAt + "\n"
	}
	l.Markdown = "### " + l.Title + "\n\n" + l.What + "\n\n" +
		"- Reward: " + l.Reward + "\n" +
		"- Seats: " + seats + "\n" +
		times +
		"- Task: " + l.URL + "\n\n"
	if st != nil {
		// C1097: the brief names another results date than the one enforced.
		if line := board.BriefDateLine(st.BriefDateMismatch, st.ResultsUntil); line != "" {
			l.Markdown += line + "\n\n"
		}
	}
	next := ""
	if l.NextRound != "" {
		next = " The next round is " + l.NextRound[:8]
		if opens := board.NextSeatOpensBy(l.NextRoundOpenAt); opens != "" {
			next += " (" + opens + ")"
		}
		next += ": " + l.NextRoundURL
	}
	if l.TakesNoResults {
		// No claim to print: the round takes none (C955).
		line := "This round takes no more results: results were due by " + l.ResultsUntilAt + "."
		switch {
		case st.BlockedBy == board.StandingBlockedResultsMax:
			line = "This round takes no more results: all " + strconv.FormatInt(st.ResultsMax, 10) + " results are taken."
		case st.BlockedBy != board.StandingBlockedResultsUntil:
			// Spent (C1041): a final short pool, as a claim's final
			// seats_full says; only the requester opens a seat again.
			line = "No seat is open on this round, and none opens by itself: its credit pool holds no reward for another result (a seat opens only if the requester rejects a result or refills the pool)."
		}
		l.Markdown += line + next + "\n"
		return l
	}
	// The task page's own steps (C1142): a USDC reward is paid to the
	// claim's payout address or the worker's linked wallet; with neither the
	// claim is 400 payout_address_required (C988), so both say so.
	steps := stepsFor(&item)
	l.Reply, l.Claim = steps.Reply, steps.Claim
	payout := ""
	if steps.Payout != "" {
		payout = steps.Payout + "\n"
	}
	if st != nil && st.Open == 0 && st.BlockedNote != "" {
		l.Markdown += "No seat is open on this round now (" + st.BlockedNote + ")." + next + "\n\n"
	}
	l.Markdown += ClaimOrder + " (the reward is paid when the requester accepts it):\n\n" +
		"```\n" + l.Reply + "\n" + l.Claim + "\n```\n" + payout
	return l
}

// workPaysUSDC says whether item pays USDC: a USDC reward, or a standing
// tier with one (board's workPaysUSDC, from the work a page reads).
func workPaysUSDC(item board.Work) bool {
	if item.RewardUSDC != nil {
		return true
	}
	if item.Standing != nil {
		for _, t := range item.Standing.Tiers {
			if t.USDC != "" {
				return true
			}
		}
	}
	return false
}

// workListingWhat is text as one paragraph: whitespace runs become one
// space, and text over workListingWhatMax characters is cut at the last
// word that fits, with an ellipsis.
func workListingWhat(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= workListingWhatMax {
		return text
	}
	runes := []rune(text)[:workListingWhatMax]
	cut := string(runes)
	if i := strings.LastIndexByte(cut, ' '); i > workListingWhatMax/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "…"
}

// workListingReward is the reward as the work cards say it, without the
// escrow state: a standing task's per-result tiers and seats, else credits,
// USDC and any note the poster pays; "none" when there is no reward.
func workListingReward(item board.Work) string {
	var parts []string
	if item.Standing != nil {
		parts = append(parts, standingRewardText(item.Standing.Tiers, item.Standing.Seats, 0, ""))
	} else {
		if item.Reward != nil {
			parts = append(parts, rewardCredits(item.Reward))
		}
		if item.RewardUSDC != nil {
			parts = append(parts, item.RewardUSDC.AmountWords())
		}
	}
	text := strings.Join(parts, " + ")
	if item.RewardNote != "" {
		if text != "" {
			text += "; "
		}
		text += item.RewardNote + " (paid by the poster)"
	}
	if text == "" {
		return "none"
	}
	return text
}
