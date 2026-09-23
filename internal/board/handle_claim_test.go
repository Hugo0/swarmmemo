package board

import "testing"

func handleOf(t *testing.T, s *Store, key string) string {
	t.Helper()
	var handle string
	if err := s.db.QueryRow("SELECT coalesce(max(handle),'') FROM identities WHERE id=?", key).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	return handle
}

func storedHandle(t *testing.T, s *Store, id string) string {
	t.Helper()
	res := run(t, s, Command{Operation: "message.get", MessageID: id})
	if len(res.Messages) != 1 {
		t.Fatalf("message %s not readable", id)
	}
	return res.Messages[0].Handle
}

func TestSignedPostClaimsHandleOnFirstUse(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	jill, other := keyFor(201), keyFor(202)

	first := run(t, s, signed(jill, Command{Operation: "post", Text: "hello", Handle: "Jill"}))
	if first.Receipt.HandleNotApplied != nil || handleOf(t, s, keyID(jill)) != "jill" || storedHandle(t, s, first.Receipt.ID) != "jill" {
		t.Fatalf("first signed post did not claim: %+v", first.Receipt.HandleNotApplied)
	}
	// Same handle in another case is the key's own: no advice.
	again := run(t, s, signed(jill, Command{Operation: "post", Text: "again", Handle: "JILL"}))
	if again.Receipt.HandleNotApplied != nil || storedHandle(t, s, again.Receipt.ID) != "jill" {
		t.Fatal("own handle in another case was not accepted as-is")
	}

	// Another key cannot take it, in any case; its post still succeeds, unnamed.
	taken := run(t, s, signed(other, Command{Operation: "post", Text: "me too", Handle: "jILL"}))
	if h := taken.Receipt.HandleNotApplied; h == nil || h.Reason != "taken" || h.Requested != "jILL" {
		t.Fatalf("taken hint: %+v", h)
	}
	if storedHandle(t, s, taken.Receipt.ID) != "" || handleOf(t, s, keyID(other)) != "" {
		t.Fatal("taken handle was displayed or granted")
	}

	// A key with a handle keeps it; renaming stays agent.register.
	run(t, s, signed(other, Command{Operation: "post", Text: "named", Handle: "bob"}))
	mismatch := run(t, s, signed(other, Command{Operation: "post", Text: "rename?", Handle: "robert"}))
	if h := mismatch.Receipt.HandleNotApplied; h == nil || h.Reason != "already_has_handle" || h.Requested != "robert" {
		t.Fatalf("already_has_handle hint: %+v", h)
	}
	if storedHandle(t, s, mismatch.Receipt.ID) != "bob" || handleOf(t, s, keyID(other)) != "bob" {
		t.Fatal("post with another handle was not stored under the key's real handle")
	}
	// The signature still covers the requested bytes.
	if res := run(t, s, Command{Operation: "message.get", MessageID: mismatch.Receipt.ID}); res.Messages[0].SignedPayload == "" {
		t.Fatal("signed payload missing")
	}

	// Export carries the real handle, never the requested one.
	want := map[string]string{first.Receipt.ID: "jill", again.Receipt.ID: "jill", taken.Receipt.ID: "", mismatch.Receipt.ID: "bob"}
	seen := 0
	for _, e := range run(t, s, Command{Operation: "export", Limit: 100}).Messages {
		if w, ok := want[e.ID]; ok {
			seen++
			if e.Handle != w {
				t.Fatalf("export %s handle %q, want %q", e.ID, e.Handle, w)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("export returned %d of %d posts", seen, len(want))
	}

	// An exact retry returns the stored receipt without re-deciding.
	retry := signed(other, Command{Operation: "post", Text: "retry", Handle: "zed", RequestID: "retry-1"})
	run(t, s, retry)
	if dup := run(t, s, retry); !dup.Receipt.Duplicate || dup.Receipt.HandleNotApplied != nil {
		t.Fatal("retry repeated advice or was not a duplicate")
	}
}

func TestPostHandleEdgesUnchanged(t *testing.T) {
	s := openTest(t, Config{})
	key := keyFor(203)
	// A malformed handle is refused before anything is claimed or posted.
	fails(t, s, signed(key, Command{Operation: "post", Text: "bad", Handle: "no spaces"}), "invalid_handle")
	fails(t, s, Command{Operation: "post", Text: "bad", Handle: "-dash"}, "invalid_handle")
	// Anonymous posts keep an unverified label and claim nothing.
	anon := run(t, s, Command{Operation: "post", Text: "anon", Handle: "Claimed"})
	if storedHandle(t, s, anon.Receipt.ID) != "Claimed" || anon.Receipt.HandleNotApplied != nil {
		t.Fatal("anonymous handle label changed")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM identities WHERE handle<>''").Scan(&n); err != nil || n != 0 {
		t.Fatalf("anonymous post claimed a handle: %d %v", n, err)
	}
	// A failed post rolls back its claim.
	fails(t, s, signed(key, Command{Operation: "post", Text: "x", Kind: "imported", Handle: "claimer"}), "reserved_kind")
	if handleOf(t, s, keyID(key)) != "" {
		t.Fatal("refused post kept its handle claim")
	}
}
