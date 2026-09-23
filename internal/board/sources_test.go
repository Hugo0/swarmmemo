package board

// Drift guards for the service's facts. The operation table (operations.go),
// the limits (limits.go) and the error codes the code returns are the single
// source of truth; everything that restates them is either generated from them
// here or held to them here. See docs/project/SOURCES.md.
//
// Regenerate the generated sections of docs/PROTOCOL.md with:
//
//	go generate ./internal/board

//go:generate env SWARMMEMO_WRITE_GENERATED=1 go test -run TestGeneratedProtocolSections -count=1 .

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/roomstyle"
)

// dispatchedOperations reads the operation names the store actually routes: the
// case labels of Store.execute plus the private read controls it hands off
// before dispatch.
func dispatchedOperations(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "store.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "execute" || fn.Recv == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if clause, ok := n.(*ast.CaseClause); ok {
				for _, expr := range clause.List {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						name, _ := strconv.Unquote(lit.Value)
						found[name] = true
					}
				}
			}
			return true
		})
		return false
	})
	if len(found) < 30 {
		t.Fatalf("found only %d dispatched operations; the parser is broken", len(found))
	}
	for _, op := range []string{"private_read.create", "private_read.revoke", "private_read.get", "private_read.list"} {
		if !privateReadControl(op) {
			t.Fatalf("%s is no longer a private read control; update this test", op)
		}
		found[op] = true
	}
	return found
}

func TestOperationTableMatchesDispatcher(t *testing.T) {
	dispatched := dispatchedOperations(t)
	for name := range dispatched {
		if !KnownOperation(name) {
			t.Errorf("the store dispatches %s but the operation table (operations.go) does not list it", name)
		}
	}
	for _, op := range Operations() {
		if !dispatched[op.Name] {
			t.Errorf("operation table lists %s but the store never dispatches it", op.Name)
		}
		if op.Summary == "" || op.Section == "" {
			t.Errorf("%s needs a summary and a PROTOCOL.md section", op.Name)
		}
		if op.Delegable && op.Name != "post" && strings.Contains(op.Name, "delegation") {
			t.Errorf("%s: a worker key must never manage grants", op.Name)
		}
	}
}

// probeCommand fills only the fields an operation needs to reach its
// authorization check, without a signature.
func probeCommand(op Operation) Command {
	c := Command{Operation: op.Name, RequestID: "probe-" + op.Name}
	for _, field := range strings.Fields(op.Fields) {
		if op.Name == "post" && field == "data" {
			continue // signed post data is a separate, signed-only feature
		}
		switch field {
		case "room":
			c.Room = "lobby"
		case "page":
			c.Page = "main"
		case "text":
			c.Text = "probe"
		case "message_id":
			c.MessageID = strings.Repeat("0", 32)
		case "target":
			c.Target = strings.Repeat("a", 64)
		case "data":
			c.Data = "{}"
		case "ttl":
			c.TTL = 60
		case "amount":
			c.Amount = 1
		case "filename":
			c.Filename = "probe.txt"
		case "media_type":
			c.MediaType = "text/plain"
		case "reason":
			c.Reason = "probe"
		}
	}
	return c
}

// Signed in the table must mean what the store enforces: an unsigned command
// is refused with 401, and an operation open to anyone never asks for one.
func TestOperationSignedFlagMatchesEnforcement(t *testing.T) {
	s := openTest(t, Config{})
	// These look their object up before checking the caller, so an unsigned
	// probe for a missing object is refused as missing, not as unsigned.
	lookupFirst := map[string]bool{"blob.delete": true, "private_read.create": true, "private_read.revoke": true, "private_read.get": true, "private_read.list": true}
	for _, op := range Operations() {
		_, err := s.Execute(context.Background(), probeCommand(op), "192.0.2.1")
		var e *Error
		errors.As(err, &e)
		unsigned := e != nil && e.Status == 401
		switch {
		case op.Signed && !unsigned && !lookupFirst[op.Name]:
			t.Errorf("%s is marked Signed but an unsigned command was not refused with 401: %v", op.Name, err)
		case !op.Signed && unsigned:
			t.Errorf("%s is open to anyone in the table but the store demanded a signature: %v", op.Name, err)
		}
	}
}

