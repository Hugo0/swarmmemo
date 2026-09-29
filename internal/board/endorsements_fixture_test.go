package board

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/endorsement_export.jsonl is a real export (a legacy vote, votes and
// vouches) that scripts/trust/recompute.py also verifies, with its own Ed25519
// code: the Go and Python verifiers must agree on it.
// UPDATE_ENDORSEMENT_FIXTURE=1 regenerates it (post ids are random, so the
// file changes each time).
func TestEndorsementExportFixture(t *testing.T) {
	path := filepath.Join("testdata", "endorsement_export.jsonl")
	if os.Getenv("UPDATE_ENDORSEMENT_FIXTURE") == "1" {
		writeEndorsementFixture(t, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r EndorsementRecord
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		types = append(types, r.Type)
		if r.Type == "legacy_vote" {
			continue
		}
		if err := VerifyEndorsementRecord("swarmmemo.com", r); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
	}
	if strings.Join(types, ",") != "legacy_vote,vote,vote,vouch,vouch,vouch" {
		t.Fatalf("fixture records: %v", types)
	}
}

func writeEndorsementFixture(t *testing.T, path string) {
	t.Helper()
	s := openTest(t, Config{})
	alice, bob, carol := keyFor(101), keyFor(102), keyFor(103)
	post := postAs(t, s, alice, Command{Room: "lobby", Text: "alice", RequestID: "a1", Nonce: "n-a1"})
	seasoned(t, s, bob, carol)
	vote := func(k ed25519.PrivateKey, value, nonce string) {
		if _, err := s.Execute(testContext, signed(k, Command{Operation: "vote", MessageID: post, Data: `{"value":` + value + `}`, Nonce: nonce}), "test-origin"); err != nil {
			t.Fatal(err)
		}
	}
	vote(bob, "1", "n-legacy")
	s.config.Features = Features{VoteRecords: true, ExportEndorsements: true}
	vote(carol, "1", "n-c1")
	vote(carol, "-1", "n-c2")
	for i, v := range []struct {
		from ed25519.PrivateKey
		data string
	}{{bob, `{"schema":1,"value":1,"sponsor":true}`}, {carol, `{"schema":1,"value":1}`}, {carol, `{"schema":1,"value":0}`}} {
		if _, err := s.Execute(testContext, signed(v.from, Command{Operation: "vouch", Target: keyID(alice), Data: v.data, Nonce: fmt.Sprintf("n-v%d", i)}), "test-origin"); err != nil {
			t.Fatal(err)
		}
	}
	var buf strings.Builder
	for _, r := range exportAll(t, s, 1000) {
		raw, _ := json.Marshal(r)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
