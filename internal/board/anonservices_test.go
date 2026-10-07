package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/services"
)

// Service calls without a key, end to end through the store and the real
// ledger: an unsigned service.call of a method the catalogue marks anonymous
// is billed in credit to the caller's network (IPv4 /24, IPv6 /64), within
// that network's daily share and the anonymous tier's share of the budget,
// refused outright for every other method, switched off by the
// signed-services lever, and idempotent by request_id.

// openAnonServices opens a store with the notary and memory on the real
// ledger, keyed by network prefix, with a credit budget of 1,000: each
// network gets cap credits a day and the anonymous tier sharePPM of the
// budget.
func openAnonServices(t *testing.T, cap, sharePPM int64) *Store {
	t.Helper()
	c := updatesConfig()
	c.Features = Features{Services: []string{"notary", "memory"}, Ledger: LedgerOn, AnonPrefix: true}
	s := openTest(t, c)
	t.Cleanup(s.stopServices)
	setAnonCredit(t, s, cap, sharePPM)
	return s
}

func setAnonCredit(t *testing.T, s *Store, cap, sharePPM int64) {
	t.Helper()
	p := s.allowanceDefaults()
	rp := p.Resources[allowance.Credit]
	rp.Budget, rp.SpendCeiling, rp.InboundCap = 1000, 1000, 1000
	rp.Cap = []int64{400, 200, 100, cap}
	rp.Floor = []int64{16, 16, 16, cap}
	rp.RootCap = []int64{1000, 800, 400, cap}
	rp.ShareMaxPPM = []int64{1_000_000, 1_000_000, 1_000_000, sharePPM}
	if _, err := s.SetAllowanceParams(testContext, ledger.AllowanceNamespace, p.Marshal(), "anonymous credit test", 0); err != nil {
		t.Fatal(err)
	}
}

// stampFrom stamps text from source; a non-empty requestID is lengthened to
// the AnonymousRequestIDMin a call without a key takes (anonID).
func stampFrom(s *Store, source, text, requestID string, maxCost int64) (Result, error) {
	if requestID != "" {
		requestID = anonID(requestID)
	}
	c := Command{Operation: "service.call", Target: "notary", Data: svcData("stamp", map[string]any{"text": text}, maxCost), RequestID: requestID}
	return s.Execute(testContext, c, source)
}

// anonID is id lengthened to a request_id a call without a key takes.
func anonID(id string) string { return id + "-0123456789abcdef" }

func codeOf(err error) (string, int) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.Status
	}
	return fmt.Sprint(err), 0
}

