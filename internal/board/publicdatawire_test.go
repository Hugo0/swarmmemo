package board

import (
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
	"swarmmemo/internal/services/servicestest"
)

// public_data is off unless SERVICES names it; its key directory defaults to
// /etc/swarmmemo/keys and must be absolute; a dataset whose key file is
// missing lists as unavailable and a fetch of it answers service_unavailable,
// spending nothing and sending nothing.
func TestPublicDataWiring(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	f, err := ParseFeatures(env(map[string]string{"SERVICES": "public_data"}))
	if err != nil || f.PublicDataKeyDir != services.PublicDataKeyDir {
		t.Fatalf("default key directory: %+v %v", f, err)
	}
	if _, err = ParseFeatures(env(map[string]string{"SERVICES": "public_data", "PUBLIC_DATA_KEY_DIR": "keys"})); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative key directory must be refused: %v", err)
	}
	if f, err = ParseFeatures(env(map[string]string{"SERVICES": "memory", "PUBLIC_DATA_KEY_DIR": "keys"})); err != nil || f.PublicDataKeyDir != "" {
		t.Fatalf("PUBLIC_DATA_KEY_DIR is ignored while public_data is off: %+v %v", f, err)
	}

	s := openTest(t, Config{Features: Features{Services: []string{"public_data"}, PublicDataKeyDir: t.TempDir()}})
	meter := servicestest.NewMeter(1 << 20)
	s.UseServiceMeter(meter, &servicestest.Params{})
	t.Cleanup(s.stopServices)
	list := run(t, s, Command{Operation: "services.list"})
	svcs, _ := svcField(t, list.Data, "services").([]any)
	if len(svcs) != 1 || svcField(t, svcs[0], "id") != "public_data" || svcField(t, svcs[0], "network") != true {
		t.Fatalf("catalogue: %+v", list.Data)
	}
	read := run(t, s, svcRead(nil, "public_data", "datasets", map[string]any{}))
	if ds, _ := svcField(t, read.Data, "result", "datasets").([]any); len(ds) != 10 {
		t.Fatalf("datasets read: %+v", read.Data)
	}
	args := map[string]any{"dataset": "congress_bills", "params": map[string]any{"query": "shutdown"}}
	fails(t, s, svcCall(keyFor(1), "public_data", "fetch", args, 100, "pd-1"), "service_unavailable")
	for _, e := range svcSpends(t, s, meter) {
		if e.Kind == "hold" && e.State != "refunded" {
			t.Fatalf("nothing may be charged: %+v", e)
		}
	}
	// Its per-caller limits follow the board's own tier classifier (Design 0
	// here), not the provider's signed-tier fallback.
	k := keyFor(7)
	d0Identity(t, s, k)
	if err := s.GrantTier(testContext, keyID(k), 2, "proven test"); err != nil {
		t.Fatal(err)
	}
	st, err := s.serviceDeps().Classifier.Classify(testContext, s.db, allowance.Subject{ID: keyID(k), Signed: true, KeyID: keyID(k)}, s.now().Unix())
	if err != nil || st.Tier != allowance.TierProven {
		t.Fatalf("public_data classifier: %+v %v", st, err)
	}
}
