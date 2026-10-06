package board

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
)

// Spend limits on worker keys, on the real store and ledger: set at
// delegation.create, enforced at the ledger's reserve for the key's actor
// (subject carries the credential) while the owner key is unaffected,
// changed only by the owner, and read back by the owner and the key.

func spendLimitStore(t *testing.T) *Store {
	t.Helper()
	if _, _, ok := ledgerTestOverride(); ok {
		t.Skip("the ledger mode is overridden for the whole suite")
	}
	s := openTest(t, Config{Features: Features{Ledger: LedgerOn}})
	p := s.allowanceDefaults()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 10_000, 10_000, 10_000
	rp.Cap = []int64{1000, 1000, 1000, 10}
	rp.Floor = []int64{500, 500, 500, 10}
	rp.RootCap = []int64{5000, 5000, 5000, 10}
	if _, err := s.SetAllowanceParams(testContext, ledger.AllowanceNamespace, p.Marshal(), "spend limit test", 0); err != nil {
		t.Fatal(err)
	}
	return s
}

func limitedGrant(s *Store, parent, child ed25519.PrivateKey, room string, limit map[string]any) Command {
	data, _ := json.Marshal(map[string]any{"schema": 1, "generation": s.generation, "operations": []string{"post", "messages.list"}, "disclosure": "public", "spend_limit": limit})
	c := signed(parent, Command{Operation: "delegation.create", Room: room, Target: base64.RawURLEncoding.EncodeToString(child.Public().(ed25519.PublicKey)), TTL: 3600, Amount: 1 << 20, Data: string(data), Timestamp: s.now().Unix()})
	c.Proof = base64.RawURLEncoding.EncodeToString(ed25519.Sign(child, Canonical(s.config.ServiceID, c)))
	return c
}

func limitCommand(s *Store, key ed25519.PrivateKey, target string, limit map[string]any) Command {
	limit["schema"] = 1
	data, _ := json.Marshal(limit)
	return signed(key, Command{Operation: "spend_limit.set", Target: target, Data: string(data), Timestamp: s.now().Unix()})
}

// reserveAs reserves credit as the actor a command of a would be (the
// ledger's subject(a)) and settles it in full, in one transaction.
func reserveAs(s *Store, a actor, units int64, key string) error {
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	h, err := s.ledger.led.Reserve(testContext, tx, subject(a), allowance.Credit, units, key, ledger.Ref{Service: "fetch", Op: "service.call", Method: "page"}, 60, s.now().Unix())
	if err != nil {
		return fromAllowance(err)
	}
	// The call settles at its maximum, so it holds no slot.
	if _, err = s.ledger.led.Commit(testContext, tx, h.ID, units, s.now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func TestWorkerKeySpendLimit(t *testing.T) {
	s := spendLimitStore(t)
	parent, child := keyFor(171), keyFor(172)
	owner, grant := keyID(parent), keyID(child)
	run(t, s, signed(parent, Command{Operation: "room.create", Room: "limit-room", Visibility: "public"}))
	ack := run(t, s, limitedGrant(s, parent, child, "limit-room", map[string]any{"credit_per_day": 100, "credit_per_call": 60})).Data["ack"].(DelegationAck)
	if ack.SpendLimit == nil || *ack.SpendLimit.CreditPerDay != 100 || *ack.SpendLimit.CreditPerCall != 60 {
		t.Fatalf("ack: %+v", ack)
	}
	// The key's commands resolve to this actor (resolveDelegation).
	worker := actor{id: grant, account: owner, signed: true, grant: &delegationRow{ID: grant}, credential: credentialKeyPrefix + grant}
	root := actor{id: owner, account: owner, signed: true}

	if err := reserveAs(s, worker, 61, "big"); !spendLimitIs(err, "credit_per_call 60") {
		t.Fatalf("over the per-call limit: %v", err)
	}
	if err := reserveAs(s, worker, 60, "a"); err != nil {
		t.Fatal(err)
	}
	err := reserveAs(s, worker, 41, "b")
	var e *Error
	if !spendLimitIs(err, "credit_per_day 100") || !errors.As(err, &e) || e.RetryAfter != untilMidnight(s.now().Unix()) {
		t.Fatalf("over the daily limit: %v", err)
	}
	if err = reserveAs(s, root, 500, "owner"); err != nil {
		t.Fatalf("the owner key is not limited: %v", err)
	}

	// The key cannot change its limit; the owner can.
	childLimit := childCommand(s, child, &DelegationContext{Schema: 1, GrantID: grant, Generation: s.generation}, Command{Operation: "spend_limit.set", Target: grant, Data: `{"schema":1,"credit_per_day":1000}`})
	fails(t, s, childLimit, "delegation_forbidden")
	fails(t, s, limitCommand(s, parent, grant, map[string]any{"expires_at": s.now().Unix() + 60}), "invalid_spend_limit")
	fails(t, s, limitCommand(s, parent, strings.Repeat("ab", 32), map[string]any{"credit_per_day": 1}), "not_found")
	fails(t, s, limitCommand(s, keyFor(173), grant, map[string]any{"credit_per_day": 1}), "not_found")
	raised := run(t, s, limitCommand(s, parent, grant, map[string]any{"credit_per_day": 200}))
	if v := raised.Data["spend_limit"].(*SpendLimitView); *v.CreditPerDay != 200 || v.CreditPerCall != nil || v.CreditSpentToday != 60 || *v.CreditRemainingToday != 140 {
		t.Fatalf("raised: %+v", v)
	}
	if err = reserveAs(s, worker, 100, "c"); err != nil {
		t.Fatalf("after the owner raised it: %v", err)
	}

	// Read back: the owner's list, and the key's own status.
	list := run(t, s, signed(parent, Command{Operation: "delegations.list", Timestamp: s.now().Unix()})).Data["delegations"].([]DelegationStatus)
	if len(list) != 1 || list[0].SpendLimit == nil || list[0].SpendLimit.CreditSpentToday != 160 || *list[0].SpendLimit.CreditRemainingToday != 40 {
		t.Fatalf("delegations.list: %+v", list)
	}
	own := run(t, s, childCommand(s, child, &DelegationContext{Schema: 1, GrantID: grant, Generation: s.generation}, Command{Operation: "delegation.get", Target: grant})).Data["delegation"].(DelegationStatus)
	if own.SpendLimit == nil || *own.SpendLimit.CreditRemainingToday != 40 {
		t.Fatalf("the key's own status: %+v", own)
	}
	// A stranger's public read does not show it.
	public := run(t, s, signed(keyFor(174), Command{Operation: "delegation.get", Target: grant, Timestamp: s.now().Unix()})).Data["delegation"].(DelegationRecord)
	if public.SpendLimit != nil {
		t.Fatal("the public grant record shows the spend limit")
	}

	// The next UTC day starts afresh.
	s.now = func() time.Time { return time.Unix(86400+1, 0) }
	if err = s.ledger.led.Mint(testContext, s.db, owner, allowance.Credit, allowance.Paid, 1000, "a top-up", s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err = reserveAs(s, worker, 150, "next-day"); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func spendLimitIs(err error, which string) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 429 && e.Code == "spend_limit" && strings.Contains(e.Message, which)
}
