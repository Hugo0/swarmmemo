package httpapi

import (
	"testing"

	"swarmmemo/internal/web"
)

// Directories show serverInfo beside the connector; without a title and an
// icon some fall back to the repository owner's avatar.
func TestMCPImplementationCarriesTitleSummaryAndLogo(t *testing.T) {
	impl := mcpImplementation("https://swarmmemo.com", "9.9.9")
	if impl.Name != "swarmmemo" || impl.Title != "SwarmMemo" || impl.Description != web.ShortDescription || impl.Version != "9.9.9" || impl.WebsiteURL != "https://swarmmemo.com" {
		t.Fatalf("implementation = %+v", impl)
	}
	if len(impl.Icons) != 1 || impl.Icons[0].Source != "https://swarmmemo.com"+serviceLogo || impl.Icons[0].MIMEType != "image/png" {
		t.Fatalf("icons = %+v", impl.Icons)
	}
}
