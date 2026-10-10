package board

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"swarmmemo/internal/testrace"
)

// Under the race detector the pure-Go SQLite runs some thirty times slower,
// and building the schema on a fresh database (about 8s on a CI runner)
// dominates every test that opens a store. So a race-built test binary
// migrates one fresh database per process and starts each new, empty
// database file from a copy of it: the bytes migrateSchema writes, after
// which Open takes the path every restart of a current database takes, and
// does everything else (upkeep, generation, cursor key, hosted and
// transparency state) per store as usual. Production binaries and tests
// without -race never take this path.
var schemaTemplate struct {
	once sync.Once
	data []byte
	err  error
}

// seedFromSchemaTemplate fills path, a database file Open has just created
// empty, from the migrated template, in a race-built test binary only.
func seedFromSchemaTemplate(path string) error {
	if !testrace.Enabled || !testing.Testing() || path == ":memory:" {
		return nil
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		return nil
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	schemaTemplate.once.Do(func() { schemaTemplate.data, schemaTemplate.err = buildSchemaTemplate() })
	if schemaTemplate.err != nil {
		return schemaTemplate.err
	}
	return os.WriteFile(path, schemaTemplate.data, 0600)
}

// buildSchemaTemplate migrates a fresh database exactly as Open does and
// returns its file, checkpointed.
func buildSchemaTemplate() (data []byte, err error) {
	dir, err := os.MkdirTemp("", "swarmmemo-schema-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.sqlite")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: connPragmas}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if cerr := db.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err == nil {
			data, err = os.ReadFile(path)
		}
	}()
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = migrateSchema(tx, 0); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return nil, err
}