// anonCreditSpent is what anonymous subjects spent in credit today.
func anonCreditSpent(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow("SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE resource='credit' AND kind IN ('spend','commit') AND account LIKE 'anon:%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnonServicesAllowlistAndBilling(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	r, err := stampFrom(s, "198.51.100.7", "hello", "a1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if svcField(t, r.Data, "call", "cost") != float64(1) || svcField(t, r.Data, "result", "receipt", "hash") == nil {
		t.Fatalf("stamp without a key: %+v", r.Data)
	}
	var subject string
	var tier int
	if err = s.db.QueryRow("SELECT subject,tier FROM allowance_claims WHERE resource='credit'").Scan(&subject, &tier); err != nil || !strings.HasPrefix(subject, "anon:") || tier != 4 || strings.Contains(subject, "198.51.100") {
		t.Fatalf("billed to the network's pseudonym in tier 4: %q %d %v", subject, tier, err)
	}
	// Every method the catalogue does not mark anonymous needs a key, and
	// the refusal names the ones that do not.
	_, err = s.Execute(testContext, Command{Operation: "service.call", Target: "memory", Data: svcData("put", map[string]any{"key": "k", "value": "v"}, 1000), RequestID: "m1"}, "198.51.100.7")
	var e *Error
	if !errors.As(err, &e) || e.Status != 401 || e.Code != "signature_required" || !strings.Contains(e.Message, "notary.stamp") {
		t.Fatalf("memory put without a key: %v", err)
	}
	// A request_id of the caller's own must be long enough (left out, one is made).
	if code, status := codeOf(func() error {
		_, err := s.Execute(testContext, Command{Operation: "service.call", Target: "notary", Data: svcData("stamp", map[string]any{"text": "short id"}, 1), RequestID: "short-id"}, "198.51.100.7")
		return err
	}()); code != "invalid_request" || status != 400 {
		t.Fatalf("short request_id: %s %d", code, status)
	}
	// max_cost below the price: refused, nothing spent.
	before := anonCreditSpent(t, s)
	if _, err = stampFrom(s, "198.51.100.7", "too cheap", "a2", 0); err == nil || !strings.Contains(err.Error(), "max_cost") {
		t.Fatalf("max_cost 0: %v", err)
	}
	if code, _ := codeOf(err); code != "price_exceeds_max" || anonCreditSpent(t, s) != before {
		t.Fatalf("price_exceeds_max spends nothing: %s", code)
	}
	// services.list says what an agent without a key gets.
	list := run(t, s, Command{Operation: "services.list"})
	nk, ok := list.Data["without_key"].(services.NoKey)
	if !ok || !nk.Available || nk.CreditsPerDay != 3 || nk.AllCreditsPerDay != 100 || nk.Line != "No key needed for the notary: 3 credits a day per network." || !strings.HasPrefix(nk.Example, "/call/notary/stamp?") {
		t.Fatalf("without_key: %+v", list.Data["without_key"])
	}
}

func TestAnonServicesPerNetworkShare(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	for i := 0; i < 3; i++ {
		if _, err := stampFrom(s, "198.51.100.7", fmt.Sprintf("a%d", i), fmt.Sprintf("a%d", i), 1); err != nil {
			t.Fatalf("stamp %d: %v", i, err)
		}
	}
	r, err := stampFrom(s, "198.51.100.7", "a3", "a3", 1)
	code, _ := codeOf(err)
	if code != "quota_exhausted" || !strings.Contains(err.Error(), "network") {
		t.Fatalf("past the network's share: %+v %v", r, err)
	}
	// Another address in the same /24 is the same network.
	if code, _ := codeOf(func() error { _, err := stampFrom(s, "198.51.100.250", "b", "b", 1); return err }()); code != "quota_exhausted" {
		t.Fatalf("same /24: %s", code)
	}
	// Another /24 has its own share.
	if _, err := stampFrom(s, "198.51.101.7", "c", "c", 1); err != nil {
		t.Fatalf("another /24: %v", err)
	}
	// IPv6: rotating through one /64 is one network, and so is moving to
	// another /64 of the same /48 (security review 1.21, M3).
	for i := 0; i < 3; i++ {
		if _, err := stampFrom(s, fmt.Sprintf("2001:db8:1:2::%x", i+1), fmt.Sprintf("v6-%d", i), fmt.Sprintf("v6-%d", i), 1); err != nil {
			t.Fatalf("v6 stamp %d: %v", i, err)
		}
	}
	if code, _ := codeOf(func() error { _, err := stampFrom(s, "2001:db8:1:2:ffff::9", "v6-x", "v6-x", 1); return err }()); code != "quota_exhausted" {
		t.Fatalf("a rotated address in the same /64: %s", code)
	}
	if code, _ := codeOf(func() error { _, err := stampFrom(s, "2001:db8:1:3::1", "v6-y", "v6-y", 1); return err }()); code != "quota_exhausted" {
		t.Fatalf("another /64 of the same /48: %s", code)
	}
	if _, err := stampFrom(s, "2001:db8:2:3::1", "v6-z", "v6-z", 1); err != nil {
		t.Fatalf("another /48: %v", err)
	}
	// Posting from the other /64 still has the /64's own share.
	if _, err := s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "from another /64"}, "2001:db8:1:3::1"); err != nil {
		t.Fatalf("posting from another /64: %v", err)
	}
}

func TestAnonServicesTierWideCap(t *testing.T) {
	// 3 a network, 10 for every network together (1% of 1,000).
	s := openAnonServices(t, 3, 10_000)
	// 23:00 UTC: the tier's whole day is released (it is released hour by
	// hour, security review 1.21, M3).
	s.now = func() time.Time { return time.Unix(testTime+23*3600, 0) }
	spent := 0
	var last error
	for n := 0; n < 20 && last == nil; n++ {
		if n == 2 {
			// Later still: spill has handed tier 4 the signed tiers'
			// unused water, far above its share.
			s.now = func() time.Time { return time.Unix(testTime+23*3600+1800, 0) }
		}
		for i := 0; i < 3 && last == nil; i++ {
			if _, last = stampFrom(s, fmt.Sprintf("203.0.%d.1", n), fmt.Sprintf("n%d-%d", n, i), fmt.Sprintf("n%d-%d", n, i), 1); last == nil {
				spent++
			}
		}
	}
	if code, _ := codeOf(last); code != "global_quota_exhausted" || spent != 10 || anonCreditSpent(t, s) != 10 {
		t.Fatalf("every network together: spent %d (ledger %d), last %v", spent, anonCreditSpent(t, s), last)
	}
	// Signed calls are not part of it.
	run(t, s, signed(keyFor(1), Command{Operation: "service.call", Target: "notary", Data: svcData("stamp", map[string]any{"text": "signed"}, 1), RequestID: "s1", Timestamp: testTime + 23*3600 + 1800}))
}

