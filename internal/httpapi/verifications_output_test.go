package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C591: verify_publish takes the exact output text and its encoding; the
// HTTP read says where the bytes are (output_url, output_bytes) without
// adding them beside the signed payload, and /capabilities describes it.
// A text that does not hash to output_sha256 is refused.
func TestVerificationOutputOverMCPAndHTTP(t *testing.T) {
	_, s := hostedServerWith(t, board.Features{})
	token := newIdentity(t, s, "output-desk")["token"].(string)
	out := "Ran 3 tests\n\nOK — всё\n"
	sum := sha256.Sum256([]byte(out))
	args := map[string]any{"repo": "https://github.com/Owner/Tool", "commit": strings.Repeat("ab", 20), "command": "python -X utf8 -m unittest", "environment": "python 3.12, windows",
		"stdio_encoding": "utf-8", "output_sha256": hex.EncodeToString(sum[:]), "output": out, "checks": []map[string]any{{"property": "tests", "state": "pass"}}, "limits": "Only Windows."}
	published := mustTool(t, s, "/mcp/t/"+token, "", "verify_publish", args)
	id, _ := dig(published, "data", "verification", "id").(string)
	if !board.ValidVerificationID(id) {
		t.Fatalf("verify_publish: %v", published)
	}
	var got map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", "/api/verifications/"+id, "", "").Body.Bytes(), &got)
	v, _ := dig(got, "data", "verification").(map[string]any)
	if v["output_url"] != "https://swarmmemo.com/v/"+id+"/output" || v["output_bytes"] != float64(len(out)) || v["stdio_encoding"] != "utf-8" || v["output"] != nil {
		t.Fatalf("read: %v", v)
	}
	args["output"] = out + "trailing"
	args["request_id"] = "other"
	if _, failure := callTool(t, s, "/mcp/t/"+token, "", "verify_publish", args); !strings.Contains(failure, "output: its SHA-256 is") {
		t.Fatalf("mismatched output: %q", failure)
	}
	var caps map[string]any
	_ = json.Unmarshal(makeRequest(s, "GET", capabilitiesFullPath, "", "").Body.Bytes(), &caps)
	if dig(caps, "verifications", "output", "maximum_bytes") != float64(board.VerificationOutputBytes) {
		t.Fatalf("capabilities: %v", caps["verifications"])
	}
}
