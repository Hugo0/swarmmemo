package board

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/services/servicestest"
)

// Shared docs on a real store: a group is a private room's members, read
// through the board view, every version becomes a doc leaf of the
// transparency log (ids and the hash only), and a conflict answers 409 with
// the current version.
func TestDocsGroupsAndLogLeaves(t *testing.T) {
	s := openTest(t, Config{Features: Features{Services: []string{"docs", "paste"}}})
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	owner, member, stranger := keyFor(41), keyFor(42), keyFor(43)
	register(t, s, member)
	register(t, s, stranger)
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "circle", Visibility: "private"}))
	run(t, s, signed(owner, Command{Operation: "room.member.add", Room: "circle", Target: keyID(member)}))
	run(t, s, signed(owner, Command{Operation: "room.create", Room: "plaza"}))

	fails(t, s, svcCall(stranger, "docs", "create", map[string]any{"title": "t", "text": "x", "group": "circle"}, 100, "s1"), "doc_group_not_found")
	fails(t, s, svcCall(owner, "docs", "create", map[string]any{"title": "t", "text": "x", "group": "plaza"}, 100, "o0"), "doc_group_not_found")
	created := run(t, s, svcCall(owner, "docs", "create", map[string]any{"title": "Circle notes", "text": "first", "group": "circle"}, 100, "o1"))
	id, _ := svcField(t, created.Data, "result", "doc", "id").(string)
	versionID, _ := svcField(t, created.Data, "result", "version_id").(string)
	if len(id) != 32 || len(versionID) != 32 {
		t.Fatalf("create: %+v", created.Data)
	}
	read := run(t, s, svcCall(member, "docs", "read", map[string]any{"id": id}, 10, "m1"))
	if svcField(t, read.Data, "result", "text") != "first" || svcField(t, read.Data, "result", "own") != false {
		t.Fatalf("member read: %+v", read.Data)
	}
	// Without moderation there is no classifier: the answer says so.
	if svcField(t, read.Data, "result", "screen") != "unavailable" || svcField(t, read.Data, "result", "screened") != false {
		t.Fatalf("screening state: %+v", read.Data)
	}
	run(t, s, svcCall(member, "docs", "write", map[string]any{"id": id, "base_version": 1, "text": "second"}, 100, "m2"))
	fails(t, s, svcCall(stranger, "docs", "read", map[string]any{"id": id}, 10, "s2"), "doc_not_found")

	// The conflict carries the current version.
	_, err := s.Execute(context.Background(), svcCall(owner, "docs", "write", map[string]any{"id": id, "base_version": 1, "text": "stale"}, 100, "o2"), "test-origin")
	var conflict *Error
	if code, _ := svcErr(err); code != "doc_conflict" || !asError(err, &conflict) || conflict.Status != 409 {
		t.Fatalf("stale write: %v", err)
	}
	details, _ := json.Marshal(conflict.Details)
	if !strings.Contains(string(details), `"version":2`) || strings.Contains(string(details), "second") {
		t.Fatalf("conflict details carry the version, never the text: %s", details)
	}

	// Each version is a doc leaf: its id, the doc, the version and the hash.
	rows, err := s.db.Query("SELECT data FROM tlog_leaves WHERE kind='doc' ORDER BY idx")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var leaves []string
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, data)
	}
	if len(leaves) != 2 || !strings.Contains(leaves[0], `"id":"`+versionID+`"`) || !strings.Contains(leaves[0], `"target":"`+id+`"`) || !strings.Contains(leaves[1], `"seq":2`) {
		t.Fatalf("doc leaves: %v", leaves)
	}
	for _, leaf := range leaves {
		if strings.Contains(leaf, "first") || strings.Contains(leaf, "second") || strings.Contains(leaf, "circle") || strings.Contains(leaf, keyID(owner)) || strings.Contains(leaf, keyID(member)) {
			t.Fatalf("a doc leaf carries text, the group or an author: %s", leaf)
		}
	}
	var ref string
	if err = s.db.QueryRow("SELECT ref FROM tlog_leaves WHERE kind='doc' ORDER BY idx LIMIT 1").Scan(&ref); err != nil || ref != versionID {
		t.Fatalf("a doc leaf's proof ref is its version id: %q %v", ref, err)
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// Unlisted pastes open for a stranger by id; private ones do not exist to
// anyone but their owner.
func TestPasteOpensOverTheBoard(t *testing.T) {
	s := openTest(t, Config{Features: Features{Services: []string{"paste"}}})
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	owner, stranger := keyFor(51), keyFor(52)
	private := run(t, s, svcCall(owner, "paste", "create", map[string]any{"text": "mine"}, 10, "p1"))
	unlisted := run(t, s, svcCall(owner, "paste", "create", map[string]any{"text": "shared", "visibility": "unlisted"}, 10, "p2"))
	fails(t, s, svcCall(stranger, "paste", "open", map[string]any{"id": svcField(t, private.Data, "result", "paste", "id")}, 10, "s1"), "paste_not_found")
	got := run(t, s, svcCall(stranger, "paste", "open", map[string]any{"id": svcField(t, unlisted.Data, "result", "paste", "id")}, 10, "s2"))
	if svcField(t, got.Data, "result", "text") != "shared" {
		t.Fatalf("open: %+v", got.Data)
	}
	// An exact retry answers the receipt without the text: it was shown once.
	again, err := s.Execute(context.Background(), svcCall(stranger, "paste", "open", map[string]any{"id": svcField(t, unlisted.Data, "result", "paste", "id")}, 10, "s2"), "test-origin")
	if err == nil && svcField(t, again.Data, "result", "text") != nil {
		t.Fatalf("a retry repeats the text: %+v", again.Data)
	}
}
