package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// Work addressed through an edited request, and results bound by their text,
// on the real store. The board shows a request's newest version, so every
// work operation and read takes any version's ID and resolves it to the root;
// only the requester's own in-place edits resolve. A submit binds the result's
// newest text by SHA-256, an accept signs over that same text, and an edit
// after the submit shows as a change instead of slipping in.

// editPost publishes a new version of message of, signed by key.
func editPost(t *testing.T, s *Store, key ed25519.PrivateKey, room, kind, of, text string) string {
	t.Helper()
	return run(t, s, signedNow(s, key, Command{Operation: "post", Room: room, Kind: kind, Text: text, Data: `{"schema":1,"supersedes":"` + of + `"}`})).Receipt.ID
}

// editReply publishes a new version of reply of, to the message parent.
func editReply(t *testing.T, s *Store, key ed25519.PrivateKey, parent, of, text string) string {
	t.Helper()
	return run(t, s, signedNow(s, key, Command{Operation: "post", Room: "lobby", ReplyTo: parent, Text: text, Data: `{"schema":1,"supersedes":"` + of + `"}`})).Receipt.ID
}

// workHashCommand is workCommand with result_sha256 in data.
func workHashCommand(s *Store, key ed25519.PrivateKey, c Command, hash string) Command {
	c.Data = `{"schema":1,"generation":"` + s.generation + `","result_sha256":"` + hash + `"}`
	c.Timestamp = s.now().Unix()
	return signed(key, c)
}

func workHistoryOf(t *testing.T, s *Store, id string) Result {
	t.Helper()
	return run(t, s, Command{Operation: "work.history", MessageID: id})
}

func TestWorkEditedRequestVersionResolvesToRoot(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(250), keyFor(251)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	second := editPost(t, s, owner, "lobby", "request", id, "Brief, second version: claim this message with work.claim")
	third := editPost(t, s, owner, "lobby", "request", second, "Brief, third version")

	// A read by the newest version names the root and the version it was given,
	// and still carries the newest text.
	w := getTestWork(t, s, third)
	if w.ID != id || w.ResolvedFrom != third || w.Request == nil || w.Request.VersionID != third || w.Request.Text != "Brief, third version" || w.State != "open" {
		t.Fatalf("read by version: %+v request %+v", w, w.Request)
	}
	if w := getTestWork(t, s, id); w.ResolvedFrom != "" || w.Request.VersionID != third {
		t.Fatalf("read by root: %+v", w)
	}

	// Claim by an older version: the ack is the root's and says which version.
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: second, TTL: 600})).Data["ack"].(WorkAck)
	if ack.WorkID != id || ack.ResolvedFrom != second || ack.State != "claimed" || ack.Fence != 1 {
		t.Fatalf("claim by version: %+v", ack)
	}
	result := workResult(t, s, worker, id, "lobby")
	submit := run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: third, Amount: 1, Target: result})).Data["ack"].(WorkAck)
	if submit.WorkID != id || submit.ResolvedFrom != third || submit.State != "submitted" {
		t.Fatalf("submit by version: %+v", submit)
	}
	accept := run(t, s, workCommand(s, owner, Command{Operation: "work.accept", MessageID: third, Amount: 1})).Data["ack"].(WorkAck)
	if accept.WorkID != id || accept.State != "accepted" {
		t.Fatalf("accept by version: %+v", accept)
	}
	if w := getTestWork(t, s, id); w.State != "accepted" || w.ResultID != result {
		t.Fatalf("after accept: %+v", w)
	}
	// Creating work again through a version finds the root's.
	fails(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: second}), "work_exists")

	// History by a version is the root's; each transition keeps its exact
	// signed bytes and says which version it named.
	h := workHistoryOf(t, s, third)
	if h.Data["work_id"] != id || h.Data["resolved_from"] != third {
		t.Fatalf("history by version: %v", h.Data)
	}
	transitions := h.Data["transitions"].([]WorkTransition)
	if len(transitions) != 4 {
		t.Fatalf("transitions %d", len(transitions))
	}
	want := []string{"", second, third, third}
	for i, tr := range transitions {
		pub, _ := base64.RawURLEncoding.DecodeString(tr.PublicKey)
		sig, _ := base64.RawURLEncoding.DecodeString(tr.Signature)
		if !ed25519.Verify(pub, []byte(tr.SignedPayload), sig) || tr.ResolvedFrom != want[i] {
			t.Fatalf("transition %d: resolved_from %q (want %q)", i, tr.ResolvedFrom, want[i])
		}
	}
	if !strings.Contains(transitions[1].SignedPayload, `"message_id":"`+second+`"`) {
		t.Fatal("the signed claim lost the ID it named")
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}

