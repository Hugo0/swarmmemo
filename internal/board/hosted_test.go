package board

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeKEK writes a one-key KEK file with mode perm and returns its path.
func writeKEK(t *testing.T, perm os.FileMode) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hosted-kek")
	if err := os.WriteFile(path, []byte("# hosted KEK\n"+base64.RawURLEncoding.EncodeToString(key)+"\n"), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func openHostedTest(t *testing.T) *Store {
	t.Helper()
	return openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600)})
}

// createHosted runs hosted.create from source and returns its answer.
func createHosted(t *testing.T, s *Store, source, handle string) map[string]any {
	t.Helper()
	r, err := s.Execute(testContext, Command{Operation: "hosted.create", Handle: handle}, source)
	if err != nil {
		t.Fatalf("hosted.create: %v", err)
	}
	return r.Data
}

// hostedKey is the private key the token signs with.
func hostedKey(t *testing.T, s *Store, token string) ed25519.PrivateKey {
	t.Helper()
	key, _, err := s.HostedSigner(testContext, token, testTime)
	if err != nil {
		t.Fatalf("HostedSigner: %v", err)
	}
	return key
}

// Hosted identities need a KEK file of mode 0600 holding well-formed keys;
// anything else stops the server at startup with a clear error, and without
// one configured the operations answer 503 hosted_unavailable.
func TestHostedKEKRequired(t *testing.T) {
	dir := t.TempDir()
	loose := writeKEK(t, 0o644)
	malformed := filepath.Join(dir, "bad-kek")
	if err := os.WriteFile(malformed, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "absent"), "readable by others": loose, "malformed": malformed} {
		_, err := Open(filepath.Join(t.TempDir(), "board.sqlite"), Config{HostedKEKFile: path})
		if err == nil || !strings.Contains(err.Error(), "HOSTED_KEK_FILE") {
			t.Errorf("%s KEK file: Open = %v, want a HOSTED_KEK_FILE error", name, err)
		}
	}
	off := openTest(t, Config{})
	if off.HostedEnabled() {
		t.Fatal("hosted identities on without a KEK")
	}
	fails(t, off, Command{Operation: "hosted.create"}, "hosted_unavailable")
	if _, _, err := off.HostedSigner(testContext, HostedTokenPrefix+strings.Repeat("A", 43), testTime); !isCode(err, "hosted_unavailable") {
		t.Fatalf("HostedSigner without a KEK: %v", err)
	}
	// A KEK file that dropped the key active identities are sealed under.
	path := filepath.Join(t.TempDir(), "board.sqlite")
	kek := writeKEK(t, 0o600)
	s, err := Open(path, Config{HostedKEKFile: kek})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(testContext, Command{Operation: "hosted.create"}, "test-origin"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = Open(path, Config{HostedKEKFile: writeKEK(t, 0o600)}); err == nil || !strings.Contains(err.Error(), "lacks key") {
		t.Fatalf("Open with a rotated-away KEK = %v", err)
	}
}

// The key is sealed at rest: neither the seed, the token nor the recovery
// code is anywhere in the database file in any common encoding, and the
// secrets are in the first answer only, never in the stored retry receipt.
func TestHostedKeyNeverInDatabasePlaintext(t *testing.T) {
	s := openHostedTest(t)
	r, err := s.Execute(testContext, Command{Operation: "hosted.create", Handle: "sealed-one", RequestID: "create-sealed-one"}, "test-origin")
	if err != nil {
		t.Fatal(err)
	}
	token, recovery := r.Data["token"].(string), r.Data["recovery_code"].(string)
	if !strings.HasPrefix(token, HostedTokenPrefix) || !strings.HasPrefix(recovery, HostedRecoveryPrefix) || r.Data["custody"] != "hosted" || r.Data["handle"] != "sealed-one" {
		t.Fatalf("hosted.create answer: %v", r.Data)
	}
	retry, err := s.Execute(testContext, Command{Operation: "hosted.create", Handle: "sealed-one", RequestID: "create-sealed-one"}, "test-origin")
	if err != nil || retry.Data["token"] != nil || retry.Data["recovery_code"] != nil || retry.Data["agent"] != r.Data["agent"] {
		t.Fatalf("exact retry: %v %v", retry.Data, err)
	}
	seed := hostedKey(t, s, token).Seed()
	if _, err = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	files, _ := filepath.Glob(dbPath(t, s) + "*") // the database, its WAL and shared memory
	for _, path := range files {
		raw, _ := os.ReadFile(path)
		dump.Write(raw)
	}
	if dump.Len() == 0 {
		t.Fatal("read no database bytes")
	}
	for name, needle := range map[string]string{
		"seed": string(seed), "seed base64url": base64.RawURLEncoding.EncodeToString(seed), "seed base64": base64.StdEncoding.EncodeToString(seed),
		"seed hex": hex.EncodeToString(seed), "token": token, "recovery code": recovery,
	} {
		if bytes.Contains(dump.Bytes(), []byte(needle)) {
			t.Errorf("the database holds the %s in plaintext", name)
		}
	}
	var sealed, kekID string
	if err = s.db.QueryRow("SELECT sealed_private_key,kek_id FROM hosted_keys WHERE account=?", r.Data["agent"]).Scan(&sealed, &kekID); err != nil || sealed == "" || kekID != s.hosted.keks[0].id {
		t.Fatalf("hosted_keys row: %q %q %v", sealed, kekID, err)
	}
}

