package board

import (
	"crypto/ed25519"
	"testing"
)

// An agent's own page shows the work it is part of. "Part of" means requested or
// worked, and it follows the account rather than one key, so rotation does not
// hide an agent's own history from its own page.
func TestWorkListScopesToOneAgentAcrossRotation(t *testing.T) {
	s := openTest(t, Config{})
	requester, worker, bystander := keyFor(61), keyFor(62), keyFor(63)
	for _, key := range []ed25519.PrivateKey{requester, worker, bystander} {
		run(t, s, signed(key, Command{Operation: "agent.register"}))
	}
	mine := createTestWork(t, s, requester, "lobby", "request", 600)
	other := createTestWork(t, s, bystander, "lobby", "request", 600)
	run(t, s, workCommand(s, worker, Command{Operation: "work.claim", MessageID: mine, TTL: 600}))

	ids := func(target string) map[string]bool {
		seen := map[string]bool{}
		for _, w := range run(t, s, Command{Operation: "works.list", Target: target}).Data["works"].([]Work) {
			seen[w.ID] = true
		}
		return seen
	}
	if got := ids(keyID(requester)); !got[mine] || got[other] {
		t.Fatal("requester's page must show its own work and only its own")
	}
	if got := ids(keyID(worker)); !got[mine] || got[other] {
		t.Fatal("claimed work belongs on the worker's page too")
	}
	if got := ids(keyID(bystander)); got[mine] || !got[other] {
		t.Fatal("an unrelated agent's page borrowed someone else's work")
	}
	if len(run(t, s, Command{Operation: "works.list"}).Data["works"].([]Work)) != 2 {
		t.Fatal("the unscoped listing must still show every public work")
	}
	fails(t, s, Command{Operation: "works.list", Target: "not-a-fingerprint"}, "invalid_agent")
}
