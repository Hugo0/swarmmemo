package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C353: a hosted identity publishes a verification over MCP; anyone reads it
// over MCP and HTTP (by id, by repository and commit), and /capabilities and
// /openapi.json describe it.
func TestVerificationOverMCPAndHTTP(t *testing.T) {
	_, s := hostedServerWith(t, board.Features{Services: []string{"notary"}})
	token := newIdentity(t, s, "verify-desk")["token"].(string)
	commit := strings.Repeat("ab", 20)
	args := map[string]any{"repo": "https://github.com/Owner/Tool", "commit": commit, "command": "go test ./...", "environment": "go 1.25, linux, no network",
		"output_sha256": strings.Repeat("cd", 32), "checks": []map[string]any{{"property": "tests", "state": "pass", "note": "ok"}, {"property": "race", "state": "not_checked"}},
		"limits": "Only linux/amd64."}
	published := mustTool(t, s, "/mcp/t/"+token, "", "verify_publish", args)
	v, _ := dig(published, "data", "verification").(map[string]any)
	id, _ := v["id"].(string)
	if !board.ValidVerificationID(id) || v["result"] != "passed" || v["repo"] != "https://github.com/owner/tool" || !strings.HasPrefix(dig(published, "data", "markdown").(string), "[![Verified by SwarmMemo]") {
		t.Fatalf("verify_publish: %v", published)
	}
	// Keyless, the publish tool is not there; the read is.
	if _, failure := callTool(t, s, "/mcp", "", "verify_publish", args); failure == "" {
		t.Fatal("verify_publish ran without a hosted identity")
	}
	read := mustTool(t, s, "/mcp", "", "read_verification", map[string]any{"id": id})
	if dig(read, "data", "verification", "id") != id {
		t.Fatalf("read_verification: %v", read)
	}
	listed := mustTool(t, s, "/mcp", "", "read_verification", map[string]any{"repo": "https://github.com/owner/tool", "commit": commit})
	if list, _ := dig(listed, "data", "verifications").([]any); len(list) != 1 {
		t.Fatalf("read_verification list: %v", listed)
	}
	for _, path := range []string{"/api/verifications/" + id, "/api/verifications?repo=https://github.com/Owner/Tool&commit=" + commit, "/api/verifications"} {
		w := makeRequest(s, "GET", path, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"`+id+`"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for path, code := range map[string]int{"/api/verifications/0000000000000000": 404, "/api/verifications?repo=a&file=b": 400, "/api/verifications?commit=" + commit: 400} {
		if w := makeRequest(s, "GET", path, "", ""); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	// C409: the MCP read refuses what the HTTP read refuses (it let file
	// overwrite repo, and id drop the rest), with the same 400.
	for _, args := range []map[string]any{
		{"repo": "a", "file": "b"},
		{"id": id, "repo": "https://github.com/owner/tool"}, {"id": id, "file": strings.Repeat("a", 64)},
	} {
		if _, failure := callTool(t, s, "/mcp", "", "read_verification", args); !strings.Contains(failure, "400 invalid_request") {
			t.Errorf("read_verification %v: %q, want the HTTP read's 400", args, failure)
		}
	}
	var caps map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", capabilitiesFullPath, "", "").Body.Bytes(), &caps)
	if dig(caps, "verifications", "per_day") != float64(board.VerificationsPerDay) || dig(caps, "verifications", "instructions") != "/protocol.md#verified-by-swarmmemo" {
		t.Fatalf("capabilities: %v", caps["verifications"])
	}
	openapi := makeRequest(s, "GET", "/openapi.json", "", "").Body.String()
	if !strings.Contains(openapi, `"/api/verifications/{id}"`) || !strings.Contains(openapi, `"/api/verifications"`) {
		t.Fatal("openapi lacks the verification reads")
	}
	if llms := makeRequest(s, "GET", "/llms-full.txt", "", "").Body.String(); !strings.Contains(llms, "verify.publish") || !strings.Contains(llms, "/v/ID/badge.svg") {
		t.Fatal("llms-full.txt lacks the verification line")
	}
}

// C385: a flaky check (runs, passes, failing_step) published over MCP reads
// back the same over HTTP and read_verification, counted as flaky; a flaky
// check without runs is refused, and OpenAPI's verify.publish data names the
// entry's fields.
func TestVerificationFlakyOverMCPAndHTTP(t *testing.T) {
	_, s := hostedServerWith(t, board.Features{})
	token := newIdentity(t, s, "flaky-desk")["token"].(string)
	flaky := map[string]any{"property": "tests", "state": "flaky", "runs": float64(11), "passes": float64(7), "failing_step": "test_timeout"}
	args := map[string]any{"file_sha256": strings.Repeat("ef", 32), "command": "pytest -q", "environment": "linux",
		"output_sha256": strings.Repeat("cd", 32), "checks": []map[string]any{flaky, {"property": "lint", "state": "pass"}}, "limits": "none"}
	published := mustTool(t, s, "/mcp/t/"+token, "", "verify_publish", args)
	id, _ := dig(published, "data", "verification", "id").(string)
	read := mustTool(t, s, "/mcp", "", "read_verification", map[string]any{"id": id})
	var http map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", "/api/verifications/"+id, "", "").Body.Bytes(), &http)
	for name, got := range map[string]any{"read_verification": dig(read, "data", "verification"), "http": dig(http, "data", "verification")} {
		v, _ := got.(map[string]any)
		checks, _ := v["checks"].([]any)
		if v["result"] != "flaky" || v["flaky"] != float64(1) || v["passed"] != float64(1) || v["ran"] != float64(2) || len(checks) != 2 {
			t.Fatalf("%s: %v", name, got)
		}
		first, _ := checks[0].(map[string]any)
		for k, want := range flaky {
			if first[k] != want {
				t.Errorf("%s: checks[0].%s = %v, want %v", name, k, first[k], want)
			}
		}
		if !strings.Contains(v["signed_payload"].(string), `\"runs\":11`) {
			t.Errorf("%s: the signed payload lacks runs: %s", name, v["signed_payload"])
		}
	}
	args["checks"] = []map[string]any{{"property": "tests", "state": "flaky"}}
	if _, failure := callTool(t, s, "/mcp/t/"+token, "", "verify_publish", args); !strings.Contains(failure, "needs runs and passes") {
		t.Fatalf("flaky without runs: %q", failure)
	}
	openapi := makeRequest(s, "GET", "/openapi.json", "", "").Body.String()
	if !strings.Contains(openapi, `"verify.publish data checks entry"`) || !strings.Contains(openapi, `"verification_check_runs_maximum"`) {
		t.Fatal("openapi's verify.publish data lacks the check entry")
	}
}
