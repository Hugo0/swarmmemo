package board

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Schema 27 (work_usdc, workusdc.go, RFC 0016, C156): a populated schema-24,
// 25 or 26 database (testdata/schemaN.sql) migrates to exactly the schema a
// new database gets, keeps every row, has the three work_usdc indexes, and
// its second start runs no DDL.
func TestSchema27Upgrade(t *testing.T) {
	for _, from := range []int{24, 25, 26} {
		t.Run(fmt.Sprint(from), func(t *testing.T) { schema27UpgradeFrom(t, from) })
	}
}

func schema27UpgradeFrom(t *testing.T, from int) {
	ddl, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("schema%d.sql", from)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("schema%d.db", from))
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
	if _, err = db.Exec(fmt.Sprintf("PRAGMA user_version=%d", from)); err != nil {
		t.Fatal(err)
	}
	seedEveryTable(t, db)
	before := tableCounts(t, db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	openClose(t, path, Config{})
	db = rawDB(t, path)
	if v := pragmaInt(t, db, "user_version"); v != SchemaVersion || SchemaVersion < 27 {
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
	for _, index := range []string{"work_usdc_tx", "work_usdc_open", "work_usdc_requester"} {
		var n int
		if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&n); err != nil || n != 1 {
			t.Fatalf("index %s after the upgrade: %d (%v)", index, n, err)
		}
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
