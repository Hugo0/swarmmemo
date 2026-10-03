package web

import (
	"fmt"
	"net/url"
	"strings"
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