var codeWord = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$|^[a-z]+$`)

// serviceErrorCodes reads every error code the service can return, with its
// HTTP statuses, from the source: problem(...) calls, Error composite
// literals, JSON error maps, and the per-feature helpers that pick a status
// from a code.
func serviceErrorCodes(t *testing.T) map[string]map[int]bool {
	t.Helper()
	codes := map[string]map[int]bool{}
	add := func(code string, status int) {
		if codes[code] == nil {
			codes[code] = map[int]bool{}
		}
		codes[code][status] = true
	}
	httpStatus := map[string]int{"StatusBadRequest": 400, "StatusForbidden": 403, "StatusNotFound": 404, "StatusMethodNotAllowed": 405, "StatusGone": 410, "StatusTooManyRequests": 429, "StatusServiceUnavailable": 503}
	intOf := func(expr ast.Expr) int {
		switch v := expr.(type) {
		case *ast.BasicLit:
			n, _ := strconv.Atoi(v.Value)
			return n
		case *ast.SelectorExpr:
			return httpStatus[v.Sel.Name]
		}
		return 0
	}
	stringOf := func(expr ast.Expr) string {
		if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, _ := strconv.Unquote(lit.Value)
			return s
		}
		return ""
	}
	// Helpers that choose the status from the code; the store package owns them.
	helpers := map[string]func(string) error{"delegationError": delegationError, "privateReadError": privateReadError, "webhookError": webhookError, "linkError": linkError}
	var unresolved []string
	for _, dir := range []string{".", "../httpapi", "../web", "../references", "../transport", "../roomstyle", "../markdown"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					name := ""
					switch fun := node.Fun.(type) {
					case *ast.Ident:
						name = fun.Name
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					}
					// Any call passing a literal HTTP error status followed by a
					// code: problem(404, "not_found", ...), referenceFailure(w, r,
					// 429, "reference_busy", ...) and helpers yet to be written.
					for i := 0; i+1 < len(node.Args); i++ {
						if status := intOf(node.Args[i]); status >= 400 && status < 600 {
							if code := stringOf(node.Args[i+1]); codeWord.MatchString(code) {
								add(code, status)
							}
						}
					}
					switch {
					case name == "rateError" && len(node.Args) >= 2:
						if code := stringOf(node.Args[1]); code != "" {
							add(code, 429)
						}
					case helpers[name] != nil && len(node.Args) == 1:
						if code := stringOf(node.Args[0]); code != "" {
							var e *Error
							errors.As(helpers[name](code), &e)
							add(e.Code, e.Status)
						}
					case name == "jsonResponse" && len(node.Args) == 3:
						ast.Inspect(node.Args[2], func(n ast.Node) bool {
							if kv, ok := n.(*ast.KeyValueExpr); ok && stringOf(kv.Key) == "code" {
								if code := stringOf(kv.Value); code != "" {
									add(code, intOf(node.Args[1]))
								}
							}
							return true
						})
					}
				case *ast.CompositeLit:
					typeName := ""
					switch typ := node.Type.(type) {
					case *ast.Ident:
						typeName = typ.Name
					case *ast.SelectorExpr:
						typeName = typ.Sel.Name
					}
					if typeName != "Error" {
						return true
					}
					code, status := "", 0
					for _, elt := range node.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							switch fmt.Sprint(kv.Key) {
							case "Code":
								code = stringOf(kv.Value)
							case "Status":
								status = intOf(kv.Value)
							}
						}
					}
					if code != "" {
						add(code, status)
					}
				}
				return true
			})
		}
	}
	for code, statuses := range codes {
		if statuses[0] {
			unresolved = append(unresolved, code)
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Fatalf("could not read the HTTP status of %v; extend serviceErrorCodes", unresolved)
	}
	if len(codes) < 120 {
		t.Fatalf("found only %d error codes; the scanner is broken", len(codes))
	}
	return codes
}

func readRepo(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// wordsIn collects the space-separated identifiers inside string literals that
// match pattern (group 1), e.g. a JavaScript 'a b c'.split(' ') set.
func wordsIn(source, pattern string) map[string]bool {
	words := map[string]bool{}
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(source, -1) {
		for _, word := range strings.Fields(match[1]) {
			words[word] = true
		}
	}
	return words
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The JavaScript client names every code the service can return (anything else
// reaches agents as a bare http_error, which 1.4.2 and 1.5.0 both shipped), and
// never names a code the service does not return.
func TestClientErrorCodesMatchService(t *testing.T) {
	codes := serviceErrorCodes(t)
	js := readRepo(t, "clients/javascript/swarmmemo.mjs")
	known := wordsIn(js, "(?s)(?:remoteCodes = new Set\\(|for \\(const code of )[\"'`]([a-z0-9_ ]+)[\"'`]\\.split")
	if len(known) < 100 {
		t.Fatalf("parsed only %d codes out of the javascript client; the parser is broken", len(known))
	}
	var missing, phantom []string
	for code := range codes {
		if !known[code] {
			missing = append(missing, code)
		}
	}
	for code := range known {
		if codes[code] == nil && code != "http_error" {
			phantom = append(phantom, code)
		}
	}
	sort.Strings(missing)
	sort.Strings(phantom)
	if len(missing) > 0 {
		t.Errorf("clients/javascript/swarmmemo.mjs cannot name %d service error codes, so agents see http_error: %s", len(missing), strings.Join(missing, " "))
	}
	if len(phantom) > 0 {
		t.Errorf("clients/javascript/swarmmemo.mjs names codes the service never returns: %s", strings.Join(phantom, " "))
	}
	// The Python allowlists pass a server code through or raise a local one;
	// a name that is neither is a typo that silently degrades to http_error.
	python := readRepo(t, "clients/python/swarmmemo_outbox.py") + readRepo(t, "clients/python/swarmmemo.py")
	mcp := readRepo(t, "clients/mcp/policy.py") + readRepo(t, "clients/mcp/operations.py") + readRepo(t, "clients/mcp/swarmmemo_mcp.py")
	for name, allow := range map[string]map[string]bool{
		"clients/python/swarmmemo_outbox.py SAFE_CODES": wordsIn(python, `(?s)SAFE_CODES(?: = |\.update\()\{([^}]*)\}`),
		"clients/mcp/policy.py SAFE_CODES":              wordsIn(mcp, `SAFE_CODES = frozenset\("([^"]*)"`),
	} {
		cleaned := map[string]bool{}
		for word := range allow {
			cleaned[strings.Trim(word, `",`)] = true
		}
		if len(cleaned) < 10 {
			t.Fatalf("parsed only %d codes from %s", len(cleaned), name)
		}
		local := python + mcp
		for code := range cleaned {
			if codes[code] != nil {
				continue
			}
			// A local code is raised somewhere, as a call argument, not only listed.
			raised := strings.Contains(local, `"`+code+`")`) || strings.Contains(local, `'`+code+`')`) || strings.Contains(local, `= "`+code+`"`)
			if !raised {
				t.Errorf("%s lists %q, which the service never returns and the client never raises", name, code)
			}
		}
	}
}

