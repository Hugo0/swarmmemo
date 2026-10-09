package board

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Schema 22 (webhook_subscriptions.kinds, inbox_push.go): a populated
// schema-21 database (testdata/schema21.sql) migrates to exactly the schema a
// new database gets, keeps every row (a subscription keeps the default set),
// and its second start runs no DDL.
func TestSchema22UpgradeFrom21(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("testdata", "schema21.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema21.db")
	db := rawDB(t, path)
	_, body, _ := strings.Cut(string(ddl), "do not edit.\n")
	blocks := strings.Split(strings.TrimPrefix(body, "-- "), "\n-- ")
	for _, tables := range []bool{true, false} {
		for _, block := range blocks {
			head, statement, _ := strings.Cut(block, "\n")
			if strings.HasPrefix(head, "table ") != tables {
				continue
			}
			if _, err = db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", head, err)
			}
		}
	}
	if _, err = db.Exec("PRAGMA user_version=21"); err != nil {
		t.Fatal(err)
	}
	seedEveryTable(t, db)
	before := tableCounts(t, db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	openClose(t, path, Config{})
	db = rawDB(t, path)
	if v := pragmaInt(t, db, "user_version"); v != SchemaVersion || SchemaVersion < 22 {
		t.Fatalf("user_version %d, SchemaVersion %d", v, SchemaVersion)
	}
	if got, fresh := normalizedSchema(t, db), freshSchema(t, Config{}); got != fresh {
		t.Fatalf("upgraded schema differs from a new database's:\n%s", schemaDiff(got, fresh))
	}
	after := tableCounts(t, db)
	for table, n := range before {
		if after[table] < n {
			t.Errorf("%s: %d rows before, %d after", table, n, after[table])
		}
	}
	var stray int
	if err = db.QueryRow("SELECT count(*) FROM webhook_subscriptions WHERE kinds<>''").Scan(&stray); err != nil || stray != 0 {
		t.Fatalf("upgraded subscriptions keep the default set: %d with kinds (%v)", stray, err)
	}
	cookie := pragmaInt(t, db, "schema_version")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	openClose(t, path, Config{})
	if got := pragmaInt(t, rawDB(t, path), "schema_version"); got != cookie {
		t.Fatalf("schema cookie %d -> %d: the second start changed the schema", cookie, got)
	}
}
