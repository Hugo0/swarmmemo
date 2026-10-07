package board

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/ots"
	"swarmmemo/internal/tlog"
)

// steppingClock advances one second per read, so every write has its own
// second and the log's (created_at, rank, seq) order is the commit order.
func steppingClock(s *Store) {
	var now atomic.Int64
	now.Store(testTime)
	s.now = func() time.Time { return time.Unix(now.Add(1), 0) }
}

func logLeaves(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT data FROM tlog_leaves ORDER BY idx")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func decodeHashes(t *testing.T, in []string) []tlog.Hash {
	t.Helper()
	out := make([]tlog.Hash, len(in))
	for i, s := range in {
		h, err := tlog.ParseHash(s)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = h
	}
	return out
}

// verifyCheckpoint opens a checkpoint's note with the log key and returns its
// parsed body, which must match the JSON fields.
func verifyCheckpoint(t *testing.T, s *Store, cp LogCheckpoint) tlog.Checkpoint {
	t.Helper()
	text, err := tlog.OpenNote([]byte(cp.Note), s.LogVerifierKey())
	if err != nil {
		t.Fatal(err)
	}
	body, err := tlog.ParseCheckpoint(text)
	if err != nil {
		t.Fatal(err)
	}
	if body.Origin != "swarmmemo.com/log" || body.Size != cp.Size || base64.StdEncoding.EncodeToString(body.Root[:]) != cp.Root {
		t.Fatalf("checkpoint fields disagree with the signed note: %+v %+v", body, cp)
	}
	return body
}

func verifyInclusion(t *testing.T, s *Store, p LogInclusion, cp tlog.Checkpoint) {
	t.Helper()
	leaf := tlog.LeafHash([]byte(p.Leaf.Data))
	if base64.StdEncoding.EncodeToString(leaf[:]) != p.Leaf.Hash {
		t.Fatal("leaf hash does not match leaf data")
	}
	if err := tlog.VerifyInclusion(p.Leaf.Index, cp.Size, leaf, decodeHashes(t, p.Proof), cp.Root); err != nil {
		t.Fatalf("inclusion of leaf %d: %v", p.Leaf.Index, err)
	}
}

// buildHistory writes a public history with private noise around it.
func buildHistory(t *testing.T, s *Store) (author ed25519.PrivateKey, postID, hiddenID string) {
	author, private, other := keyFor(61), keyFor(62), keyFor(63)
	run(t, s, signed(author, Command{Operation: "agent.register", Handle: "logged-agent"}))
	postID = run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "first public post"})).Receipt.ID
	// Private noise: a private room and its post, and a private-only key
	// that rotates. None of it may reach the log.
	run(t, s, signed(private, Command{Operation: "room.create", Room: "hidden-room", Visibility: "private"}))
	run(t, s, signed(private, Command{Operation: "post", Room: "hidden-room", Text: "private text"}))
	rotate := signed(private, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(keyFor(64).Public().(ed25519.PublicKey))})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(keyFor(64), Canonical("swarmmemo.com", rotate)))
	run(t, s, rotate)
	// A first post that claims a handle: the claim precedes the post.
	hiddenID = run(t, s, signed(other, Command{Operation: "post", Room: "lobby", Text: "spam link", Handle: "other-agent"})).Receipt.ID
	if err := s.Moderate(testContext, hiddenID, "phishing", true); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantTier(testContext, keyID(author), 2, "verified bounty work"); err != nil {
		t.Fatal(err)
	}
	return author, postID, hiddenID
}