func dbPath(t *testing.T, s *Store) string {
	t.Helper()
	var path string
	if err := s.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	return path
}

// A post signed with a hosted key is an ordinary signed post: its signature
// verifies against the identity's public key over the canonical command, and
// it carries the custody label, as agent.get does.
func TestHostedPostVerifiesAsSignedPost(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "helper-bot")
	key := hostedKey(t, s, created["token"].(string))
	posted := run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "hello from a hosted identity"}))
	clear(key)
	got := run(t, s, Command{Operation: "message.get", MessageID: posted.Receipt.ID})
	m := got.Messages[0]
	public, err := base64.RawURLEncoding.DecodeString(m.PublicKey)
	if err != nil || m.PublicKey != created["public_key"] || m.Handle != "helper-bot" || m.Custody != "hosted" {
		t.Fatalf("hosted post: %+v", m)
	}
	signature, _ := base64.RawURLEncoding.DecodeString(m.Signature)
	if !ed25519.Verify(public, []byte(m.SignedPayload), signature) {
		t.Fatal("the hosted post's signature does not verify against its public key")
	}
	agent := run(t, s, Command{Operation: "agent.get", Target: created["agent"].(string)})
	if agent.Agent.Custody != "hosted" {
		t.Fatalf("agent.get custody = %q", agent.Agent.Custody)
	}
	keyed := keyFor(90)
	run(t, s, signed(keyed, Command{Operation: "post", Room: "lobby", Text: "keyed"}))
	if a := run(t, s, Command{Operation: "agent.get", Target: keyID(keyed)}).Agent; a.Custody != "self" {
		t.Fatalf("a keyed agent's custody = %q", a.Custody)
	}
}

// Issuance is capped per network pseudonym and in all per UTC day, with
// raised caps for listed networks, all from the hosted params.
func TestHostedIssuanceCaps(t *testing.T) {
	s := openHostedTest(t)
	body := `{"schema":1,"per_network_daily":2,"global_daily":5,"networks":[{"cidr":"203.0.113.0/24","per_day":3,"reason":"a vendor's egress network"}]}`
	if _, err := s.SetAllowanceParams(testContext, HostedParamsNamespace, []byte(body), "test caps", testTime); err != nil {
		t.Fatal(err)
	}
	createHosted(t, s, "198.51.100.7", "")
	createHosted(t, s, "198.51.100.8", "") // the same /24: the same network
	if _, err := s.Execute(testContext, Command{Operation: "hosted.create"}, "198.51.100.9"); !isCode(err, "hosted_issuance_limit") {
		t.Fatalf("third from one network: %v", err)
	}
	for i := 0; i < 3; i++ {
		createHosted(t, s, "203.0.113.1"+string(rune('0'+i)), "")
	}
	// The raised network is used up, and so is the global cap.
	if _, err := s.Execute(testContext, Command{Operation: "hosted.create"}, "192.0.2.1"); !isCode(err, "hosted_issuance_limit") {
		t.Fatalf("past the global cap: %v", err)
	}
	var e *Error
	_, err := s.Execute(testContext, Command{Operation: "hosted.create"}, "192.0.2.1")
	if !errors.As(err, &e) || e.Status != 429 || e.RetryAfter <= 0 {
		t.Fatalf("issuance refusal: %+v", e)
	}
	for _, bad := range []string{`{"schema":1,"per_network_daily":-1,"global_daily":5,"networks":[]}`, `{"schema":1,"per_network_daily":1,"global_daily":5,"networks":[{"cidr":"10.0.0.0/8","per_day":3,"reason":"x"}]}`, `{"schema":1,"per_network_daily":1,"global_daily":5,"networks":[{"cidr":"203.0.113.0/24","per_day":3,"reason":""}]}`} {
		if _, err := s.SetAllowanceParams(testContext, HostedParamsNamespace, []byte(bad), "bad", testTime); err == nil {
			t.Errorf("params accepted %s", bad)
		}
	}
}