func TestWorkVersionResolutionRefusesOthers(t *testing.T) {
	s := openTest(t, Config{})
	owner, other, worker := keyFor(252), keyFor(253), keyFor(254)
	run(t, s, signedNow(s, owner, Command{Operation: "room.create", Room: "side", Visibility: "public"}))
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	edit := editPost(t, s, owner, "lobby", "request", id, "Brief, edited")

	// Nobody else can publish a version of the request.
	fails(t, s, signedNow(s, other, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "hijack", Data: `{"schema":1,"supersedes":"` + edit + `"}`}), "supersede_forbidden")

	// Even a row that claims the root as its origin never redirects work when
	// its author or room differ: it is read as itself, which is no work.
	forged := run(t, s, signedNow(s, other, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "not a version"})).Receipt.ID
	moved := run(t, s, signedNow(s, owner, Command{Operation: "post", Room: "side", Kind: "request", Text: "elsewhere"})).Receipt.ID
	for _, fake := range []string{forged, moved} {
		if _, err := s.db.Exec("UPDATE events SET origin=? WHERE id=?", id, fake); err != nil {
			t.Fatal(err)
		}
		fails(t, s, Command{Operation: "work.get", MessageID: fake}, "not_found")
		fails(t, s, Command{Operation: "work.history", MessageID: fake}, "not_found")
		fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: fake, TTL: 600}), "not_found")
	}
	if w := getTestWork(t, s, id); w.State != "open" || w.Fence != 0 {
		t.Fatalf("a refused claim changed the work: %+v", w)
	}

	// A version of a request that is not work is not work either.
	plain := run(t, s, signedNow(s, owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "no work here"})).Receipt.ID
	plainEdit := editPost(t, s, owner, "lobby", "request", plain, "still no work")
	fails(t, s, Command{Operation: "work.get", MessageID: plainEdit}, "not_found")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: plainEdit, TTL: 600}), "not_found")
	// Opening it as work through the version opens the root.
	run(t, s, workCommand(s, owner, Command{Operation: "work.create", MessageID: plainEdit}))
	if w := getTestWork(t, s, plain); w.ID != plain || w.Request == nil || w.Request.VersionID != plainEdit {
		t.Fatalf("work created through a version: %+v", w)
	}

	// A hidden version addresses nothing; the root and a visible version still
	// reach the work, which shows no request text (a hide removes the message).
	newest := editPost(t, s, owner, "lobby", "request", edit, "Brief, newest")
	if _, err := s.db.Exec("UPDATE events SET hidden=1 WHERE id=?", newest); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "work.get", MessageID: newest}, "not_found")
	fails(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: newest, TTL: 600}), "not_found")
	if w := getTestWork(t, s, edit); w.ID != id || w.ResolvedFrom != edit || w.Request != nil {
		t.Fatalf("read through a visible version beside a hidden one: %+v", w)
	}
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: edit, TTL: 600}))
}

