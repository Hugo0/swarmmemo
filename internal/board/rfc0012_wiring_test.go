package board

import (
	"slices"
	"testing"

	"swarmmemo/internal/trust"
)

// The integration of RFC0012's builders: one ledger for posting and
// services, one parameter store that validates every namespace, and
// classifier v1 under TRUST=allocation.
func TestRFC0012Wiring(t *testing.T) {
	s := openTest(t, Config{Features: Features{Services: []string{"memory", "echo"}}})
	if s.serviceMeter() != s.allowanceLedger() {
		t.Fatal("the services meter through a ledger of their own")
	}
	for _, tc := range []struct {
		namespace, good, bad string
		want                 int64
	}{
		{"services", `{"schema":1,"prices":{}}`, `{"schema":1,"prices":{"nope":{}}}`, 1},
		{"trust", string(trust.DefaultParams().Body()), `{"schema":1}`, trust.DefaultVersion + 1},
	} {
		if _, err := s.SetAllowanceParams(testContext, tc.namespace, []byte(tc.bad), "test", 0); err == nil {
			t.Fatalf("%s: an invalid body was stored", tc.namespace)
		}
		if v, err := s.SetAllowanceParams(testContext, tc.namespace, []byte(tc.good), "test", 0); err != nil || v != tc.want {
			t.Fatalf("%s: version %d, %v", tc.namespace, v, err)
		}
	}
	pv, err := s.AllowanceParams(testContext, "trust", trust.DefaultVersion)
	if err != nil || !pv.CompiledIn {
		t.Fatalf("trust version 1: %+v %v", pv, err)
	}
	p, err := trust.ParseParams(pv.Version, pv.Body)
	if err != nil || !slices.Contains(p.ServiceAccounts, "031d734fde4d37a59f39471fc4c452c32180bee8186844654177626d6ed0e774") || !slices.Contains(p.ServiceAccounts, "4de11d5d8e4ef9f822bb51b95a557687713f9977802caffac31f911663ccce18") {
		t.Fatalf("trust version 1 service accounts: %v %v", p.ServiceAccounts, err)
	}
	if _, ok := s.classifier().(design0Classifier); !ok {
		t.Fatal("classifier without TRUST=allocation is not Design 0")
	}
	alloc := openTest(t, Config{Features: Features{Trust: TrustAllocation}})
	if _, ok := alloc.classifier().(trustClassifier); !ok {
		t.Fatal("TRUST=allocation does not classify with trust")
	}
}
