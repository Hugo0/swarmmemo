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
	Reply      string `json:"reply"`       // the reply that carries the result
	Claim      string `json:"claim"`       // the claim with that reply, which submits it
	URL        string `json:"url"`         // the task's page, with ?ref= when the read carried a valid one
	Markdown   string `json:"markdown"`    // the whole block
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
		DeadlineAt: time.Unix(item.Deadline, 0).UTC().Format("2006-01-02 15:04 UTC"),
		Reward:     workListingReward(item), Seats: 1, URL: canonicalOrigin + "/work/" + item.ID}
	if item.State == "open" {
		l.SeatsOpen = 1
	}
	if st := item.Standing; st != nil {
		l.Seats, l.SeatsOpen = st.Seats, st.Open
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
	steps := stepsFor(item.ID, item.Room)
	l.Reply, l.Claim = steps.Reply, steps.Claim
	seats := strconv.FormatInt(l.Seats, 10)
	if l.Seats != 1 {
		seats += " (" + strconv.FormatInt(l.SeatsOpen, 10) + " open)"
	}
	l.Markdown = "### " + l.Title + "\n\n" + l.What + "\n\n" +
		"- Reward: " + l.Reward + "\n" +
		"- Seats: " + seats + "\n" +
		"- Deadline: " + l.DeadlineAt + "\n" +
		"- Task: " + l.URL + "\n\n" +
		ClaimOrder + " (the reward is paid when the requester accepts it):\n\n" +
		"```\n" + l.Reply + "\n" + l.Claim + "\n```\n"
	return l
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
			parts = append(parts, creditAmount(item.Reward.Amount))
		}
		if item.RewardUSDC != nil {
			parts = append(parts, item.RewardUSDC.Amount+" USDC")
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
