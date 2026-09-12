package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCanonicalToolPreservesVersionAndRejectsAmbiguity(t *testing.T) {
	raw, err := os.ReadFile("../../clients/python/signing-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Command   json.RawMessage `json:"command"`
		Canonical string          `json:"canonical"`
	}
	if err = json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	actual, err := canonicalInput(strings.NewReader(string(vector.Command)), "swarmmemo.com")
	if err != nil || string(actual) != vector.Canonical {
		t.Fatal("v1 canonical vector changed", err)
	}
	context := `{"schema":1,"grant_id":"` + strings.Repeat("a", 64) + `","generation":"` + strings.Repeat("b", 32) + `"}`
	actual, err = canonicalInput(strings.NewReader(`{"operation":"post","delegation":`+context+`}`), "swarmmemo.com")
	if err != nil || !strings.HasPrefix(string(actual), `{"version":2,`) || !strings.Contains(string(actual), `"delegation":`+context) {
		t.Fatal("v2 canonical context lost", err)
	}
	for _, input := range []string{
		`{"operation":"post","delegation":null}`, `{"operation":"post","delegation":{}}`,
		`{"operation":"post","operation":"message.get"}`, `{"Operation":"post"}`,
		`{"operation":"post"} {"operation":"message.get"}`, `{"operation":"post","extra":1}`,
		`{"operation":"post","delegation":` + strings.Replace(context, `"schema":1`, `"schema":1,"schema":1`, 1) + `}`,
		strings.Repeat(" ", (2<<20)+1),
	} {
		if output, err := canonicalInput(strings.NewReader(input), "swarmmemo.com"); err == nil || output != nil {
			t.Fatal("ambiguous input produced signable bytes")
		}
	}
}
