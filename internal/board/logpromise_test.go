package board

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/tlog"
)

// openPromise opens a promise note under the log key and parses its body.
func openPromise(t *testing.T, s *Store, note string) tlog.Promise {
	t.Helper()
	text, err := tlog.OpenNote([]byte(note), s.LogVerifierKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tlog.ParseCheckpoint(text); err == nil {
		t.Fatal("a promise body parsed as a checkpoint")
	}
	p, err := tlog.ParsePromise(text)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func promiseCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// A fresh public post returns a promise for exactly its leaf, signed by the
// log key; a retry returns none and the route serves the stored one, byte
// for byte; it is pending, then kept once a checkpoint covers it, and still
// kept after a hide. A private post gets none.
func TestLogPromiseOnFreshPublicPost(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	author := keyFor(71)
	cmd := signed(author, Command{Operation: "post", Room: "lobby", Text: "promised post", RequestID: "promise-1"})
	res := run(t, s, cmd)
	lp := res.LogPromise
	if lp == nil || res.Receipt == nil || res.Receipt.Duplicate {
		t.Fatalf("no promise on a fresh public post: %+v", res)
	}
	p := openPromise(t, s, lp.Note)
	var idx int64
	var data string
	if err := s.db.QueryRow("SELECT idx,data FROM tlog_leaves WHERE ref=? AND kind='message'", res.Receipt.ID).Scan(&idx, &data); err != nil {
		t.Fatal(err)
	}
	if p.Origin != "swarmmemo.com/log" || p.Index != idx || p.Leaf != tlog.LeafHash([]byte(data)) || p.Kind != "message" || p.ID != res.Receipt.ID ||
		p.Received != res.Receipt.AcceptedAt || p.MergeBy != p.Received+int64(defaultMergeDelay/time.Second) {
		t.Fatalf("promise %+v for leaf %d %s, receipt %+v", p, idx, data, res.Receipt)
	}
	if lp.Index != idx || lp.LeafHash != base64.StdEncoding.EncodeToString(p.Leaf[:]) || lp.MergeBy != p.MergeBy || lp.Check != LogPromisePath+"?message="+res.Receipt.ID {
		t.Fatalf("restated fields: %+v", lp)
	}
	// The retry result is stored before the promise is set: it has none.
	var stored string
	if err := s.db.QueryRow("SELECT result FROM requests WHERE request_key='id:promise-1'").Scan(&stored); err != nil || strings.Contains(stored, "log_promise") || strings.Contains(stored, "promise/v1") {
		t.Fatalf("stored retry result: %s %v", stored, err)
	}
	again := run(t, s, cmd)
	if again.LogPromise != nil || !again.Receipt.Duplicate || again.Receipt.ID != res.Receipt.ID {
		t.Fatalf("retry: %+v", again)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM tlog_promises"); n != 1 {
		t.Fatalf("%d promises after a retry", n)
	}
	st, err := s.ReadLogPromise(testContext, res.Receipt.ID, -1)
	if err != nil || st.Note != lp.Note || st.State != PromisePending || st.Proof != nil || st.VerifierKey != s.LogVerifierKey() {
		t.Fatalf("pending: %+v %v", st, err)
	}
	if byLeaf, err := s.ReadLogPromise(testContext, "", idx); err != nil || byLeaf.Note != lp.Note {
		t.Fatalf("by leaf: %+v %v", byLeaf, err)
	}
	// A private room's post: no leaf, no promise.
	run(t, s, signed(author, Command{Operation: "room.create", Room: "promise-private", Visibility: "private"}))
	private := run(t, s, signed(author, Command{Operation: "post", Room: "promise-private", Text: "private"}))
	if private.LogPromise != nil {
		t.Fatalf("private post promised: %+v", private.LogPromise)
	}
	if _, err = s.ReadLogPromise(testContext, private.Receipt.ID, -1); promiseCode(err) != "not_found" {
		t.Fatalf("private promise read: %v", err)
	}
	// Other operations, and reads, never carry one.
	if r := run(t, s, Command{Operation: "messages.list", Room: "lobby"}); r.LogPromise != nil {
		t.Fatal("a read carried a promise")
	}
	for _, bad := range []struct {
		message string
		index   int64
		code    string
	}{{"not-an-id", -1, "invalid_request"}, {"", -1, "invalid_request"}, {strings.Repeat("0", 32), -1, "not_found"}, {"", 999, "not_found"}} {
		if _, err = s.ReadLogPromise(testContext, bad.message, bad.index); promiseCode(err) != bad.code {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	// Kept once a checkpoint covers it, with the leaf's proof.
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	st, err = s.ReadLogPromise(testContext, res.Receipt.ID, -1)
	if err != nil || st.State != PromiseKept || st.Proof == nil || st.Proof.Leaf.Index != idx || st.Proof.Leaf.Hash != lp.LeafHash {
		t.Fatalf("kept: %+v %v", st, err)
	}
	verifyInclusion(t, s, *st.Proof, verifyCheckpoint(t, s, st.Proof.Checkpoint))
	// A hide appends a moderation leaf; the promise was of inclusion, not
	// display, and stays kept.
	if err = s.Moderate(testContext, res.Receipt.ID, "test hide", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	if st, err = s.ReadLogPromise(testContext, res.Receipt.ID, -1); err != nil || st.State != PromiseKept || st.Note != lp.Note {
		t.Fatalf("after a hide: %+v %v", st, err)
	}
	// Promises are append-only.
	if _, err = s.db.Exec("UPDATE tlog_promises SET merge_by=0"); err == nil {
		t.Fatal("a promise was rewritten")
	}
	if _, err = s.db.Exec("DELETE FROM tlog_promises"); err == nil {
		t.Fatal("a promise was deleted")
	}
}

// An anonymous public post is promised too, and its exact retry is not.
func TestLogPromiseAnonymousRetry(t *testing.T) {
	s := openTest(t, Config{})
	cmd := Command{Operation: "post", Room: "lobby", Text: "anonymous promised post", RequestID: "promise-anon-1"}
	first := run(t, s, cmd)
	if first.LogPromise == nil {
		t.Fatal("no promise on an anonymous public post")
	}
	again := run(t, s, cmd)
	if again.LogPromise != nil || !again.Receipt.Duplicate {
		t.Fatalf("retry: %+v", again)
	}
	if st, err := s.ReadLogPromise(testContext, first.Receipt.ID, -1); err != nil || st.Note != first.LogPromise.Note {
		t.Fatalf("stored: %+v %v", st, err)
	}
}

// Overdue: no checkpoint covers the leaf after merge-by. The merge delay is
// the configured one, fixed at issue.
func TestLogPromiseOverdue(t *testing.T) {
	s := openTest(t, Config{})
	s.transparency.mergeDelay.Store(600)
	now := int64(1_800_000_000)
	s.now = func() time.Time { return time.Unix(now, 0) }
	res := run(t, s, Command{Operation: "post", Room: "lobby", Text: "late post"})
	if res.LogPromise == nil || res.LogPromise.MergeBy != now+600 {
		t.Fatalf("promise: %+v", res.LogPromise)
	}
	s.transparency.mergeDelay.Store(3600)
	if st, err := s.ReadLogPromise(testContext, res.Receipt.ID, -1); err != nil || st.State != PromisePending || st.MergeBy != now+600 {
		t.Fatalf("pending: %+v %v", st, err)
	}
	now += 601
	if st, err := s.ReadLogPromise(testContext, res.Receipt.ID, -1); err != nil || st.State != PromiseOverdue {
		t.Fatalf("overdue: %+v %v", st, err)
	}
}

// A promise under the log key for another leaf than the checkpoint holds at
// its index is broken: the route says so, and verify_log.py reports it with
// evidence; the genuine promise from the same log verifies as kept. Both are
// Go-signed fixtures checked by the published Python verifier.
func TestLogPromiseBrokenEvidence(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	author := keyFor(72)
	run(t, s, signed(author, Command{Operation: "agent.register", Handle: "promise-agent"}))
	res := run(t, s, signed(author, Command{Operation: "post", Room: "lobby", Text: "kept post"}))
	if res.LogPromise == nil {
		t.Fatal("no promise")
	}
	// Forge: the log key promises leaf 0 (the agent's identity leaf) is a
	// message with another leaf hash.
	var data string
	if err := s.db.QueryRow("SELECT data FROM tlog_leaves WHERE idx=0").Scan(&data); err != nil || strings.Contains(data, `"kind":"message"`) {
		t.Fatalf("leaf 0: %s %v", data, err)
	}
	forged := tlog.Promise{Origin: s.transparency.origin, Index: 0, Leaf: tlog.LeafHash([]byte("another leaf")), Kind: "message", ID: strings.Repeat("ab", 16), Received: testTime, MergeBy: testTime + 1800}
	note, err := s.transparency.signer.Sign(forged.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO tlog_promises(idx,note,merge_by,created_at) VALUES(0,?,?,?)", note, forged.MergeBy, testTime); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	st, err := s.ReadLogPromise(testContext, "", 0)
	if err != nil || st.State != PromiseBroken || st.Proof == nil {
		t.Fatalf("forged promise: %+v %v", st, err)
	}
	python, err := exec.LookPath("python3")
	if err != nil || exec.Command(python, "-c", "import cryptography").Run() != nil {
		t.Skip("python3 with cryptography is not available")
	}
	dir := t.TempDir()
	write := func(name string, v any) string {
		path := filepath.Join(dir, name)
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	verify := func(args ...string) (string, int) {
		cmd := exec.Command(python, append([]string{"-B", filepath.Join("..", "..", "clients", "python", "verify_log.py"), "--base", "http://127.0.0.1:9", "--key", s.LogVerifierKey(), "promise"}, args...)...)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	// The genuine promise, as the post result carried it, with the saved proof.
	kept, err := s.ReadLogPromise(testContext, res.Receipt.ID, -1)
	if err != nil || kept.State != PromiseKept {
		t.Fatalf("genuine: %+v %v", kept, err)
	}
	out, code := verify(write("result.json", res), "--proof", write("kept-proof.json", kept.Proof))
	if code != 0 || !strings.Contains(out, "OK kept: leaf ") {
		t.Fatalf("kept: %d\n%s", code, out)
	}
	evidence := filepath.Join(dir, "evidence.json")
	out, code = verify(write("forged.json", st), "--proof", write("forged-proof.json", st.Proof), "--evidence", evidence)
	if code != 3 || !strings.Contains(out, "BROKEN checkpoint ") {
		t.Fatalf("broken: %d\n%s", code, out)
	}
	raw, err := os.ReadFile(evidence)
	if err != nil || !strings.Contains(string(raw), "swarmmemo-promise-violation/1") {
		t.Fatalf("evidence: %s %v", raw, err)
	}
	// A note altered after signing fails outright.
	tampered := filepath.Join(dir, "tampered.txt")
	if err = os.WriteFile(tampered, []byte(strings.Replace(res.LogPromise.Note, "kind message", "kind messagf", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code = verify(tampered, "--proof", filepath.Join(dir, "kept-proof.json")); code != 1 {
		t.Fatalf("tampered: %d\n%s", code, out)
	}
}