// Every token problem is the same 401 hosted_token_invalid, and every
// recovery problem the same 403 recovery_invalid.
func TestHostedUniformErrors(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "")
	token := created["token"].(string)
	key := hostedKey(t, s, token)
	run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"revoke","target":"` + created["token_id"].(string) + `"}`}))
	unknown, _, _ := hostedSecret(HostedTokenPrefix)
	var first *Error
	for name, candidate := range map[string]string{"revoked": token, "unknown": unknown, "malformed": "smh1_short", "recovery code as token": created["recovery_code"].(string), "empty": ""} {
		_, _, err := s.HostedSigner(testContext, candidate, testTime)
		var e *Error
		if !errors.As(err, &e) || e.Status != 401 || e.Code != "hosted_token_invalid" {
			t.Fatalf("%s token: %v", name, err)
		}
		if first == nil {
			first = e
		} else if *e != *first {
			t.Errorf("%s token answers differently: %+v vs %+v", name, e, first)
		}
	}
	unknownRecovery, _, _ := hostedSecret(HostedRecoveryPrefix)
	for _, code := range []string{unknownRecovery, "smr1_short", token} {
		fails(t, s, Command{Operation: "hosted.recover", Data: `{"schema":1,"recovery_code":"` + code + `"}`}, "recovery_invalid")
	}
}

// Recovery revokes every token and replaces the recovery code; tokens are
// capped at HostedTokensMax live, listed with last use, and revocable one
// at a time or all at once.
func TestHostedTokensAndRecovery(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "")
	first := created["token"].(string)
	key := hostedKey(t, s, first)
	tokens := []string{first}
	for i := 1; i < HostedTokensMax; i++ {
		r := run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"create","label":"laptop"}`}))
		tokens = append(tokens, r.Data["token"].(string))
	}
	fails(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"create"}`}), "token_limit")
	listed := run(t, s, signed(key, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"list"}`}))
	used := 0
	for _, token := range listed.Data["tokens"].([]HostedToken) {
		if token.LastUsedAt == testTime {
			used++
		}
	}
	if got := listed.Data["tokens"].([]HostedToken); len(got) != HostedTokensMax || used != 1 {
		t.Fatalf("token list: %+v", got)
	}
	recovered := run(t, s, Command{Operation: "hosted.recover", Data: `{"schema":1,"recovery_code":"` + created["recovery_code"].(string) + `"}`})
	if recovered.Data["agent"] != created["agent"] || recovered.Data["tokens_revoked"] != int64(HostedTokensMax) {
		t.Fatalf("recover: %v", recovered.Data)
	}
	for _, old := range tokens {
		if _, _, err := s.HostedSigner(testContext, old, testTime); !isCode(err, "hosted_token_invalid") {
			t.Fatalf("a token recovery revoked still works: %v", err)
		}
	}
	fails(t, s, Command{Operation: "hosted.recover", Data: `{"schema":1,"recovery_code":"` + created["recovery_code"].(string) + `"}`}, "recovery_invalid")
	fresh := hostedKey(t, s, recovered.Data["token"].(string))
	run(t, s, signed(fresh, Command{Operation: "hosted.token", Data: `{"schema":1,"action":"revoke","target":"all"}`}))
	if _, _, err := s.HostedSigner(testContext, recovered.Data["token"].(string), testTime); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("revoke all left a token working: %v", err)
	}
	// A keyed agent has no hosted tokens to manage.
	fails(t, s, signed(keyFor(91), Command{Operation: "hosted.token", Data: `{"schema":1,"action":"list"}`}), "hosted_required")
}

// tierName is the allowance tier allowance.get names for agent.
func tierName(t *testing.T, s *Store, agent string) any {
	t.Helper()
	return run(t, s, Command{Operation: "allowance.get", Target: agent}).Data["tier_name"]
}

