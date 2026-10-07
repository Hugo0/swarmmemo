package board

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/services/servicestest"
)

// The inference service is off unless SERVICES names it and INFERENCE_CONFIG
// is set; with a config but no key file it lists as unavailable and every
// call answers service_unavailable, spending nothing and sending nothing.
func TestInferenceWiring(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := ParseFeatures(env(map[string]string{"SERVICES": "inference"})); err == nil || !strings.Contains(err.Error(), "INFERENCE_CONFIG") {
		t.Fatalf("inference without a config must be refused: %v", err)
	}
	f, err := ParseFeatures(env(map[string]string{"SERVICES": "memory", "INFERENCE_CONFIG": "/x"}))
	if err != nil || f.InferenceConfig != "" {
		t.Fatalf("INFERENCE_CONFIG is ignored while inference is off: %+v %v", f, err)
	}

	dir := t.TempDir()
	if _, err = Open(filepath.Join(dir, "a.sqlite"), Config{Features: Features{Services: []string{"inference"}, InferenceConfig: filepath.Join(dir, "missing.json")}}); err == nil {
		t.Fatal("a missing INFERENCE_CONFIG must stop the store opening")
	}
	cfg := filepath.Join(dir, "inference.json")
	body := `{"schema":1,"upstreams":[{"name":"groq","kind":"openai_compat","base_url":"https://api.example.invalid/openai/v1","key_file":"` + filepath.Join(dir, "no-key") + `","daily_spend_cap":1000,
 "models":{"m":{"price":{"base":1,"input_per_mtok":0,"output_per_mtok":0},"max_output_tokens":16}}}],"models":{"small":[{"upstream":"groq","model":"m"}]}}`
	if err = os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openTest(t, Config{Features: Features{Services: []string{"inference"}, InferenceConfig: cfg}})
	meter := servicestest.NewMeter(1 << 20)
	s.UseServiceMeter(meter, &servicestest.Params{})
	t.Cleanup(s.stopServices)
	list := run(t, s, Command{Operation: "services.list"})
	services, _ := svcField(t, list.Data, "services").([]any)
	// The service, then tools, the one search and call over it.
	if len(services) != 2 || svcField(t, services[0], "id") != "inference" || svcField(t, services[1], "id") != "tools" || svcField(t, services[0], "available") != false || svcField(t, services[0], "network") != true {
		t.Fatalf("catalogue: %+v", list.Data)
	}
	args := map[string]any{"model": "small", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	fails(t, s, svcCall(keyFor(1), "inference", "complete", args, 100, "inf-1"), "service_unavailable")
	for _, e := range svcSpends(t, s, meter) {
		if e.Kind == "hold" && e.State != "refunded" {
			t.Fatalf("nothing may be charged: %+v", e)
		}
	}
}
