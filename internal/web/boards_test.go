package web

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// schemaErrors checks the subset of JSON Schema that boards.schema.json uses; the
// public repository's CI checks the same subset in Python.
func schemaErrors(v any, schema any, path string) []string {
	switch s := schema.(type) {
	case bool:
		if !s {
			return []string{path + ": not allowed"}
		}
		return nil
	case map[string]any:
		var errs []string
		if kinds, ok := s["type"]; ok {
			list, _ := kinds.([]any)
			if str, ok := kinds.(string); ok {
				list = []any{str}
			}
			match := false
			for _, k := range list {
				switch k {
				case "object":
					_, match = v.(map[string]any)
				case "array":
					_, match = v.([]any)
				case "string":
					_, match = v.(string)
				case "boolean":
					_, match = v.(bool)
				case "null":
					match = v == nil
				}
				if match {
					break
				}
			}
			if !match {
				return []string{fmt.Sprintf("%s: expected %v", path, kinds)}
			}
		}
		if c, ok := s["const"]; ok && !reflect.DeepEqual(v, c) {
			errs = append(errs, fmt.Sprintf("%s: must be %v", path, c))
		}
		if e, ok := s["enum"].([]any); ok {
			found := false
			for _, x := range e {
				found = found || reflect.DeepEqual(v, x)
			}
			if !found {
				errs = append(errs, path+": not in enum")
			}
		}
		if str, ok := v.(string); ok {
			n := float64(len([]rune(str)))
			if min, ok := s["minLength"].(float64); ok && n < min {
				errs = append(errs, path+": too short")
			}
			if max, ok := s["maxLength"].(float64); ok && n > max {
				errs = append(errs, path+": too long")
			}
			if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
				errs = append(errs, path+": does not match "+p)
			}
		}
		if arr, ok := v.([]any); ok {
			if min, ok := s["minItems"].(float64); ok && float64(len(arr)) < min {
				errs = append(errs, path+": too few items")
			}
			if items, ok := s["items"]; ok {
				for i, x := range arr {
					errs = append(errs, schemaErrors(x, items, fmt.Sprintf("%s[%d]", path, i))...)
				}
			}
		}
		if obj, ok := v.(map[string]any); ok {
			props, _ := s["properties"].(map[string]any)
			if req, ok := s["required"].([]any); ok {
				for _, k := range req {
					if _, ok := obj[k.(string)]; !ok {
						errs = append(errs, fmt.Sprintf("%s: missing %v", path, k))
					}
				}
			}
			for k, x := range obj {
				if p, ok := props[k]; ok {
					errs = append(errs, schemaErrors(x, p, path+"."+k)...)
				} else if s["additionalProperties"] == false {
					errs = append(errs, path+": unknown field "+k)
				}
			}
		}
		if all, ok := s["allOf"].([]any); ok {
			for _, sub := range all {
				errs = append(errs, schemaErrors(v, sub, path)...)
			}
		}
		if cond, ok := s["if"]; ok && len(schemaErrors(v, cond, path)) == 0 {
			if then, ok := s["then"]; ok {
				errs = append(errs, schemaErrors(v, then, path)...)
			}
		}
		return errs
	}
	return []string{path + ": unsupported schema"}
}

func readJSON(t *testing.T, name string) any {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBoardsJSONMatchesSchema(t *testing.T) {
	schema := readJSON(t, "boards.schema.json")
	var data any
	if err := json.Unmarshal(boardsJSON, &data); err != nil {
		t.Fatal(err)
	}
	if errs := schemaErrors(data, schema, "$"); len(errs) > 0 {
		t.Fatalf("embedded boards.json fails its schema:\n%s", strings.Join(errs, "\n"))
	}
	// The validator itself must reject what the schema forbids.
	for name, bad := range map[string]string{
		"verified without link":  `{"name":"X","group":"verified","section":"S","url":"https://x.test/","link":false,"about":"A.","access":"A.","identity":"I.","checked":"2026-01-01"}`,
		"reported with url":      `{"name":"X","group":"reported","url":"https://x.test/","link":false,"note":"N."}`,
		"completeness no reason": `{"name":"X","group":"completeness","url":null,"link":false,"checked":"2026-01-01"}`,
		"http url":               `{"name":"X","group":"completeness","url":"http://x.test/","link":true,"reason":"R.","checked":"2026-01-01"}`,
		"unknown field":          `{"name":"X","group":"completeness","url":null,"link":false,"reason":"R.","checked":"2026-01-01","extra":1}`,
		"reason without period":  `{"name":"X","group":"completeness","url":null,"link":false,"reason":"R","checked":"2026-01-01"}`,
	} {
		var entry any
		if err := json.Unmarshal([]byte(bad), &entry); err != nil {
			t.Fatal(err)
		}
		doc := map[string]any{"checked": "2026-09-22", "sections": []any{"S"}, "boards": []any{entry}}
		if len(schemaErrors(doc, schema, "$")) == 0 {
			t.Errorf("schema accepted %s", name)
		}
	}
}

func TestLoadBoardMapFailsFast(t *testing.T) {
	ok := `{"name":"A","group":"verified","section":"S","url":"https://a.test/","link":true,"about":"A.","access":"A.","identity":"I.","checked":"2026-09-01"}`
	doc := func(entries ...string) []byte {
		return []byte(`{"checked":"2026-09-22","sections":["S"],"boards":[` + strings.Join(entries, ",") + `]}`)
	}
	if _, err := loadBoardMap(doc(ok)); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	for name, raw := range map[string][]byte{
		"duplicate url":     doc(ok, strings.Replace(ok, `"A"`, `"B"`, 1)),
		"unknown field":     doc(strings.Replace(ok, `"link"`, `"extra":1,"link"`, 1)),
		"unknown section":   doc(strings.Replace(ok, `"S"`, `"T"`, 1)),
		"future check":      doc(strings.Replace(ok, "2026-09-01", "2027-01-01", 1)),
		"reported with url": doc(ok, `{"name":"R","group":"reported","url":"https://r.test/","link":false,"note":"N."}`),
		"http url":          doc(strings.Replace(ok, "https://a.test/", "http://a.test/", 1)),
		"unknown group":     doc(ok, `{"name":"R","group":"other","url":null,"link":false}`),
	} {
		if _, err := loadBoardMap(raw); err == nil {
			t.Errorf("loader accepted %s", name)
		}
	}
	// link:false keeps a known address out of the rendered page.
	m, err := loadBoardMap(doc(ok, `{"name":"look.test","group":"completeness","url":"https://look.test/","link":false,"reason":"Lookalike of a.test.","checked":"2026-09-01"}`))
	if err != nil || m.Completeness[0].URL != "" {
		t.Fatalf("text-only entry kept its link: %v %+v", err, m)
	}
}
