package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Every successful write carries the allowance note with the ledger on, its
// remaining falling by exactly what the write charged; an exact retry and a
// delegated command carry none (C36). Far from the limit its line is empty:
// tools are free, and the line is for a caller near its limit (2026-10-21).
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
		if charged <= 0 || res.Allowance.Remaining != last.Remaining-charged || res.Allowance.Line != "" || res.Allowance.Resource != "post_bytes" {
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

// Near the limit the line comes back, a fact on the write (never a tip:
// allowance_today is retired); at the limit the refusal says what happened,
// when it resets and how to get more. An allowance read always carries the
// line.
func TestAllowanceLineNearTheLimit(t *testing.T) {
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{Features: Features{Ledger: LedgerOn}})
	p := s.allowanceDefaults()
	rp := p.Resources[allowance.PostBytes]
	rp.Floor, rp.Cap, rp.FloorSlicePPM = 20_000, 20_000, 1_000_000
	if _, err := s.SetAllowanceParams(testContext, ledger.AllowanceNamespace, p.Marshal(), "near limit test", 0); err != nil {
		t.Fatal(err)
	}
	alice := keyFor(207)
	register(t, s, alice)
	if read := run(t, s, signed(alice, Command{Operation: "allowance.get"})); read.Allowance == nil || read.Allowance.Line == "" {
		t.Fatalf("an allowance read without its line: %+v", read.Allowance)
	}
	sawLine := false
	for i := 0; i < 100; i++ {
		c := signed(alice, Command{Operation: "post", Text: strings.Repeat("x", 200) + strconv.Itoa(i), RequestID: randomID()})
		res, err := s.Execute(testContext, c, "198.51.100.77")
		if err != nil {
			var be *Error
			if !errors.As(err, &be) || be.Code != "quota_exhausted" || !strings.Contains(be.Message, "00:00 UTC") || !strings.Contains(be.Message, "standing") || be.RetryAfter <= 0 {
				t.Fatalf("at the limit: %v", err)
			}
			if !sawLine {
				t.Fatal("the limit came without the line first")
			}
			return
		}
		a := res.Allowance
		near := AllowanceNearLimit(a.Remaining, a.Entitlement)
		if near != (a.Line != "") {
			t.Fatalf("remaining %d of %d: line %q", a.Remaining, a.Entitlement, a.Line)
		}
		if near && !sawLine {
			sawLine = true
			s.Nudge(testContext, c, &res, NudgeEnv{Base: "https://swarmmemo.com", Surface: "receipt", Source: "198.51.100.77"})
			if nudgeOf(res) == "allowance_today" {
				t.Fatal("near the limit, the retired allowance_today tip")
			}
		}
	}
	t.Fatal("never reached the limit")
}

func TestAllowanceNearLimit(t *testing.T) {
	for _, c := range []struct {
		remaining, total int64
		near             bool
	}{{100, 100, false}, {20, 100, false}, {19, 100, true}, {0, 100, true}, {0, 0, true}} {
		if AllowanceNearLimit(c.remaining, c.total) != c.near {
			t.Errorf("%d of %d", c.remaining, c.total)
		}
	}
}
