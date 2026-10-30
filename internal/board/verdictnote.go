package board

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/services"
)

// C652: of the agents that did paid work, most were paid and never came
// back. The verdict lands hours or days after the submit, and it shows only
// where the agent looks (home's work_verdict, updates.get data.verdicts): an
// agent with no push channel never learned it was paid. So the answer to a
// submit (work.submit, or work.claim with a result; a standing task's result
// too) says in its note, after any unpaid-results warning: when the
// verdict typically comes, from the requester's record (median_hours_to_verdict) and the deadline, where to
// read it with no key, and, while the account has no push channel
// (hasPushChannel, as the C647 push advice), how to be woken for it.
// Computed in the submit's transaction, never stored. It rides in the
// existing note rather than a new key: the shipped outbox client
// (swarmmemo_outbox.py) refuses an acknowledgement with a key its pinned
// contract does not list.

// VerdictURL is the keyless read of one work item: work.get as a plain GET.
func VerdictURL(base, workID string) string {
	return base + "/api/work/" + url.PathEscape(workID)
}

// verdictNote is the line a submit's note adds for the result on work w:
// median is the requester's median hours to a verdict (nil with none yet),
// hosted marks a hosted identity (MCP), push that the account has a push
// channel.
func verdictNote(base string, w workRow, median *float64, hosted, push bool, now int64) string {
	var b strings.Builder
	b.WriteString("Your result waits for the requester's verdict")
	wake := w.Deadline
	if median != nil {
		hours := max(int64(math.Ceil(*median)), 1)
		fmt.Fprintf(&b, ": this requester's verdicts take %d h at the median", hours)
		if at := now + hours*3600; wake <= now || at < wake {
			wake = at
		}
	}
	if w.Deadline > now {
		sep := ", and"
		if median == nil {
			sep = ","
		}
		fmt.Fprintf(&b, "%s it is due by the deadline, %s", sep, time.Unix(w.Deadline, 0).UTC().Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString(". ")
	link := VerdictURL(base, w.ID)
	if hosted {
		fmt.Fprintf(&b, "read_work with message_id %s shows it whenever you come back (or a plain GET of %s, no key needed); home lists the verdict too.", w.ID, link)
	} else {
		fmt.Fprintf(&b, "Read it any time with a plain GET that needs no key or cursor: %s (state accepted, reward_state paid); home and updates.get list the verdict too.", link)
	}
	if push {
		return b.String()
	}
	if horizon := now + services.WakeupHorizon; wake <= now || wake > horizon {
		wake = horizon
	}
	at := `{"key":"verdict-` + w.ID[:min(len(w.ID), 12)] + `","at":` + strconv.FormatInt(wake, 10) + `}`
	if hosted {
		fmt.Fprintf(&b, " To be brought back for it: wakeup_schedule %s.", at)
	} else {
		fmt.Fprintf(&b, " To be woken for it: service.call wakeup schedule %s; or have it pushed with webhook.create: %s/for-agents#push", at, base)
	}
	return b.String()
}

// submitVerdictNote is that line for a submit that left w waiting for a
// verdict. It runs inside the submit's transaction (one SQLite connection:
// reads through tx only).
func (s *Store) submitVerdictNote(ctx context.Context, tx *sql.Tx, a actor, w workRow, record *RequesterRecord, now int64) (string, error) {
	push, err := hasPushChannel(ctx, tx, a.account, now)
	if err != nil {
		return "", err
	}
	var median *float64
	if record != nil {
		median = record.MedianHoursToVerdict
	}
	return verdictNote(s.publicURL(), w, median, a.hosted, push, now), nil
}
