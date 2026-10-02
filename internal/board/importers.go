package board

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"swarmmemo/internal/services"
)

// ImportersParamsNamespace uses the existing audited operator parameter store.
// Entries are continuity account fingerprints, so key rotation retains access.
const ImportersParamsNamespace = "importers"

type importerParams struct {
	Schema   int      `json:"schema"`
	Accounts []string `json:"accounts"`
}

func defaultImporterParams() []byte { return []byte(`{"schema":1,"accounts":[]}`) }

func parseImporterParams(body []byte) (importerParams, error) {
	var p importerParams
	err := services.StrictObject(body, &p)
	if err != nil || p.Schema != 1 || p.Accounts == nil || len(p.Accounts) > 1024 {
		return importerParams{}, errors.New("importers requires schema 1 and accounts: an array of at most 1024 account fingerprints")
	}
	seen := map[string]bool{}
	for _, account := range p.Accounts {
		if !fingerprintRE.MatchString(account) || seen[account] {
			return importerParams{}, errors.New("importer accounts must be unique 64-character lowercase hex fingerprints")
		}
		seen[account] = true
	}
	return p, nil
}

func (s *Store) allowedImporter(ctx context.Context, tx *sql.Tx, a actor, now int64) bool {
	if !a.signed || a.grant != nil || !fingerprintRE.MatchString(a.account) {
		return false
	}
	v, err := s.ledger.params.Get(ctx, tx, ImportersParamsNamespace, -1, now)
	if err != nil {
		return false
	}
	p, err := parseImporterParams(v.Body)
	return err == nil && slices.Contains(p.Accounts, a.account)
}
