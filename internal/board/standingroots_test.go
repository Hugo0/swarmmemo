package board

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

var walletKey = bytes.Repeat([]byte{0x11}, 32)

func standingCmd(s *Store, key ed25519.PrivateKey, op string, data map[string]any) Command {
	raw, _ := json.Marshal(data)
	return signed(key, Command{Operation: op, Data: string(raw), RequestID: randomID(), Timestamp: s.now().Unix()})
}

func challenge(t *testing.T, s *Store, key ed25519.PrivateKey, data map[string]any) map[string]any {
	t.Helper()
	data["schema"] = 1
	return run(t, s, standingCmd(s, key, "standing.challenge", data)).Data
}

func walletAddress(t *testing.T) string {
	t.Helper()
	addr, _, err := services.SignPersonal(walletKey, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	return addr.String()
}

func TestStandingWalletLink(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(71), keyFor(72)
	postAs(t, s, alice, Command{Room: "lobby", Text: "alice here", RequestID: "wl-alice"})
	addr := walletAddress(t)
	ch := challenge(t, s, alice, map[string]any{"kind": "wallet", "value": strings.ToLower(addr)})
	if ch["value"] != addr || ch["single_use"] != true || ch["expires_at"].(int64) != testTime+StandingWalletTTL {
		t.Fatalf("challenge: %v", ch)
	}
	message, nonce := ch["message"].(string), ch["nonce"].(string)
	if !strings.Contains(message, keyID(alice)) || !strings.Contains(message, "Nonce: "+nonce) || !strings.HasPrefix(message, "swarmmemo.com wants you to sign in with your Ethereum account:\n"+addr+"\n") {
		t.Fatalf("message: %q", message)
	}
	// Only the nonce's hash is stored.
	var stored int
	if err := s.db.QueryRow("SELECT count(*) FROM standing_challenges WHERE nonce_sha256=? AND agent=?", sha256Hex([]byte(nonce)), keyID(alice)).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored %d %v", stored, err)
	}
	if n := strings.Count(dumpTable(t, s, "standing_challenges"), nonce); n != 0 {
		t.Fatal("the nonce itself is stored")
	}
	_, sig, err := services.SignPersonal(walletKey, []byte(message))
	if err != nil {
		t.Fatal(err)
	}
	link := func(key ed25519.PrivateKey, value, proof, nonce string) Command {
		return standingCmd(s, key, "identity.link", map[string]any{"schema": 1, "kind": "wallet", "value": value, "proof": proof, "nonce": nonce})
	}
	// Another key cannot use alice's challenge; a wrong signer or message is
	// refused and leaves the challenge usable (the command rolls back).
	fails(t, s, link(bob, addr, sig, nonce), "invalid_challenge")
	_, otherSig, _ := services.SignPersonal(bytes.Repeat([]byte{0x22}, 32), []byte(message))
	fails(t, s, link(alice, addr, otherSig, nonce), "invalid_wallet_signature")
	_, wrongMsg, _ := services.SignPersonal(walletKey, []byte(message+" "))
	fails(t, s, link(alice, addr, wrongMsg, nonce), "invalid_wallet_signature")
	fails(t, s, link(alice, addr, "0xzz", nonce), "invalid_wallet_signature")
	fails(t, s, standingCmd(s, alice, "identity.link", map[string]any{"schema": 1, "kind": "wallet", "value": addr, "proof": sig}), "invalid_link_proof")
	res := run(t, s, link(alice, addr, sig, nonce)).Data
	if res["state"] != "verified" || res["root"] != "wallet:eip155:1:"+strings.ToLower(addr) {
		t.Fatalf("link: %v", res)
	}
	// Replay: the challenge works once.
	fails(t, s, link(alice, addr, sig, nonce), "challenge_used")
	links := agentLinks(t, s, alice).Links
	if len(links) != 1 || links[0].Kind != "wallet" || links[0].State != "verified" || links[0].Proof != sig || links[0].Statement != message || links[0].CheckedAt != testTime || links[0].Method != "eip4361-signature" {
		t.Fatalf("links: %+v", links)
	}
	// Anyone can recover the signer from what the read shows.
	if got, ok := services.RecoverPersonalSign([]byte(links[0].Statement), links[0].Proof); !ok || got.String() != addr {
		t.Fatalf("recover from the read: %v %v", got, ok)
	}
	// Expiry.
	ch = challenge(t, s, alice, map[string]any{"kind": "wallet", "value": addr})
	_, sig2, _ := services.SignPersonal(walletKey, []byte(ch["message"].(string)))
	s.now = func() time.Time { return time.Unix(testTime+StandingWalletTTL, 0) }
	fails(t, s, link(alice, addr, sig2, ch["nonce"].(string)), "challenge_expired")
	// A challenge for one address does not link another.
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	other, _, _ := services.SignPersonal(bytes.Repeat([]byte{0x22}, 32), []byte("x"))
	ch = challenge(t, s, alice, map[string]any{"kind": "wallet", "value": addr})
	_, sig3, _ := services.SignPersonal(bytes.Repeat([]byte{0x22}, 32), []byte(strings.Replace(ch["message"].(string), addr, other.String(), 1)))
	fails(t, s, link(alice, other.String(), sig3, ch["nonce"].(string)), "invalid_challenge")
	// Bad values never reach a challenge.
	for _, v := range []string{"", "0x0000000000000000000000000000000000000000", "0x12", "12345678901234567890123456789012345678901234", "0x14791697260e4C9A71f18484C9f997B308e59325"} {
		fails(t, s, standingCmd(s, alice, "standing.challenge", map[string]any{"schema": 1, "kind": "wallet", "value": v}), "invalid_link_value")
	}
}