func TestTransparencyLogRecordsThePublicRecordOnly(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	author, postID, hiddenID := buildHistory(t, s)
	leaves := logLeaves(t, s)
	kinds := []string{}
	for _, d := range leaves {
		var l logLeaf
		if err := json.Unmarshal([]byte(d), &l); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, l.Kind+":"+l.Op)
	}
	want := "identity:agent.register message: identity:handle.claim message: moderation:hide tier:grant"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("leaf kinds\n got %s\nwant %s", strings.Join(kinds, " "), want)
	}
	all := strings.Join(leaves, "\n")
	for _, leak := range []string{"private text", "hidden-room", keyID(keyFor(62)), keyID(keyFor(64)), "first public post"} {
		if strings.Contains(all, leak) {
			t.Fatalf("log contains %q:\n%s", leak, all)
		}
	}
	text := sha256.Sum256([]byte("first public post"))
	if !strings.Contains(leaves[1], `"text_sha256":"`+hex.EncodeToString(text[:])+`"`) || !strings.Contains(leaves[1], postID) || !strings.Contains(leaves[1], keyID(author)) {
		t.Fatalf("message leaf: %s", leaves[1])
	}
	// Moderation appended a leaf; the hidden message's own leaf is unchanged.
	if !strings.Contains(leaves[4], `"reason":"phishing"`) || !strings.Contains(leaves[4], hiddenID) || !strings.Contains(leaves[3], hiddenID) {
		t.Fatalf("hide leaves: %s / %s", leaves[3], leaves[4])
	}
	if err := s.Moderate(testContext, hiddenID, "false positive", false); err != nil {
		t.Fatal(err)
	}
	after := logLeaves(t, s)
	if len(after) != len(leaves)+1 || strings.Join(after[:len(leaves)], "\n") != all || !strings.Contains(after[len(leaves)], `"op":"restore"`) {
		t.Fatalf("restore did not append exactly one leaf:\n%s", strings.Join(after, "\n"))
	}
	// The log refuses to be rewritten.
	if _, err := s.db.Exec("UPDATE tlog_leaves SET data='x' WHERE idx=0"); err == nil {
		t.Fatal("a leaf was rewritten")
	}
	if _, err := s.db.Exec("DELETE FROM tlog_hashes"); err == nil {
		t.Fatal("hashes were deleted")
	}
}

func TestTransparencyCheckpointsAndProofs(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	_, postID, hiddenID := buildHistory(t, s)
	if _, err := s.ReadLogProof(testContext, 0, postID, -1); err == nil {
		t.Fatal("proof without a checkpoint")
	}
	if signedNow, err := s.SignCheckpoint(testContext); err != nil || !signedNow {
		t.Fatal("first checkpoint", err)
	}
	if signedNow, err := s.SignCheckpoint(testContext); err != nil || signedNow {
		t.Fatal("a checkpoint was signed for an unchanged tree", err)
	}
	first, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	firstBody := verifyCheckpoint(t, s, first)
	if first.Size != int64(len(logLeaves(t, s))) {
		t.Fatalf("checkpoint size %d", first.Size)
	}
	// A message's proof carries its hide too, each verifying.
	proof, err := s.ReadLogProof(testContext, 0, hiddenID, -1)
	if err != nil {
		t.Fatal(err)
	}
	verifyInclusion(t, s, proof, firstBody)
	if len(proof.Related) != 1 || !strings.Contains(proof.Related[0].Leaf.Data, "phishing") {
		t.Fatalf("related leaves: %+v", proof.Related)
	}
	verifyInclusion(t, s, proof.Related[0], firstBody)
	// A tampered leaf fails.
	proof.Leaf.Data = strings.Replace(proof.Leaf.Data, hiddenID, postID, 1)
	if tlog.VerifyInclusion(proof.Leaf.Index, firstBody.Size, tlog.LeafHash([]byte(proof.Leaf.Data)), decodeHashes(t, proof.Proof), firstBody.Root) == nil {
		t.Fatal("tampered leaf verified")
	}
	// The tree grows; the new checkpoint is consistent with the first.
	for i := range 5 {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: "more " + string(rune('a'+i))})
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	c, err := s.ReadLogConsistency(testContext, first.Size, -1)
	if err != nil {
		t.Fatal(err)
	}
	secondBody := verifyCheckpoint(t, s, c.To)
	if secondBody.Size != first.Size+5 {
		t.Fatalf("second checkpoint size %d", secondBody.Size)
	}
	if err = tlog.VerifyConsistency(firstBody.Size, secondBody.Size, decodeHashes(t, c.Proof), firstBody.Root, secondBody.Root); err != nil {
		t.Fatal(err)
	}
	// A proof against the older checkpoint still verifies against it.
	old, err := s.ReadLogProof(testContext, 1, "", first.Size)
	if err != nil {
		t.Fatal(err)
	}
	verifyInclusion(t, s, old, firstBody)
	if _, err = s.ReadLogProof(testContext, first.Size+1, "", first.Size); err == nil {
		t.Fatal("proof of a leaf beyond its checkpoint")
	}
	if _, err = s.ReadLogConsistency(testContext, first.Size+2, -1); err == nil {
		t.Fatal("consistency from a size that was never signed")
	}
	page, size, err := s.ReadLogLeaves(testContext, 0, 3)
	if err != nil || len(page) != 3 || size != secondBody.Size || page[2].Index != 2 {
		t.Fatalf("leaves page: %v %d %v", page, size, err)
	}
}