// Command fields and operation sets the clients hard-code, held to the table.
func TestClientOperationSetsMatchTable(t *testing.T) {
	js := readRepo(t, "clients/javascript/swarmmemo.mjs")
	py := readRepo(t, "clients/python/swarmmemo.py")
	fields := strings.Join(CommandFields(), " ")
	if !strings.Contains(js, "export const FIELDS = Object.freeze('"+fields+"'.split(' '))") {
		t.Errorf("clients/javascript/swarmmemo.mjs FIELDS differs from the canonical command fields:\n%s", fields)
	}
	if !strings.Contains(py, `FIELDS = "`+fields+`".split()`) {
		t.Errorf("clients/python/swarmmemo.py FIELDS differs from the canonical command fields:\n%s", fields)
	}
	delegable, mutations, reads := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, op := range Operations() {
		if op.Delegable {
			delegable[op.Name] = true
		}
		// The Node client refuses private read grants by design (README).
		if strings.HasPrefix(op.Name, "private_read.") {
			continue
		}
		if op.Mutation {
			mutations[op.Name] = true
		} else {
			reads[op.Name] = true
		}
	}
	want := strings.Join(sortedKeys(delegable), " ")
	for name, got := range map[string]map[string]bool{
		"javascript delegatedOperations": wordsIn(js, `const delegatedOperations = new Set\('([^']*)'`),
		"python DELEGATED_OPERATIONS":    wordsIn(py, `DELEGATED_OPERATIONS = frozenset\("([^"]*)"`),
		"javascript mutations":           wordsIn(js, `const mutations = new Set\('([^']*)'`),
		"javascript reads":               wordsIn(js, `const reads = new Set\('([^']*)'`),
	} {
		expected := want
		switch name {
		case "javascript mutations":
			expected = strings.Join(sortedKeys(mutations), " ")
		case "javascript reads":
			expected = strings.Join(sortedKeys(reads), " ")
		}
		if have := strings.Join(sortedKeys(got), " "); have != expected {
			t.Errorf("%s drifted from the operation table:\n have %s\n want %s", name, have, expected)
		}
	}
}

