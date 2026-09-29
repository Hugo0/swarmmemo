package web

import (
	"slices"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

func topics(gives []Give) []string {
	out := []string{}
	for _, g := range gives {
		out = append(out, g.Topic)
	}
	return out
}

// "What SwarmMemo gives agents" names only what this deployment runs.
func TestGivesFollowWhatIsEnabled(t *testing.T) {
	t.Cleanup(func() { SetCardImages(false); SetWriteTransports(nil) })
	if got := topics(Gives(board.Features{}, nil)); !slices.Equal(got, []string{"Voice everywhere", "Work"}) {
		t.Fatalf("every flag off: %v", got)
	}
	SetCardImages(true)
	SetWriteTransports([]string{"dns"})
	f := board.Features{Services: []string{"memory", "public_data", "x402"}, Trust: board.TrustShadow}
	gives := Gives(f, services.Catalog(f.Services))
	if got := topics(gives); !slices.Equal(got, []string{"Voice everywhere", "Memory", "Search and data", "Images", "Work", "Trust"}) {
		t.Fatalf("topics: %v", got)
	}
	if !strings.Contains(gives[0].Line, "DNS") {
		t.Errorf("the voice line omits a running wire: %s", gives[0].Line)
	}
	data := gives[2].Line
	for _, e := range services.Catalog([]string{"x402", "public_data"}) {
		if !strings.Contains(data, e.Line) {
			t.Errorf("search and data lacks %s", e.ID)
		}
	}
}

// The quickstart's services step and the /llms.txt services section come
// from the catalogue and appear only while a service runs.
func TestQuickstartAndServicesTextFromTheCatalogue(t *testing.T) {
	if strings.Contains(QuickstartFor("", board.Features{}), "Optional: services") || ServicesText("https://x.test", nil) != "" {
		t.Fatal("services text with no service enabled")
	}
	f := board.Features{Services: services.Known()}
	quick := QuickstartFor("https://x.test", f)
	text := ServicesText("https://x.test", services.Catalog(f.Services))
	if !strings.Contains(quick, "(/api/services)") {
		t.Error("the quickstart does not name the catalogue")
	}
	for _, e := range services.Catalog(f.Services) {
		if !strings.Contains(quick, e.Title) {
			t.Errorf("quickstart lacks %s", e.Title)
		}
		for _, m := range e.Methods {
			if !strings.Contains(text, "- "+m.Name+" ("+m.Access()) {
				t.Errorf("services text lacks %s.%s", e.ID, m.Name)
			}
		}
	}
	if html := string(renderQuickstart(false, f.Services)); !strings.Contains(html, "Optional: services") {
		t.Error("the HTML quickstart lacks the services step")
	}
}
