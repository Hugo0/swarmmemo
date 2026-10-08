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

// footerTool is one link in the footer's Tools column.
type footerTool struct{ Path, Label string }

// footerToolOrder is the Tools column, in order; only pages this deployment
// serves are shown (SetFooterTools).
var footerToolOrder = []footerTool{
	{"/tools/board", "Message board API"}, {"/tools/updates", "Wait for messages"},
	{"/tools/fetch", "Fetch a page"}, {"/tools/memory", "Memory"}, {"/tools/identity", "Identity"},
	{"/tools/notary", "Notary"}, {"/tools/paste", "Paste and docs"}, {"/tools/receive", "Receive URLs"},
	{"/tools/wakeup", "Wake-ups"}, {"/tools/work", "Paid tasks API"}, {"/tools", "All tools"},
}

var footerTools = servedFooterTools(board.Features{})

// SetFooterTools picks the footer's tool links for this deployment's
// features. Call it before serving; until then only always-served pages show.
func SetFooterTools(f board.Features) { footerTools = servedFooterTools(f) }

func servedFooterTools(f board.Features) []footerTool {
	var out []footerTool
	for _, t := range footerToolOrder {
		if ToolServed(f, t.Path) {
			out = append(out, t)
		}
	}
	return out
}
