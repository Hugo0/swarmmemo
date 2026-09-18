package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The JavaScript client keeps an allowlist of error codes and reports anything
// else as a bare http_error, dropping the reason. A code added to the service
// but not to that list therefore reaches agents as "request failed" with no
// cause: 1.4.2 and 1.5.0 both shipped codes the client could not name.
func TestClientsKnowEveryServiceErrorCode(t *testing.T) {
	code := regexp.MustCompile(`problem\(\s*\d+\s*,\s*"([a-z0-9_]+)"|Code:\s*"([a-z0-9_]+)"`)
	codes := map[string]string{}
	for _, dir := range []string{"../board", "../httpapi", "../web", "../references"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			source, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", entry.Name(), err)
			}
			for _, match := range code.FindAllStringSubmatch(string(source), -1) {
				if name := match[1] + match[2]; name != "" {
					codes[name] = entry.Name()
				}
			}
		}
	}
	if len(codes) < 50 {
		t.Fatalf("only found %d error codes; the scan is broken, not the clients", len(codes))
	}
	client, err := os.ReadFile("../../clients/javascript/swarmmemo.mjs")
	if err != nil {
		t.Skipf("javascript client not present: %v", err)
	}
	// The client builds its set from string literals split on spaces; read those
	// literals rather than searching the file, so a code named in a comment does
	// not count as handled.
	known := map[string]bool{}
	literal := regexp.MustCompile("[\"'`]([a-z0-9_ ]+)[\"'`]\\.split\\(' '\\)")
	for _, match := range literal.FindAllStringSubmatch(string(client), -1) {
		for _, name := range strings.Fields(match[1]) {
			known[name] = true
		}
	}
	if len(known) < 50 {
		t.Fatalf("only parsed %d codes out of the javascript client; the parser is broken", len(known))
	}
	var missing []string
	for name, file := range codes {
		if !known[name] {
			missing = append(missing, name+" ("+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the javascript client cannot name %d service error codes, so agents see http_error instead:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}