// Generated sections of docs/PROTOCOL.md. Each sits between
// <!-- BEGIN GENERATED: name --> and <!-- END GENERATED: name -->.
func generatedSections(t *testing.T) map[string]string {
	var ops strings.Builder
	ops.WriteString("| Operation | Signature | Fields | What it does |\n|---|---|---|---|\n")
	for _, op := range Operations() {
		sig := "optional"
		if op.Signed {
			sig = "required"
		}
		fields := "none"
		if op.Fields != "" {
			fields = "`" + strings.Join(strings.Fields(op.Fields), "` `") + "`"
		}
		fmt.Fprintf(&ops, "| [`%s`](#%s) | %s | %s | %s |\n", op.Name, op.Section, sig, fields, op.Summary)
	}
	var writes, workers []string
	for _, op := range Operations() {
		if op.Mutation {
			writes = append(writes, "`"+op.Name+"`")
		}
		if op.Delegable {
			workers = append(workers, "`"+op.Name+"`")
		}
	}
	ops.WriteString("\nEvery command may also carry the envelope: `public_key`, `signature`, `timestamp`,\n`nonce`, `request_id` and, for a worker key, `delegation`. Writes take a `request_id`\nand return their original receipt on an exact retry. The writes are:\n")
	ops.WriteString(wrapList("", writes, ""))
	ops.WriteString("\nA scoped worker key may be granted only these:\n")
	ops.WriteString(wrapList("", workers, ""))

	var limits strings.Builder
	limits.WriteString("| Limit | Value | `/capabilities` key |\n|---|---|---|\n")
	for _, l := range PublicLimits() {
		fmt.Fprintf(&limits, "| %s | %s | `%s` |\n", l.Meaning, l.Text(), l.Key)
	}

	codes := serviceErrorCodes(t)
	byStatus := map[int][]string{}
	for code, statuses := range codes {
		for status := range statuses {
			byStatus[status] = append(byStatus[status], code)
		}
	}
	statuses := make([]int, 0, len(byStatus))
	for status := range byStatus {
		statuses = append(statuses, status)
	}
	sort.Ints(statuses)
	var errs strings.Builder
	for _, status := range statuses {
		list := byStatus[status]
		sort.Strings(list)
		quoted := make([]string, len(list))
		for i, code := range list {
			quoted[i] = "`" + code + "`"
		}
		errs.WriteString(wrapList(fmt.Sprintf("- **%d**: ", status), quoted, "  "))
	}
	names := make([]string, 0, len(roomstyle.Hooks))
	for name := range roomstyle.Hooks {
		names = append(names, name)
	}
	sort.Strings(names)
	hooks := make([]string, len(names))
	for i, name := range names {
		hooks[i] = "`." + name + "`"
	}
	var via strings.Builder
	via.WriteString("| `via` | Badge | Set by |\n|---|---|---|\n")
	for _, v := range Vias() {
		carrier := v.Carrier
		if v.Bridge {
			carrier += " (bridge claim)"
		}
		fmt.Fprintf(&via, "| `%s` | via %s | %s |\n", v.Name, v.Label, carrier)
	}
	groups := make([]string, 0, len(viaGroups))
	for name := range viaGroups {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	for _, name := range groups {
		fmt.Fprintf(&via, "\n`write_via` may name the group `%s`, meaning `%s`.\n", name, strings.Join(viaGroups[name], "` `"))
	}
	var free []string
	for i, name := range names {
		if slices.Contains(roomstyle.FreeClasses, roomstyle.Hooks[name]) {
			free = append(free, hooks[i])
		}
	}
	return map[string]string{"operations": ops.String(), "limits": limits.String(), "errors": errs.String(), "hooks": wrapList("", hooks, ""), "vias": via.String(),
		// Inside a list item: indented, and the end marker keeps its indent.
		"free-hooks": wrapList("  ", free, "  ") + "  "}
}

// wrapList joins items with ", " and wraps them at 88 columns, starting after
// a prefix already written on the first line and indenting continuation lines.
func wrapList(prefix string, items []string, indent string) string {
	var b strings.Builder
	b.WriteString(prefix)
	line := len(prefix)
	for i, item := range items {
		sep := ", "
		if i == 0 {
			sep = ""
		}
		if i == len(items)-1 {
			item += "."
		}
		if i > 0 && line+len(sep)+len(item) > 88 {
			b.WriteString(",\n" + indent)
			line = len(indent)
			sep = ""
		}
		b.WriteString(sep + item)
		line += len(sep) + len(item)
	}
	return b.String() + "\n"
}

func TestGeneratedProtocolSections(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "PROTOCOL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	updated := doc
	for name, body := range generatedSections(t) {
		begin := "<!-- BEGIN GENERATED: " + name + " (go generate ./internal/board) -->\n"
		end := "<!-- END GENERATED: " + name + " -->"
		start := strings.Index(updated, begin)
		stop := strings.Index(updated, end)
		if start < 0 || stop < start {
			t.Fatalf("docs/PROTOCOL.md has no generated %s section", name)
		}
		updated = updated[:start+len(begin)] + body + updated[stop:]
	}
	if updated == doc {
		return
	}
	if os.Getenv("SWARMMEMO_WRITE_GENERATED") == "1" {
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("docs/PROTOCOL.md generated sections are stale; run: go generate ./internal/board")
}

// Every operation's Section names a real heading of docs/PROTOCOL.md.
func TestOperationSectionsExist(t *testing.T) {
	anchors := markdownAnchors(readRepo(t, "docs/PROTOCOL.md"))
	for _, op := range Operations() {
		if !anchors[op.Section] {
			t.Errorf("%s points at #%s, which is not a heading in docs/PROTOCOL.md", op.Name, op.Section)
		}
	}
}

// markdownAnchors returns the GitHub-style anchors of a document's headings.
func markdownAnchors(doc string) map[string]bool {
	anchors := map[string]bool{}
	strip := regexp.MustCompile(`[^a-z0-9 _-]`)
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(line, "#"))
		title = strings.ReplaceAll(title, "`", "")
		anchors[strings.ReplaceAll(strip.ReplaceAllString(strings.ToLower(title), ""), " ", "-")] = true
	}
	return anchors
}
