package board

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// normalizedSchema is sqlite_master as text: every object but SQLite's own
// and Litestream's, ordered by type and name, with its exact SQL.
func normalizedSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite\\_%' ESCAPE '\\' AND name NOT LIKE '\\_litestream\\_%' ESCAPE '\\' ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, table, text string
		if err = rows.Scan(&typ, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "-- %s %s on %s\n%s;\n", typ, name, table, text)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// rawDB opens path without the store: no migration, foreign keys off.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func pragmaInt(t *testing.T, db *sql.DB, pragma string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// tableCounts is every table's row count.
func tableCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	rows.Close()
	counts := map[string]int{}
	for _, name := range names {
		var n int
		if err = db.QueryRow(`SELECT count(*) FROM "` + name + `"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[name] = n
	}
	return counts
}

func integrity(t *testing.T, db *sql.DB) (string, int) {
	t.Helper()
	var ok string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return ok, n
}

// openClose opens path with the store once and closes it.
func openClose(t *testing.T, path string, c Config) {
	t.Helper()
	s, err := Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
}

// freshSchema is the schema Open gives a new database.
func freshSchema(t *testing.T, c Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fresh.db")
	openClose(t, path, c)
	db := rawDB(t, path)
	if v := pragmaInt(t, db, "user_version"); v != SchemaVersion {
		t.Fatalf("fresh user_version %d, want %d", v, SchemaVersion)
	}
	return normalizedSchema(t, db)
}

var numericAffinity = regexp.MustCompile(`(?i)INT|REAL|FLOA|DOUB|NUM`)

var checkInRE = regexp.MustCompile(`(?s)CHECK\(\s*(\w+)\s+IN\s*\(\s*'([^']*)'`)

// seedEveryTable puts one row into every table of a schema-15 database
// (foreign keys off, so the rows need not refer to anything): every NOT NULL
// column without a default gets a value its CHECK accepts, the rest keep
// their defaults, so each later ALTER TABLE ... ADD COLUMN runs on a
// populated table. The transparency log's own tables stay empty.
func seedEveryTable(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query("SELECT name,sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' AND name NOT LIKE 'tlog\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string]string{}
	for rows.Next() {
		var name, text string
		if err = rows.Scan(&name, &text); err != nil {
			t.Fatal(err)
		}
		tables[name] = text
	}
	rows.Close()
	for name, text := range tables {
		checks := map[string]string{}
		for _, m := range checkInRE.FindAllStringSubmatch(text, -1) {
			checks[m[1]] = m[2]
		}
		// A lone INTEGER PRIMARY KEY is the rowid: SQLite numbers it.
		cols, err := db.Query("SELECT name,type,\"notnull\",dflt_value IS NOT NULL,pk=1 AND upper(type)='INTEGER' AND (SELECT count(*) FROM pragma_table_info(?) WHERE pk>0)=1 FROM pragma_table_info(?)", name, name)
		if err != nil {
			t.Fatal(err)
		}
		type column struct {
			name, typ string
		}
		var needed []column
		for cols.Next() {
			var col, typ string
			var notNull, hasDefault, rowid int
			if err = cols.Scan(&col, &typ, &notNull, &hasDefault, &rowid); err != nil {
				t.Fatal(err)
			}
			if hasDefault == 0 && notNull == 1 && rowid == 0 {
				needed = append(needed, column{col, typ})
			}
		}
		cols.Close()
		// Integers are 1, or 0 where a CHECK refuses 1 (a quality in [0,1) ...).
		for _, integer := range []int{1, 0} {
			var names, marks []string
			var args []any
			for _, c := range needed {
				var v any = "seed-" + name + "-" + c.name
				switch {
				case checks[c.name] != "":
					v = checks[c.name]
				case numericAffinity.MatchString(c.typ):
					v = integer
				case strings.EqualFold(c.typ, "BLOB"):
					v = []byte{0}
				}
				names, marks, args = append(names, `"`+c.name+`"`), append(marks, "?"), append(args, v)
			}
			query := `INSERT INTO "` + name + `" DEFAULT VALUES`
			if len(names) > 0 {
				query = `INSERT INTO "` + name + `"(` + strings.Join(names, ",") + `) VALUES(` + strings.Join(marks, ",") + `)`
			}
			if _, err = db.Exec(query, args...); err == nil {
				break
			} else if integer == 0 || !strings.Contains(err.Error(), "CHECK") {
				t.Fatalf("seed %s: %v", name, err)
			}
		}
	}
}

// schema15DB is a populated database at production's schema 15
// (testdata/schema15.sql: the DDL of the production database's
// sqlite_master on 2026-10-07, before 1.41 and C11; no rows).
func schema15DB(t *testing.T) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("testdata", "schema15.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "schema15.db")
	db := rawDB(t, path)
	if _, err = db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version=15"); err != nil {
		t.Fatal(err)
	}
	seedEveryTable(t, db)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Schema 16: a populated production schema-15 database migrates to exactly
// the schema a new database gets, keeps every row, changes nothing on the
// next start, and a schema-16 database runs no DDL at open.
func TestSchema16UpgradeMatchesFresh(t *testing.T) {
	for _, c := range []struct {
		name   string
		config Config
	}{{"moderation off", Config{}}, {"moderation on", Config{Features: Features{Moderation: true}}}} {
		t.Run(c.name, func(t *testing.T) {
			path := schema15DB(t)
			before := tableCounts(t, rawDB(t, path))
			_, fkBefore := integrity(t, rawDB(t, path))

			openClose(t, path, c.config)
			db := rawDB(t, path)
			if v := pragmaInt(t, db, "user_version"); v != SchemaVersion {
				t.Fatalf("user_version %d after migration, want %d", v, SchemaVersion)
			}
			if ok, fk := integrity(t, db); ok != "ok" || fk != fkBefore {
				t.Fatalf("integrity %q, foreign key violations %d (before %d)", ok, fk, fkBefore)
			}
			fresh := freshSchema(t, c.config)
			if got := normalizedSchema(t, db); got != fresh {
				t.Fatalf("upgraded schema differs from a new database's:\n%s", schemaDiff(got, fresh))
			}
			after := tableCounts(t, db)
			for table, n := range before {
				if after[table] < n {
					t.Errorf("%s: %d rows before, %d after: the migration removed rows", table, n, after[table])
				}
			}
			// The one-time fold of pastes into docs copied the seeded paste.
			if after["docs"] != before["docs"]+1 || after["doc_versions"] != before["doc_versions"]+1 {
				t.Errorf("docs %d -> %d, doc_versions %d -> %d: want the one paste folded in", before["docs"], after["docs"], before["doc_versions"], after["doc_versions"])
			}
			for table, n := range after {
				if _, old := before[table]; !old && n != 0 && !strings.HasPrefix(table, "tlog_") {
					t.Errorf("new table %s has %d rows", table, n)
				}
			}

			// The second start: no DDL (the schema cookie stays), no row changes.
			cookie := pragmaInt(t, db, "schema_version")
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			openClose(t, path, c.config)
			db = rawDB(t, path)
			if got := pragmaInt(t, db, "schema_version"); got != cookie {
				t.Fatalf("schema cookie %d -> %d: the second start changed the schema", cookie, got)
			}
			again := tableCounts(t, db)
			for table, n := range after {
				if again[table] != n {
					t.Errorf("%s: %d rows after the migration, %d after the second start", table, n, again[table])
				}
			}
		})
	}
}

// A new database runs the whole migration once; its second start changes
// no schema, and the moderation tables exist whatever MODERATION says.
func TestSchema16FreshIsStable(t *testing.T) {
	if off, on := freshSchema(t, Config{}), freshSchema(t, Config{Features: Features{Moderation: true}}); off != on {
		t.Fatalf("schema depends on MODERATION:\n%s", schemaDiff(off, on))
	}
	path := filepath.Join(t.TempDir(), "board.db")
	openClose(t, path, Config{})
	db := rawDB(t, path)
	cookie := pragmaInt(t, db, "schema_version")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	openClose(t, path, Config{Features: Features{Moderation: true}})
	if got := pragmaInt(t, rawDB(t, path), "schema_version"); got != cookie {
		t.Fatalf("schema cookie %d -> %d on a schema-16 database", cookie, got)
	}
}

// testdata/schemaN.sql pins schema N. A failure here means the schema a new
// database gets changed: that needs a new version (raise SchemaVersion, see
// migrateSchema), or an existing database, which runs no DDL at open, never
// gets it. Then regenerate the pin:
// SWARMMEMO_UPDATE_SCHEMA=1 go test ./internal/board -run TestSchemaPinned
func TestSchemaPinned(t *testing.T) {
	got := freshSchema(t, Config{})
	file := filepath.Join("testdata", fmt.Sprintf("schema%d.sql", SchemaVersion))
	if os.Getenv("SWARMMEMO_UPDATE_SCHEMA") == "1" {
		header := fmt.Sprintf("-- Schema %d: sqlite_master of a new database, ordered by type and name.\n-- Generated by TestSchemaPinned (SWARMMEMO_UPDATE_SCHEMA=1); do not edit.\n", SchemaVersion)
		if err := os.WriteFile(file, []byte(header+got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	_, pinned, _ := strings.Cut(string(want), "do not edit.\n")
	if got != pinned {
		t.Fatalf("schema %d changed without a new version; see this test's comment:\n%s", SchemaVersion, schemaDiff(got, pinned))
	}
}

// schemaDiff names the objects whose SQL differs between two normalized schemas.
func schemaDiff(a, b string) string {
	split := func(s string) map[string]string {
		out := map[string]string{}
		for _, block := range strings.Split(s, "\n-- ") {
			head, body, _ := strings.Cut(strings.TrimPrefix(block, "-- "), "\n")
			out[head] = body
		}
		return out
	}
	am, bm := split(a), split(b)
	var lines []string
	for k, v := range am {
		if w, ok := bm[k]; !ok {
			lines = append(lines, "only in first: "+k)
		} else if v != w {
			lines = append(lines, "differs: "+k+"\n  first:  "+v+"\n  second: "+w)
		}
	}
	for k := range bm {
		if _, ok := am[k]; !ok {
			lines = append(lines, "only in second: "+k)
		}
	}
	return strings.Join(lines, "\n")
}
