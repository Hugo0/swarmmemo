package references

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Independent pre-optimization path, including its final encoder, so these
// comparisons do not merely compare two callers of encodeParsedJSON.
func previousCanonicalForTest(value any) ([]byte, error) {
	if validValue(reflect.ValueOf(value), 0) != nil {
		return nil, ErrUnavailable
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnavailable
	}
	parsed, err := parseJSON(raw)
	if err != nil {
		return nil, ErrUnavailable
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(parsed) != nil {
		return nil, ErrUnavailable
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func TestParsedEncodingDifferential(t *testing.T) {
	for _, test := range []struct{ name, input, expected string }{
		{"sorted nested objects", ` {"z":[{"b":false,"a":true},null,[]],"a":{}} `, `{"a":{},"z":[{"a":true,"b":false},null,[]]}`},
		{"unicode keys and text", `{"雪":"café","😀":"雪","A":"é"}`, `{"A":"é","雪":"café","😀":"雪"}`},
		{"surrogate pair", `"\ud83d\ude00"`, `"😀"`},
		{"HTML and slash", `"\u003c\u003e\u0026\/"`, `"<>&/"`},
		{"line separators", "\"\u2028\u2029\"", `"\u2028\u2029"`},
		{"controls", `"\u0000\b\f\n\r\t\u001f\"\\"`, `"\u0000\b\f\n\r\t\u001f\"\\"`},
		{"escaped keys", `{"\u0062":2,"\u0061":1}`, `{"a":1,"b":2}`},
		{"null", `null`, `null`}, {"empty array", `[]`, `[]`}, {"empty object", `{}`, `{}`},
		{"empty string", `""`, `""`}, {"booleans", `[true,false]`, `[true,false]`},
		{"safe integers", `[0,1,9007199254740991]`, `[0,1,9007199254740991]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := parseJSON([]byte(test.input))
			if err != nil {
				t.Fatal(err)
			}
			previous, err := previousCanonicalForTest(value)
			if err != nil {
				t.Fatal(err)
			}
			got, err := encodeParsedJSON(value)
			if err != nil || string(got) != test.expected || !bytes.Equal(got, previous) || hash(got) != hash(previous) {
				t.Fatalf("parsed encoding changed: %q, %v", got, err)
			}
			generic, err := Canonical(value)
			if err != nil || !bytes.Equal(generic, previous) {
				t.Fatal("generic canonical changed", err)
			}
			if _, err := canonicalJSON(got); err != nil {
				t.Fatal("canonical output rejected", err)
			}
			if _, err := canonicalJSON([]byte(test.input)); (err == nil) != (test.input == test.expected) {
				t.Fatal("noncanonical admission changed", err)
			}
		})
	}
}

func TestParsedEncodingStrictAdmission(t *testing.T) {
	for _, raw := range []string{
		`{"a":null,"a":true}`, `{"a":1,"\u0061":2}`, `[{"nested":{"x":0,"x":1}}]`,
		`"\ud800"`, `"\udfff"`, `"\ud800\u0041"`, `"\udc00\ud800"`, `"\uZZZZ"`,
		string([]byte{'"', 0xff, '"'}), "\"raw\ncontrol\"", `"unfinished`,
		`-0`, `-1`, `1.0`, `1e0`, `1E0`, `01`, `+1`, `NaN`, `Infinity`, `9007199254740992`,
		`[0,]`, `{"a":}`, `{} {}`, "", strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33),
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseJSON([]byte(raw)); !errors.Is(err, ErrUnavailable) {
				t.Fatal("strict parser admitted malformed input", err)
			}
			if _, err := canonicalJSON([]byte(raw)); !errors.Is(err, ErrUnavailable) {
				t.Fatal("canonical admission bypassed parser", err)
			}
		})
	}
	for _, raw := range []string{` {}`, "{}\n", `{"b":0,"a":1}`, `"\u0061"`, `"\/"`, `"\ud83d\ude00"`, "\"\u2028\""} {
		if _, err := parseJSON([]byte(raw)); err != nil {
			t.Fatal("invalid noncanonical fixture", err)
		}
		if _, err := canonicalJSON([]byte(raw)); !errors.Is(err, ErrUnavailable) {
			t.Fatal("admitted noncanonical input", raw, err)
		}
	}
}

func TestParsedEncodingDepthAndNodeBoundaries(t *testing.T) {
	for _, raw := range []string{
		strings.Repeat("[", 32) + "0" + strings.Repeat("]", 32),
		"[" + strings.Repeat("null,", 149998) + "null]", // Array + 149,999 values = 150,000 nodes.
	} {
		value, err := parseJSON([]byte(raw))
		if err != nil {
			t.Fatal("valid exact limit rejected", err)
		}
		previous, err := previousCanonicalForTest(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := encodeParsedJSON(value)
		if err != nil || string(got) != raw || !bytes.Equal(got, previous) {
			t.Fatal("exact limit encoding changed", err)
		}
		if _, err := canonicalJSON(got); err != nil {
			t.Fatal(err)
		}
	}
	tooMany := "[" + strings.Repeat("null,", 149999) + "null]"
	if _, err := canonicalJSON([]byte(tooMany)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("node limit widened", err)
	}
}

func TestParsedEncodingPreservesSchemaAndProjectionHashes(t *testing.T) {
	reg, supp, snapshot := fixture(t)
	raw := mustCanonical(t, snapshot)
	loaded, err := validate(reg, supp, raw, fixtureNow)
	if err != nil || loaded.Digest != hash(raw) || loaded.RegistrySHA256 != hash(reg) || loaded.SuppressionSHA256 != hash(supp) {
		t.Fatal("projection hashes changed", err)
	}
	for _, field := range []string{"root", "source", "reference", "author"} {
		for _, mutation := range []string{"unknown", "missing"} {
			t.Run(field+"/"+mutation, func(t *testing.T) {
				value, err := parseJSON(raw)
				if err != nil {
					t.Fatal(err)
				}
				root := value.(map[string]any)
				item, required := root, "version"
				switch field {
				case "source":
					item, required = root["sources"].([]any)[0].(map[string]any), "id"
				case "reference":
					item, required = root["references"].([]any)[0].(map[string]any), "title"
				case "author":
					item = root["references"].([]any)[0].(map[string]any)["authors"].([]any)[0].(map[string]any)
					required = "name"
				}
				if mutation == "unknown" {
					item["unexpected"] = "inert"
				} else {
					delete(item, required)
				}
				changed := mustCanonical(t, value)
				if _, err := validate(reg, supp, changed, fixtureNow); !errors.Is(err, ErrUnavailable) {
					t.Fatal("schema validation bypassed", err)
				}
			})
		}
	}
}

func TestParsedEncodingGenericCanonicalStillValidates(t *testing.T) {
	for _, value := range []any{float64(1), float32(1), -1, json.Number("-0"), json.Number("1e0"),
		json.Number("9007199254740992"), uint64(maxSafeInteger) + 1, string([]byte{0xff}),
		map[int]string{1: "wrong key type"}, []any{map[string]any{"nested": float64(1)}},
	} {
		if _, err := Canonical(value); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("generic arbitrary value bypassed validation: %T, %v", value, err)
		}
	}
}
