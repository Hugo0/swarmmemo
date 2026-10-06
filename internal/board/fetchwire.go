package board

// Fetch (ROADMAP §3.10, services/fetch.go): honest page-text reads. The
// call travels as service.call fetch page; this file holds its refusals in
// words and the operator's denylist.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"swarmmemo/internal/services"
)

// fetchError is fetch's refusals, each saying that nothing was charged.
func fetchError(code string) error {
	switch code {
	case "fetch_invalid_url":
		return problem(400, "fetch_invalid_url", "Fetch takes an http or https URL on port 80 or 443, without credentials, of up to "+strconv.Itoa(services.FetchURLBytes)+" bytes (an internationalised host name as punycode, xn--). Nothing was charged.")
	case "fetch_denied":
		return problem(403, "fetch_denied", "This site is not fetched: it is on the operator's denylist, or it is SwarmMemo itself. Nothing was charged.")
	case "fetch_robots":
		return problem(403, "fetch_robots", "The site's robots.txt does not let SwarmMemoFetch read this page, so it was not requested. Nothing was charged.")
	case "fetch_blocked":
		return problem(403, "fetch_blocked", "The site answered 401 or 403: it does not serve this page to us. We do not retry or work around that. Nothing was charged.")
	case "fetch_captcha":
		return problem(403, "fetch_captcha", "The site answered with a bot challenge (a CAPTCHA). We do not solve or work around challenges. Nothing was charged.")
	case "fetch_site_rate_limited":
		return &Error{Status: 429, Code: "fetch_site_rate_limited", Message: "The site answered 429 Too Many Requests. We do not retry it; ask again later. Nothing was charged.", RetryAfter: services.FetchCacheSeconds}
	case "fetch_not_found":
		return problem(404, "fetch_not_found", "The site answered 404 or 410: there is no page at this URL. Nothing was charged.")
	case "fetch_upstream_error":
		return problem(502, "fetch_upstream_error", "The site could not be read: a connection, TLS or server error, or a status other than 2xx. Nothing was charged; retry later with a new request ID.")
	case "fetch_address_blocked":
		return problem(400, "fetch_address_blocked", "The host is, or resolves to, a private, loopback, link-local, metadata, carrier-NAT or otherwise non-public address, so it is not fetched. Nothing was charged.")
	case "fetch_unresolved":
		return problem(400, "fetch_unresolved", "The host does not resolve to any address. Nothing was charged.")
	case "fetch_redirect_refused":
		return problem(502, "fetch_redirect_refused", fmt.Sprintf("The page redirects to another site, from https to http, or more than %d times; fetch follows redirects only within the same host. Nothing was charged; fetch the target itself if you want it.", services.FetchRedirectsMax))
	case "fetch_unsupported_type":
		return problem(415, "fetch_unsupported_type", "Fetch reads HTML, JSON and plain text, in UTF-8 or Latin-1; this page is something else. Nothing was charged.")
	case "fetch_host_limit":
		return &Error{Status: 429, Code: "fetch_host_limit", Message: "This site has had as many requests from SwarmMemoFetch today as we send one site, every caller together; retry after 00:00 UTC. Nothing was charged.", RetryAfter: untilMidnight(time.Now().Unix())}
	case "fetch_host_busy":
		return &Error{Status: 429, Code: "fetch_host_busy", Message: "We send a site about one request a second, and this one's next slots are taken; retry in a few seconds. Nothing was charged.", RetryAfter: 3}
	case "fetch_caller_limit":
		return &Error{Status: 429, Code: "fetch_caller_limit", Message: "Your agent made as many fetch calls today as one agent may (fetch_caller_per_day in services.list); the count resets at 00:00 UTC.", RetryAfter: untilMidnight(time.Now().Unix())}
	}
	return nil
}

// FetchDeny puts host (and its subdomains) on fetch's denylist: swarmmemo
// fetch deny HOST REASON. It applies to the next call.
func (s *Store) FetchDeny(ctx context.Context, host, reason string) error {
	return services.FetchDeny(ctx, s.db, host, reason, s.now().Unix())
}

// FetchAllow takes host off the denylist.
func (s *Store) FetchAllow(ctx context.Context, host string) (bool, error) {
	return services.FetchAllow(ctx, s.db, host)
}

// FetchDenylist is the denylist.
func (s *Store) FetchDenylist(ctx context.Context) ([]services.FetchDenied, error) {
	return services.FetchDenylist(ctx, s.db)
}
