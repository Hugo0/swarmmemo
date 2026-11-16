package clientcontract

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

const (
	contractFile = "../../clients/contract.json"
	pythonFile   = "../../clients/python/swarmmemo.py"
	jsFile       = "../../clients/javascript/swarmmemo.mjs"
)

// compact is the contract for the clients' generated blocks: one line per
// entry of each top-level object, keys sorted.
func compact() string {
	return compactObject(JSON(), 0)
}

func compactObject(raw []byte, depth int) string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		panic(err)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	indent := strings.Repeat(" ", depth+1)
	var out strings.Builder
	out.WriteString("{\n")
	for i, key := range keys {
		value := object[key]
		name, _ := json.Marshal(key)
		out.WriteString(indent + string(name) + ": ")
		if depth == 0 && bytes.HasPrefix(bytes.TrimSpace(value), []byte("{")) {
			out.WriteString(compactObject(value, depth+1))
		} else {
			var line bytes.Buffer
			if err := json.Compact(&line, value); err != nil {
				panic(err)
			}
			out.WriteString(line.String())
		}
		if i < len(keys)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString(strings.Repeat(" ", depth) + "}")
	return out.String()
}

const blockComment = "The server facts this client checks (field sets, enums, operations, limits,\n" +
	"patterns, defaults), written from the Go code that enforces them; the same\n" +
	"data as clients/contract.json. Never edit by hand.\n"

func commented(prefix string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(strings.TrimSuffix(blockComment, "\n"), "\n") {
		out.WriteString(prefix + line)
	}
	return out.String() + "\n"
}

// Each generated block: its file, markers and body.
func blocks() []struct{ file, begin, end, body string } {
	return []struct{ file, begin, end, body string }{
		{pythonFile, "# BEGIN GENERATED: CONTRACT (go generate ./internal/clientcontract)\n", "# END GENERATED: CONTRACT\n",
			commented("# ") + `CONTRACT = json.loads(r"""` + "\n" + compact() + `""")` + "\n" +
				"# The SHA-256 of clients/contract.json, as /capabilities clients.contract_sha256\n" +
				"# serves it, and the command that updates an MCP bridge checkout (C1030).\n" +
				`CONTRACT_SHA256 = "` + SHA256() + `"` + "\n" + `BRIDGE_UPDATE = "` + BridgeUpdate + `"` + "\n"},
		{jsFile, "// BEGIN GENERATED: CONTRACT (go generate ./internal/clientcontract)\n", "// END GENERATED: CONTRACT\n",
			commented("// ") + "export const CONTRACT = Object.freeze(" + compact() + ");\n"},
	}
}

// The contract file and every client's generated block hold exactly what
// JSON says; with SWARMMEMO_WRITE_GENERATED=1 this writes them instead.
func TestGeneratedContractFiles(t *testing.T) {
	write := os.Getenv("SWARMMEMO_WRITE_GENERATED") == "1"
	if strings.Contains(string(JSON()), `"""`) {
		t.Fatal(`the contract contains """, which the Python block cannot hold`)
	}
	got, err := os.ReadFile(contractFile)
	if write {
		err = os.WriteFile(contractFile, JSON(), 0o644)
	} else if err == nil && !bytes.Equal(got, JSON()) {
		t.Errorf("%s is stale; run go generate ./internal/clientcontract", contractFile)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks() {
		raw, err := os.ReadFile(b.file)
		if err != nil {
			t.Fatal(err)
		}
		source := string(raw)
		begin, end := strings.Index(source, b.begin), strings.Index(source, b.end)
		if begin < 0 || end < begin || strings.Count(source, b.begin) != 1 {
			t.Fatalf("%s needs exactly one CONTRACT block between its markers", b.file)
		}
		fresh := source[:begin] + b.begin + b.body + source[end:]
		switch {
		case write && fresh != source:
			if err = os.WriteFile(b.file, []byte(fresh), 0o644); err != nil {
				t.Fatal(err)
			}
		case !write && fresh != source:
			t.Errorf("%s's CONTRACT block is stale; run go generate ./internal/clientcontract", b.file)
		}
	}
}
