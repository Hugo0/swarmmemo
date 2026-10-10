package httpapi

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/web"
)

// The MCP-only framework pages state the core profile's tools/list size
// (C134b), measured from the served profile: the rendered KB and tool count
// match the real tools/list answer within rounding, the JSON twin carries
// the same line, and a package framework's page does not.
func TestFrameworkToolsListSize(t *testing.T) {
	s := pinServerWith(t, true)
	size, tools, err := s.CoreToolsList(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wire, listed := toolsListAnswer(t, s, mcpProfileCore)
	if tools != len(listed) || tools == 0 {
		t.Fatalf("measured %d tools, tools/list answers %d", tools, len(listed))
	}
	web.SetCoreToolsList(size, tools)

	line := regexp.MustCompile(`tools/list is about (\d+) KB for (\d+) tools; attach only the tools you use\.`)
	for _, path := range web.FrameworkPaths() {
		var view struct {
			Kind      string `json:"kind"`
			ToolsList string `json:"tools_list"`
		}
		if err := json.Unmarshal(get(s, path+".json", "").Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		body := html.UnescapeString(get(s, path, "text/html").Body.String())
		if view.Kind != "mcp" {
			if view.ToolsList != "" || line.MatchString(body) {
				t.Errorf("%s (a package framework) states the tools/list size", path)
			}
			continue
		}
		m := line.FindStringSubmatch(body)
		if m == nil || !strings.Contains(body, view.ToolsList) || view.ToolsList != m[0] {
			t.Fatalf("%s: no tools/list line, or the JSON twin's %q differs", path, view.ToolsList)
		}
		kb, _ := strconv.Atoi(m[1])
		count, _ := strconv.Atoi(m[2])
		// Within rounding to the nearest KB, plus the transport's framing.
		if diff := kb*1000 - wire; count != len(listed) || diff > 600 || diff < -600 {
			t.Errorf("%s says about %d KB for %d tools; tools/list is %d bytes for %d tools", path, kb, count, wire, len(listed))
		}
	}
}
