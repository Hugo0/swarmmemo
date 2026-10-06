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
	// Pastes and docs: the screening mode, and a content domain apart from the board's.
	content, err := ParseFeatures(env(map[string]string{"SERVICES": "paste,docs", "CONTENT_SCREEN": "forced", "CONTENT_URL": "https://swarmmemo-content.net"}))
	if err != nil || content.ContentScreen != "forced" || content.ContentURL != "https://swarmmemo-content.net" {
		t.Fatalf("content flags: %+v %v", content, err)
	}
	if plain, _ := ParseFeatures(env(map[string]string{"SERVICES": "paste"})); plain.ContentURL != "" || plain.ContentScreen != "default_on" {
		t.Fatalf("public links are off and screening on by default: %+v", plain)
	}
	for _, bad := range []string{"http://swarmmemo-content.net", "https://swarmmemo-content.net/", "https://swarmmemo-content.net/p", "https://swarmmemo.com", "https://files.swarmmemo.com", "https://u@x.net", "swarmmemo-content.net", "https://x.net?a=1"} {
		if _, err := ParseFeatures(env(map[string]string{"SERVICES": "paste", "CONTENT_URL": bad})); err == nil {
			t.Errorf("CONTENT_URL=%s must be refused", bad)
		}
	}
	if _, err := ParseFeatures(env(map[string]string{"SERVICES": "docs", "CONTENT_SCREEN": "sometimes"})); err == nil {
		t.Error("CONTENT_SCREEN=sometimes must be refused")
	}
}