func dumpTable(t *testing.T, s *Store, table string) string {
	t.Helper()
	rows, err := s.db.Query("SELECT * FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(strconv.Quote(toString(v)), `"`, "")) + " ")
		}
	}
	return b.String()
}

func toString(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}

// fakeGitHub answers the API from maps and counts calls.
type fakeGitHub struct {
	users map[string]githubUser
	gists map[string]gistAnswer
	calls []string
	limit bool
}

func (f *fakeGitHub) fetch(_ context.Context, path string) (int, http.Header, []byte, error) {
	f.calls = append(f.calls, path)
	h := http.Header{}
	if f.limit {
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Reset", strconv.FormatInt(testTime+600, 10))
		return 403, h, []byte(`{"message":"API rate limit exceeded"}`), nil
	}
	var v any
	switch {
	case strings.HasPrefix(path, "/users/"):
		u, ok := f.users[strings.TrimPrefix(path, "/users/")]
		if !ok {
			return 404, h, []byte(`{}`), nil
		}
		v = u
	case strings.HasPrefix(path, "/gists/"):
		g, ok := f.gists[strings.TrimPrefix(path, "/gists/")]
		if !ok {
			return 404, h, []byte(`{}`), nil
		}
		v = g
	}
	raw, _ := json.Marshal(v)
	return 200, h, raw, nil
}

func gist(ownerID int64, content string) gistAnswer {
	var g gistAnswer
	g.Owner.ID = ownerID
	g.Files = map[string]struct {
		Content string `json:"content"`
	}{"swarmmemo.txt": {Content: content}}
	return g
}

func TestStandingGitHubLink(t *testing.T) {
	s, _ := linkTest(t)
	gh := &fakeGitHub{users: map[string]githubUser{"octo-cat": {Login: "Octo-Cat", ID: 4242, CreatedAt: "2023-09-06T00:00:00Z", PublicRepos: 12, Followers: 8}}, gists: map[string]gistAnswer{}}
	s.github.fetch = gh.fetch
	alice := keyFor(73)
	postAs(t, s, alice, Command{Room: "lobby", Text: "alice here", RequestID: "gh-alice"})
	ch := challenge(t, s, alice, map[string]any{"kind": "github", "value": "Octo-Cat"})
	statement, nonce := ch["statement"].(string), ch["nonce"].(string)
	if statement != GitHubStatement("swarmmemo.com", keyID(alice), "octo-cat", nonce) {
		t.Fatalf("statement %q", statement)
	}
	const gistA, gistB = "aa11bb22cc33dd44ee55", "ff11bb22cc33dd44ee55"
	gh.gists[gistA] = gist(4242, "hello\n"+statement+"\n")
	gh.gists[gistB] = gist(9999, statement) // someone else's gist
	link := func(proof, nonce string) Command {
		return standingCmd(s, alice, "identity.link", map[string]any{"schema": 1, "kind": "github", "value": "octo-cat", "proof": proof, "nonce": nonce})
	}
	fails(t, s, link("not a gist", nonce), "invalid_link_proof")
	res := run(t, s, link("https://gist.github.com/octo-cat/"+gistB, nonce)).Data
	if res["state"] != "claimed" || res["statement"] != statement {
		t.Fatalf("link: %v", res)
	}
	fails(t, s, link(gistA, nonce), "challenge_used")
	// The checker: someone else's gist fails.
	if worked, err := s.checkLinkOnce(testContext); !worked || err != nil {
		t.Fatalf("check: %v %v", worked, err)
	}
	if l := agentLinks(t, s, alice).Links[0]; l.State != "claimed" || l.Proof != "" || l.Method != "" || l.Statement != statement {
		t.Fatalf("someone else's gist verified the link, or a claim shows a proof: %+v", l)
	}
	// A new challenge with the account's own gist verifies it, and records
	// the assessment under the numeric id.
	ch = challenge(t, s, alice, map[string]any{"kind": "github", "value": "octo-cat"})
	gh.gists[gistA] = gist(4242, ch["statement"].(string))
	run(t, s, link(gistA, ch["nonce"].(string)))
	s.now = func() time.Time { return time.Unix(testTime+linkMinInterval, 0) }
	if worked, err := s.checkLinkOnce(testContext); !worked || err != nil {
		t.Fatalf("check: %v %v", worked, err)
	}
	l := agentLinks(t, s, alice).Links[0]
	if l.State != "verified" || l.Proof != gistA || l.Statement != ch["statement"] || l.Method != "github-gist" {
		t.Fatalf("link after check: %+v", l)
	}
	var root string
	var assessed, next int64
	if err := s.db.QueryRow("SELECT root,assessed FROM standing_assessments WHERE agent=? AND kind='github'", keyID(alice)).Scan(&root, &assessed); err != nil {
		t.Fatal(err)
	}
	age := (testTime + linkMinInterval - time.Date(2023, 9, 6, 0, 0, 0, 0, time.UTC).Unix()) / 86400
	if root != "github:4242" || assessed != trust.GitHubCents(age, 12, 8) || assessed <= 100 || assessed > 300 {
		t.Fatalf("assessment %s %d (age %d)", root, assessed, age)
	}
	if err := s.db.QueryRow("SELECT next_check_at FROM identity_links WHERE agent=? AND kind='github'", keyID(alice)).Scan(&next); err != nil || next != testTime+linkMinInterval+standingGitHubRecheck {
		t.Fatalf("weekly recheck: %d %v", next, err)
	}
	// The user is cached: a second check reads only the gist.
	before := len(gh.calls)
	if _, err := s.db.Exec("UPDATE identity_links SET next_check_at=1 WHERE agent=?", keyID(alice)); err != nil {
		t.Fatal(err)
	}
	s.checkLinkOnce(testContext)
	if got := gh.calls[before:]; len(got) != 1 || !strings.HasPrefix(got[0], "/gists/") {
		t.Fatalf("calls after cache: %v", got)
	}
	// A spent rate limit pauses every request until its reset: inconclusive,
	// never a failure.
	gh.limit = true
	if _, err := s.db.Exec("UPDATE identity_links SET next_check_at=1 WHERE agent=?", keyID(alice)); err != nil {
		t.Fatal(err)
	}
	s.checkLinkOnce(testContext)
	calls := len(gh.calls)
	if _, err := s.db.Exec("UPDATE identity_links SET next_check_at=1 WHERE agent=?", keyID(alice)); err != nil {
		t.Fatal(err)
	}
	s.github.users = nil
	s.checkLinkOnce(testContext)
	if len(gh.calls) != calls {
		t.Fatal("a request went out while the rate limit was spent")
	}
	if l := agentLinks(t, s, alice).Links[0]; l.State != "verified" {
		t.Fatalf("a rate limit lapsed the link: %+v", l)
	}
}

// solvePow finds a solution by brute force (bits are small in tests).
func solvePow(prefix string, bits int64) string {
	for i := 0; ; i++ {
		sol := strconv.Itoa(i)
		if leadingZeroBits(sha256.Sum256([]byte(prefix+sol))) >= bits {
			return sol
		}
	}
}

func TestStandingProofOfWork(t *testing.T) {
	s := openTest(t, Config{})
	alice, bob := keyFor(74), keyFor(75)
	ch := challenge(t, s, alice, map[string]any{"kind": "pow", "bits": StandingPowBitsMin})
	prefix, nonce := ch["prefix"].(string), ch["nonce"].(string)
	if prefix != PowPrefix("swarmmemo.com", keyID(alice), nonce) || ch["bits"].(int64) != StandingPowBitsMin {
		t.Fatalf("challenge %v", ch)
	}
	work := func(key ed25519.PrivateKey, nonce, solution string) Command {
		return standingCmd(s, key, "standing.work", map[string]any{"schema": 1, "nonce": nonce, "solution": solution})
	}
	sol := solvePow(prefix, StandingPowBitsMin)
	// A wrong solution is refused and leaves the challenge usable.
	wrong := "x"
	for leadingZeroBits(sha256.Sum256([]byte(prefix+wrong))) >= StandingPowBitsMin {
		wrong += "x"
	}
	fails(t, s, work(alice, nonce, wrong), "invalid_solution")
	fails(t, s, work(alice, nonce, "not valid!"), "invalid_standing")
	fails(t, s, work(bob, nonce, sol), "invalid_challenge")
	res := run(t, s, work(alice, nonce, sol)).Data
	if res["work_units"].(int64) != 1 || res["total_work_units"].(int64) != 1 || res["solutions"].(int64) != 1 || res["root"] != "pow:"+keyID(alice) {
		t.Fatalf("work: %v", res)
	}
	fails(t, s, work(alice, nonce, sol), "challenge_used")
	// A second solution adds up.
	ch = challenge(t, s, alice, map[string]any{"kind": "pow", "bits": StandingPowBitsMin + 1})
	run(t, s, work(alice, ch["nonce"].(string), solvePow(ch["prefix"].(string), StandingPowBitsMin+1)))
	var total int64
	if err := s.db.QueryRow("SELECT assessed FROM standing_assessments WHERE agent=? AND kind='pow'", keyID(alice)).Scan(&total); err != nil || total != 3 {
		t.Fatalf("total %d %v", total, err)
	}
	// Bits out of range, value on pow, bits on wallet.
	for _, d := range []map[string]any{{"kind": "pow", "bits": 19}, {"kind": "pow", "bits": 49}, {"kind": "pow", "value": "x"}, {"kind": "github", "value": "a", "bits": 20}, {"kind": "domain", "value": "x.org"}, {"kind": "pow", "extra": 1}} {
		d["schema"] = 1
		fails(t, s, standingCmd(s, alice, "standing.challenge", d), "invalid_standing")
	}
	// Open proof-of-work challenges are capped.
	for i := 0; i < StandingPowOpen; i++ {
		challenge(t, s, bob, map[string]any{"kind": "pow"})
	}
	fails(t, s, standingCmd(s, bob, "standing.challenge", map[string]any{"schema": 1, "kind": "pow"}), "challenge_rate")
	// Delegated keys and unsigned callers cannot ask.
	_, err := s.Execute(testContext, Command{Operation: "standing.challenge", Data: `{"schema":1,"kind":"pow"}`, RequestID: randomID()}, "test-origin")
	if errCode(err) != "signature_required" {
		t.Fatalf("unsigned: %v", err)
	}
}

func TestStandingChallengeDailyLimit(t *testing.T) {
	s := openTest(t, Config{})
	alice := keyFor(76)
	for i := 0; i < StandingChallengesPerDay; i++ {
		challenge(t, s, alice, map[string]any{"kind": "github", "value": "user" + strconv.Itoa(i)})
	}
	_, err := s.Execute(testContext, standingCmd(s, alice, "standing.challenge", map[string]any{"schema": 1, "kind": "wallet", "value": walletAddress(t)}), "test-origin")
	var e *Error
	if !errorsAs(err, &e) || e.Code != "challenge_rate" || e.RetryAfter <= 0 || e.Status != 429 {
		t.Fatalf("21st challenge: %v", err)
	}
	// The proof-of-work class is counted apart, and the next UTC day resets.
	challenge(t, s, alice, map[string]any{"kind": "pow"})
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	challenge(t, s, alice, map[string]any{"kind": "github", "value": "next-day"})
}

func errorsAs(err error, e **Error) bool {
	x, ok := err.(*Error)
	if ok {
		*e = x
	}
	return ok
}

func TestStandingWaysAndRun(t *testing.T) {
	s := openTest(t, Config{Features: Features{Trust: TrustShadow}})
	alice := keyFor(77)
	s.now = func() time.Time { return time.Unix(testTime-2*86400, 0) }
	postAs(t, s, alice, Command{Room: "lobby", Text: "alice here", RequestID: "w-alice"})
	addr := walletAddress(t)
	ch := challenge(t, s, alice, map[string]any{"kind": "wallet", "value": addr})
	_, sig, _ := services.SignPersonal(walletKey, []byte(ch["message"].(string)))
	run(t, s, standingCmd(s, alice, "identity.link", map[string]any{"schema": 1, "kind": "wallet", "value": addr, "proof": sig, "nonce": ch["nonce"]}))
	ch = challenge(t, s, alice, map[string]any{"kind": "pow", "bits": StandingPowBitsMin})
	run(t, s, standingCmd(s, alice, "standing.work", map[string]any{"schema": 1, "nonce": ch["nonce"], "solution": solvePow(ch["prefix"].(string), StandingPowBitsMin)}))
	// Corroborate's answer, as the checker records it.
	if err := s.saveAssessment(testContext, keyID(alice), "wallet", addr, walletRoot(addr), 420, map[string]any{"source": "test"}, s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime+3600, 0) }
	if _, err := s.RunTrust(testContext); err != nil {
		t.Fatal(err)
	}
	ways := run(t, s, Command{Operation: "standing.ways", Target: keyID(alice)}).Data
	list := ways["ways"].([]StandingWay)
	if len(list) != 5 || ways["params_version"].(int64) != trust.DefaultVersion {
		t.Fatalf("ways: %+v", ways)
	}
	byKind := map[string]StandingWay{}
	for _, w := range list {
		byKind[w.Kind] = w
	}
	w := byKind["wallet"]
	if w.State != "verified" || w.Value != addr || w.Root != walletRoot(addr) || w.AddsCents == nil || w.AddsCents.Max != 600 || w.AssessedCents == nil || *w.AssessedCents != 420 ||
		w.CountedCents == nil || *w.CountedCents != 420 || !strings.HasPrefix(w.Action, "standing.challenge") {
		t.Fatalf("wallet way: %+v", w)
	}
	p := byKind["pow"]
	if p.State != "verified" || p.AddsCents.Max != 50 || p.AssessedCents == nil || *p.AssessedCents != 0 {
		t.Fatalf("pow way: %+v", p)
	}
	if d := byKind["domain"]; d.State != "none" || d.AddsCents.Max != 400 || !d.Priced {
		t.Fatalf("domain way: %+v", d)
	}
	if g := byKind["github"]; g.State != "none" || g.AddsCents.Max != 300 {
		t.Fatalf("github way: %+v", g)
	}
	if e := byKind["earned"]; e.AddsCents != nil {
		t.Fatalf("earned way: %+v", e)
	}
	st := ways["standing"].(map[string]any)
	if st["standing_cents"].(int64) < 420 {
		t.Fatalf("standing: %v", st)
	}
	// The snapshot carries the assessed roots for anyone to recompute.
	snapReader, _, _, err := s.TrustSnapshot(testContext, 1)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := trust.ReadJSONL(snapReader)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range snap.Proofs {
		if r.Kind == "wallet" && r.Root == walletRoot(addr) && r.Assessed == 420 {
			found["wallet"] = true
		}
		if r.Kind == "pow" && r.Root == "pow:"+keyID(alice) && r.Assessed == 1 && r.CheckedAt > 0 {
			found["pow"] = true
		}
	}
	if !found["wallet"] || !found["pow"] {
		t.Fatalf("snapshot proofs: %+v", snap.Proofs)
	}
	// A private agent is not found; trust off is unavailable.
	fails(t, s, Command{Operation: "standing.ways", Target: keyID(keyFor(78))}, "not_found")
	off := openTest(t, Config{})
	fails(t, off, Command{Operation: "standing.ways", Target: keyID(alice)}, "service_unavailable")
}

func FuzzParseStandingData(f *testing.F) {
	for _, seed := range []string{`{"schema":1,"kind":"pow","bits":24}`, `{"schema":1,"kind":"wallet","value":"0x14791697260E4c9A71f18484C9f997B308e59325"}`,
		`{"schema":1,"nonce":"00112233445566778899aabbccddeeff","solution":"abc"}`, `{"schema":1,"kind":"github","value":"Octo-Cat"}`, `{}`, `[]`, `{"schema":1,"schema":1}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		d, _, err := parseStandingData(raw, "kind", "value", "bits", "nonce", "solution")
		if err != nil {
			return
		}
		if len(raw) > standingChallengeBytes || d.Schema != 1 {
			t.Fatalf("accepted %q", raw)
		}
		if v, ok := normalizeWallet(d.Value); ok && (len(v) != 42 || !strings.HasPrefix(v, "0x")) {
			t.Fatalf("wallet %q", v)
		}
		if v, ok := normalizeGitHub(d.Value); ok && (len(v) > 39 || v != strings.ToLower(v)) {
			t.Fatalf("github %q", v)
		}
		if v, ok := gistID(d.Value); ok && len(v) > 40 {
			t.Fatalf("gist %q", v)
		}
	})
}
