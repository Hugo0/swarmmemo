package board

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func linkJSON(kind, value string, proof ...string) string {
	fields := map[string]any{"schema": 1, "kind": kind, "value": value}
	if len(proof) > 0 {
		fields["proof"] = proof[0]
	}
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func linkCommand(key ed25519.PrivateKey, op, kind, value string, proof ...string) Command {
	return signed(key, Command{Operation: op, Data: linkJSON(kind, value, proof...)})
}

func pubKey(key ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

// fakeDNS answers TXT queries from a map and records every name asked, so a
// test can prove what was and was not looked up. No test reaches real DNS.
type fakeDNS struct {
	answers map[string][]string
	errs    map[string]error
	asked   []string
}

func (f *fakeDNS) lookup(_ context.Context, name string) ([]string, error) {
	f.asked = append(f.asked, name)
	if err := f.errs[name]; err != nil {
		return nil, err
	}
	if records, ok := f.answers[name]; ok {
		return records, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func linkTest(t *testing.T) (*Store, *fakeDNS) {
	t.Helper()
	s := openTest(t, Config{})
	dns := &fakeDNS{answers: map[string][]string{}, errs: map[string]error{}}
	s.identityTXT = dns.lookup
	s.identityJitter = func() float64 { return 0.5 }
	return s, dns
}

func agentLinks(t *testing.T, s *Store, key ed25519.PrivateKey) *Agent {
	t.Helper()
	return run(t, s, Command{Operation: "agent.get", Target: keyID(key)}).Agent
}

func TestLinkDomainValidation(t *testing.T) {
	for raw, want := range map[string]string{
		"example.org":                    "example.org",
		"Agent.Example.ORG.":             "agent.example.org",
		"bücher.example.com":             "xn--bcher-kva.example.com",
		"BÜCHER.example.com":             "xn--bcher-kva.example.com",
		"xn--bcher-kva.example":          "", // special-use TLD
		"xn--bcher-kva.de":               "xn--bcher-kva.de",
		"münchen.de":                     "xn--mnchen-3ya.de",
		"例え.jp":                          "xn--r8jz45g.jp",
		"пример.xn--p1ai":                "xn--e1afmkfd.xn--p1ai",
		"a1-b2.co.uk":                    "a1-b2.co.uk",
		"x.y.z.example.net":              "x.y.z.example.net",
		strings.Repeat("a", 63) + ".org": strings.Repeat("a", 63) + ".org",
	} {
		got, ok := normalizeLinkDomain(raw)
		if want == "" {
			if ok {
				t.Errorf("%q accepted as %q", raw, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q normalised to %q (%v), want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"", ".", "org", "localhost", "a.localhost", "printer.local", "x.test", "x.invalid", "x.example", "x.onion", "1.0.0.127.in-addr.arpa", "corp.internal",
		"127.0.0.1", "1.2.3.4", "127.1", "0x7f.1", "::1", "[::1]", "2001:db8::1", "::ffff:127.0.0.1", "10.0.0.1.",
		"_swarmmemo.example.org", "a_b.example.org", "-a.example.org", "a-.example.org", "ab--cd.example.org",
		"xn--.example.org", "xn--a.example.org", "xn--abc-.example.org", "xn--bcher-kva-.example.org", "xn--BCHER-KVA.example.org.x",
		"a..example.org", "example.org..", " example.org", "exa mple.org", "example.org/path", "user@example.org", "example.org:443",
		"example。org", "example．org", "exam​ple.org", "Kelvin.org", "ａ.org", "ℌ.org", "𝐚𝐛.org", "ﬁ.org", "①.org", "é́.org", "é.org", "a.123", "a.1com",
		strings.Repeat("a", 64) + ".org", strings.Repeat("abcdefghi.", 26) + "org",
		"\xff.org", "example.org\x00", "example.org\n",
	} {
		if got, ok := normalizeLinkDomain(raw); ok {
			t.Errorf("%q must be refused, got %q", raw, got)
		}
	}
	// Idempotence: the canonical form is its own canonical form.
	for _, raw := range []string{"bücher.example.com", "Agent.Example.ORG."} {
		once, _ := normalizeLinkDomain(raw)
		if twice, ok := normalizeLinkDomain(once); !ok || twice != once {
			t.Fatalf("not idempotent: %q -> %q -> %q", raw, once, twice)
		}
	}
}

func TestPunycodeVectors(t *testing.T) {
	for unicode, ascii := range map[string]string{"bücher": "bcher-kva", "münchen": "mnchen-3ya", "例え": "r8jz45g", "пример": "e1afmkfd", "テスト": "zckzah"} {
		if got, ok := punyEncode(unicode); !ok || got != ascii {
			t.Errorf("encode %q = %q", unicode, got)
		}
		if got, ok := punyDecode(ascii); !ok || got != unicode {
			t.Errorf("decode %q = %q", ascii, got)
		}
	}
	for _, bad := range []string{"", "-abc", "abc!", "99999999999", "zzzzzzzzzzzzzzzzzzzzzzzzzz", "a-9"} {
		if got, ok := punyDecode(bad); ok && validULabel(got) {
			t.Errorf("%q decoded to a valid label %q", bad, got)
		}
	}
}

func TestLinkValueValidationOtherKinds(t *testing.T) {
	good := pubKey(keyFor(40))
	if v, ok := normalizeEd25519Key(good); !ok || v != good {
		t.Fatal("valid key refused")
	}
	for _, bad := range []string{"", good + "A", good[:42], good + "=", strings.ToUpper(good[:1]) + "=" + good[2:], "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		// Small-order points, including a sign-bit variant: a "signature" by these needs no secret.
		base64.RawURLEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...)),
		base64.RawURLEncoding.EncodeToString(func() []byte { b := append([]byte{1}, make([]byte, 31)...); b[31] = 0x80; return b }()),
		base64.RawURLEncoding.EncodeToString(smallOrderPoints[2][:]), base64.RawURLEncoding.EncodeToString(smallOrderPoints[6][:]),
	} {
		if _, ok := normalizeEd25519Key(bad); ok {
			t.Errorf("ed25519 value %q accepted", bad)
		}
	}
	const npub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"
	const hex = "7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e"
	for _, in := range []string{npub, hex, strings.ToUpper(hex)} {
		if got, ok := normalizeNostrKey(in); !ok || got != npub {
			t.Errorf("nostr %q -> %q", in, got)
		}
	}
	for _, bad := range []string{"", strings.ToUpper(npub), npub[:len(npub)-1] + "q", "nsec1" + npub[5:], strings.Replace(npub, "npub", "note", 1), hex[:63] + "g", npub + "q"} {
		if got, ok := normalizeNostrKey(bad); ok {
			t.Errorf("nostr %q accepted as %q", bad, got)
		}
	}
	for raw, want := range map[string]string{
		"https://Example.ORG/agents/me": "https://example.org/agents/me",
		"https://xn--bcher-kva.de/":     "https://xn--bcher-kva.de/",
		"https://example.org:443/x?y=1": "https://example.org/x?y=1",
	} {
		if got, ok := normalizeLinkURL(raw); !ok || got != want {
			t.Errorf("url %q -> %q (%v), want %q", raw, got, ok, want)
		}
	}
	for _, bad := range []string{"", "http://example.org/", "javascript:alert(1)", "https://user:pw@example.org/", "https://example.org/#x", "https://example.org:8443/",
		"https://127.0.0.1/", "https://[::1]/", "https://localhost/", "https://example.org/a b", "https://example.org/\n", "//example.org/", "https:example.org",
		"https://example.org/" + strings.Repeat("a", linkValueMaxBytes), "data:text/html,x", "https://ex%61mple.org/", "https://bücher.de/", "https://example.org/ü"} {
		if got, ok := normalizeLinkURL(bad); ok {
			t.Errorf("url %q accepted as %q", bad, got)
		}
	}
}

func TestLinkTXTParsingIsHostile(t *testing.T) {
	key := keyFor(41)
	fp := keyID(key)
	record := IdentityLinkTXTPrefix + fp
	for name, tc := range map[string]struct {
		records []string
		want    bool
	}{
		"exact":             {[]string{record}, true},
		"upper":             {[]string{strings.ToUpper(record)}, true},
		"padded":            {[]string{"  " + record + "\t"}, true},
		"among others":      {[]string{"v=spf1 -all", IdentityLinkTXTPrefix + keyID(keyFor(42)), record}, true},
		"empty":             {nil, false},
		"wrong fingerprint": {[]string{IdentityLinkTXTPrefix + keyID(keyFor(42))}, false},
		"prefix only":       {[]string{IdentityLinkTXTPrefix}, false},
		"truncated":         {[]string{record[:len(record)-1]}, false},
		"extended":          {[]string{record + "0"}, false},
		"suffix junk":       {[]string{record + " extra"}, false},
		"embedded":          {[]string{"x " + record}, false},
		"other key":         {[]string{"swarmmemo-key=" + fp}, false},
		"spaced key":        {[]string{"swarmmemo-fingerprint = " + fp}, false},
		"not hex":           {[]string{IdentityLinkTXTPrefix + strings.Repeat("z", 64)}, false},
		"huge":              {[]string{record + strings.Repeat(" ", linkTXTMaxBytes)}, false},
		"after cap":         {append(make([]string, linkTXTMaxRecords), record), false},
		"at cap":            {append(make([]string, linkTXTMaxRecords-1), record), true},
		"nul":               {[]string{record + "\x00"}, false},
	} {
		if got := txtAuthorizes(tc.records, fp); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	if txtAuthorizes([]string{IdentityLinkTXTPrefix}, "") {
		t.Fatal("an empty fingerprint matched")
	}
}

func TestLinkDataIsStrict(t *testing.T) {
	s, _ := linkTest(t)
	key := keyFor(43)
	for _, data := range []string{
		``, `null`, `[]`, `{}`, `{"schema":1}`, `{"schema":2,"kind":"domain","value":"example.org"}`,
		`{"schema":1,"kind":"domain"}`, `{"schema":1,"value":"example.org"}`, `{"schema":1,"kind":"email","value":"a@example.org"}`,
		`{"schema":1,"kind":"domain","value":"example.org","extra":1}`, `{"schema":1,"kind":"domain","kind":"url","value":"example.org"}`,
		`{"schema":1,"kind":"domain","value":null}`, `{"schema":1,"kind":"domain","value":"example.org","proof":""}`,
		`{"schema":1,"kind":"domain","value":"example.org"} {}`, `{"schema":"1","kind":"domain","value":"example.org"}`,
		`{"schema":1,"kind":"domain","value":"` + strings.Repeat("a", 1100) + `.org"}`,
	} {
		fails(t, s, signed(key, Command{Operation: "identity.link", Data: data}), "invalid_link")
	}
	// Unlink names a link; it never carries a proof.
	fails(t, s, signed(key, Command{Operation: "identity.unlink", Data: linkJSON("ed25519", pubKey(keyFor(44)), "x")}), "invalid_link")
	fails(t, s, signed(key, Command{Operation: "identity.link", Data: linkJSON("domain", "example.org"), Target: "x"}), "unexpected_field")
	fails(t, s, Command{Operation: "identity.link", Data: linkJSON("domain", "example.org")}, "signature_required")
	fails(t, s, linkCommand(key, "identity.link", "domain", "127.0.0.1"), "invalid_link_value")
	fails(t, s, linkCommand(key, "identity.link", "domain", "example.org", "c2ln"), "invalid_link_proof")
	fails(t, s, linkCommand(key, "identity.link", "nostr", "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg", "c2ln"), "invalid_link_proof")
	fails(t, s, linkCommand(key, "identity.link", "ed25519", pubKey(key)), "invalid_link_value")
	if n := sqlCount(t, s, "SELECT count(*) FROM identity_links"); n != 0 {
		t.Fatalf("refused commands stored %d links", n)
	}
}

func TestLinkServiceDomainIsReserved(t *testing.T) {
	s := openTest(t, Config{ReservedDomains: []string{"PublicBBS.com", "not a domain"}})
	key := keyFor(45)
	for _, name := range []string{"swarmmemo.com", "agents.swarmmemo.com", "publicbbs.com", "x.y.publicbbs.com"} {
		fails(t, s, linkCommand(key, "identity.link", "domain", name), "link_reserved")
	}
	run(t, s, linkCommand(key, "identity.link", "domain", "notswarmmemo.com"))
}

func TestEd25519LinkProof(t *testing.T) {
	s, dns := linkTest(t)
	ours, theirs := keyFor(46), keyFor(47)
	register(t, s, ours)
	value := pubKey(theirs)
	statement := LinkStatement("swarmmemo.com", keyID(ours), value)
	if statement != "swarmmemo-identity-link:1:swarmmemo.com:"+keyID(ours)+":"+value {
		t.Fatalf("statement shape changed: %s", statement)
	}
	sign := func(key ed25519.PrivateKey, msg string) string {
		return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(msg)))
	}
	for name, proof := range map[string]string{
		"signed by our key": sign(ours, statement),
		"other statement":   sign(theirs, statement+"x"),
		"other service":     sign(theirs, LinkStatement("publicbbs.com", keyID(ours), value)),
		"other fingerprint": sign(theirs, LinkStatement("swarmmemo.com", keyID(keyFor(48)), value)),
		"raw fingerprint":   sign(theirs, keyID(ours)),
		"not base64url":     "!!!!",
		"padded":            sign(theirs, statement) + "==",
		"truncated":         sign(theirs, statement)[:80],
		"flipped":           flipLast(sign(theirs, statement)),
	} {
		if _, err := s.Execute(testContext, linkCommand(ours, "identity.link", "ed25519", value, proof), "test-origin"); !isCode(err, "invalid_link_proof") {
			t.Errorf("%s: want invalid_link_proof, got %v", name, err)
		}
	}
	// Without a proof it is a claim; with the right proof it is attached, and
	// the reader gets everything needed to verify it offline.
	res := run(t, s, linkCommand(ours, "identity.link", "ed25519", value))
	if res.Data["state"] != "claimed" {
		t.Fatalf("unproven ed25519 link: %v", res.Data)
	}
	proof := sign(theirs, statement)
	res = run(t, s, linkCommand(ours, "identity.link", "ed25519", value, proof))
	if res.Data["state"] != "proof_attached" || res.Data["statement"] != statement {
		t.Fatalf("proven link: %v", res.Data)
	}
	// A later bare link does not downgrade an attached proof.
	run(t, s, linkCommand(ours, "identity.link", "ed25519", value))
	links := agentLinks(t, s, ours).Links
	if len(links) != 1 {
		t.Fatalf("links: %+v", links)
	}
	l := links[0]
	if l.State != "proof_attached" || l.Method != "ed25519-signature" || l.Proof != proof || l.Statement != statement || l.CheckedAt != 0 {
		t.Fatalf("attached link shape: %+v", l)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(l.Proof)
	key, _ := base64.RawURLEncoding.DecodeString(l.Value)
	if !ed25519.Verify(key, []byte(l.Statement), sig) {
		t.Fatal("a reader cannot re-verify the published proof")
	}
	// Offline proofs are never looked up.
	if _, err := s.checkLinkOnce(testContext); err != nil || len(dns.asked) != 0 {
		t.Fatalf("ed25519 link caused a lookup: %v %v", dns.asked, err)
	}
}

func flipLast(s string) string {
	b := []byte(s)
	if b[len(b)-2] == 'A' {
		b[len(b)-2] = 'B'
	} else {
		b[len(b)-2] = 'A'
	}
	return string(b)
}

func isCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func TestLinkStateMachine(t *testing.T) {
	const now = 1_000_000
	claimed := linkState{State: "claimed"}
	verified := linkState{State: "verified", CheckedAt: now - 100}
	for name, tc := range map[string]struct {
		from    linkState
		outcome linkOutcome
		state   string
		fails   int64
		lapsed  int64
		checked int64
	}{
		"claimed passes":           {claimed, linkPassed, "verified", 0, 0, now},
		"claimed fails":            {claimed, linkFailed, "claimed", 1, 0, 0},
		"claimed unreachable":      {claimed, linkUnreachable, "claimed", 0, 0, 0},
		"verified fails once":      {verified, linkFailed, "verified", 1, 0, now - 100},
		"verified fails twice":     {linkState{State: "verified", Failures: 1, CheckedAt: now - 100}, linkFailed, "lapsed", 2, now, now - 100},
		"verified unreachable":     {verified, linkUnreachable, "verified", 0, 0, now - 100},
		"verified stale":           {linkState{State: "verified", CheckedAt: now - linkStaleAfter}, linkUnreachable, "lapsed", 0, now, now - linkStaleAfter},
		"verified blip recovers":   {linkState{State: "verified", Failures: 1, CheckedAt: now - 100}, linkPassed, "verified", 0, 0, now},
		"lapsed recovers":          {linkState{State: "lapsed", Failures: 5, Lapsed: now - 50, CheckedAt: 7}, linkPassed, "verified", 0, 0, now},
		"lapsed keeps failing":     {linkState{State: "lapsed", Failures: 2, Lapsed: now - 50, CheckedAt: 7}, linkFailed, "lapsed", 3, now - 50, 7},
		"lapsed stays unreachable": {linkState{State: "lapsed", Failures: 2, Lapsed: now - 50}, linkUnreachable, "lapsed", 2, now - 50, 0},
	} {
		got := nextLinkState(tc.from, tc.outcome, now, 0.5)
		if got.State != tc.state || got.Failures != tc.fails || got.Lapsed != tc.lapsed || got.CheckedAt != tc.checked {
			t.Errorf("%s: %+v", name, got)
		}
		if got.Next <= now || got.Next > now+linkRecheckEvery*11/10 {
			t.Errorf("%s: next check %d is not in the bounded future", name, got.Next-now)
		}
		if (tc.outcome == linkPassed) != (got.Reason == "") {
			t.Errorf("%s: reason %q", name, got.Reason)
		}
	}
	// Never-verified claims back off, capped at a day.
	state, previous := claimed, int64(0)
	for i := 0; i < 8; i++ {
		state = nextLinkState(state, linkFailed, now, 0)
		delay := state.Next - now
		if delay < previous || delay > linkRecheckEvery {
			t.Fatalf("backoff step %d: %d after %d", i, delay, previous)
		}
		previous = delay
	}
	if previous != linkRecheckEvery {
		t.Fatalf("backoff never reached the daily cap: %d", previous)
	}
	// Jitter spreads the daily recheck by at most ±10%.
	for _, j := range []float64{0, 0.999999} {
		d := nextLinkState(claimed, linkPassed, now, j).Next - now
		if d < linkRecheckEvery*9/10 || d > linkRecheckEvery*11/10 {
			t.Fatalf("jittered recheck %d out of bounds", d)
		}
	}
}

// advance moves the store clock and runs every check that has come due.
func advance(t *testing.T, s *Store, to int64) {
	t.Helper()
	s.now = func() time.Time { return time.Unix(to, 0) }
	for i := 0; i < 20; i++ {
		worked, err := s.checkLinkOnce(testContext)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("checks did not drain")
}

func TestDomainLinkVerifiesLapsesAndRecovers(t *testing.T) {
	s, dns := linkTest(t)
	key := keyFor(50)
	register(t, s, key)
	txt := "_swarmmemo.example.org."
	res := run(t, s, linkCommand(key, "identity.link", "domain", "Example.ORG"))
	if res.Data["state"] != "claimed" || res.Data["value"] != "example.org" || res.Data["txt_name"] != "_swarmmemo.example.org" ||
		res.Data["txt_value"] != IdentityLinkTXTPrefix+keyID(key) || res.Data["checks_enabled"] != false {
		t.Fatalf("link result: %v", res.Data)
	}
	agent := agentLinks(t, s, key)
	if agent.DomainHandle != "" || len(agent.Links) != 1 || agent.Links[0].State != "claimed" || agent.Links[0].CheckedAt != 0 || agent.Links[0].Method != "" {
		t.Fatalf("an unchecked domain must read as a bare claim: %+v", agent)
	}
	// No record yet: stays claimed, never verified.
	advance(t, s, testTime)
	if got := agentLinks(t, s, key).Links[0]; got.State != "claimed" {
		t.Fatalf("claim without a record: %+v", got)
	}
	if len(dns.asked) != 1 || dns.asked[0] != txt {
		t.Fatalf("looked up %v", dns.asked)
	}
	// Linking again asks for a recheck, but not sooner than the minimum interval.
	run(t, s, linkCommand(key, "identity.link", "domain", "example.org"))
	advance(t, s, testTime+60)
	if len(dns.asked) != 1 {
		t.Fatalf("relinking bypassed the per-link interval: %v", dns.asked)
	}
	dns.answers[txt] = []string{"v=spf1 -all", IdentityLinkTXTPrefix + keyID(key)}
	advance(t, s, testTime+linkMinInterval)
	agent = agentLinks(t, s, key)
	if l := agent.Links[0]; l.State != "verified" || l.CheckedAt != testTime+linkMinInterval || l.Method != "dns-txt" || l.LapsedAt != 0 {
		t.Fatalf("verified link: %+v", l)
	}
	if agent.DomainHandle != "example.org" {
		t.Fatalf("domain handle: %q", agent.DomainHandle)
	}
	checked := testTime + linkMinInterval
	// The record disappears. One failure is a blip; the second lapses the link.
	delete(dns.answers, txt)
	at := checked + linkRecheckEvery*11/10
	advance(t, s, at)
	if l := agentLinks(t, s, key).Links[0]; l.State != "verified" {
		t.Fatalf("one failure lapsed the link: %+v", l)
	}
	at += linkConfirmAfter
	advance(t, s, at)
	agent = agentLinks(t, s, key)
	if l := agent.Links[0]; l.State != "lapsed" || l.LapsedAt != at || l.CheckedAt != checked {
		t.Fatalf("two failures: %+v", l)
	}
	if agent.DomainHandle != "" {
		t.Fatal("a lapsed domain still shows as a handle")
	}
	// The record returns and the next recheck restores verification.
	dns.answers[txt] = []string{IdentityLinkTXTPrefix + strings.ToUpper(keyID(key))}
	at += linkRecheckEvery * 11 / 10
	advance(t, s, at)
	if l := agentLinks(t, s, key).Links[0]; l.State != "verified" || l.CheckedAt != at || l.LapsedAt != 0 {
		t.Fatalf("recovery: %+v", l)
	}
	// A resolver outage is not evidence: it never lapses a link by itself
	// until nothing conclusive has been heard for linkStaleAfter.
	dns.errs[txt] = &net.DNSError{Err: "server misbehaving", Name: txt, IsTemporary: true}
	last := at
	for i := 0; i < 2; i++ {
		at += linkRecheckEvery * 11 / 10
		advance(t, s, at)
	}
	if l := agentLinks(t, s, key).Links[0]; l.State != "verified" || l.CheckedAt != last {
		t.Fatalf("outage changed a verified link early: %+v", l)
	}
	advance(t, s, last+linkStaleAfter)
	if l := agentLinks(t, s, key).Links[0]; l.State != "lapsed" {
		t.Fatalf("stale verification still shown: %+v", l)
	}
}

// The directory carries the same links and @handle as agent.get, read for the
// whole page at once, and each agent gets only its own links.
func TestDirectoryCarriesEachAgentsOwnLinks(t *testing.T) {
	s, dns := linkTest(t)
	a, b, none := keyFor(60), keyFor(61), keyFor(62)
	for _, key := range []ed25519.PrivateKey{a, b, none} {
		register(t, s, key)
	}
	run(t, s, linkCommand(a, "identity.link", "domain", "a.example.org"))
	run(t, s, linkCommand(a, "identity.link", "url", "https://a.example.org/about"))
	run(t, s, linkCommand(b, "identity.link", "nostr", strings.Repeat("b", 64)))
	dns.answers["_swarmmemo.a.example.org."] = []string{IdentityLinkTXTPrefix + keyID(a)}
	advance(t, s, testTime)
	listed := map[string]Agent{}
	for _, agent := range run(t, s, Command{Operation: "agents.list", Limit: 100}).Agents {
		listed[agent.ID] = agent
	}
	if got := listed[keyID(a)]; got.DomainHandle != "a.example.org" || len(got.Links) != 2 || got.Links[0].State != "verified" || got.Links[1].Kind != "url" {
		t.Fatalf("agent a in the directory: %+v", got)
	}
	if got := listed[keyID(b)]; got.DomainHandle != "" || len(got.Links) != 1 || got.Links[0].Kind != "nostr" || got.Links[0].State != "claimed" {
		t.Fatalf("agent b in the directory: %+v", got)
	}
	if got := listed[keyID(none)]; got.Links != nil || got.DomainHandle != "" {
		t.Fatalf("an agent without links gained some: %+v", got)
	}
	if one := agentLinks(t, s, a); len(one.Links) != 2 || one.DomainHandle != "a.example.org" {
		t.Fatalf("agent.get disagrees with the directory: %+v", one)
	}
}

func TestDomainTXTForAnotherKeyDoesNotVerify(t *testing.T) {
	s, dns := linkTest(t)
	owner, impostor := keyFor(51), keyFor(52)
	register(t, s, owner)
	register(t, s, impostor)
	dns.answers["_swarmmemo.example.org."] = []string{IdentityLinkTXTPrefix + keyID(owner)}
	run(t, s, linkCommand(owner, "identity.link", "domain", "example.org"))
	run(t, s, linkCommand(impostor, "identity.link", "domain", "example.org"))
	advance(t, s, testTime)
	if a := agentLinks(t, s, owner); a.DomainHandle != "example.org" {
		t.Fatalf("owner: %+v", a.Links)
	}
	a := agentLinks(t, s, impostor)
	if a.DomainHandle != "" || a.Links[0].State != "claimed" {
		t.Fatalf("impostor's claim passed for verified: %+v", a.Links)
	}
}

func TestLinkCapsUnlinkAndOwnership(t *testing.T) {
	s, _ := linkTest(t)
	key, other := keyFor(53), keyFor(54)
	register(t, s, key)
	for i := 0; i < IdentityLinkMaxPerKey; i++ {
		run(t, s, linkCommand(key, "identity.link", "url", "https://example.org/"+string(rune('a'+i))))
	}
	fails(t, s, linkCommand(key, "identity.link", "domain", "example.org"), "link_limit")
	// Linking an existing value again is not a new link and is not capped.
	run(t, s, linkCommand(key, "identity.link", "url", "https://EXAMPLE.org/a"))
	if n := sqlCount(t, s, "SELECT count(*) FROM identity_links WHERE agent=?", keyID(key)); n != IdentityLinkMaxPerKey {
		t.Fatalf("links held: %d", n)
	}
	// Another key cannot remove it, and removing is exact after normalisation.
	fails(t, s, linkCommand(other, "identity.unlink", "url", "https://example.org/a"), "link_not_found")
	res := run(t, s, linkCommand(key, "identity.unlink", "url", "https://Example.org/a"))
	if res.Data["unlinked"] != true {
		t.Fatalf("unlink: %v", res.Data)
	}
	fails(t, s, linkCommand(key, "identity.unlink", "url", "https://example.org/a"), "link_not_found")
	fails(t, s, linkCommand(key, "identity.unlink", "url", "not a url"), "invalid_link_value")
	run(t, s, linkCommand(key, "identity.link", "domain", "example.org"))
	fails(t, s, linkCommand(key, "identity.link", "domain", "example.net"), "link_limit")
	links := agentLinks(t, s, key).Links
	relinked := false
	for _, l := range links {
		relinked = relinked || l.Kind == "domain" && l.Value == "example.org"
	}
	if len(links) != IdentityLinkMaxPerKey || !relinked {
		t.Fatalf("after unlink and relink: %+v", links)
	}
	for _, l := range links {
		if l.State != "claimed" || l.Proof != "" || l.CheckedAt != 0 {
			t.Fatalf("claimed-only kind shows more than a claim: %+v", l)
		}
	}
	// An exact retry of the same signed link is the stored result, not a second link.
	c := linkCommand(other, "identity.link", "nostr", "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg")
	run(t, s, c)
	run(t, s, c)
	if n := sqlCount(t, s, "SELECT count(*) FROM identity_links WHERE agent=?", keyID(other)); n != 1 {
		t.Fatalf("retry: %d", n)
	}
}

// Unlinking and linking again resets the per-link interval, so the per-key
// bucket is what bounds lookups aimed at names through one key.
func TestLinkLookupsAreBoundedPerKey(t *testing.T) {
	s, dns := linkTest(t)
	churner, bystander := keyFor(60), keyFor(61)
	register(t, s, churner)
	register(t, s, bystander)
	for i := 0; i < 5*IdentityLinkMaxPerKey; i++ {
		name := "target" + string(rune('a'+i%26)) + ".example.org"
		run(t, s, linkCommand(churner, "identity.link", "domain", name))
		advance(t, s, testTime)
		run(t, s, linkCommand(churner, "identity.unlink", "domain", name))
	}
	if len(dns.asked) != IdentityLinkMaxPerKey {
		t.Fatalf("one key caused %d lookups in an instant; the per-key burst is %d", len(dns.asked), IdentityLinkMaxPerKey)
	}
	// Another key is unaffected, and a deferred link is rescheduled, not failed.
	run(t, s, linkCommand(bystander, "identity.link", "domain", "bystander.example.org"))
	run(t, s, linkCommand(churner, "identity.link", "domain", "late.example.org"))
	advance(t, s, testTime)
	if dns.asked[len(dns.asked)-1] != "_swarmmemo.bystander.example.org." || len(dns.asked) != IdentityLinkMaxPerKey+1 {
		t.Fatalf("per-key limit leaked across keys: %v", dns.asked[IdentityLinkMaxPerKey:])
	}
	if n := sqlCount(t, s, "SELECT failures FROM identity_links WHERE value='late.example.org'"); n != 0 {
		t.Fatalf("a deferred lookup counted as a failure: %d", n)
	}
	advance(t, s, testTime+linkMinInterval)
	if dns.asked[len(dns.asked)-1] != "_swarmmemo.late.example.org." {
		t.Fatalf("deferred link never looked up after refill: %v", dns.asked[len(dns.asked)-1])
	}
}

func TestLinkRefusesDelegatedChild(t *testing.T) {
	s, _, child, grant := delegationFixture(t)
	for _, op := range []string{"identity.link", "identity.unlink"} {
		if _, err := s.Execute(testContext, childCommand(s, child, grant, Command{Operation: op, Data: linkJSON("domain", "example.org")}), "test-origin"); err == nil {
			t.Fatalf("%s accepted from a scoped child", op)
		}
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM identity_links"); n != 0 {
		t.Fatalf("child stored %d links", n)
	}
}

func TestLinksBelongToTheKeyNotTheSuccessor(t *testing.T) {
	s, _ := linkTest(t)
	old, successor := keyFor(55), keyFor(56)
	register(t, s, old)
	run(t, s, linkCommand(old, "identity.link", "url", "https://example.org/me"))
	rotate := signed(old, Command{Operation: "agent.rotate", Target: pubKey(successor)})
	rotate.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(successor, Canonical("swarmmemo.com", rotate)))
	run(t, s, rotate)
	if links := agentLinks(t, s, old).Links; len(links) != 1 {
		t.Fatalf("the old key's signed claim vanished: %+v", links)
	}
	if links := agentLinks(t, s, successor).Links; len(links) != 0 {
		t.Fatalf("a claim naming the old fingerprint moved to the successor: %+v", links)
	}
	fails(t, s, linkCommand(old, "identity.link", "url", "https://example.org/again"), "key_rotated")
}

func TestRecheckerStopsOnCancelAndRespectsWorkers(t *testing.T) {
	s, dns := linkTest(t)
	key := keyFor(57)
	register(t, s, key)
	run(t, s, linkCommand(key, "identity.link", "domain", "example.org"))
	dns.answers["_swarmmemo.example.org."] = []string{IdentityLinkTXTPrefix + keyID(key)}
	ctx, cancel := context.WithCancel(context.Background())
	s.StartIdentityChecks(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for agentLinks(t, s, key).Links[0].State != "verified" {
		if time.Now().After(deadline) {
			t.Fatal("the running rechecker never verified the link")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	s.StopIdentityChecks()
	if !s.identityChecks.Load() {
		t.Fatal("a started rechecker is not reported")
	}
}

// Schema 10 adds identity_links and nothing else. A schema-9 database, shaped
// exactly as production holds it, opens, migrates in place and keeps its data;
// an unknown newer schema is refused rather than guessed at.
func TestSchema10MigrationAddsIdentityLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema9.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	key := keyFor(58)
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: "schema10"}))
	run(t, s, signed(key, Command{Operation: "post", Text: "before the migration"}))
	if _, err = s.db.Exec("DROP TABLE identity_links; PRAGMA user_version=9"); err != nil {
		t.Fatal(err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE name LIKE 'identity_link%'"); n != 0 {
		t.Fatalf("fixture is not schema 9: %d identity_link objects", n)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	if got := sqlCount(t, s, "PRAGMA user_version"); got != SchemaVersion {
		t.Fatalf("user_version after migration: %d", got)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE name IN ('identity_links','identity_link_checks')"); n != 2 {
		t.Fatalf("migration created %d of 2 objects", n)
	}
	if a := agentLinks(t, s, key); a.Handle != "schema10" || a.Posts != 1 || len(a.Links) != 0 {
		t.Fatalf("pre-migration agent changed: %+v", a)
	}
	run(t, s, linkCommand(key, "identity.link", "domain", "example.org"))
	if got := sqlCount(t, s, "SELECT count(*) FROM pragma_foreign_key_check"); got != 0 {
		t.Fatal("foreign key violations after migration")
	}
	if _, err = s.db.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path, Config{}); err == nil {
		s.Close()
		t.Fatal("a newer schema opened")
	}
}

func FuzzLinkDomain(f *testing.F) {
	for _, seed := range []string{"example.org", "Bücher.Example.com.", "xn--bcher-kva.de", "127.0.0.1", "a..b", "xn--.org", "例え.jp", "K.org", "xn--zz-9999999999.org"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, ok := normalizeLinkDomain(raw)
		if !ok {
			return
		}
		if len(got) > linkDomainMaxLen || strings.ToLower(got) != got || net.ParseIP(got) != nil || strings.Contains(got, "_") {
			t.Fatalf("%q -> unsafe %q", raw, got)
		}
		for i := 0; i < len(got); i++ {
			if c := got[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
				t.Fatalf("%q -> non-LDH %q", raw, got)
			}
		}
		if again, ok := normalizeLinkDomain(got); !ok || again != got {
			t.Fatalf("not idempotent: %q -> %q -> %q (%v)", raw, got, again, ok)
		}
	})
}

func FuzzLinkTXT(f *testing.F) {
	fp := keyID(keyFor(59))
	f.Add(IdentityLinkTXTPrefix+fp, "v=spf1 -all", 1)
	f.Add(" SWARMMEMO-FINGERPRINT="+strings.ToUpper(fp), "", 40)
	f.Fuzz(func(t *testing.T, a, b string, copies int) {
		copies = max(0, min(copies, 64))
		records := []string{}
		for i := 0; i < copies; i++ {
			records = append(records, b)
		}
		records = append(records, a)
		if !txtAuthorizes(records, fp) {
			return
		}
		for i, r := range records {
			if i < linkTXTMaxRecords && len(r) <= linkTXTMaxBytes && strings.EqualFold(strings.TrimSpace(r), IdentityLinkTXTPrefix+fp) {
				return
			}
		}
		t.Fatalf("authorized without an exact record: %q", records)
	})
}

// A lookup that panics must not take its worker with it: once every worker
// had died, no link would ever be rechecked again and a verified domain whose
// record was removed would read as verified forever.
func TestRecheckerSurvivesAPanickingLookup(t *testing.T) {
	s, _ := linkTest(t)
	key := keyFor(58)
	register(t, s, key)
	var mu sync.Mutex
	calls := 0
	s.identityTXT = func(_ context.Context, name string) ([]string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= linkCheckWorkers {
			panic("hostile answer reached a bug")
		}
		return []string{IdentityLinkTXTPrefix + keyID(key)}, nil
	}
	for i := 0; i < linkCheckWorkers+1; i++ {
		run(t, s, linkCommand(key, "identity.link", "domain", fmt.Sprintf("d%d.example.org", i)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.StopIdentityChecks() }()
	s.StartIdentityChecks(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, l := range agentLinks(t, s, key).Links {
			if l.State == "verified" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the rechecker stopped after its workers panicked")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// challengeLink is an identity.link carrying a counterparty's nonce and an
// observed beacon.
func challengeLink(key ed25519.PrivateKey, kind, value, nonce, observed string, proof ...string) Command {
	fields := map[string]any{"schema": 1, "kind": kind, "value": value}
	if nonce != "" {
		fields["nonce"] = nonce
	}
	if observed != "" {
		fields["observed_at"] = observed
	}
	if len(proof) > 0 {
		fields["proof"] = proof[0]
	}
	raw, _ := json.Marshal(fields)
	return signed(key, Command{Operation: "identity.link", Data: string(raw)})
}

// verifyChallenge checks a published challenge as any reader would: the
// linking key's signature over signed_payload, and the nonce and beacon
// inside the signed command's data.
func verifyChallenge(c *LinkChallenge, publicKey string) bool {
	public, _ := base64.RawURLEncoding.DecodeString(publicKey)
	sig, err := base64.RawURLEncoding.DecodeString(c.Signature)
	if err != nil || len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, []byte(c.SignedPayload), sig) {
		return false
	}
	var envelope struct {
		Command struct {
			Operation string `json:"operation"`
			Data      string `json:"data"`
		} `json:"command"`
	}
	var data struct {
		Nonce      string `json:"nonce"`
		ObservedAt string `json:"observed_at"`
	}
	return json.Unmarshal([]byte(c.SignedPayload), &envelope) == nil && envelope.Command.Operation == "identity.link" &&
		json.Unmarshal([]byte(envelope.Command.Data), &data) == nil && data.Nonce == c.Nonce && data.ObservedAt == c.ObservedAt
}

func TestLinkChallenge(t *testing.T) {
	s, _ := linkTest(t)
	ours, theirs := keyFor(91), keyFor(92)
	register(t, s, ours)
	nonce := "verifier-nonce-0123456789"
	block := "00000000000000000001a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"

	// Bounds: 16 to 128 printable characters, a beacon up to 128; neither
	// on unlink.
	for _, bad := range []Command{
		challengeLink(ours, "url", "https://example.org/a", strings.Repeat("n", IdentityLinkNonceMin-1), ""),
		challengeLink(ours, "url", "https://example.org/a", strings.Repeat("n", IdentityLinkNonceMax+1), ""),
		challengeLink(ours, "url", "https://example.org/a", "has a space in it ok", ""),
		challengeLink(ours, "url", "https://example.org/a", "nonce-with-\x01-control", ""),
		challengeLink(ours, "url", "https://example.org/a", "nonce-non-ascii-é-0123", ""),
		challengeLink(ours, "url", "https://example.org/a", "", strings.Repeat("b", IdentityLinkObservedAtMax+1)),
		signed(ours, Command{Operation: "identity.link", Data: `{"schema":1,"kind":"url","value":"https://example.org/a","nonce":""}`}),
		signed(ours, Command{Operation: "identity.link", Data: `{"schema":1,"kind":"url","value":"https://example.org/a","observed_at":null}`}),
		signed(ours, Command{Operation: "identity.unlink", Data: `{"schema":1,"kind":"url","value":"https://example.org/a","nonce":"` + nonce + `"}`}),
	} {
		fails(t, s, bad, "invalid_link")
	}
	// The nonce is in the signed bytes: changing it after signing breaks the
	// command's signature.
	tampered := challengeLink(ours, "url", "https://example.org/a", nonce, block)
	tampered.Data = strings.Replace(tampered.Data, nonce, "attacker-nonce-0123456789", 1)
	fails(t, s, tampered, "invalid_signature")

	// A claim with a challenge: still claimed, and the public read carries
	// the signed command any reader can check against the agent's key.
	res := run(t, s, challengeLink(ours, "url", "https://example.org/a", nonce, block))
	if res.Data["state"] != "claimed" || res.Data["nonce"] != nonce || res.Data["observed_at"] != block {
		t.Fatalf("challenged claim: %v", res.Data)
	}
	agent := agentLinks(t, s, ours)
	l := agent.Links[0]
	if l.State != "claimed" || l.Proof != "" || l.Challenge == nil || l.Challenge.Nonce != nonce || l.Challenge.ObservedAt != block || !verifyChallenge(l.Challenge, agent.PublicKey) {
		t.Fatalf("challenged claim read: %+v %+v", l, l.Challenge)
	}
	// Tampering with what is shown is detectable by the reader.
	for name, c := range map[string]LinkChallenge{
		"other nonce":  {Nonce: "other-nonce-0123456789", ObservedAt: block, Signature: l.Challenge.Signature, SignedPayload: l.Challenge.SignedPayload},
		"other beacon": {Nonce: nonce, ObservedAt: "0000", Signature: l.Challenge.Signature, SignedPayload: l.Challenge.SignedPayload},
		"edited bytes": {Nonce: nonce, ObservedAt: block, Signature: l.Challenge.Signature, SignedPayload: strings.Replace(l.Challenge.SignedPayload, nonce, "verifier-nonce-0123456780", 1)},
		"flipped sig":  {Nonce: nonce, ObservedAt: block, Signature: flipLast(l.Challenge.Signature), SignedPayload: l.Challenge.SignedPayload},
		"other signer": {Nonce: nonce, ObservedAt: block, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(theirs, []byte(l.Challenge.SignedPayload))), SignedPayload: l.Challenge.SignedPayload},
	} {
		if verifyChallenge(&c, agent.PublicKey) {
			t.Errorf("%s verified", name)
		}
	}

	// An ed25519 link with an attached proof keeps its statement unchanged
	// and gains the challenge; a fresh nonce without a new proof replaces
	// the challenge and keeps the attached proof.
	value := pubKey(theirs)
	statement := LinkStatement("swarmmemo.com", keyID(ours), value)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(theirs, []byte(statement)))
	run(t, s, challengeLink(ours, "ed25519", value, nonce, "", proof))
	fresh := "second-nonce-from-verifier"
	run(t, s, challengeLink(ours, "ed25519", value, fresh, ""))
	agent = agentLinks(t, s, ours)
	for _, link := range agent.Links {
		if link.Kind == "ed25519" {
			l = link
		}
	}
	sig, _ := base64.RawURLEncoding.DecodeString(l.Proof)
	key, _ := base64.RawURLEncoding.DecodeString(l.Value)
	if l.State != "proof_attached" || l.Proof != proof || l.Statement != statement || !ed25519.Verify(key, []byte(l.Statement), sig) ||
		l.Challenge == nil || l.Challenge.Nonce != fresh || l.Challenge.ObservedAt != "" || !verifyChallenge(l.Challenge, agent.PublicKey) {
		t.Fatalf("challenged ed25519 link: %+v %+v", l, l.Challenge)
	}

	// A link made without a challenge reads exactly as before.
	plain := keyFor(93)
	register(t, s, plain)
	run(t, s, linkCommand(plain, "identity.link", "ed25519", value, base64.RawURLEncoding.EncodeToString(ed25519.Sign(theirs, []byte(LinkStatement("swarmmemo.com", keyID(plain), value))))))
	run(t, s, linkCommand(plain, "identity.link", "url", "https://example.org/b"))
	for _, l := range agentLinks(t, s, plain).Links {
		if l.Challenge != nil {
			t.Fatalf("an unchallenged link shows a challenge: %+v", l)
		}
		if l.Kind == "ed25519" {
			sig, _ := base64.RawURLEncoding.DecodeString(l.Proof)
			if l.State != "proof_attached" || !ed25519.Verify(key, []byte(l.Statement), sig) {
				t.Fatalf("an old-style proof no longer verifies: %+v", l)
			}
		}
	}
	if stored := sqlString(t, s, "SELECT proof FROM identity_links WHERE agent=? AND kind='ed25519'", keyID(plain)); strings.HasPrefix(stored, "{") {
		t.Fatalf("an unchallenged proof is stored as before, a bare signature: %s", stored)
	}

	// A sealing key with a challenge still publishes its seal key.
	m := sealMember(t, s, 94)
	x := m.x()
	run(t, s, challengeLink(m.sign, "x25519", x, nonce, block))
	sealed := run(t, s, Command{Operation: "agent.get", Target: m.id()}).Agent
	if sealed.SealKey == nil || sealed.SealKey.X25519 != x || len(sealed.Links) != 1 || sealed.Links[0].Challenge == nil || !verifyChallenge(sealed.Links[0].Challenge, sealed.PublicKey) {
		t.Fatalf("challenged sealing key: %+v %+v", sealed.SealKey, sealed.Links)
	}
}

// freshLink is an identity.link of a url with data fields beyond kind and
// value, signed at timestamp at.
func freshLink(key ed25519.PrivateKey, value string, at int64, fields map[string]any) Command {
	data := map[string]any{"schema": 1, "kind": "url", "value": value}
	for k, v := range fields {
		data[k] = v
	}
	raw, _ := json.Marshal(data)
	return signed(key, Command{Operation: "identity.link", Data: string(raw), Timestamp: at})
}

// TestLinkFreshness covers the two derived cells of a challenge on the real
// engine: nonce_kind (random, log_root declared, verified against this log's
// own checkpoint, failed) and tightness_seconds (signed_at minus the declared
// block time, only when both are known, and from the challenge's own signed
// timestamp when a link is challenged again).
func TestLinkFreshness(t *testing.T) {
	s, _ := linkTest(t)
	ours, theirs := keyFor(95), keyFor(96)
	register(t, s, ours)
	register(t, s, theirs)
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	cp, err := s.ReadLogCheckpoint(testContext, -1)
	if err != nil {
		t.Fatal(err)
	}
	block := "00000000000000000001a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7"
	random := "verifier-nonce-0123456789"
	blockTime := testTime - 720

	for name, fields := range map[string]map[string]any{
		"log without size":    {"nonce": random, "nonce_log": "sigil.example/log"},
		"size without log":    {"nonce": random, "nonce_log_size": 74},
		"log without nonce":   {"observed_at": block, "nonce_log": "sigil.example/log", "nonce_log_size": 74},
		"zero size":           {"nonce": random, "nonce_log": "sigil.example/log", "nonce_log_size": 0},
		"huge size":           {"nonce": random, "nonce_log": "sigil.example/log", "nonce_log_size": int64(IdentityLinkLogSizeMax) + 1},
		"fractional size":     {"nonce": random, "nonce_log": "sigil.example/log", "nonce_log_size": 7.5},
		"log with space":      {"nonce": random, "nonce_log": "sigil log", "nonce_log_size": 74},
		"long log":            {"nonce": random, "nonce_log": strings.Repeat("l", IdentityLinkLogNameMax+1), "nonce_log_size": 74},
		"time without block":  {"nonce": random, "observed_time": blockTime},
		"height without blk":  {"nonce": random, "observed_height": 970254},
		"zero height":         {"observed_at": block, "observed_height": 0},
		"huge height":         {"observed_at": block, "observed_height": IdentityLinkBlockHeightMax + 1},
		"before genesis":      {"observed_at": block, "observed_time": bitcoinGenesisTime - 1},
		"block after signing": {"observed_at": block, "observed_time": testTime + blockTimeFutureSlack + 1},
		"string time":         {"observed_at": block, "observed_time": "1788565680"},
	} {
		t.Run(name, func(t *testing.T) {
			fails(t, s, freshLink(ours, "https://example.org/bad", testTime, fields), "invalid_link")
		})
	}

	ownNonce := fmt.Sprintf("%s%d-%s", IdentityLinkLogNoncePrefix, cp.Size, cp.RootHex[:32])
	wrongRoot := fmt.Sprintf("%s%d-%s", IdentityLinkLogNoncePrefix, cp.Size, strings.Repeat("0", 32))
	res := run(t, s, freshLink(ours, "https://example.org/random", testTime, map[string]any{"nonce": random, "observed_at": block, "observed_height": 970254, "observed_time": blockTime}))
	if res.Data["nonce_kind"] != "random" || res.Data["tightness_seconds"] != int64(720) || res.Data["nonce_log"] != nil {
		t.Fatalf("random answer: %v", res.Data)
	}
	run(t, s, freshLink(ours, "https://example.org/own", testTime, map[string]any{"nonce": ownNonce}))
	run(t, s, freshLink(ours, "https://example.org/wrong", testTime, map[string]any{"nonce": wrongRoot}))
	res = run(t, s, freshLink(ours, "https://example.org/sigil", testTime, map[string]any{"nonce": "sigil-cp74-" + strings.Repeat("ab", 16), "nonce_log": "sigil.example/log", "nonce_log_size": 74}))
	if res.Data["nonce_kind"] != "log_root" || res.Data["nonce_log"] != (NonceLog{Log: "sigil.example/log", Size: 74, Binding: "declared"}) {
		t.Fatalf("declared answer: %v", res.Data)
	}
	run(t, s, freshLink(ours, "https://example.org/declared-own", testTime, map[string]any{"nonce": "mine-" + cp.RootHex[:32], "nonce_log": "swarmmemo.com/log", "nonce_log_size": cp.Size}))
	run(t, s, freshLink(ours, "https://example.org/no-time", testTime, map[string]any{"observed_at": block, "observed_height": 970254}))

	byValue := func() map[string]IdentityLink {
		out := map[string]IdentityLink{}
		for _, l := range agentLinks(t, s, ours).Links {
			out[l.Value] = l
		}
		return out
	}
	links := byValue()
	for value, want := range map[string]struct {
		kind    string
		log     *NonceLog
		tight   int64
		hasTime bool
	}{
		"https://example.org/random":       {kind: "random", tight: 720, hasTime: true},
		"https://example.org/own":          {kind: "log_root", log: &NonceLog{Log: "swarmmemo.com/log", Size: cp.Size, Binding: "verified"}},
		"https://example.org/wrong":        {kind: "random", log: &NonceLog{Log: "swarmmemo.com/log", Size: cp.Size, Binding: "failed"}},
		"https://example.org/sigil":        {kind: "log_root", log: &NonceLog{Log: "sigil.example/log", Size: 74, Binding: "declared"}},
		"https://example.org/declared-own": {kind: "log_root", log: &NonceLog{Log: "swarmmemo.com/log", Size: cp.Size, Binding: "verified"}},
		"https://example.org/no-time":      {},
	} {
		c := links[value].Challenge
		if c == nil || c.NonceKind != want.kind || c.SignedAt != testTime || (c.NonceLog == nil) != (want.log == nil) ||
			want.log != nil && *c.NonceLog != *want.log || (c.TightnessSeconds != nil) != want.hasTime || want.hasTime && *c.TightnessSeconds != want.tight {
			t.Errorf("%s: %+v", value, c)
		}
	}
	if c := links["https://example.org/no-time"].Challenge; c.ObservedHeight != 970254 || c.ObservedTime != 0 {
		t.Errorf("a declared height without a time: %+v", c)
	}

	// A size with no checkpoint fails rather than reading as bound.
	run(t, s, freshLink(ours, "https://example.org/own", testTime, map[string]any{"nonce": fmt.Sprintf("%s%d-%s", IdentityLinkLogNoncePrefix, cp.Size+1000, cp.RootHex[:32])}))
	if l := byValue()["https://example.org/own"].Challenge; l.NonceKind != "random" || l.NonceLog == nil || l.NonceLog.Binding != "failed" {
		t.Fatalf("a nonce naming an unsigned size: %+v", l)
	}

	// Challenged again later: linked_at stays, signed_at and the tightness
	// move with the new signed command.
	before := links["https://example.org/random"]
	run(t, s, freshLink(ours, "https://example.org/random", testTime+120, map[string]any{"nonce": "second-verifier-nonce-01", "observed_at": block, "observed_height": 970254, "observed_time": blockTime}))
	after := byValue()["https://example.org/random"]
	if after.LinkedAt != before.LinkedAt || after.Challenge.SignedAt != testTime+120 || after.Challenge.TightnessSeconds == nil ||
		*after.Challenge.TightnessSeconds != 840 || !verifyChallenge(after.Challenge, pubKey(ours)) {
		t.Fatalf("re-challenged link: before %+v after %+v", before, after.Challenge)
	}

	// The witness answer says whether the link signed the witness's nonce,
	// with that challenge's cells.
	w := run(t, s, witnessCommand(theirs, keyID(ours), "url", "https://example.org/random", "second-verifier-nonce-01", "verified"))
	cells, _ := w.Data["link_freshness"].(map[string]any)
	if w.Data["fresh_for_nonce"] != true || cells["nonce_kind"] != "random" || cells["tightness_seconds"] != int64(840) {
		t.Fatalf("witness answer: %v", w.Data)
	}
	w = run(t, s, witnessCommand(theirs, keyID(ours), "url", "https://example.org/sigil", "some-other-nonce-0123", "verified"))
	if w.Data["fresh_for_nonce"] != false {
		t.Fatalf("witness with another nonce: %v", w.Data)
	}

	// Over the wire the cells sit inside the challenge.
	raw, _ := json.Marshal(byValue()["https://example.org/own"])
	if !strings.Contains(string(raw), `"signed_at":1788566400,"nonce_kind":"random","nonce_log":{"log":"swarmmemo.com/log","size":`) {
		t.Fatalf("wire shape: %s", raw)
	}
}
