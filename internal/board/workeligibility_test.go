package board

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"
)

// Work eligibility on the real store: each rule admits and refuses on
// work.claim, the default stays open, and existing work is unchanged.

func eligibleCreate(t *testing.T, s *Store, owner ed25519.PrivateKey, eligibility string) string {
	t.Helper()
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Bounty brief", Timestamp: s.now().Unix()})).Receipt.ID
	d := map[string]any{"schema": 1, "generation": s.generation, "title": "Bounty", "capabilities": []string{"review"}}
	if eligibility != "" {
		d["eligibility"] = eligibility
	}
	b, _ := json.Marshal(d)
	run(t, s, signed(owner, Command{Operation: "work.create", MessageID: id, Data: string(b), Timestamp: s.now().Unix()}))
	return id
}

func claimCommand(s *Store, key ed25519.PrivateKey, id string) Command {
	return workCommand(s, key, Command{Operation: "work.claim", MessageID: id, TTL: 600})
}

func TestWorkEligibilityDefaultOpenAndValidation(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(200), keyFor(201)
	id := eligibleCreate(t, s, owner, "")
	if w := getTestWork(t, s, id); w.Eligibility != WorkEligibilityOpen {
		t.Fatalf("default eligibility %q", w.Eligibility)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE id=? AND eligibility=''", id); n != 1 {
		t.Fatal("default work must store the legacy empty value")
	}
	run(t, s, claimCommand(s, worker, id))

	explicit := eligibleCreate(t, s, owner, "open")
	if n := sqlCount(t, s, "SELECT count(*) FROM works WHERE id=? AND eligibility=''", explicit); n != 1 {
		t.Fatal("explicit open must store like the default")
	}
	stored := eligibleCreate(t, s, owner, "linked")
	if w := getTestWork(t, s, stored); w.Eligibility != WorkEligibilityLinked {
		t.Fatalf("stored eligibility %q", w.Eligibility)
	}

	bad := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Bad brief", Timestamp: s.now().Unix()})).Receipt.ID
	for _, data := range []string{
		`{"schema":1,"generation":"` + s.generation + `","title":"x","capabilities":[],"eligibility":"vip"}`,
		`{"schema":1,"generation":"` + s.generation + `","title":"x","capabilities":[],"eligibility":""}`,
		`{"schema":1,"generation":"` + s.generation + `","title":"x","capabilities":[],"eligibility":1}`,
	} {
		fails(t, s, signed(owner, Command{Operation: "work.create", MessageID: bad, Data: data, Timestamp: s.now().Unix()}), "invalid_work_data")
	}
	// Eligibility is set once, at create: a claim cannot carry it.
	fails(t, s, signed(worker, Command{Operation: "work.claim", MessageID: stored, TTL: 600, Data: `{"schema":1,"generation":"` + s.generation + `","eligibility":"open"}`, Timestamp: s.now().Unix()}), "invalid_work_data")
}

func TestWorkEligibilityExistingWorkUnchanged(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(202), keyFor(203)
	id := createTestWork(t, s, owner, "lobby", "request", 0)
	// A row from before the column reads '' and stays open to anyone.
	if _, err := s.db.Exec("UPDATE works SET eligibility='' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if w := getTestWork(t, s, id); w.Eligibility != WorkEligibilityOpen {
		t.Fatalf("legacy eligibility %q", w.Eligibility)
	}
	run(t, s, claimCommand(s, worker, id))
}

func TestWorkEligibilityFirstWork(t *testing.T) {
	s := openTest(t, Config{})
	owner, veteran, newcomer := keyFor(204), keyFor(205), keyFor(206)
	earlier := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, claimCommand(s, veteran, earlier))

	id := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	fails(t, s, claimCommand(s, veteran, id), "not_eligible")
	run(t, s, claimCommand(s, newcomer, id))
	if w := getTestWork(t, s, id); w.Worker == nil || w.Worker.ID != keyID(newcomer) || w.Eligibility != WorkEligibilityFirstWork {
		t.Fatalf("first_work claim: %+v", w)
	}
	// Having claimed once, the newcomer is no longer a first-time worker.
	other := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	fails(t, s, claimCommand(s, newcomer, other), "not_eligible")
}

func TestWorkEligibilityLinked(t *testing.T) {
	s, _ := linkTest(t)
	owner, plain, proven, anchored, witness := keyFor(207), keyFor(208), keyFor(210), keyFor(211), keyFor(212)
	registerAll(t, s, owner, plain, proven, anchored, witness)
	id := eligibleCreate(t, s, owner, WorkEligibilityLinked)

	// No link, a claim-only link, and a sealing key do not qualify.
	fails(t, s, claimCommand(s, plain, id), "not_eligible")
	run(t, s, linkCommand(plain, "identity.link", "url", "https://other.example.org/agents/plain"))
	fails(t, s, claimCommand(s, plain, id), "not_eligible")
	member := sealMember(t, s, 213)
	fails(t, s, claimCommand(s, member.sign, id), "not_eligible")

	// A link with proof attached qualifies.
	provenLink(t, s, proven, keyFor(214), "")
	run(t, s, claimCommand(s, proven, id))

	// So does a claimed url another agent witnessed.
	second := eligibleCreate(t, s, owner, WorkEligibilityLinked)
	const page = "https://other.example.org/agents/anchored"
	run(t, s, linkCommand(anchored, "identity.link", "url", page))
	fails(t, s, claimCommand(s, anchored, second), "not_eligible")
	run(t, s, witnessCommand(witness, keyID(anchored), "url", page, "anchor-nonce-0123456789", "verified"))
	run(t, s, claimCommand(s, anchored, second))
}

func TestWorkEligibilityNewAgent(t *testing.T) {
	s := openTest(t, Config{})
	owner, old, fresh := keyFor(215), keyFor(216), keyFor(217)
	run(t, s, signedNow(s, old, Command{Operation: "agent.register"}))
	s.now = func() time.Time { return time.Unix(testTime+WorkNewAgentWindow+3600, 0) }
	run(t, s, signedNow(s, fresh, Command{Operation: "agent.register"}))
	id := eligibleCreate(t, s, owner, WorkEligibilityNewAgent)
	fails(t, s, claimCommand(s, old, id), "not_eligible")
	run(t, s, claimCommand(s, fresh, id))
}
