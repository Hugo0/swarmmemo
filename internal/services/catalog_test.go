package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// argShapes is each method's args type as its own parser decodes it.
var argShapes = map[string]any{
	"memory.put": memoryPut{}, "memory.delete": memoryKeyArgs{}, "memory.get": memoryKeyArgs{}, "memory.list": memoryListArgs{},
	"wakeup.schedule": wakeupScheduleArgs{}, "wakeup.cancel": wakeupRef{}, "wakeup.list": struct{}{}, "wakeup.notices": wakeupNoticesArgs{},
	"notary.stamp": notaryStampArgs{}, "notary.get": struct {
		Hash string `json:"hash"`
	}{}, "notary.key": struct{}{},
	"inference.complete": inferenceArgs{},
	"x402.call":          x402Args{}, "x402.resources": struct{}{},
	"public_data.fetch": pdRequestArgs{}, "public_data.bulk": struct {
		Requests []pdRequestArgs `json:"requests"`
	}{}, "public_data.datasets": struct{}{},
	"runs.run": runsArgs{}, "runs.log": struct {
		Run string `json:"run"`
	}{},
	"echo.echo": echoArgs{},
}

func jsonFields(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out[name] = true
	}
	return out
}

// Every catalogue example parses with the method's own parser, every
// documented argument is one that parser accepts, and every service has the
// fields each discovery surface renders.
func TestCatalogExamplesParse(t *testing.T) {
	const now = 1_760_000_000
	runsTest := &runs{cfg: &RunsConfig{Languages: []string{"javascript", "python"}, Limits: RunsLimits{CPUMsDefault: 100, CPUMsMax: 30000, WallMsDefault: 5000, WallMsMax: 30000}}}
	for _, e := range Catalog(Known()) {
		if e.Title == "" || e.Line == "" || strings.Contains(e.Line, "\n") || len(e.Methods) == 0 {
			t.Errorf("%s: a catalogue entry needs a title, a one-line line and methods", e.ID)
		}
		for _, l := range e.Limits {
			if l.Key == "" || l.Value <= 0 || l.Note == "" {
				t.Errorf("%s: limit %+v", e.ID, l)
			}
		}
		for _, m := range e.Methods {
			name := e.ID + "." + m.Name
			if m.Line == "" {
				t.Errorf("%s: no line", name)
			}
			args := json.RawMessage(FillPlaceholders(string(m.Example)))
			d, err := ParseData(FillPlaceholders(m.Data(m.MaxCost())), m.Write())
			if err != nil || d.Method != m.Name || string(d.Args) != string(args) || len(args) > m.ArgsMax {
				t.Errorf("%s: example data does not parse: %v", name, err)
			}
			shape, ok := argShapes[name]
			if !ok {
				t.Errorf("%s: add its args type to argShapes", name)
				continue
			}
			typ := reflect.TypeOf(shape)
			if err := StrictObject(args, reflect.New(typ).Interface()); err != nil {
				t.Errorf("%s: example %s is refused by the parser", name, args)
			}
			fields := jsonFields(typ)
			for _, a := range m.Args {
				if !fields[a.Name] {
					t.Errorf("%s: documents argument %q, which the parser refuses", name, a.Name)
				}
			}
			// The providers' own parsers, where they need no configuration.
			switch name {
			case "memory.put":
				_, err = parsePut(args)
			case "memory.delete":
				_, err = parseKeyArgs(args, false)
			case "memory.get":
				_, err = parseKeyArgs(args, true)
			case "wakeup.schedule":
				_, err = parseWakeup(args, now)
			case "wakeup.cancel":
				_, err = parseWakeupRef(args)
			case "notary.stamp":
				_, err = parseStamp(args)
			case "public_data.fetch", "public_data.bulk":
				_, err = parsePublicData(m.Name, args, now)
			case "runs.run":
				_, err = runsTest.plan(args)
			case "echo.echo":
				_, err = echo{}.Quote(Call{Method: "echo", Args: args, Price: m.Price.(Price)})
			}
			if err != nil {
				t.Errorf("%s: example refused: %v", name, err)
			}
		}
	}
}

// Each service's Docs anchor is a heading of docs/PROTOCOL.md.
func TestCatalogDocsAnchorsExist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range Catalog(Known()) {
		if !strings.Contains(string(raw), "\n### "+e.Title+"\n") {
			t.Errorf("%s: docs/PROTOCOL.md needs a heading ### %s (its Docs anchor %s)", e.ID, e.Title, e.Docs)
		}
	}
}

// The static catalogue and services.list agree: same services, same methods.
func TestCatalogIsServicesList(t *testing.T) {
	enabled := Known()
	static := Catalog(enabled)
	live := NewBuiltinRegistry(enabled, Deps{}).catalog(DefaultPrices(), true)
	a, _ := json.Marshal(static)
	var b []Entry
	for _, e := range live {
		e.Extra = nil
		b = append(b, e)
	}
	bb, _ := json.Marshal(b)
	if string(a) != string(bb) {
		t.Fatalf("static catalogue differs from services.list:\n%s\n%s", a, bb)
	}
}
