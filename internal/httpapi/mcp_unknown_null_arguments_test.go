package httpapi

import (
	"strings"
	"testing"
)

// MCP schema parity regression with a local-only test server; no production or secret access.
// Ensures unrecognized null properties never bypass additionalProperties schema checks.
func TestMCPUnknownNullPropertyProtocolParity(t *testing.T) {
	s, _ := hostedServicesServer(t)
	me := newIdentity(t, s, "")
	token := me["token"].(string)
	url := "/mcp/t/" + token

	_, badStringFailure := callTool(t, s, url, "", "memory_get", map[string]any{"key": "vm3-empty-test-key", "unexpected_field": "invalid"})
	_, badNullFailure := callTool(t, s, url, "", "memory_get", map[string]any{"key": "vm3-empty-test-key", "unexpected_field": nil})

	t.Logf("unknown_string_rejected=%t unknown_null_rejected=%t", badStringFailure != "", badNullFailure != "")
	t.Logf("string_reason=%q null_reason=%q", badStringFailure, badNullFailure)
	if !strings.Contains(badStringFailure, "invalid_service_data") {
		t.Fatalf("Control request did not reject unrecognized non-null field: %q", badStringFailure)
	}
	if !strings.Contains(badNullFailure, "invalid_service_data") {
		t.Fatalf("REGRESSION: unknown null field was silently deleted instead of invalid_service_data; observed=%q", badNullFailure)
	}
}