func TestWorkResultBoundBySHA256(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(248), keyFor(249)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: id, TTL: 600}))

	// Strict data: only submit, accept and a claim with a result take a hash,
	// and only a 64-hex one.
	hash := func(text string) string { return sha256Hex([]byte(text)) }
	fails(t, s, workHashCommand(s, worker, Command{Operation: "work.renew", MessageID: id, Amount: 1, TTL: 900}, hash("x")), "invalid_work_data")
	fails(t, s, workHashCommand(s, owner, Command{Operation: "work.cancel", MessageID: id, Reason: "no"}, hash("x")), "invalid_work_data")
	fails(t, s, workHashCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: id}, "ABC"), "invalid_work_data")

	// The worker posts its result and fixes it before submitting: the submit
	// binds the newest text, which a hash of the older one does not match.
	first := run(t, s, signedNow(s, worker, Command{Operation: "post", ReplyTo: id, Text: "Result, draft"})).Receipt.ID
	fixed := editReply(t, s, worker, id, first, "Result, fixed")
	fails(t, s, workHashCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: first}, hash("Result, draft")), "work_result_changed")
	// An old client sends no hash; the board records what it bound, and the
	// ack keeps its original shape.
	ack := run(t, s, workCommand(s, worker, Command{Operation: "work.submit", MessageID: id, Amount: 1, Target: first})).Data["ack"].(WorkAck)
	raw, _ := json.Marshal(ack)
	if strings.Contains(string(raw), "result_sha256") || strings.Contains(string(raw), "resolved_from") {
		t.Fatalf("old-client ack changed shape: %s", raw)
	}
	w := getTestWork(t, s, id)
	if w.ResultID != fixed || w.ResultSHA256 != hash("Result, fixed") || w.ResultEdited() || w.ResultChangedSinceSubmit == nil {
		t.Fatalf("submitted: %+v", w)
	}
	h := workHistoryOf(t, s, id).Data["transitions"].([]WorkTransition)
	if last := h[len(h)-1]; last.Operation != "work.submit" || last.ResultSHA256 != hash("Result, fixed") || last.ResultSHA256Signed {
		t.Fatalf("recorded submit: %+v", last)
	}

	// An edit after the submit shows as a change; the work still names the
	// submitted version, and an accept of the edit's text is refused.
	editReply(t, s, worker, id, fixed, "Result, swapped after submit")
	w = getTestWork(t, s, id)
	if w.ResultID != fixed || w.ResultSHA256 != hash("Result, fixed") || !w.ResultEdited() {
		t.Fatalf("edited after submit: %+v", w)
	}
	fails(t, s, workHashCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}, hash("Result, swapped after submit")), "work_result_changed")
	ack = run(t, s, workHashCommand(s, owner, Command{Operation: "work.accept", MessageID: id, Amount: 1}, hash("Result, fixed"))).Data["ack"].(WorkAck)
	if ack.State != "accepted" || ack.ResultSHA256 != hash("Result, fixed") {
		t.Fatalf("accept: %+v", ack)
	}
	h = workHistoryOf(t, s, id).Data["transitions"].([]WorkTransition)
	if last := h[len(h)-1]; last.Operation != "work.accept" || last.ResultSHA256 != hash("Result, fixed") || !last.ResultSHA256Signed || !strings.Contains(last.SignedPayload, hash("Result, fixed")) {
		t.Fatalf("signed accept: %+v", last)
	}

	// A claim that names its result may sign the hash too; without a result it
	// may not.
	second := createTestWork(t, s, owner, "lobby", "request", 0)
	fails(t, s, workHashCommand(s, worker, Command{Operation: "work.claim", MessageID: second, TTL: 600}, hash("x")), "invalid_work_data")
	reply := run(t, s, signedNow(s, worker, Command{Operation: "post", ReplyTo: second, Text: "One-step result"})).Receipt.ID
	claimed := run(t, s, workHashCommand(s, worker, Command{Operation: "work.claim", MessageID: second, Target: reply}, hash("One-step result"))).Data["ack"].(WorkAck)
	if claimed.State != "submitted" || claimed.ResultSHA256 != hash("One-step result") {
		t.Fatalf("claim with result and hash: %+v", claimed)
	}
	// A reject clears the attempt: its recorded hash goes, its signed one stays.
	run(t, s, workCommand(s, owner, Command{Operation: "work.reject", MessageID: second, Amount: 1, Reason: "not yet"}))
	h = workHistoryOf(t, s, second).Data["transitions"].([]WorkTransition)
	if tr := h[1]; tr.Operation != "work.claim" || tr.ResultSHA256 != hash("One-step result") || !tr.ResultSHA256Signed {
		t.Fatalf("signed claim after reject: %+v", tr)
	}
	if w := getTestWork(t, s, second); w.ResultID != "" || w.ResultSHA256 != "" || w.ResultChangedSinceSubmit != nil {
		t.Fatalf("rejected work still shows a result: %+v", w)
	}
	if err := s.Integrity(testContext); err != nil {
		t.Fatal(err)
	}
}
