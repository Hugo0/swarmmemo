package trust

import (
	"encoding/json"
	"testing"
)

// CanonicalJSON is the one sorted-key encoder: trust documents (Canonical)
// and journal seals hash its bytes, which a value read back from them
// reproduces; an unencodable value is an error, never a panic.
func TestCanonicalJSONIsSortedAndStable(t *testing.T) {
	v := struct {
		Zeta  string         `json:"zeta"`
		Alpha map[string]any `json:"alpha"`
		N     float64        `json:"n"`
	}{"<a & b>", map[string]any{"y": 1, "b": []any{"x", 2.5}}, 1e21}
	got, err := CanonicalJSON(v)
	const want = `{"alpha":{"b":["x",2.5],"y":1},"n":1e+21,"zeta":"<a & b>"}`
	if err != nil || string(got) != want {
		t.Fatalf("CanonicalJSON = %s, %v; want %s", got, err, want)
	}
	if string(Canonical(v)) != want {
		t.Fatal("Canonical and CanonicalJSON differ")
	}
	var back any
	if err = json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if again, _ := CanonicalJSON(back); string(again) != want {
		t.Fatalf("read back: %s", again)
	}
	if _, err = CanonicalJSON(map[string]any{"c": make(chan int)}); err == nil {
		t.Fatal("an unencodable value encoded")
	}
}
