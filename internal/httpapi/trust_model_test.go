package httpapi

import (
	"os"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
)

// The trust model paper (docs/TRUST_MODEL.md) is the one statement of the
// model: served as is at /trust-model.md, rendered at /trust-model, and
// pointed at by every surface that describes trust, so none drifts into a
// second statement.
func TestTrustModelIsTheSingleSource(t *testing.T) {
	src, err := os.ReadFile("../../docs/TRUST_MODEL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(publicdocs.TrustModel()) != string(src) {
		t.Fatal("the embedded trust model differs from docs/TRUST_MODEL.md")
	}
	s, _ := rfc0012Server(allOn)
	if w := makeRequest(s, "GET", "/trust-model.md", "", ""); w.Code != 200 || w.Body.String() != string(src) {
		t.Fatalf("/trust-model.md is not docs/TRUST_MODEL.md as is: %d", w.Code)
	}
	page := getHTML(t, s, "/trust-model")
	for _, want := range []string{"The SwarmMemo trust model", `id="md-endorsements-as-stakes"`, `id="md-why-not-pagerank"`, `href="/trust-model.md"`} {
		if !strings.Contains(page, want) {
			t.Errorf("/trust-model lacks %q", want)
		}
	}
	if !strings.Contains(getHTML(t, s, "/trust"), `href="/trust-model"`) {
		t.Error("/trust does not link the trust model")
	}
	if !strings.Contains(getHTML(t, s, "/docs"), `href="/trust-model"`) {
		t.Error("/docs does not link the trust model")
	}
	trust, _ := getJSON(t, s, "GET", "/capabilities", "")["trust"].(map[string]any)
	standing, _ := trust["standing"].(map[string]any)
	if trust["model"] != TrustModelPath || standing["model"] != TrustModelPath {
		t.Errorf("/capabilities trust does not point at %s: %v, %v", TrustModelPath, trust["model"], standing["model"])
	}
	if llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String(); !strings.Contains(llms, "/trust-model") {
		t.Error("/llms.txt does not point at the trust model")
	}
	if !strings.Contains(string(publicdocs.Glossary()), "https://swarmmemo.com/trust-model") {
		t.Error("the glossary does not link the trust model")
	}
}
