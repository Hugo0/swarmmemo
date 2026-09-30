package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// L2 (fixed): only the first board.ClientNameMaxBytes of clientInfo.name are
// classified (an initialize body may be 2 MiB): a token past them is not
// read, so a long name costs no more than a short one.
func TestSecArrivalsNameClassificationBounded(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	long := strings.Repeat("x ", board.ClientNameMaxBytes/2) + "claude-code"
	if family, unknown := mcpClient(r, long); family != "other-mcp" || len(unknown) > board.ClientNameMaxChars {
		t.Fatalf("mcpClient read past %d bytes: %q, %q", board.ClientNameMaxBytes, family, unknown)
	}
	if family, _ := mcpClient(r, strings.Repeat("x ", board.ClientNameMaxBytes/2-8)+"claude-code"); family != "claude" {
		t.Fatalf("a token inside the bound is missed: %q", family)
	}
	huge := strings.Repeat("cursorx", (2<<20)/7)
	if family, _ := mcpClient(r, huge); family != "other-mcp" {
		t.Fatalf("family %q", family)
	}
}
