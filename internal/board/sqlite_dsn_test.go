package board

import "testing"

func TestSqliteFileDSN(t *testing.T) {
	for _, c := range []struct{ path, q, want string }{
		{"/var/lib/x.db", "a=b", "file:///var/lib/x.db?a=b"},
		{`C:\Users\a\swarmmemo.db`, "a=b", "file:///C:/Users/a/swarmmemo.db?a=b"},
		{"C:/Users/a/x.db", "", "file:///C:/Users/a/x.db"},
		{"/tmp/my dir/100%.db", "a=b", "file:///tmp/my%20dir/100%25.db?a=b"},
		{":memory:", "a=b", ":memory:?a=b"},
		{"file::memory:", "", "file::memory:"},
		{"file::memory:?cache=shared", "", "file::memory:?cache=shared"},
	} {
		if got := sqliteFileDSN(c.path, c.q); got != c.want {
			t.Errorf("sqliteFileDSN(%q,%q)=%q want %q", c.path, c.q, got, c.want)
		}
	}
}