// A message's proof alone checks the text against the leaf's text_sha256 and
// the leaf's signature over signed_payload; a hidden message's carries neither.
func TestTransparencyProofCarriesSignedPayload(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	author, postID, hiddenID := buildHistory(t, s)
	anonID := run(t, s, Command{Operation: "post", Room: "lobby", Text: "anonymous <b>&</b>"}).Receipt.ID
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	proof, err := s.ReadLogProof(testContext, 0, postID, -1)
	if err != nil {
		t.Fatal(err)
	}
	var leaf logLeaf
	if err = json.Unmarshal([]byte(proof.Leaf.Data), &leaf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(proof.Leaf.Data, "first public post") || strings.Contains(proof.Leaf.Data, "signed_payload") {
		t.Fatalf("the leaf itself changed: %s", proof.Leaf.Data)
	}
	if proof.Text == nil || *proof.Text != "first public post" || sha256Hex([]byte(*proof.Text)) != leaf.TextSHA256 {
		t.Fatalf("proof text %v for leaf %s", proof.Text, proof.Leaf.Data)
	}
	var env struct {
		Service string
		Command Command
	}
	if err = json.Unmarshal([]byte(proof.SignedPayload), &env); err != nil {
		t.Fatalf("signed_payload %q: %v", proof.SignedPayload, err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(env.Command.PublicKey)
	sig, _ := base64.RawURLEncoding.DecodeString(leaf.Signature)
	if !ed25519.Verify(pub, []byte(proof.SignedPayload), sig) || fingerprint(pub) != leaf.Agent || leaf.Agent != keyID(author) ||
		env.Command.Text != *proof.Text || env.Command.Room != leaf.Room || env.Service != "swarmmemo.com" {
		t.Fatalf("offline check fails: payload %s leaf %s", proof.SignedPayload, proof.Leaf.Data)
	}
	// By leaf index: the same.
	byIndex, err := s.ReadLogProof(testContext, proof.Leaf.Index, "", -1)
	if err != nil || byIndex.SignedPayload != proof.SignedPayload || byIndex.Text == nil {
		t.Fatalf("by index: %+v %v", byIndex, err)
	}
	// Hidden: the leaf and its hide are proven, the text and payload withheld.
	hidden, err := s.ReadLogProof(testContext, 0, hiddenID, -1)
	if err != nil || hidden.Text != nil || hidden.SignedPayload != "" || len(hidden.Related) != 1 {
		t.Fatalf("hidden message proof: %+v %v", hidden, err)
	}
	if raw, _ := json.Marshal(hidden); strings.Contains(string(raw), "spam link") {
		t.Fatalf("hidden text leaked: %s", raw)
	}
	// Anonymous: text, no payload.
	anon, err := s.ReadLogProof(testContext, 0, anonID, -1)
	if err != nil || anon.Text == nil || *anon.Text != "anonymous <b>&</b>" || anon.SignedPayload != "" {
		t.Fatalf("anonymous proof: %+v %v", anon, err)
	}
	// Other kinds of leaf carry neither.
	first, err := s.ReadLogProof(testContext, 0, "", -1)
	if err != nil || first.Leaf.Kind == "message" || first.Text != nil || first.SignedPayload != "" {
		t.Fatalf("identity leaf proof: %+v %v", first, err)
	}
}

// TestTransparencyBackfillEqualsIncremental opens a database the way
// production has it before schema 15 (no log) and checks the one-time
// backfill builds the very tree the live path built.
func TestTransparencyBackfillEqualsIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill.sqlite")
	s, err := Open(path, Config{EchoSimulate: true})
	if err != nil {
		t.Fatal(err)
	}
	steppingClock(s)
	buildHistory(t, s)
	for i := range 3 {
		run(t, s, signed(keyFor(61), Command{Operation: "post", Room: "lobby", Text: "again " + string(rune('a'+i))}))
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	live, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	liveLeaves := logLeaves(t, s)
	if _, err = s.db.Exec("DROP TABLE tlog_anchors; DROP TABLE tlog_checkpoints; DROP TABLE tlog_leaves; DROP TABLE tlog_hashes; DROP TABLE tlog_cursors; PRAGMA user_version=14"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{EchoSimulate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := sqlCount(t, s, "PRAGMA user_version"); got != SchemaVersion {
		t.Fatalf("schema %d", got)
	}
	if got := logLeaves(t, s); strings.Join(got, "\n") != strings.Join(liveLeaves, "\n") {
		t.Fatalf("backfill differs:\n%s\n---\n%s", strings.Join(got, "\n"), strings.Join(liveLeaves, "\n"))
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	again, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	if again.Root != live.Root || again.Size != live.Size {
		t.Fatalf("backfilled root %s/%d, live %s/%d", again.Root, again.Size, live.Root, live.Size)
	}
	// The same key signs after a restart (log.key beside the database).
	if again.VerifierKey != live.VerifierKey {
		t.Fatal("the log key changed across a restart")
	}
}

func TestTransparencyRecord(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	author, _, _ := buildHistory(t, s)
	newKey := keyFor(65)
	rotate := signed(author, Command{Operation: "agent.rotate", Target: base64.RawURLEncoding.EncodeToString(newKey.Public().(ed25519.PublicKey))})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(newKey, Canonical("swarmmemo.com", rotate)))
	run(t, s, rotate)
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"logged-agent", "LOGGED-AGENT", keyID(author), keyID(newKey)} {
		rec, err := s.ReadLogRecord(testContext, who)
		if err != nil {
			t.Fatal(who, err)
		}
		r := rec.Record
		if r.Agent != keyID(newKey) || r.Handle != "logged-agent" || len(r.Keys) != 2 || r.Keys[0].Successor != keyID(newKey) {
			t.Fatalf("record keys: %+v", r)
		}
		if len(r.Handles) != 1 || r.Handles[0].Handle != "logged-agent" || r.Counts["public_messages"] != 1 || r.Counts["key_events"] != 2 {
			t.Fatalf("record history: %+v %+v", r.Handles, r.Counts)
		}
		// The note signs the record's exact bytes.
		text, err := tlog.OpenNote([]byte(rec.Note), s.LogVerifierKey())
		if err != nil {
			t.Fatal(err)
		}
		var signedRecord LogRecord
		if err = json.Unmarshal([]byte(text), &signedRecord); err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(signedRecord)
		b, _ := json.Marshal(r)
		if !bytes.Equal(a, b) {
			t.Fatal("the signed record differs from the record")
		}
		body := verifyCheckpoint(t, s, r.Checkpoint)
		if len(r.Proofs) != 2 {
			t.Fatalf("proofs %d", len(r.Proofs))
		}
		for _, p := range r.Proofs {
			verifyInclusion(t, s, p, body)
		}
	}
	for _, who := range []string{keyID(keyFor(62)), keyID(keyFor(64)), "nobody-here"} {
		var e *Error
		if _, err := s.ReadLogRecord(testContext, who); !errors.As(err, &e) || e.Status != 404 {
			t.Fatalf("%s: want 404, got %v", who, err)
		}
	}
}

// fakeCalendar answers POST /digest with a pending attestation and, once
// ready, GET /timestamp/... with a Bitcoin one.
func fakeCalendar(t *testing.T, ready *atomic.Bool, gets *atomic.Int64) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out bytes.Buffer
		if r.Method == http.MethodGet && gets != nil {
			gets.Add(1)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/digest":
			digest, _ := io.ReadAll(io.LimitReader(r.Body, 64))
			if len(digest) != 32 {
				http.Error(w, "bad digest", 400)
				return
			}
			uri := []byte(srv.URL)
			payload := append([]byte{byte(len(uri))}, uri...)
			commitment := sha256.Sum256(digest)
			(&ots.Timestamp{Msg: digest, Branches: []ots.Branch{{Op: ots.Op{Tag: 0x08}, Stamp: &ots.Timestamp{Msg: commitment[:], Attestations: []ots.Attestation{{Tag: ots.TagPending, Payload: payload}}}}}}).Serialize(&out)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/timestamp/") && ready.Load():
			msg, _ := hex.DecodeString(strings.TrimPrefix(r.URL.Path, "/timestamp/"))
			(&ots.Timestamp{Msg: msg, Attestations: []ots.Attestation{{Tag: ots.TagBitcoin, Payload: []byte{0x64}}}}).Serialize(&out)
		default:
			http.NotFound(w, r)
			return
		}
		w.Write(out.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTransparencyAnchors(t *testing.T) {
	s := openTest(t, Config{})
	run(t, s, Command{Operation: "post", Room: "lobby", Text: "anchor me"})
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	cal := fakeCalendar(t, &ready, nil)
	client := &ots.Client{Calendars: []string{cal.URL}}
	if err := s.AnchorCheckpoints(testContext, client, 3); err != nil {
		t.Fatal(err)
	}
	anchors, err := s.ReadLogAnchors(testContext, 0, 10)
	if err != nil || len(anchors) != 1 || anchors[0].State != "pending" {
		t.Fatalf("anchors %+v %v", anchors, err)
	}
	cp, _ := s.ReadLogCheckpoint(testContext, anchors[0].Size)
	digest := sha256.Sum256([]byte(cp.Note))
	raw, err := s.ReadLogAnchorFile(testContext, anchors[0].Size)
	if err != nil {
		t.Fatal(err)
	}
	file, err := ots.ParseFile(raw)
	if err != nil || !bytes.Equal(file.Digest, digest[:]) || anchors[0].Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("the .ots does not timestamp the signed note: %v", err)
	}
	// Anchoring again does nothing: every checkpoint has its proof.
	if err = s.AnchorCheckpoints(testContext, client, 3); err != nil {
		t.Fatal(err)
	}
	// The timeline: signed, submitted, next check half an hour on.
	submitted := anchors[0].SubmittedAt
	if anchors[0].CheckpointAt != cp.CreatedAt || submitted < cp.CreatedAt || anchors[0].NextCheckAt != submitted+anchorFirstCheck || anchors[0].ConfirmedAt != 0 ||
		anchors[0].OTS != fmt.Sprintf("/api/log/anchors/%d.ots", cp.Size) || anchors[0].Note != fmt.Sprintf("/api/log/checkpoint/note?size=%d", cp.Size) {
		t.Fatalf("pending anchor timeline: %+v", anchors[0])
	}
	// A proof names the anchor of the first checkpoint covering its leaf.
	proof, err := s.ReadLogProof(testContext, 0, "", -1)
	if err != nil || proof.Anchor == nil || proof.Anchor.Size != cp.Size || proof.Anchor.State != "pending" {
		t.Fatalf("proof anchor: %+v %v", proof.Anchor, err)
	}
	// Too early to upgrade, even though the calendar has committed: nothing
	// is asked before the first check.
	ready.Store(true)
	at := func(sec int64) { s.now = func() time.Time { return time.Unix(submitted+sec, 0) } }
	at(anchorFirstCheck - anchorCheckEarly - 1)
	if err = s.UpgradeAnchors(testContext, client, 5); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.ReadLogAnchors(testContext, 0, 10); a[0].State != "pending" {
		t.Fatal("upgraded before the wait")
	}
	at(anchorFirstCheck)
	if err = s.UpgradeAnchors(testContext, client, 5); err != nil {
		t.Fatal(err)
	}
	a, _ := s.ReadLogAnchors(testContext, 0, 10)
	if a[0].State != "confirmed" || a[0].BitcoinHeight != 100 || a[0].ConfirmedAt != submitted+anchorFirstCheck || a[0].NextCheckAt != 0 {
		t.Fatalf("after upgrade: %+v", a[0])
	}
	if proof, err = s.ReadLogProof(testContext, 0, "", -1); err != nil || proof.Anchor == nil || proof.Anchor.State != "confirmed" || proof.Anchor.BitcoinHeight != 100 || proof.Anchor.ConfirmedAt != a[0].ConfirmedAt {
		t.Fatalf("proof anchor after upgrade: %+v %v", proof.Anchor, err)
	}
	// A confirmed anchor is never asked again.
	at(anchorFirstCheck + 3600)
	if err = s.UpgradeAnchors(testContext, client, 5); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.ReadLogAnchors(testContext, 0, 10); b[0].ConfirmedAt != a[0].ConfirmedAt {
		t.Fatalf("a confirmed anchor was checked again: %+v", b[0])
	}
	raw, _ = s.ReadLogAnchorFile(testContext, a[0].Size)
	if file, err = ots.ParseFile(raw); err != nil {
		t.Fatal(err)
	}
	if _, height := file.Stamp.Status(); height != 100 {
		t.Fatalf("stored proof height %d", height)
	}
}
