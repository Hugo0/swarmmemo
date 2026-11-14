package httpapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// C970: ?pretty=1 (or true) on a /call/ URL answers the same JSON indented
// by two spaces; pretty is the route's field, never a tool argument; the
// default answer stays compact, byte for byte.
func TestCallPretty(t *testing.T) {
	_, s := anonCallServer(t, 2000)
	rid := "c970prettyretry0001"
	get := func(q string) (int, string) {
		w := secReq(s, "GET", "/call/notary/stamp?text=pretty&request_id="+rid+q, "", "198.51.100.43:1", nil)
		return w.Code, w.Body.String()
	}
	if code, raw := get(""); code != 200 {
		t.Fatalf("first call: %d %s", code, raw)
	}
	// Retries with the same request_id return the first answer.
	code, compact := get("")
	if code != 200 || strings.Count(compact, "\n") != 1 || !strings.HasSuffix(compact, "}\n") {
		t.Fatalf("default is one compact line: %d %s", code, compact)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(compact)); err != nil || want.String()+"\n" != compact {
		t.Fatalf("default is compact JSON: %v %s", err, compact)
	}
	for _, q := range []string{"&pretty=1", "&pretty=true"} {
		code, pretty := get(q)
		if code != 200 || !strings.Contains(pretty, "\n  \"data\": {\n") {
			t.Fatalf("%s: %d %s", q, code, pretty)
		}
		var a, b any
		if json.Unmarshal([]byte(compact), &a) != nil || json.Unmarshal([]byte(pretty), &b) != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: not the same value:\n%s\n%s", q, compact, pretty)
		}
	}
	if code, raw := get("&pretty=0"); code != 200 || raw != compact {
		t.Fatalf("pretty=0 is the default: %d %s", code, raw)
	}
	if code, raw := get("&pretty=yes"); code != 400 || !strings.Contains(raw, "pretty may only be") {
		t.Fatalf("pretty=yes: %d %s", code, raw)
	}
}