func TestAnonServicesLeverOff(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	if _, err := stampFrom(s, "198.51.100.7", "before", "l1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PullLever(testContext, LeverPull{Name: LeverSignedServices, Reason: "abuse review"}); err != nil {
		t.Fatal(err)
	}
	_, err := stampFrom(s, "198.51.100.7", "during", "l2", 1)
	if code, status := codeOf(err); code != "signed_only" || status != 403 {
		t.Fatalf("with signed-services pulled: %v", err)
	}
	list := run(t, s, Command{Operation: "services.list"})
	if nk := list.Data["without_key"].(services.NoKey); nk.Available || nk.Line != "" || !strings.Contains(nk.Why, "lever") {
		t.Fatalf("without_key while pulled: %+v", nk)
	}
	// Signed calls and anonymous posts go on.
	run(t, s, svcCall(keyFor(1), "notary", "stamp", map[string]any{"text": "signed"}, 1, "s1"))
	if _, err = s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: "still posting"}, "198.51.100.7"); err != nil {
		t.Fatalf("anonymous post with signed-services pulled: %v", err)
	}
	if r, err := s.LeverReport(testContext); err != nil || len(r.Log) == 0 || r.Log[0].Name != LeverSignedServices {
		t.Fatalf("the pull is public: %+v %v", r, err)
	}
	if _, err = s.ReleaseLever(testContext, LeverSignedServices, nil, "", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := stampFrom(s, "198.51.100.7", "after", "l3", 1); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestAnonServicesOffWithoutCredit(t *testing.T) {
	// Today's parameters (no anonymous credit): calls without a key are
	// refused before anything is parsed or reserved, and nothing
	// advertises them.
	s := openAnonServices(t, 0, 0)
	_, err := stampFrom(s, "198.51.100.7", "x", "o1", 1)
	if code, status := codeOf(err); code != "signed_only" || status != 403 {
		t.Fatalf("no anonymous credit: %v", err)
	}
	if nk := s.NoKey(testContext); nk.Available || nk.Line != "" || nk.Example != "" {
		t.Fatalf("NoKey without credit: %+v", nk)
	}
}

func TestAnonServicesIdempotentRetry(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	first, err := stampFrom(s, "198.51.100.7", "once", "r1", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The same call again, even from another address of the same network,
	// returns the first answer and is not charged again.
	again, err := stampFrom(s, "198.51.100.8", "once", "r1", 1)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(svcField(t, first.Data, "call"))
	b, _ := json.Marshal(svcField(t, again.Data, "call"))
	if string(a) != string(b) || anonCreditSpent(t, s) != 1 {
		t.Fatalf("retry: %s vs %s, spent %d", a, b, anonCreditSpent(t, s))
	}
	// A different call under the same request_id is a conflict.
	if code, _ := codeOf(func() error { _, err := stampFrom(s, "198.51.100.7", "other", "r1", 1); return err }()); code != "idempotency_conflict" {
		t.Fatalf("reused request_id: %s", code)
	}
	// Another network reusing r1 is a conflict too (C26): one request_id
	// without a key is one call, run and charged once, its answer kept for
	// the network that made it.
	if code, _ := codeOf(func() error { _, err := stampFrom(s, "203.0.113.9", "once", "r1", 1); return err }()); code != "idempotency_conflict" {
		t.Fatalf("another network, same request_id: %s", code)
	}
	if _, err := stampFrom(s, "203.0.113.9", "once", "r2", 1); err != nil {
		t.Fatalf("another network, new request_id: %v", err)
	}
	if anonCreditSpent(t, s) != 2 {
		t.Fatalf("spent %d", anonCreditSpent(t, s))
	}
}

// The public statistics split each service's spend between signed and
// anonymous callers, and show the anonymous tier's credit pool.
func TestAnonServicesVisibleInStats(t *testing.T) {
	s := openAnonServices(t, 3, 100_000)
	if _, err := stampFrom(s, "198.51.100.7", "anon", "v1", 1); err != nil {
		t.Fatal(err)
	}
	run(t, s, svcCall(keyFor(1), "notary", "stamp", map[string]any{"text": "signed"}, 1, "s1"))
	stats, err := s.AllowanceStats(testContext, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stats)
	var got struct {
		Services []struct {
			Service, Resource, Subjects string
			Units                       int64
		} `json:"services"`
	}
	_ = json.Unmarshal(raw, &got)
	seen := map[string]int64{}
	for _, sv := range got.Services {
		if sv.Service == "notary" && sv.Resource == "credit" {
			seen[sv.Subjects] += sv.Units
		}
	}
	if seen["anonymous"] != 1 || seen["signed"] != 1 {
		t.Fatalf("services by subjects: %s", raw)
	}
}
