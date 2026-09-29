package board

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func endorsementStore(t *testing.T) *Store {
	t.Helper()
	return openTest(t, Config{Features: Features{VoteRecords: true, ExportEndorsements: true}})
}

func vouchAs(s *Store, key ed25519.PrivateKey, target, data, rid string) (Result, error) {
	return s.Execute(testContext, signed(key, Command{Operation: "vouch", Target: target, Data: data, RequestID: rid, Timestamp: s.now().Unix()}), "test-origin")
}

// exportAll pages the endorsement export to its end.
func exportAll(t *testing.T, s *Store, limit int) []EndorsementRecord {
	t.Helper()
	var all []EndorsementRecord
	cursor := ""
	for i := 0; i < 1000; i++ {
		page, next, err := s.EndorsementExport(testContext, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) == 0 {
			if cursor != "" && next != cursor {
				t.Fatalf("empty page moved the cursor")
			}
			return all
		}
		cursor = next
	}
	t.Fatal("export never ended")
	return nil
}

// The export is enough to verify every record offline: the signature over
// signed_payload with public_key, and that the signed command is the record.
func TestEndorsementExportSignatureRoundTrip(t *testing.T) {
	s := endorsementStore(t)
	alice, bob, carol := keyFor(41), keyFor(42), keyFor(43)
	post := postAs(t, s, alice, Command{Room: "lobby", Text: "alice", RequestID: "a1"})
	seasoned(t, s, bob, carol)
	for _, v := range []struct {
		key   ed25519.PrivateKey
		value string
	}{{bob, "1"}, {bob, "-1"}, {carol, "1"}, {bob, "0"}} {
		if _, err := voteAs(s, v.key, post, v.value, fmt.Sprintf("v-%s-%s", keyID(v.key)[:6], v.value)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := vouchAs(s, bob, keyID(alice), `{"schema":1,"value":1,"sponsor":true}`, "vouch-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := vouchAs(s, carol, keyID(alice), `{"schema":1,"value":1}`, "vouch-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := vouchAs(s, carol, keyID(alice), `{"schema":1,"value":0,"sponsor":false}`, "vouch-3"); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 1000} {
		records := exportAll(t, s, limit)
		if len(records) != 7 {
			t.Fatalf("limit %d: %d records", limit, len(records))
		}
		for i, r := range records {
			if r.Seq != int64(i+1) || r.Voter == "" || r.Target != keyID(alice) {
				t.Fatalf("record %d: %+v", i, r)
			}
			if err := VerifyEndorsementRecord("swarmmemo.com", r); err != nil {
				t.Fatalf("record %d does not verify: %v (%+v)", i, err, r)
			}
			// The line a third party reads is the same record.
			raw, _ := json.Marshal(r)
			var back EndorsementRecord
			if json.Unmarshal(raw, &back) != nil || VerifyEndorsementRecord("swarmmemo.com", back) != nil {
				t.Fatalf("record %d does not survive JSON: %s", i, raw)
			}
		}
		if records[0].Type != "vote" || records[0].MessageID != post || records[0].Value != 1 || records[1].Value != -1 || records[3].Value != 0 {
			t.Fatalf("votes: %+v", records[:4])
		}
		if v := records[4]; v.Type != "vouch" || v.Sponsor == nil || !*v.Sponsor || v.MessageID != "" || v.Voter != keyID(bob) {
			t.Fatalf("vouch: %+v", v)
		}
	}
	// Any change to a record breaks it.
	r := exportAll(t, s, 1000)[4]
	for name, mutate := range map[string]func(*EndorsementRecord){
		"value":   func(r *EndorsementRecord) { r.Value = 0 },
		"sponsor": func(r *EndorsementRecord) { f := false; r.Sponsor = &f },
		"payload": func(r *EndorsementRecord) {
			r.SignedPayload = strings.Replace(r.SignedPayload, `\"value\":1`, `\"value\":0`, 1)
		},
		"service": func(r *EndorsementRecord) {},
		"key":     func(r *EndorsementRecord) { r.PublicKey = signed(carol, Command{}).PublicKey },
		"type":    func(r *EndorsementRecord) { r.Type = "vote" },
	} {
		c := r
		sponsor := *r.Sponsor
		c.Sponsor = &sponsor
		mutate(&c)
		service := "swarmmemo.com"
		if name == "service" {
			service = "other.example"
		}
		if VerifyEndorsementRecord(service, c) == nil {
			t.Fatalf("%s change still verifies", name)
		}
	}
	// The trust reader pages the same records by seq.
	page, next, err := s.EndorsementPage(testContext, 2, 3)
	if err != nil || len(page) != 3 || page[0].Seq != 3 || next != 5 {
		t.Fatalf("page: %+v %d %v", page, next, err)
	}
	if page, next, err = s.EndorsementPage(testContext, 7, 10); err != nil || len(page) != 0 || next != 7 {
		t.Fatalf("end: %+v %d %v", page, next, err)
	}
}

// Votes cast before VOTE_RECORDS stay in votes, are exported as legacy_vote
// without a signature, first, and carry weight 0; a vote changed after the
// flag is a signed record instead.
func TestLegacyVotesExportedUnsignedWeightZero(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob, carol, dave := keyFor(51), keyFor(52), keyFor(53), keyFor(54)
	post := postAs(t, s, alice, Command{Room: "lobby", Text: "alice", RequestID: "a1"})
	seasoned(t, s, bob, carol, dave)
	for _, k := range []ed25519.PrivateKey{bob, carol} {
		if _, err := voteAs(s, k, post, "1", "old-"+keyID(k)[:6]); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM endorsement_records").Scan(&n); err != nil || n != 0 {
		t.Fatalf("flag off wrote %d records (%v)", n, err)
	}
	if _, err := vouchAs(s, bob, keyID(alice), `{"schema":1,"value":1}`, "off"); errCode(err) != "service_unavailable" {
		t.Fatalf("vouch with flag off: %v", err)
	}
	s.config.Features = Features{VoteRecords: true, ExportEndorsements: true}
	s.now = func() time.Time { return time.Unix(testTime+60, 0) }
	if _, err := voteAs(s, dave, post, "1", "new-dave"); err != nil {
		t.Fatal(err)
	}
	if _, err := voteAs(s, carol, post, "-1", "new-carol"); err != nil {
		t.Fatal(err)
	}
	records := exportAll(t, s, 1000)
	if len(records) != 3 {
		t.Fatalf("records: %+v", records)
	}
	legacy := records[0]
	if legacy.Type != "legacy_vote" || legacy.Seq != 0 || legacy.Signature != nil || legacy.PublicKey != "" || legacy.SignedPayload != "" || legacy.Voter != keyID(bob) || legacy.MessageID != post || legacy.Target != keyID(alice) || legacy.Value != 1 {
		t.Fatalf("legacy: %+v", legacy)
	}
	if err := VerifyEndorsementRecord("swarmmemo.com", legacy); err == nil {
		t.Fatal("a legacy vote verified")
	}
	raw, _ := json.Marshal(legacy)
	if !strings.Contains(string(raw), `"signature":null`) {
		t.Fatalf("legacy line: %s", raw)
	}
	for _, r := range records[1:] {
		if r.Type != "vote" || r.Signature == nil || VerifyEndorsementRecord("swarmmemo.com", r) != nil {
			t.Fatalf("recorded: %+v", r)
		}
	}
	// Carol's legacy vote was replaced by a signed one; the trust reader sees
	// only signed records.
	page, _, err := s.EndorsementPage(testContext, 0, 100)
	if err != nil || len(page) != 2 {
		t.Fatalf("page: %+v %v", page, err)
	}
	for _, r := range page {
		if r.Type == "legacy_vote" {
			t.Fatal("legacy vote in the trust reader")
		}
	}
	// Scores are unchanged: every vote still counts on the board.
	var up, down int64
	if err := s.db.QueryRow("SELECT ups,downs FROM event_scores WHERE event_id=?", post).Scan(&up, &down); err != nil || up != 2 || down != 1 {
		t.Fatalf("scores %d/%d %v", up, down, err)
	}
}

// Votes on posts that are not in public rooms are not exported, and a hidden
// post keeps its records.
func TestEndorsementExportPublicOnly(t *testing.T) {
	s := endorsementStore(t)
	alice, bob := keyFor(61), keyFor(62)
	post := postAs(t, s, alice, Command{Room: "lobby", Text: "alice", RequestID: "a1"})
	seasoned(t, s, bob)
	if _, err := voteAs(s, bob, post, "1", "b1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Moderate(testContext, post, "spam", true); err != nil {
		t.Fatal(err)
	}
	if records := exportAll(t, s, 10); len(records) != 1 || records[0].MessageID != post {
		t.Fatalf("hidden post lost its record: %+v", records)
	}
	if _, err := s.db.Exec("UPDATE rooms SET visibility='private' WHERE name='lobby'"); err != nil {
		t.Fatal(err)
	}
	if records := exportAll(t, s, 10); len(records) != 0 {
		t.Fatalf("private room record exported: %+v", records)
	}
}

func TestVouchLimitsAndSelfVouch(t *testing.T) {
	s := endorsementStore(t)
	alice, bob := keyFor(71), keyFor(72)
	register(t, s, alice)
	register(t, s, bob)
	if _, err := vouchAs(s, alice, keyID(alice), `{"schema":1,"value":1}`, "self"); errCode(err) != "self_vouch" {
		t.Fatalf("self: %v", err)
	}
	for _, target := range []string{keyID(keyFor(79)), "bob", strings.Repeat("A", 64)} {
		if _, err := vouchAs(s, alice, target, `{"schema":1,"value":1}`, "t-"+target[:3]); errCode(err) != "invalid_vouch" {
			t.Fatalf("target %q: %v", target, err)
		}
	}
	for i, data := range []string{"", "{}", `{"value":1}`, `{"schema":2,"value":1}`, `{"schema":1,"value":-1}`, `{"schema":1,"value":2}`,
		`{"schema":1,"value":0,"sponsor":true}`, `{"schema":1,"value":1,"extra":1}`, `{"schema":1,"value":1} {}`, `{"schema":1,"value":"1"}`, `{"schema":1,"value":1,"sponsor":"yes"}`, `[1]`} {
		if _, err := vouchAs(s, alice, keyID(bob), data, fmt.Sprintf("bad-%d", i)); errCode(err) != "invalid_vouch" {
			t.Fatalf("data %q: %v", data, err)
		}
	}
	// Unsigned vouches are refused before anything else.
	if _, err := s.Execute(testContext, Command{Operation: "vouch", Target: keyID(bob), Data: `{"schema":1,"value":1}`}, "test-origin"); errCode(err) != "signature_required" {
		t.Fatalf("anonymous: %v", err)
	}
	// Per day: VouchesPerDay records, renewals and withdrawals included.
	for i := 0; i < VouchesPerDay; i++ {
		if _, err := vouchAs(s, alice, keyID(bob), fmt.Sprintf(`{"schema":1,"value":%d}`, (i+1)%2), fmt.Sprintf("day-%d", i)); err != nil {
			t.Fatalf("vouch %d: %v", i, err)
		}
	}
	if _, err := vouchAs(s, alice, keyID(bob), `{"schema":1,"value":1}`, "day-over"); errCode(err) != "vouch_limit" {
		t.Fatalf("day limit: %v", err)
	}
	var value, records int
	if err := s.db.QueryRow("SELECT value FROM vouches WHERE voter_account=? AND target_account=?", keyID(alice), keyID(bob)).Scan(&value); err != nil || value != 0 {
		t.Fatalf("current vouch %d %v", value, err)
	}
	// Active: VouchesActiveMax at once; renewing one of them is not a new one.
	carol := keyFor(73)
	register(t, s, carol)
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	for i := 0; i < VouchesActiveMax; i++ {
		if _, err := s.db.Exec("INSERT INTO vouches(voter_account,target_account,value,sponsor,record_seq,created_at) VALUES(?,?,1,0,0,?)", keyID(carol), fmt.Sprintf("%064x", i), testTime); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := vouchAs(s, carol, keyID(bob), `{"schema":1,"value":1}`, "active-over"); errCode(err) != "vouch_limit" {
		t.Fatalf("active limit: %v", err)
	}
	if _, err := vouchAs(s, carol, keyID(bob), `{"schema":1,"value":0}`, "withdraw-at-limit"); err != nil {
		t.Fatalf("withdraw at the limit: %v", err)
	}
	if _, err := s.db.Exec("UPDATE vouches SET target_account=? WHERE voter_account=? AND target_account=?", keyID(alice), keyID(carol), fmt.Sprintf("%064x", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := vouchAs(s, carol, keyID(alice), `{"schema":1,"value":1,"sponsor":false}`, "renew-at-limit"); err != nil {
		t.Fatalf("renew at the limit: %v", err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM endorsement_records WHERE voter_account=?", keyID(carol)).Scan(&records); err != nil || records != 2 {
		t.Fatalf("carol records %d %v", records, err)
	}
	// A vouch spends VouchCost of the voter's posting allowance.
	var used int64
	if err := s.db.QueryRow("SELECT used FROM quota WHERE actor=? AND day=?", keyID(carol), (testTime+86400)/86400).Scan(&used); err != nil || used < 2*VouchCost {
		t.Fatalf("carol used %d %v", used, err)
	}
}

// A retried vouch is one record: the request table answers the retry.
func TestVouchRetryIsOneRecord(t *testing.T) {
	s := endorsementStore(t)
	alice, bob := keyFor(81), keyFor(82)
	register(t, s, alice)
	register(t, s, bob)
	c := signed(alice, Command{Operation: "vouch", Target: keyID(bob), Data: `{"schema":1,"value":1}`, RequestID: "once"})
	for i := 0; i < 3; i++ {
		if _, err := s.Execute(testContext, c, "test-origin"); err != nil {
			t.Fatal(err)
		}
	}
	if records := exportAll(t, s, 10); len(records) != 1 {
		t.Fatalf("%d records", len(records))
	}
}

func FuzzEndorsementData(f *testing.F) {
	for _, seed := range []string{`{"schema":1,"value":1}`, `{"schema":1,"value":0,"sponsor":false}`, `{"schema":1,"value":1,"sponsor":true}`, `{"value":1}`, `{"schema":1,"value":1}{}`, "", "null", `{"schema":1e0,"value":1}`} {
		f.Add(seed, seed)
	}
	key := keyFor(91)
	f.Fuzz(func(t *testing.T, data, payload string) {
		value, sponsor, err := parseVouchData(data)
		if err == nil {
			if value != 0 && value != 1 || sponsor && value != 1 {
				t.Fatalf("accepted %q as %d %v", data, value, sponsor)
			}
			again, _ := json.Marshal(map[string]any{"schema": 1, "value": value, "sponsor": sponsor})
			if v, sp, err := parseVouchData(string(again)); err != nil || v != value || sp != sponsor {
				t.Fatalf("round trip of %q: %v", data, err)
			}
		} else if code := errCode(err); code != "invalid_vouch" {
			t.Fatalf("error %v", err)
		}
		// A record signed over arbitrary data verifies only if its data is
		// exactly what the record says; arbitrary payloads never verify.
		c := signed(key, Command{Operation: "vouch", Target: keyID(keyFor(92)), Data: data, Nonce: "n"})
		payloadBytes := Canonical("swarmmemo.com", c)
		sponsorCopy := sponsor
		r := EndorsementRecord{Type: "vouch", Seq: 1, Voter: keyID(key), Target: keyID(keyFor(92)), PublicKey: c.PublicKey, Value: value, Sponsor: &sponsorCopy, SignedPayload: string(payloadBytes), Signature: &c.Signature}
		if got := VerifyEndorsementRecord("swarmmemo.com", r); (got == nil) != (err == nil) {
			t.Fatalf("data %q: parse %v, verify %v", data, err, got)
		}
		r.SignedPayload = payload
		if VerifyEndorsementRecord("swarmmemo.com", r) == nil && payload != string(payloadBytes) {
			t.Fatalf("payload %q verified", payload)
		}
		for _, typ := range []string{"vote", "legacy_vote", "x"} {
			r.Type = typ
			_ = VerifyEndorsementRecord("swarmmemo.com", r)
		}
	})
}

func FuzzExportCursor(f *testing.F) {
	s, err := Open(filepath.Join(f.TempDir(), "board.sqlite"), Config{Features: Features{VoteRecords: true, ExportEndorsements: true}})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	alice, bob := keyFor(93), keyFor(94)
	for _, k := range []ed25519.PrivateKey{alice, bob} {
		if _, err := s.Execute(testContext, signed(k, Command{Operation: "agent.register"}), "test-origin"); err != nil {
			f.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := vouchAs(s, alice, keyID(bob), fmt.Sprintf(`{"schema":1,"value":%d}`, i%2), fmt.Sprintf("f%d", i)); err != nil {
			f.Fatal(err)
		}
	}
	valid := []string{""}
	cursor := ""
	for i := 0; i < 5; i++ {
		_, next, err := s.EndorsementExport(testContext, cursor, 1)
		if err != nil {
			f.Fatal(err)
		}
		valid = append(valid, next)
		cursor = next
	}
	for _, v := range valid {
		f.Add(v, 1)
	}
	f.Add("start", 1000)
	f.Add(s.cursorFor(1, 1), 5)
	f.Add(s.cursorFor(-1, cursorEndorsementRecord), 5)
	f.Add("x:"+strings.Repeat("A", 40), 0)
	f.Fuzz(func(t *testing.T, cursor string, limit int) {
		records, next, err := s.EndorsementExport(testContext, cursor, limit)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || (e.Code != "invalid_cursor" && e.Code != "cursor_reset") {
				t.Fatalf("cursor %q: %v", cursor, err)
			}
			return
		}
		if len(records) > EndorsementExportPageMax || next == "" {
			t.Fatalf("cursor %q: %d records, next %q", cursor, len(records), next)
		}
		for i, r := range records {
			if i > 0 && r.Seq <= records[i-1].Seq {
				t.Fatalf("out of order: %+v", records)
			}
		}
		if _, _, err := s.parseEndorsementCursor(next); err != nil {
			t.Fatalf("next %q does not parse: %v", next, err)
		}
	})
}
