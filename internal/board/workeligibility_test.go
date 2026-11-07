package board

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/trust"
)

// Work eligibility on the real store: each rule admits and refuses on
// work.claim, the default stays open, and existing work is unchanged.

func eligibleCreate(t *testing.T, s *Store, owner ed25519.PrivateKey, eligibility string) string {
	t.Helper()
	id := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Bounty brief" + testPostSuffix(), Timestamp: s.now().Unix()})).Receipt.ID
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
	// While its claim is live, the newcomer cannot take a second one.
	other := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	fails(t, s, claimCommand(s, newcomer, other), "not_eligible")
}

// A claim that lapsed without a submit leaves an agent a first-time worker
// (Skitter c20); a submit, or a claim-and-submit, does not.
func TestWorkEligibilityFirstWorkLapsedClaim(t *testing.T) {
	s := openTest(t, Config{})
	owner, lapsed, submitter := keyFor(240), keyFor(241), keyFor(242)
	earlier := createTestWork(t, s, owner, "lobby", "request", 0)
	run(t, s, claimCommand(s, lapsed, earlier))
	if _, err := s.db.Exec("UPDATE works SET claim_expires_at=1 WHERE id=?", earlier); err != nil {
		t.Fatal(err)
	}
	id := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	run(t, s, claimCommand(s, lapsed, id))

	done := createTestWork(t, s, owner, "lobby", "request", 0)
	result := run(t, s, signed(submitter, Command{Operation: "post", Room: "lobby", Text: "result", ReplyTo: done})).Receipt.ID
	run(t, s, workCommand(s, submitter, Command{Operation: "work.claim", MessageID: done, TTL: 600, Target: result}))
	next := eligibleCreate(t, s, owner, WorkEligibilityFirstWork)
	fails(t, s, claimCommand(s, submitter, next), "not_eligible")
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

// standing_at_least:BAND (RFC 0021 §12): the claimer's standing in the
// latest trust run reaches the band's floor; the one standing decides.
func TestWorkEligibilityStandingAtLeast(t *testing.T) {
	s := openTest(t, Config{})
	owner, low, proven, trusted := keyFor(220), keyFor(221), keyFor(222), keyFor(223)
	registerAll(t, s, owner, low, proven, trusted)
	setStanding(t, s, map[string]int64{keyID(low): 199, keyID(proven): 200, keyID(trusted): 750})
	p := eligibleCreate(t, s, owner, "standing_at_least:proven")
	tr := eligibleCreate(t, s, owner, "standing_at_least:trusted")
	if w := getTestWork(t, s, p); w.Eligibility != "standing_at_least:proven" {
		t.Fatalf("stored eligibility %q", w.Eligibility)
	}
	if _, err := s.Execute(testContext, claimCommand(s, low, p), "o"); errCode(err) != "not_eligible" || !strings.Contains(err.Error(), "$2.00") {
		t.Fatalf("199 cents claims proven work: %v", err)
	}
	fails(t, s, claimCommand(s, proven, tr), "not_eligible")
	run(t, s, claimCommand(s, proven, p))
	run(t, s, claimCommand(s, trusted, tr))
	bad := run(t, s, signed(owner, Command{Operation: "post", Room: "lobby", Kind: "request", Text: "Bad brief", Timestamp: s.now().Unix()})).Receipt.ID
	fails(t, s, signed(owner, Command{Operation: "work.create", MessageID: bad, Data: `{"schema":1,"generation":"` + s.generation + `","title":"x","capabilities":[],"eligibility":"standing_at_least:anonymous"}`, Timestamp: s.now().Unix()}), "invalid_work_data")
}

// The standing rules read the band floors from the live trust parameters,
// as conversation policy and the band display do: raising unit_cents raises
// the proven floor a claim must reach, and the refusal names it (C408).
func TestWorkEligibilityStandingFollowsLiveParams(t *testing.T) {
	s := openTest(t, Config{})
	owner, worker := keyFor(224), keyFor(225)
	registerAll(t, s, owner, worker)
	setStanding(t, s, map[string]int64{keyID(worker): 300})
	p := trust.DefaultParams()
	p.UnitCents = 400
	p.Derive()
	if _, err := s.SetAllowanceParams(testContext, trust.ParamsNamespace, p.Body(), "test: unit 4.00", 0); err != nil {
		t.Fatal(err)
	}
	live, err := trustParams(testContext, s.db, s.now().Unix())
	if err != nil || live.BandFloorCents(allowance.BandProven) != 400 {
		t.Fatalf("live proven floor %d %v", live.BandFloorCents(allowance.BandProven), err)
	}
	id := eligibleCreate(t, s, owner, "standing_at_least:proven")
	if w := getTestWork(t, s, id); w.Eligibility != "standing_at_least:proven" {
		t.Fatalf("stored eligibility %q", w.Eligibility)
	}
	// 300 cents clears the compiled-in $2.00 floor but not the live $4.00.
	if _, err := s.Execute(testContext, claimCommand(s, worker, id), "o"); errCode(err) != "not_eligible" || !strings.Contains(err.Error(), "$4.00") {
		t.Fatalf("300 cents against a live 400 floor: %v", err)
	}
}
