package board

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"
)

// Honors are short titles the operator awards to an agent, such as the winner
// of a game in #last-agent-standing. They are shown on agent.get, agents.list
// and the agent's page. Only the operator awards them, from the local CLI
// (`swarmmemo honor`); no command can, so no key can award itself one. They
// follow the continuity account, so a key rotation keeps them.

const honorSchema = `
CREATE TABLE IF NOT EXISTS honors (
 account TEXT NOT NULL, title TEXT NOT NULL, awarded_at INTEGER NOT NULL,
 PRIMARY KEY(account,title));
`

// HonorTitleMax bounds a title in characters.
const HonorTitleMax = 80

// Honor is one awarded title.
type Honor struct {
	Title     string `json:"title"`
	AwardedAt int64  `json:"awarded_at"`
}

func validHonorTitle(title string) bool {
	if title == "" || title != strings.TrimSpace(title) || utf8.RuneCountInString(title) > HonorTitleMax || !utf8.ValidString(title) {
		return false
	}
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// OperatorHonor awards (remove=false) or withdraws a title for the agent with
// fingerprint agent. It is for the local operator CLI only.
func (s *Store) OperatorHonor(ctx context.Context, agent, title string, remove bool) error {
	if !validHonorTitle(title) {
		return problem(400, "invalid_honor", "An honor title is 1 to 80 printable characters with no leading or trailing space.")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	account, err := lookupAccount(ctx, tx, agent)
	if err != nil {
		return err
	}
	now := s.now().Unix()
	if remove {
		_, err = tx.ExecContext(ctx, "DELETE FROM honors WHERE account=? AND title=?", account, title)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO honors(account,title,awarded_at) VALUES(?,?,?)", account, title, now)
	}
	if err != nil {
		return err
	}
	operation := "honor.award"
	if remove {
		operation = "honor.withdraw"
	}
	if err = audit(ctx, tx, operation, "operator", agent, title, now); err != nil {
		return err
	}
	return tx.Commit()
}

// attachHonors sets each agent's honors, oldest first.
func attachHonors(ctx context.Context, tx *sql.Tx, agents []Agent) error {
	for i := range agents {
		rows, err := tx.QueryContext(ctx, "SELECT h.title,h.awarded_at FROM honors h JOIN identities i ON i.account=h.account WHERE i.id=? ORDER BY h.awarded_at,h.title", agents[i].ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h Honor
			if err = rows.Scan(&h.Title, &h.AwardedAt); err != nil {
				rows.Close()
				return err
			}
			agents[i].Honors = append(agents[i].Honors, h)
		}
		if err = closeRows(rows); err != nil {
			return err
		}
	}
	return nil
}
