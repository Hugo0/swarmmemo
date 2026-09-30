package leakscan

// The pattern list has one source, Rules; the web composer's JSON and the
// Python client's LEAK_PATTERNS block are written from it. Regenerate them
// with:
//
//	go generate ./internal/leakscan

//go:generate env SWARMMEMO_WRITE_GENERATED=1 go test -run TestGeneratedPatternFiles -count=1 .

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const (
	webPatternsFile    = "../web/assets/leak-patterns.json"
	pythonClientFile   = "../../clients/python/swarmmemo.py"
	pythonBlockBegin   = "# BEGIN GENERATED: LEAK_PATTERNS (go generate ./internal/leakscan)\n"
	pythonBlockEnd     = "# END GENERATED: LEAK_PATTERNS\n"
	pythonBlockComment = `# The leak patterns screen.leak, the web composer and this client share
# (GET /api/screen/leak-patterns). Compile each with re.compile(pattern,
# re.ASCII). A rule with "group" finds that group's span; "luhn" and "mod97"
# keep only matches whose digits pass the card or IBAN check.
`
)

// PythonBlock is the LEAK_PATTERNS block of the Python client: the served
// JSON, parsed at import.
func pythonBlock() string {
	return pythonBlockBegin + pythonBlockComment + `LEAK_PATTERNS = json.loads(r"""` + "\n" + string(PatternsJSON()) + `""")` + "\n" + pythonBlockEnd
}

// The generated files hold exactly what PatternsJSON says; with
// SWARMMEMO_WRITE_GENERATED=1 this writes them instead.
func TestGeneratedPatternFiles(t *testing.T) {
	write := os.Getenv("SWARMMEMO_WRITE_GENERATED") == "1"
	if strings.Contains(string(PatternsJSON()), `"""`) {
		t.Fatal(`the patterns contain """, which the Python block cannot hold`)
	}
	web, err := os.ReadFile(webPatternsFile)
	if write {
		err = os.WriteFile(webPatternsFile, PatternsJSON(), 0o644)
	} else if err == nil && !bytes.Equal(web, PatternsJSON()) {
		t.Errorf("%s is stale; run go generate ./internal/leakscan", webPatternsFile)
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pythonClientFile)
	if err != nil {
		t.Fatal(err)
	}
	client := string(raw)
	begin, end := strings.Index(client, pythonBlockBegin), strings.Index(client, pythonBlockEnd)
	if begin < 0 || end < begin || strings.Count(client, pythonBlockBegin) != 1 {
		t.Fatalf("%s needs exactly one LEAK_PATTERNS block between its markers", pythonClientFile)
	}
	fresh := client[:begin] + pythonBlock() + client[end+len(pythonBlockEnd):]
	switch {
	case write && fresh != client:
		if err = os.WriteFile(pythonClientFile, []byte(fresh), 0o644); err != nil {
			t.Fatal(err)
		}
	case !write && fresh != client:
		t.Errorf("%s's LEAK_PATTERNS block is stale; run go generate ./internal/leakscan", pythonClientFile)
	}
}
