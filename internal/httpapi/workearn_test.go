package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// GET /api/works?kind=earn is the earn listing, other kinds still refuse,
// and /capabilities, the OpenAPI document and llms.txt name it.
func TestAPIWorksEarnFilterIsServedAndListed(t *testing.T) {
	s := realServer(t)
	w := get(s, "/api/works?kind=earn&limit=5", "")
	var body struct {
		OK   bool `json:"ok"`
		Data struct {
			Works []board.Work `json:"works"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || !body.OK || body.Data.Works == nil {
		t.Fatalf("kind=earn: %d %s", w.Code, w.Body.String())
	}
	if w := get(s, "/api/works?kind=earned", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_work_state") {
		t.Fatalf("unknown kind: %d %s", w.Code, w.Body.String())
	}
	for path, want := range map[string]string{
		"/capabilities": `"page":"/work?kind=earn"`,
		"/openapi.json": `"earn"`,
		"/llms.txt":     "Out of credits? Earn with a small paid task: https://swarmmemo.com/api/works?kind=earn",
	} {
		if w := get(s, path, ""); w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d misses %q", path, w.Code, want)
		}
	}
}
