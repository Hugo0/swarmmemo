package board

import (
	"reflect"
	"testing"
)

func TestParseFeatures(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	off, err := ParseFeatures(env(nil))
	if err != nil || !reflect.DeepEqual(off, Features{}) {
		t.Fatalf("unset flags must be the zero Features (all off): %+v %v", off, err)
	}
	on, err := ParseFeatures(env(map[string]string{"RESERVED_HANDLES": "true", "ALLOWANCE_LEDGER": "shadow", "TRUST": "allocation", "SERVICES": "memory, echo,memory", "VOTE_RECORDS": "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if !on.ReservedHandles || on.Ledger != LedgerShadow || on.Trust != TrustAllocation || on.VoteRecords || !reflect.DeepEqual(on.Services, []string{"echo", "memory"}) || !on.ServiceEnabled("memory") {
		t.Fatalf("flags parsed wrongly: %+v", on)
	}
	for key, value := range map[string]string{"ANON_PREFIX": "1", "ALLOWANCE_LEDGER": "yes", "TRUST": "on", "SERVICES": "search"} {
		if _, err := ParseFeatures(env(map[string]string{key: value})); err == nil {
			t.Errorf("%s=%s must be refused, not read as off", key, value)
		}
	}
}
