package board

import (
	"strings"
	"testing"
)

// A prefix names only public, visible messages, in ID order, on the real
// engine: a private room's post and a removed post never match.
func TestPublicIDsWithPrefix(t *testing.T) {
	s := openTest(t, Config{ServiceID: "swarmmemo.com"})
	key := keyFor(41)
	register(t, s, key)
	run(t, s, signed(key, Command{Operation: "room.create", Room: "hush", Visibility: "private"}))
	post := func(room, text string) string {
		return run(t, s, signed(key, Command{Operation: "post", Room: room, Text: text})).Receipt.ID
	}
	ids := map[string]string{
		post("lobby", "one"):   "abcdef0100000000000000000000000a",
		post("lobby", "two"):   "abcdef0100000000000000000000000b",
		post("lobby", "three"): "abcdef0200000000000000000000000c",
		post("hush", "secret"): "abcdef0100000000000000000000000d",
		post("lobby", "gone"):  "abcdef0100000000000000000000000e",
	}
	// Fixture IDs that share a prefix; other tables keep the old IDs.
	if _, err := s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	for from, to := range ids {
		if _, err := s.db.Exec("UPDATE events SET id=? WHERE id=?", to, from); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", "abcdef0100000000000000000000000e"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prefix string
		limit  int
		want   string
	}{
		{"abcdef01", 11, "abcdef0100000000000000000000000a abcdef0100000000000000000000000b"},
		{"abcdef01", 1, "abcdef0100000000000000000000000a"},
		{"abcdef02", 11, "abcdef0200000000000000000000000c"},
		{"abcdef0100000000000000000000000", 11, "abcdef0100000000000000000000000a abcdef0100000000000000000000000b"},
		{"abcdef0100000000000000000000000d", 11, ""}, // a full ID is not a prefix
		{"abcdef03", 11, ""},
		{"abcdef0", 11, ""},  // too short
		{"ABCDEF01", 11, ""}, // lowercase only
		{"abcdef0g", 11, ""},
	} {
		got, err := s.PublicIDsWithPrefix(testContext, tc.prefix, tc.limit)
		if err != nil || strings.Join(got, " ") != tc.want {
			t.Errorf("%s: %v %v, want %q", tc.prefix, got, err, tc.want)
		}
	}
}
