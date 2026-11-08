package services

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

// envelopeParsers are the only functions that may refuse with the bare
// invalid_service_data code: they parse the {"schema","method","args",
// "max_cost"} envelope itself, whose rule is the code's board message.
var envelopeParsers = map[string]bool{"ParseData": true, "routeTool": true}

// TestNoBareArgumentRefusal (C392): an argument-level refusal names the
// argument and its rule (badArg). A bare refusal("invalid_service_data")
// reaches the agent as the envelope's message, which names the wrong fix
// for a GET form call or a bad argument value.
func TestNoBareArgumentRefusal(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || envelopeParsers[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "refusal" && len(x.Args) == 1 && isLit(x.Args[0], "invalid_service_data") {
						t.Errorf("%s: %s refuses with a bare refusal(\"invalid_service_data\"); use badArg with the argument and its rule", fset.Position(x.Pos()), fn.Name.Name)
					}
				case *ast.CompositeLit:
					sel, ok := x.Type.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Err" {
						return true
					}
					code, msg := false, false
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						switch k := kv.Key.(*ast.Ident); {
						case k == nil:
						case k.Name == "Code" && isLit(kv.Value, "invalid_service_data"):
							code = true
						case k.Name == "Message", k.Name == "Limit":
							msg = true
						}
					}
					if code && !msg {
						t.Errorf("%s: %s builds invalid_service_data with no Message; use badArg", fset.Position(x.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(files) < 20 {
		t.Fatalf("scanned only %d files; run from internal/services", len(files))
	}
}

func isLit(e ast.Expr, want string) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == want
}

// argMessage is the refusal's code and argument message.
func argMessage(t *testing.T, err error) string {
	t.Helper()
	var e *allowance.Err
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want *allowance.Err", err)
	}
	if e.Code != "invalid_service_data" {
		t.Fatalf("code = %q, want invalid_service_data", e.Code)
	}
	if e.Message == "" {
		t.Fatal("bare invalid_service_data: the board would answer with the envelope's message")
	}
	return e.Message
}

func TestArgRefusalsNameTheArgument(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  func() error
		want []string
	}{
		{"public_data unknown dataset", func() error {
			_, err := parsePublicData("fetch", json.RawMessage(`{"dataset":"nonexistent"}`), 1_790_000_000)
			return err
		}, []string{`"nonexistent"`, "public_data.datasets"}},
		{"public_data no dataset", func() error {
			_, err := parsePublicData("fetch", json.RawMessage(`{}`), 1_790_000_000)
			return err
		}, []string{"dataset is required", "/call/fetch/page?url="}},
		{"screen bad threshold", func() error {
			_, err := parseScreen(json.RawMessage(`{"text":"hello","source":"web","threshold":2}`))
			return err
		}, []string{"threshold", "0 to 1"}},
		{"screen missing text", func() error {
			_, err := parseScreen(json.RawMessage(`{"source":"web"}`))
			return err
		}, []string{"text is required"}},
		{"screen bad source", func() error {
			_, err := parseScreen(json.RawMessage(`{"text":"hi","source":"nope"}`))
			return err
		}, []string{"source is one of", "web"}},
		{"inference bad temperature", func() error {
			_, err := (&inference{}).parseInference(json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":3}`))
			return err
		}, []string{"temperature", "0 to 2"}},
		{"inference bad role", func() error {
			_, err := (&inference{}).parseInference(json.RawMessage(`{"model":"m","messages":[{"role":"robot","content":"hi"}]}`))
			return err
		}, []string{"role"}},
		{"inference unknown model", func() error {
			_, err := (&inference{cfg: &InferenceConfig{Models: map[string][]InferenceRoute{"fast": nil}}}).parseInference(json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			return err
		}, []string{"model is one of fast"}},
		{"args not an object", func() error {
			var v struct{}
			return StrictObject([]byte(`[1]`), &v)
		}, []string{"args must be one JSON object"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := argMessage(t, tc.err())
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message %q lacks %q", msg, w)
				}
			}
			if strings.Contains(msg, `"schema":1`) {
				t.Errorf("message %q is the envelope's", msg)
			}
		})
	}
}