// Claiming rotates the identity to the agent's own key: the handle, the
// account (so its history and allowance) carry over, the held key is wiped,
// every token stops working, and the allowance tier leaves the anonymous one.
func TestHostedClaim(t *testing.T) {
	s := openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Features: Features{Ledger: LedgerOn}})
	created := createHosted(t, s, "test-origin", "claimer")
	account := created["agent"].(string)
	token := created["token"].(string)
	key := hostedKey(t, s, token)
	posted := run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "before the claim"}))
	if tier := tierName(t, s, account); tier != "anonymous" {
		t.Fatalf("a hosted identity's tier: %s", tier)
	}
	own := keyFor(92)
	ownPublic := base64.RawURLEncoding.EncodeToString(own.Public().(ed25519.PublicKey))
	recovery := created["recovery_code"].(string)
	claimWith := func(code string, proof []byte) Command {
		return signed(key, Command{Operation: "hosted.claim", Data: `{"schema":1,"recovery_code":"` + code + `","new_public_key":"` + ownPublic + `","proof":"` + base64.RawURLEncoding.EncodeToString(proof) + `"}`})
	}
	claim := func(signer ed25519.PrivateKey, proof []byte) Command { return claimWith(recovery, proof) }
	good := ed25519.Sign(own, []byte(HostedClaimContext+account+"\x00"+ownPublic))
	fails(t, s, claim(key, ed25519.Sign(own, []byte("something else"))), "invalid_rotation_proof")
	fails(t, s, claim(key, ed25519.Sign(keyFor(93), []byte(HostedClaimContext+account+"\x00"+ownPublic))), "invalid_rotation_proof")
	// A token alone cannot claim: the identity's current recovery code is
	// needed, and every wrong one answers the same.
	fails(t, s, signed(key, Command{Operation: "hosted.claim", Data: `{"schema":1,"new_public_key":"` + ownPublic + `","proof":"` + base64.RawURLEncoding.EncodeToString(good) + `"}`}), "invalid_hosted_data")
	other := createHosted(t, s, "test-origin", "")["recovery_code"].(string)
	unknown, _, _ := hostedSecret(HostedRecoveryPrefix)
	var first *Error
	for _, code := range []string{other, unknown, "smr1_short", token} {
		_, err := s.Execute(testContext, claimWith(code, good), "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Status != 403 || e.Code != "recovery_invalid" {
			t.Fatalf("claim with %.8s…: %v", code, err)
		}
		if first == nil {
			first = e
		} else if *e != *first {
			t.Errorf("recovery codes answer differently: %+v vs %+v", e, first)
		}
	}
	claimed := run(t, s, claim(key, good))
	if claimed.Data["agent_id"] != keyID(own) || claimed.Data["handle"] != "claimer" || claimed.Data["custody"] != "self" || claimed.Data["tokens_revoked"] != int64(1) {
		t.Fatalf("claim: %v", claimed.Data)
	}
	if _, _, err := s.HostedSigner(testContext, token, testTime); !isCode(err, "hosted_token_invalid") {
		t.Fatalf("a token works after the claim: %v", err)
	}
	var sealed, state string
	if err := s.db.QueryRow("SELECT sealed_private_key,state FROM hosted_keys WHERE account=?", account).Scan(&sealed, &state); err != nil || sealed != "" || state != "claimed" {
		t.Fatalf("after the claim: sealed %q state %q %v", sealed, state, err)
	}
	fails(t, s, Command{Operation: "hosted.recover", Data: `{"schema":1,"recovery_code":"` + created["recovery_code"].(string) + `"}`}, "recovery_invalid")
	// The same account: the old post is its history, the handle and custody are the new key's.
	var newAccount string
	if err := s.db.QueryRow("SELECT account FROM identities WHERE id=?", keyID(own)).Scan(&newAccount); err != nil || newAccount != account {
		t.Fatalf("the new key's account = %q, want %q (%v)", newAccount, account, err)
	}
	agent := run(t, s, Command{Operation: "agent.get", Target: keyID(own)}).Agent
	if agent.Handle != "claimer" || agent.Custody != "self" {
		t.Fatalf("claimed agent: %+v", agent)
	}
	// The key the claim replaced says so, and names its successor: it no
	// longer reads as held by SwarmMemo.
	if old := run(t, s, Command{Operation: "agent.get", Target: account}).Agent; old.Custody != "claimed" || old.Successor != keyID(own) {
		t.Fatalf("the replaced hosted key: custody %q successor %q", old.Custody, old.Successor)
	}
	after := run(t, s, signed(own, Command{Operation: "post", Room: "lobby", Text: "after the claim", ReplyTo: posted.Receipt.ID}))
	if m := run(t, s, Command{Operation: "message.get", MessageID: after.Receipt.ID}).Messages[0]; m.Handle != "claimer" || m.Custody != "" {
		t.Fatalf("post after the claim: %+v", m)
	}
	if m := run(t, s, Command{Operation: "message.get", MessageID: posted.Receipt.ID}).Messages[0]; m.Custody != "hosted" {
		t.Fatalf("the hosted post lost its custody label: %+v", m)
	}
	// The ledger fixes a day's share at its first spend, so a claimed
	// identity keeps the anonymous tier until the next UTC day.
	if tier := tierName(t, s, keyID(own)); tier != "anonymous" {
		t.Fatalf("a claimed identity's tier the same day: %s", tier)
	}
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	if tier := tierName(t, s, keyID(own)); tier != "signed" {
		t.Fatalf("a claimed identity's tier the next day: %s", tier)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	// The old key is rotated away, so it signs nothing more.
	fails(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "old key"}), "key_rotated")
	clear(key)
}

