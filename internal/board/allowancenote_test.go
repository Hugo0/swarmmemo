package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

// Every successful write carries the allowance note with the ledger on, its
// remaining falling by exactly what the write charged; an exact retry and a
// delegated command carry none (C36).
func TestEveryWriteCarriesTheAllowanceNote(t *testing.T) {
	s := ledgerTest(t, LedgerOn)
	alice, requester, child := keyFor(201), keyFor(202), keyFor(203)
	register(t, s, alice)
	register(t, s, requester)
	run(t, s, signed(alice, Command{Operation: "room.create", Room: "note-room", Visibility: "public"}))
	used := func() int64 {
		return sqlCount(t, s, "SELECT coalesce(sum(used),0) FROM quota WHERE actor=?", keyID(alice))
	}
	last := run(t, s, signed(alice, Command{Operation: "quota.get"})).Allowance
	if last == nil {
		t.Fatal("quota.get carries no note")
	}
	work := createTestWork(t, s, requester, "note-room", "request", 7200)
	step := func(name string, c Command) Result {
		t.Helper()
		before := used()
		res := run(t, s, c)
		charged := used() - before
		if res.Allowance == nil {
			t.Fatalf("%s: no allowance note", name)
		}
		if charged <= 0 || res.Allowance.Remaining != last.Remaining-charged || res.Allowance.Line == "" || res.Allowance.Resource != "post_bytes" {
			t.Fatalf("%s: charged %d, remaining %d after %d", name, charged, res.Allowance.Remaining, last.Remaining)
		}
		last = res.Allowance
		return res
	}
	grant := grantCommand(s, alice, child, "note-room", 3600, 1<<20, grantOps())
	grant.RequestID, grant.Signature, grant.Proof = "grant-1", "", ""
	grant = signed(alice, grant)
	grant.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, grant)))
	step("delegation.create", grant)
	claim := workCommand(s, alice, Command{Operation: "work.claim", MessageID: work, TTL: 120, RequestID: "claim-1"})
	step("work.claim", claim)
	member := signed(alice, Command{Operation: "room.member.add", Room: "note-room", Target: keyID(requester), RequestID: "member-1"})
	step("room.member.add", member)
	// Exact retries answer the stored receipt, which never holds the note.
	for _, c := range []Command{grant, claim, member} {
		if res := run(t, s, c); res.Allowance != nil || (res.Next != nil && res.Next.Allowance != nil) {
			t.Fatalf("%s retry carries the note", c.Operation)
		}
	}
	// A delegated command gets none: its budget is the grant's ceiling.
	g := &DelegationContext{Schema: 1, GrantID: keyID(child), Generation: s.generation}
	if res := run(t, s, childCommand(s, child, g, Command{Operation: "post", Room: "note-room", Visibility: "public", Text: "from the worker"})); res.Allowance != nil {
		t.Fatal("a delegated post carries the issuer's balance")
	}
	// Reads other than the two allowance reads carry none.
	if res := run(t, s, signed(alice, Command{Operation: "messages.list", Room: "note-room"})); res.Allowance != nil {
		t.Fatal("a read carries the note")
	}
}

func TestNoAllowanceNoteWithTheLedgerOff(t *testing.T) {
	s := ledgerTest(t, LedgerOff)
	alice, requester, child := keyFor(204), keyFor(205), keyFor(206)
	register(t, s, alice)
	register(t, s, requester)
	work := createTestWork(t, s, requester, "lobby", "request", 7200)
	for _, c := range []Command{
		signed(alice, Command{Operation: "room.create", Room: "off-room", Visibility: "public"}),
		grantCommand(s, alice, child, "off-room", 3600, 1<<20, grantOps()),
		workCommand(s, alice, Command{Operation: "work.claim", MessageID: work, TTL: 120}),
	} {
		if res := run(t, s, c); res.Allowance != nil {
			t.Fatalf("%s carries a note with the ledger off", c.Operation)
		}
	}
}
