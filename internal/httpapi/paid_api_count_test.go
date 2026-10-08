package httpapi

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	publicdocs "swarmmemo/docs"
	"swarmmemo/internal/services"
)

// How many paid APIs SwarmMemo tools reach is stated once, as
// services.X402ToolsApprox. Go code builds every sentence that states it from
// the constant; the hand-written public copy (Markdown, templates, client
// docs) may state it, and each statement must be the constant's value. When
// the catalogue grows, change the constant and this test names every
// sentence still saying the old number.
func TestPaidAPICountHasOneSource(t *testing.T) {
	root := filepath.Join("..", "..")
	want := services.X402ToolsApprox
	short := strings.TrimSuffix(strings.ReplaceAll(want, ",", ""), "000") + "k"
	// A count of paid or pay-per-call APIs (or tools), as prose writes it.
	count := regexp.MustCompile(`(?i)\b([0-9][0-9,]*(?:\.[0-9]+)?k?)\+?\s+(?:paid|pay-per-call)\s+(?:APIs|tools)\b`)
	literals := []string{want, short, "~" + short}
	skip := map[string]bool{".git": true, "research": true, "deploy": true, "boardlist": true, "node_modules": true, "rfcs": true, "archive": true, "testdata": true}
	stated := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			// Hidden directories (.git, .claude worktrees) are not the source.
			if skip[d.Name()] || (path != root && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		goCode := ext == ".go" && !strings.HasSuffix(path, "_test.go")
		public := ext == ".md" || ext == ".html" || ext == ".txt" || ext == ".tmpl" || ext == ".json" && strings.HasPrefix(rel, "plugins")
		if !goCode && !public {
			return nil
		}
		// The operator's registry changelog records what each past release said.
		if strings.HasPrefix(rel, filepath.Join("ops", "mcp_registry")) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(raw)
		if goCode {
			for i, line := range strings.Split(text, "\n") {
				if rel == filepath.Join("internal", "services", "x402.go") && strings.HasPrefix(line, "const X402ToolsApprox = ") {
					continue
				}
				for _, lit := range literals {
					if strings.Contains(line, lit) {
						t.Errorf("%s:%d states the paid-API count as a literal %q; use services.X402ToolsApprox", rel, i+1, lit)
					}
				}
			}
			return nil
		}
		flat := regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
		for _, m := range count.FindAllStringSubmatch(flat, -1) {
			stated++
			if m[1] != want && m[1] != short {
				t.Errorf("%s states %q; the paid-API count is %s (services.X402ToolsApprox)", rel, m[0], want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Generated copy carries the constant too.
	for _, j := range publicdocs.Jobs {
		for _, m := range count.FindAllStringSubmatch(j.Description+" "+j.Use, -1) {
			stated++
			if m[1] != want {
				t.Errorf("%s states %q; want %s", j.Path, m[0], want)
			}
		}
	}
	if stated < 3 {
		t.Errorf("found %d statements of the paid-API count; the pattern no longer matches the copy", stated)
	}
}