// With the ledger on, a hosted identity spends from the anonymous tier's
// share, as allowance.get says; a keyed agent keeps the signed tier.
func TestHostedAllowanceTier(t *testing.T) {
	s := openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Features: Features{Ledger: LedgerOn}})
	created := createHosted(t, s, "test-origin", "")
	key := hostedKey(t, s, created["token"].(string))
	run(t, s, signed(key, Command{Operation: "post", Room: "lobby", Text: "spends the anonymous share"}))
	if tier := tierName(t, s, created["agent"].(string)); tier != "anonymous" {
		t.Fatalf("a hosted identity's tier: %v", tier)
	}
	// Its own read, signed with its key, says the same.
	if own := run(t, s, signed(key, Command{Operation: "allowance.get"})); own.Data["tier_name"] != "anonymous" {
		t.Fatalf("a hosted identity's own allowance: %v", own.Data)
	}
	keyed := keyFor(94)
	register(t, s, keyed)
	if tier := tierName(t, s, keyID(keyed)); tier != "signed" {
		t.Fatalf("a keyed agent's tier: %v", tier)
	}
}

// pause-hosted stops issuance and every hosted signature until released;
// hosted.* travels only over MCP.
func TestHostedLeverAndWire(t *testing.T) {
	s := openHostedTest(t)
	created := createHosted(t, s, "test-origin", "")
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverPauseHosted, Reason: "incident review"}); err != nil {
		t.Fatal(err)
	}
	fails(t, s, Command{Operation: "hosted.create"}, "hosted_unavailable")
	if _, _, err := s.HostedSigner(testContext, created["token"].(string), testTime); !isCode(err, "hosted_unavailable") {
		t.Fatalf("signing while paused: %v", err)
	}
	if _, err := s.ReleaseLever(testContext, LeverPauseHosted, nil, "", "reviewed"); err != nil {
		t.Fatal(err)
	}
	hostedKey(t, s, created["token"].(string))
	for _, via := range []string{"command", "ui", "c64", "tcp", "email"} {
		_, err := s.Execute(WithVia(testContext, via), Command{Operation: "hosted.create"}, "test-origin")
		if !isCode(err, "mcp_only") {
			t.Errorf("hosted.create over %s: %v", via, err)
		}
	}
	if _, err := s.Execute(WithVia(testContext, "mcp"), Command{Operation: "hosted.create"}, "test-origin"); err != nil {
		t.Fatalf("hosted.create over mcp: %v", err)
	}
}

// A hold confirms exactly one text to one place from one account, for
// HostedHoldSeconds.
func TestHostedHoldTokens(t *testing.T) {
	s := openHostedTest(t)
	hold := s.HostedHold("acct", "~room", "the text", testTime+HostedHoldSeconds)
	if !s.HostedHoldValid(hold, "acct", "~room", "the text", testTime) {
		t.Fatal("a fresh hold does not confirm its own text")
	}
	for name, ok := range map[string]bool{
		"other text":    s.HostedHoldValid(hold, "acct", "~room", "the text!", testTime),
		"other room":    s.HostedHoldValid(hold, "acct", "~other", "the text", testTime),
		"other account": s.HostedHoldValid(hold, "other", "~room", "the text", testTime),
		"expired":       s.HostedHoldValid(hold, "acct", "~room", "the text", testTime+HostedHoldSeconds+1),
		"forged":        s.HostedHoldValid(strings.Replace(hold, ".", ".A", 1), "acct", "~room", "the text", testTime),
		"far future":    s.HostedHoldValid(s.HostedHold("acct", "~room", "the text", testTime+86400), "acct", "~room", "the text", testTime),
	} {
		if ok {
			t.Errorf("a hold confirms with %s", name)
		}
	}
}
