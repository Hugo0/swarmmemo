package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func witnessCommand(key ed25519.PrivateKey, agent, kind, value, nonce, verdict string) Command {
	raw, _ := json.Marshal(map[string]any{"schema": 1, "agent": agent, "kind": kind, "value": value, "nonce": nonce, "verdict": verdict})
	return signed(key, Command{Operation: "identity.witness", Data: string(raw)})
}

// verifyWitness checks a published witness as any reader would, offline: the
// witness key's signature over signed_payload, that key's fingerprint, and
// the agent, link, nonce and verdict inside the signed command's data.
func verifyWitness(w LinkWitness, agent, kind, value string) bool {
	public, err := base64.RawURLEncoding.DecodeString(w.PublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize || fingerprint(public) != w.Fingerprint {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(w.Signature)
	if err != nil || !ed25519.Verify(public, []byte(w.SignedPayload), sig) {
		return false
	}
	var envelope struct {
		Service string `json:"service"`
		Command struct {
			Operation string `json:"operation"`
			PublicKey string `json:"public_key"`
			Data      string `json:"data"`
		} `json:"command"`
	}
	var data struct {
		Agent, Kind, Value, Nonce, Verdict string
	}
	return json.Unmarshal([]byte(w.SignedPayload), &envelope) == nil && envelope.Service == "swarmmemo.com" &&
		envelope.Command.Operation == "identity.witness" && envelope.Command.PublicKey == w.PublicKey &&
		json.Unmarshal([]byte(envelope.Command.Data), &data) == nil &&
		data.Agent == agent && data.Kind == kind && data.Value == value && data.Nonce == w.Nonce && data.Verdict == w.Verdict
}

// provenLink gives owner an ed25519 link to other's key with other's signed
// proof attached, so it reads proof_attached.
func provenLink(t *testing.T, s *Store, owner, other ed25519.PrivateKey, nonce string) string {
	t.Helper()
	value := pubKey(other)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(other, []byte(LinkStatement("swarmmemo.com", keyID(owner), value))))
	if nonce != "" {
		run(t, s, challengeLink(owner, "ed25519", value, nonce, "", proof))
	} else {
		run(t, s, linkCommand(owner, "identity.link", "ed25519", value, proof))
	}
	return value
}

func linkOf(t *testing.T, s *Store, key ed25519.PrivateKey, kind string) IdentityLink {
	t.Helper()
	for _, l := range agentLinks(t, s, key).Links {
		if l.Kind == kind {
			return l
		}
	}
	t.Fatalf("no %s link", kind)
	return IdentityLink{}
}

func TestWitnessProvenLink(t *testing.T) {
	s, _ := linkTest(t)
	a, other, b := keyFor(160), keyFor(161), keyFor(162)
	registerAll(t, s, a, b)
	nonce := "witness-b-nonce-0123456789"
	// A links its other key fresh for B: B's nonce is in A's challenge.
	value := provenLink(t, s, a, other, nonce)
	if l := linkOf(t, s, a, "ed25519"); l.Witnessed != 0 || l.Witnesses != nil {
		t.Fatalf("an unwitnessed link: %+v", l)
	}
	res := run(t, s, witnessCommand(b, keyID(a), "ed25519", value, nonce, "verified"))
	if res.Data["verdict"] != "verified" || res.Data["link_state"] != "proof_attached" || res.Data["replaced"] != false || res.Data["agent"] != keyID(a) {
		t.Fatalf("witness result: %v", res.Data)
	}
	l := linkOf(t, s, a, "ed25519")
	if l.Witnessed != 1 || len(l.Witnesses) != 1 {
		t.Fatalf("witnessed link: %+v", l)
	}
	w := l.Witnesses[0]
	if w.Fingerprint != keyID(b) || w.PublicKey != pubKey(b) || w.Verdict != "verified" || w.Nonce != nonce || w.At != testTime || !verifyWitness(w, keyID(a), "ed25519", value) {
		t.Fatalf("witness shape: %+v", w)
	}
	// The witness's nonce is the link's challenge nonce: made fresh for B.
	if l.Challenge == nil || l.Challenge.Nonce != w.Nonce || !verifyChallenge(l.Challenge, pubKey(a)) {
		t.Fatalf("challenge: %+v", l.Challenge)
	}
	// Offline verification fails on any edit a relay could make.
	for name, edit := range map[string]func(*LinkWitness){
		"other verdict": func(w *LinkWitness) { w.Verdict = "failed" },
		"other nonce":   func(w *LinkWitness) { w.Nonce = "someone-else-nonce-0123" },
		"edited bytes":  func(w *LinkWitness) { w.SignedPayload = strings.Replace(w.SignedPayload, "verified", "failed", 1) },
		"flipped sig":   func(w *LinkWitness) { w.Signature = flipLast(w.Signature) },
		"other key": func(w *LinkWitness) {
			w.PublicKey = pubKey(other)
			w.Fingerprint = keyID(other)
		},
	} {
		c := w
		edit(&c)
		if verifyWitness(c, keyID(a), "ed25519", value) {
			t.Errorf("%s verified", name)
		}
	}
	if verifyWitness(w, keyID(b), "ed25519", value) {
		t.Error("a witness verified for another agent")
	}
	// The directory carries the count, not the records.
	list := run(t, s, Command{Operation: "agents.list", Data: ""})
	for _, agent := range list.Agents {
		if agent.ID != keyID(a) {
			continue
		}
		for _, l := range agent.Links {
			if l.Kind == "ed25519" && (l.Witnessed != 1 || l.Witnesses != nil) {
				t.Fatalf("directory link: %+v", l)
			}
		}
	}
	// A failed verdict is shown but does not count toward two-party.
	c := keyFor(163)
	register(t, s, c)
	run(t, s, witnessCommand(c, keyID(a), "ed25519", value, "witness-c-nonce-0123456789", "failed"))
	l = linkOf(t, s, a, "ed25519")
	if l.Witnessed != 1 || len(l.Witnesses) != 2 || l.Witnesses[0].Fingerprint != keyID(c) || l.Witnesses[0].Verdict != "failed" {
		t.Fatalf("failed witness: %+v", l)
	}
}

func TestWitnessRefusals(t *testing.T) {
	s, dns := linkTest(t)
	a, other, b := keyFor(164), keyFor(165), keyFor(166)
	registerAll(t, s, a, b)
	value := provenLink(t, s, a, other, "")
	nonce := "witness-nonce-0123456789"

	// Self-witness.
	fails(t, s, witnessCommand(a, keyID(a), "ed25519", value, nonce, "verified"), "self_witness")
	// One of A's own linked keys: here the very key the link names.
	register(t, s, other)
	fails(t, s, witnessCommand(other, keyID(a), "ed25519", value, nonce, "verified"), "self_witness")
	// A key that A lists only as a claim is still A's own.
	claimedKey := keyFor(167)
	register(t, s, claimedKey)
	run(t, s, linkCommand(a, "identity.link", "ed25519", pubKey(claimedKey)))
	fails(t, s, witnessCommand(claimedKey, keyID(a), "ed25519", value, nonce, "verified"), "self_witness")
	// And a key that lists A's key as its own.
	mirror := keyFor(168)
	register(t, s, mirror)
	run(t, s, linkCommand(mirror, "identity.link", "ed25519", pubKey(a)))
	fails(t, s, witnessCommand(mirror, keyID(a), "ed25519", value, nonce, "verified"), "self_witness")

	// An unlinked target, an unknown agent, and claim-only links.
	fails(t, s, witnessCommand(b, keyID(a), "ed25519", pubKey(keyFor(169)), nonce, "verified"), "link_not_found")
	fails(t, s, witnessCommand(b, strings.Repeat("ab", 32), "ed25519", value, nonce, "verified"), "link_not_found")
	fails(t, s, witnessCommand(b, keyID(a), "ed25519", pubKey(claimedKey), nonce, "verified"), "link_not_witnessable")
	run(t, s, linkCommand(a, "identity.link", "url", "https://example.org/a"))
	fails(t, s, witnessCommand(b, keyID(a), "url", "https://example.org/a", nonce, "verified"), "link_not_witnessable")
	run(t, s, linkCommand(a, "identity.link", "domain", "example.org"))
	fails(t, s, witnessCommand(b, keyID(a), "domain", "example.org", nonce, "verified"), "link_not_witnessable")
	// A verified domain can be witnessed, by its canonical or raw value.
	dns.answers["_swarmmemo.example.org."] = []string{IdentityLinkTXTPrefix + keyID(a)}
	advance(t, s, testTime)
	run(t, s, witnessCommand(b, keyID(a), "domain", "Example.ORG", nonce, "verified"))
	if l := linkOf(t, s, a, "domain"); l.State != "verified" || l.Witnessed != 1 || len(l.Witnesses) != 1 {
		t.Fatalf("witnessed domain: %+v", l)
	}
	// A lapsed link keeps its witnesses but is no longer two-party.
	if _, err := s.db.Exec("UPDATE identity_links SET state='lapsed',lapsed_at=? WHERE agent=? AND kind='domain'", testTime, keyID(a)); err != nil {
		t.Fatal(err)
	}
	if l := linkOf(t, s, a, "domain"); l.Witnessed != 0 || len(l.Witnesses) != 1 {
		t.Fatalf("lapsed witnessed domain: %+v", l)
	}

	// Strict data and signing.
	for _, bad := range []string{
		`{"schema":1,"agent":"` + keyID(a) + `","kind":"ed25519","value":"` + value + `","nonce":"` + nonce + `"}`,
		`{"schema":1,"agent":"` + keyID(a) + `","kind":"ed25519","value":"` + value + `","nonce":"short","verdict":"verified"}`,
		`{"schema":1,"agent":"` + keyID(a) + `","kind":"ed25519","value":"` + value + `","nonce":"` + nonce + `","verdict":"maybe"}`,
		`{"schema":1,"agent":"` + keyID(a) + `","kind":"ed25519","value":"` + value + `","nonce":"` + nonce + `","verdict":"verified","extra":1}`,
		`{"schema":2,"agent":"` + keyID(a) + `","kind":"ed25519","value":"` + value + `","nonce":"` + nonce + `","verdict":"verified"}`,
		`{"schema":1,"agent":"A","kind":"ed25519","value":"` + value + `","nonce":"` + nonce + `","verdict":"verified"}`,
		`{"schema":1,"agent":"` + keyID(a) + `","kind":"gpg","value":"` + value + `","nonce":"` + nonce + `","verdict":"verified"}`,
	} {
		fails(t, s, signed(b, Command{Operation: "identity.witness", Data: bad}), "invalid_witness")
	}
	fails(t, s, Command{Operation: "identity.witness", Data: `{}`}, "signature_required")
	if n := sqlCount(t, s, "SELECT count(*) FROM link_witnesses"); n != 1 {
		t.Fatalf("refusals wrote %d witnesses", n)
	}
}

func TestWitnessReplaceKeepsHistoryAndRateLimit(t *testing.T) {
	s, _ := linkTest(t)
	a, other, b := keyFor(170), keyFor(171), keyFor(172)
	registerAll(t, s, a, b)
	value := provenLink(t, s, a, other, "")
	for i := 0; i < IdentityWitnessesPerDay; i++ {
		verdict := "verified"
		if i == 0 {
			verdict = "failed"
		}
		res := run(t, s, witnessCommand(b, keyID(a), "ed25519", value, fmt.Sprintf("witness-nonce-%06d", i), verdict))
		if res.Data["replaced"] != (i > 0) {
			t.Fatalf("witness %d replaced: %v", i, res.Data)
		}
	}
	// One current record, the newest; every older one is kept, superseded.
	l := linkOf(t, s, a, "ed25519")
	last := fmt.Sprintf("witness-nonce-%06d", IdentityWitnessesPerDay-1)
	if len(l.Witnesses) != 1 || l.Witnesses[0].Nonce != last || l.Witnessed != 1 || !verifyWitness(l.Witnesses[0], keyID(a), "ed25519", value) {
		t.Fatalf("current witness: %+v", l)
	}
	if kept := sqlCount(t, s, "SELECT count(*) FROM link_witnesses WHERE witness=? AND superseded_at>0", keyID(b)); kept != IdentityWitnessesPerDay-1 {
		t.Fatalf("history kept %d", kept)
	}
	// The per-key daily limit.
	fails(t, s, witnessCommand(b, keyID(a), "ed25519", value, "witness-nonce-over-limit", "verified"), "witness_limit")
	// Unlinking keeps every record; relinking starts with no witnesses.
	run(t, s, linkCommand(a, "identity.unlink", "ed25519", value))
	value = provenLink(t, s, a, other, "")
	if l := linkOf(t, s, a, "ed25519"); l.Witnessed != 0 || l.Witnesses != nil {
		t.Fatalf("a relinked link shows old witnesses: %+v", l)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM link_witnesses"); n != IdentityWitnessesPerDay {
		t.Fatalf("records after unlink: %d", n)
	}
	// The limit resets the next UTC day.
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	next := witnessCommand(b, keyID(a), "ed25519", value, "witness-nonce-next-day", "verified")
	run(t, s, signed(b, Command{Operation: next.Operation, Data: next.Data, Timestamp: testTime + 86400}))
	if l := linkOf(t, s, a, "ed25519"); l.Witnessed != 1 || len(l.Witnesses) != 1 || l.Witnesses[0].Nonce != "witness-nonce-next-day" {
		t.Fatalf("next day: %+v", l)
	}
}

func TestWitnessesShownAreCapped(t *testing.T) {
	s, _ := linkTest(t)
	a, other := keyFor(173), keyFor(174)
	register(t, s, a)
	value := provenLink(t, s, a, other, "")
	for i := 0; i < IdentityLinkWitnessesShown+2; i++ {
		w := keyFor(byte(180 + i))
		register(t, s, w)
		run(t, s, witnessCommand(w, keyID(a), "ed25519", value, fmt.Sprintf("capped-nonce-%06d", i), "verified"))
	}
	l := linkOf(t, s, a, "ed25519")
	if len(l.Witnesses) != IdentityLinkWitnessesShown || l.Witnessed != IdentityLinkWitnessesShown+2 || l.Witnesses[0].Fingerprint != keyID(keyFor(byte(180+IdentityLinkWitnessesShown+1))) {
		t.Fatalf("capped: %d shown, witnessed %d", len(l.Witnesses), l.Witnessed)
	}
}
