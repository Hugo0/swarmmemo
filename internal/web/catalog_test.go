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
	if got := topics(Gives(board.Features{}, nil)); !slices.Equal(got, []string{"Voice everywhere", "Private conversations", "Find agents", "Work", "A record you can prove"}) {
		t.Fatalf("every flag off: %v", got)
	}
	SetCardImages(true)
	SetWriteTransports([]string{"dns"})
	f := board.Features{Services: []string{"memory", "public_data", "x402"}, Trust: board.TrustShadow}
	gives := Gives(f, services.Catalog(f.Services))
	if got := topics(gives); !slices.Equal(got, []string{"Voice everywhere", "Private conversations", "Search and data", "Tools across the internet", "Memory", "Images", "Find agents", "Work", "A record you can prove", "Trust"}) {
		t.Fatalf("topics: %v", got)
	}
	if !strings.Contains(gives[0].Line, "DNS") {
		t.Errorf("the voice line omits a running wire: %s", gives[0].Line)
	}
	byTopic := map[string]Give{}
	for _, g := range gives {
		byTopic[g.Topic] = g
	}
	for topic, id := range map[string]string{"Tools across the internet": "x402", "Search and data": "public_data"} {
		if e := services.Catalog([]string{id}); !strings.Contains(byTopic[topic].Line, e[0].Line) {
			t.Errorf("%s: %s lacks its line", topic, id)
		}
	}
	if byTopic["Tools across the internet"].Line != services.X402Line {
		t.Errorf("the aggregator's one line: %s", byTopic["Tools across the internet"].Line)
	}
}

// The /llms.txt fetch section, the call example and the which-tool lines
// come from the catalogue: only what runs, with the no-key call while it is
// available, and both fetch ceilings in plain words.
func TestLLMSFetchCallAndChoosing(t *testing.T) {
	const origin = "https://x.test"
	if FetchText(origin, board.Features{}, nil, services.NoKey{}) != "" || CallText(origin, nil) != "" {
		t.Fatal("a fetch or call section with no service enabled")
	}
	f := board.Features{Services: []string{"fetch", "memory", "x402"}}
	catalog := services.Catalog(f.Services)
	fetch := FetchText(origin, f, catalog, services.NoKey{Available: true, Methods: []string{"fetch.page"}})
	for _, want := range []string{"## Fetch a web page", "Without a key: up to 8 KiB per call; signed (or a signed-in MCP connection): up to 96 KiB.",
		"    curl -sS 'https://x.test/call/fetch/page?url=https%3A%2F%2Fexample.com%2F'\n", "fetch_page", "Details: https://x.test/tools/fetch"} {
		if !strings.Contains(fetch, want) {
			t.Errorf("fetch section lacks %q:\n%s", want, fetch)
		}
	}
	if strings.Contains(fetch, "max_bytes") || strings.Contains(FetchText(origin, f, catalog, services.NoKey{}), "curl") {
		t.Errorf("the no-key example carries max_bytes, or shows while calls without a key are off:\n%s", fetch)
	}
	call := CallText(origin, catalog)
	memory := services.Catalog([]string{"memory"})[0]
	if ex := ServiceExamples(origin, memory); !strings.Contains(call, "    "+ex.POST+"\n    # "+ex.SignNote+"\n") || !strings.Contains(call, "x402_resources") {
		t.Errorf("call section:\n%s", call)
	}
	choosing := ChoosingText(services.Catalog([]string{"memory", "paste", "docs"}))
	for _, want := range []string{"- memory: a small key-value store for your own state between runs;", "- paste: share one text by id, with expiry;",
		"- shared docs: versioned text edited by several keys or a group.", "- #bounties: posts paid by their poster", "optional escrowed credit reward and an optional named"} {
		if !strings.Contains(choosing, want) {
			t.Errorf("which-tool lines lack %q:\n%s", want, choosing)
		}
	}
	if only := ChoosingText(services.Catalog([]string{"paste"})); strings.Contains(only, "memory:") || !strings.Contains(only, "- paste: share one text by id, with expiry.\n") {
		t.Errorf("stores that do not run are named:\n%s", only)
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
