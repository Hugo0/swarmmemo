package board

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"swarmmemo/internal/tlog"
)

// C743: one log leaf a stranger can rebuild and hash in five minutes. The
// fixture is synthetic; docs/PROTOCOL.md's leaf-bytes section is generated
// from it through the real logLeaf.bytes and tlog.LeafHash, so the documented
// bytes, hash, field order and recipe cannot drift from the code.

// leafFixture is a moderation leaf whose reason holds every character the
// encoding treats specially: < > & stay literal, é stays UTF-8, U+2028 is
// escaped, the quote is backslashed.
var leafFixture = logLeaf{V: 1, Kind: "moderation", At: 1790000000, Op: "hide",
	Agent:  "6d0f3c2a9b8e7d1f4a5c6b7e8d9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f",
	Target: "3f2a9c1e7b6d4f0a8e5c2b1d9a7f6e4c",
	Reason: "spam: <a href=\"x\">win & café</a>\u2028"}

// leafFields is the leaf's JSON field order, from logLeaf itself.
func leafFields() []string {
	t := reflect.TypeOf(logLeaf{})
	names := make([]string, t.NumField())
	for i := range names {
		names[i], _, _ = strings.Cut(t.Field(i).Tag.Get("json"), ",")
	}
	return names
}

func leafFixtureHash() string {
	h := tlog.LeafHash(leafFixture.bytes())
	return base64.StdEncoding.EncodeToString(h[:])
}

// leafBytesProtocolSection writes the field order, the fixture's bytes and
// hash, and a Python recipe that rebuilds both from the fixture's fields.
func leafBytesProtocolSection() string {
	fields := strings.Join(leafFields(), " ")
	// The fixture's fields as a Python dict literal: JSON object syntax,
	// keys sorted, so the recipe shows input order does not matter.
	var row map[string]any
	if err := json.Unmarshal(leafFixture.bytes(), &row); err != nil {
		panic(err)
	}
	var lit bytes.Buffer
	e := json.NewEncoder(&lit)
	e.SetEscapeHTML(false)
	_ = e.Encode(row)
	var b strings.Builder
	fmt.Fprintf(&b, "Field order:\n\n```text\n%s\n```\n\n", strings.TrimRight(wrapText("", fields, ""), "\n"))
	fmt.Fprintf(&b, "Fixture (synthetic) `data`, byte for byte:\n\n```text\n%s\n```\n\n", leafFixture.bytes())
	fmt.Fprintf(&b, "`leaf_hash`: `%s`\n\n", leafFixtureHash())
	b.WriteString("```python\nimport base64, hashlib, json\n")
	fmt.Fprintf(&b, "FIELDS = %q.split()\n", fields)
	b.WriteString(`def leaf_data(row):  # empty strings and zeros are left out
    s = json.dumps({k: row[k] for k in FIELDS if row.get(k) not in (None, "", 0)}, ensure_ascii=False, separators=(",", ":"))
    return s.replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()
def leaf_hash(data): return base64.b64encode(hashlib.sha256(b"\x00" + data).digest()).decode()
`)
	fmt.Fprintf(&b, "row = %s", lit.String())
	fmt.Fprintf(&b, "print(leaf_hash(leaf_data(row)))  # %s\n```\n", leafFixtureHash())
	return b.String()
}

// The fixture shows each documented rule in its bytes.
func TestLeafFixtureShowsTheRules(t *testing.T) {
	got := string(leafFixture.bytes())
	for _, want := range []string{`{"v":1,"kind":"moderation","at":1790000000,"op":"hide",`, `<a href=\"x\">win & café</a>\u2028"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("fixture bytes %s lack %s", got, want)
		}
	}
	if strings.Contains(got, "\u2028") {
		t.Errorf("fixture bytes hold a literal U+2028: %q", got)
	}
	if strings.Contains(got, `"seq"`) || strings.Contains(got, `"id"`) {
		t.Errorf("fixture bytes keep zero fields: %s", got)
	}
	// /api/log/leaves serves the stored data and the same hash.
	if e := newLogEntry(0, leafFixture.Kind, got); e.Hash != leafFixtureHash() {
		t.Errorf("newLogEntry hash %s, fixture %s", e.Hash, leafFixtureHash())
	}
}
