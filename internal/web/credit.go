package web

import (
	"fmt"
	"net/url"
	"strings"

	"swarmmemo/internal/board"
)

// footerCredit is the operator's small backlink in the footer ("made with love
// by HOST"). It is deployment configuration (FOOTER_CREDIT_URL), not public
// code: the source ships without one and a deployment names its own.
type footerCredit struct{ URL, Host string }

var credit footerCredit

// SetFooterCredit sets the footer backlink from an https URL; empty clears it.
// Call it before serving.
func SetFooterCredit(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		credit = footerCredit{}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("FOOTER_CREDIT_URL: %q is not an https URL", raw)
	}
	credit = footerCredit{URL: u.String(), Host: strings.TrimPrefix(u.Hostname(), "www.")}
	return nil
}

// footerTool is the footer's one link to the tools.
type footerTool struct{ Path, Label string }

// footerToolOrder lists where the footer's Tools link may point, best first:
// the tools index, else the always-served board API page. The index links
// every tool page, so the footer carries one link, not a column of them.
var footerToolOrder = []footerTool{{"/tools", "Tools"}, {"/tools/board", "Tools"}}

var footerTools = servedFooterTools(board.Features{})

// SetFooterTools picks the footer's tools link for this deployment's
// features. Call it before serving; until then only always-served pages show.
func SetFooterTools(f board.Features) { footerTools = servedFooterTools(f) }

func servedFooterTools(f board.Features) []footerTool {
	for _, t := range footerToolOrder {
		if ToolServed(f, t.Path) {
			return []footerTool{t}
		}
	}
	return nil
}
