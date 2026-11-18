package board

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// A standing task's brief can name a date for its results that is not the
// one the board enforces (C1097): Skitter's "Round 11: route QA" said
// "Results are accepted until 2026-11-22 00:45Z" while its
// standing.results_until was 2026-11-19 11:00 UTC, and a worker asked which
// one governs. standing.results_until does. BriefDateMismatch finds such a
// date; work.get shows it beside results_until (standing.brief_date_mismatch,
// BriefDateLine), and the requester hears it on the create or edit receipt
// (BriefDateAdvice).

// briefDateRE matches a date phrased as when results or claims are taken:
// a results/accepted/taken/closes/until phrase right before an ISO date
// (2026-11-22), with an optional time (00:45, 00:45:00, T00:45) and Z, UTC
// or GMT; a time with an offset (+02:00) counts at its UTC time, and one with
// another zone's name (13:00 CET) is left out. Only those phrases, so a date that says something else (a release,
// an event) is never read as the brief's results date.
var briefDateRE = regexp.MustCompile(`(?i)\b(?:` +
	`results?(?:\s+(?:are|is|will\s+be))?\s+(?:accepted|taken|due|open)\s+(?:until|till|through|by)` +
	`|results?\s+(?:until|till|by)` +
	`|(?:accepted|taken)\s+(?:until|till|through)` +
	`|submissions?\s+(?:close|closes|are\s+accepted\s+until)` +
	`|claims?\s+(?:close|closes|until|are\s+accepted\s+until|are\s+taken\s+until)` +
	`|(?:round\s+)?(?:closes|closing)(?:\s+(?:at|on))?` +
	`)\s*:?\s*(\d{4}-\d{2}-\d{2})(?:(?:T|\s+)(\d{2}:\d{2})(:\d{2})?(?:\s*(?:Z|UTC|GMT)\b|\s*([+-]\d{2}:?\d{2})\b|\s+((?-i:[A-Z]{2,5}))\b)?)?`)

// briefDateSlack is how far a brief's date may sit from results_until and
// still count as the same date: a rounded time in prose is not a mismatch.
const briefDateSlack = 3600

// BriefDateMismatch is the latest results date text names (briefDateRE), as
// "2006-01-02 15:04 UTC" or, when it names a day alone, "2006-01-02", when it
// differs from resultsUntil by more than an hour; else "". A day alone
// matches any results_until within that UTC day (an hour either side).
func BriefDateMismatch(text string, resultsUntil int64) string {
	if resultsUntil <= 0 {
		return ""
	}
	var says string
	var from, to int64 // the latest named date, as a span: a day alone spans the day
	for _, m := range briefDateRE.FindAllStringSubmatch(text, -1) {
		day, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			continue
		}
		start, end, label := day.Unix(), day.Unix()+86400, m[1]
		if m[2] != "" {
			if m[5] != "" {
				continue // a zone the board does not read: never guess
			}
			at, err := time.Parse("2006-01-02 15:04:05", m[1]+" "+m[2]+cmp.Or(m[3], ":00"))
			if err != nil {
				continue
			}
			if off := strings.ReplaceAll(m[4], ":", ""); off != "" {
				zoned, err := time.Parse("2006-01-02 15:04:05 -0700", at.Format("2006-01-02 15:04:05")+" "+off)
				if err != nil {
					continue
				}
				at = zoned.UTC()
			}
			start, end, label = at.Unix(), at.Unix(), at.Format("2006-01-02 15:04")+" UTC"
		}
		if says == "" || end > to {
			says, from, to = label, start, end
		}
	}
	if says == "" || resultsUntil >= from-briefDateSlack && resultsUntil <= to+briefDateSlack {
		return ""
	}
	return says
}

// briefDateTime is results_until as the brief-date lines say it.
func briefDateTime(at int64) string {
	return time.Unix(at, 0).UTC().Format("2006-01-02 15:04") + " UTC"
}

// BriefDateLine is the worker's line beside results_until when the brief
// names another date (standing.brief_date_mismatch), else "".
func BriefDateLine(says string, resultsUntil int64) string {
	if says == "" {
		return ""
	}
	return "The brief says " + says + "; results are taken until " + briefDateTime(resultsUntil) + " (the board enforces this date)."
}

// BriefDateAdvice is the requester's note on a standing task's create or
// edit receipt when its brief names another date, else "".
func BriefDateAdvice(says string, resultsUntil int64) string {
	if says == "" {
		return ""
	}
	return "The brief says " + says + ", but results are taken until " + briefDateTime(resultsUntil) + " (standing.results_until, the date the board enforces). Edit the brief so workers read one date."
}

// standingBriefAdvice is BriefDateAdvice for text as the brief of work id,
// when id is a standing task (not one of its results); "" otherwise.
func standingBriefAdvice(ctx context.Context, tx rowQuerier, id, text string) (string, error) {
	var deadline, ttl int64
	err := tx.QueryRowContext(ctx, `SELECT w.deadline,s.verdict_ttl FROM works w JOIN work_standing s ON s.work_id=w.id WHERE w.id=? AND w.standing_id=''`, id).Scan(&deadline, &ttl)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	until := deadline - ttl
	return BriefDateAdvice(BriefDateMismatch(text, until), until), nil
}
