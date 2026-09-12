package main

import "testing"

func TestReferenceConfigurationDefaultsDisabledAndRequiresExplicitOwner(t *testing.T) {
	for _, key := range []string{"REFERENCE_REGISTRY_PATH", "REFERENCE_SUPPRESSION_PATH", "REFERENCE_SNAPSHOT_PATH", "REFERENCE_OWNER_UID"} {
		t.Setenv(key, "")
	}
	if reader, err := referenceReaderFromEnvironment(); err != nil || reader != nil {
		t.Fatal("default not disabled", err)
	}
	t.Setenv("REFERENCE_REGISTRY_PATH", "/does-not-exist/registry.json")
	if _, err := referenceReaderFromEnvironment(); err == nil {
		t.Fatal("implicit owner accepted")
	}
	t.Setenv("REFERENCE_OWNER_UID", "0")
	if _, err := referenceReaderFromEnvironment(); err == nil {
		t.Fatal("partial configuration accepted")
	}
	t.Setenv("REFERENCE_SUPPRESSION_PATH", "/does-not-exist/suppressions.json")
	t.Setenv("REFERENCE_SNAPSHOT_PATH", "/does-not-exist/current.json")
	for _, bad := range []string{"", "-1", "01", "+1", "4294967296", "0x0"} {
		t.Setenv("REFERENCE_OWNER_UID", bad)
		if _, err := referenceReaderFromEnvironment(); err == nil {
			t.Fatal("invalid owner", bad)
		}
	}
	t.Setenv("REFERENCE_OWNER_UID", "0")
	if reader, err := referenceReaderFromEnvironment(); err != nil || reader == nil {
		t.Fatal("missing runtimefiles must not prevent server startup", err)
	}
}
