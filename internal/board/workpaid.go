package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// OpenPaidWork is one public open paid task as a guide page lists it: a
// reward in credits held in escrow (Reward, 0 when none), a reward_note the
// poster pays (RewardNote), or both.
type OpenPaidWork struct {
	ID         string
	Title      string
	Reward     int64
	RewardNote string
	CreatedAt  int64
}

// OpenPaidWorkMax caps how many tasks PublicOpenPaidWorkNaming returns;
// OpenPaidWorkScan, how many of the newest open public tasks naming a term
// it reads to find them.
const (
	OpenPaidWorkMax  = 10
	OpenPaidWorkScan = 64
)

// PublicOpenPaidWorkNaming is the public open paid work that names one of
// terms, newest first, at most limit (1 to OpenPaidWorkMax). A task names a
// term when its title holds the term as a whole word or phrase (case
// folded; a letter or digit on either side breaks the match), or when one of
// its capabilities is the term. Paid is a reward held in escrow or a
// reward_note. Hidden, private-room and simulated work is left out, as
// works.list leaves it out. One bounded statement over works narrows by
// substring and capability; Go checks the word boundaries, then reads the
// reward_note of the newest candidates until limit are paid.
func (s *Store) PublicOpenPaidWorkNaming(ctx context.Context, terms []string, limit int) ([]OpenPaidWork, error) {
	limit = min(max(limit, 1), OpenPaidWorkMax)
	clean := make([]string, 0, len(terms))
	for _, t := range terms {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" && len(t) <= 80 && utf8.ValidString(t) && !strings.ContainsRune(t, 0) && !slices.Contains(clean, t) {
			clean = append(clean, t)
		}
	}
	out := []OpenPaidWork{}
	if len(clean) == 0 {
		return out, nil
	}
	// capabilities is a JSON array of slugs, so a quoted term inside it is
	// that capability (namesTerm checks it exactly).
	match := make([]string, 0, 2*len(clean))
	args := []any{}
	for _, t := range clean {
		quoted, _ := json.Marshal(t)
		match = append(match, `instr(lower(w.title),?)>0`, `instr(w.capabilities,?)>0`)
		args = append(args, t, string(quoted))
	}
	now := s.now().Unix()
	err := s.publicRead(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var generation string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
			return err
		}
		// works drives the scan (CROSS JOIN keeps it the outer loop): the
		// stored state and deadline and the title or capability match narrow
		// it before any event or room is read. The reward_note sits in the
		// signed create payload, costly to read on every row, so it is read
		// below only for the newest candidates, until limit are paid.
		query := `SELECT w.id,w.title,w.capabilities,w.created_at,coalesce((SELECT wr.amount FROM work_rewards wr WHERE wr.work_id=w.id AND wr.state='held'),0)
 FROM works w CROSS JOIN events e ON e.id=w.id CROSS JOIN rooms r ON r.name=e.room
 WHERE w.state IN ('open','claimed') AND w.deadline>? AND (` + strings.Join(match, " OR ") + `)
 AND e.hidden=0 AND r.visibility='public' AND e.kind<>'simulation' AND (` + workEffectiveSQL + `)='open'
 ORDER BY w.created_at DESC, w.id DESC LIMIT ?`
		rows, err := tx.QueryContext(ctx, query, append(append(append([]any{now}, args...), now, generation, now), OpenPaidWorkScan)...)
		if err != nil {
			return err
		}
		candidates := []OpenPaidWork{}
		for rows.Next() {
			var w OpenPaidWork
			var capsJSON string
			if err := rows.Scan(&w.ID, &w.Title, &capsJSON, &w.CreatedAt, &w.Reward); err != nil {
				rows.Close()
				return err
			}
			var have []string
			_ = json.Unmarshal([]byte(capsJSON), &have)
			if namesTerm(w.Title, have, clean) {
				candidates = append(candidates, w)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, w := range candidates {
			if len(out) == limit {
				break
			}
			if err := tx.QueryRowContext(ctx, `SELECT `+workRewardNoteSQL+` FROM works w WHERE w.id=?`, w.ID).Scan(&w.RewardNote); err != nil {
				return err
			}
			if w.Reward > 0 || w.RewardNote != "" {
				out = append(out, w)
			}
		}
		return nil
	})
	return out, err
}

// namesTerm says whether title holds one of terms (lowercase) as a whole
// word or phrase, or caps holds one of them exactly.
func namesTerm(title string, caps, terms []string) bool {
	lower := strings.ToLower(title)
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	for _, t := range terms {
		if slices.Contains(caps, t) {
			return true
		}
		for from := 0; from < len(lower); {
			i := strings.Index(lower[from:], t)
			if i < 0 {
				break
			}
			i += from
			before, _ := utf8.DecodeLastRuneInString(lower[:i])
			after, _ := utf8.DecodeRuneInString(lower[i+len(t):])
			if (i == 0 || !word(before)) && (i+len(t) == len(lower) || !word(after)) {
				return true
			}
			from = i + 1
		}
	}
	return false
}
