package httpapi

import (
	"net/http"
	"regexp"
	"strings"

	"swarmmemo/internal/board"
)

// Request signals (C160): what the board records, operator-only, about each
// accepted write that arrived over HTTP (board/signals.go). The address is
// the peer (s.peer: X-Forwarded-For only from the loopback proxy), hashed by
// the board; nothing here is stored unless the request is an accepted write.

// bridgeSenderHeader is the sending domain a verified email bridge reports
// (deploy/cloudflare-email). It is read only after the bridge's secret checked
// out; anyone else's is ignored.
const bridgeSenderHeader = "X-SwarmMemo-Bridge-Sender-Domain"

var senderDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// withSignals records the request's headers for the board.
func withSignals(r *http.Request) *http.Request {
	var hints []string
	for _, h := range []string{"Sec-CH-UA", "Sec-CH-UA-Platform", "Sec-CH-UA-Mobile"} {
		if v := r.Header.Get(h); v != "" {
			hints = append(hints, h+"="+v)
		}
	}
	sig := board.RequestSignals{
		UserAgent:      r.Header.Get("User-Agent"),
		Referer:        r.Header.Get("Referer"),
		AcceptLanguage: r.Header.Get("Accept-Language"),
		ClientHints:    strings.Join(hints, "; "),
	}
	return r.WithContext(board.WithRequestSignals(r.Context(), sig))
}

// withBridgeSender adds a verified email bridge's sending domain.
func withBridgeSender(r *http.Request, via string) *http.Request {
	if via != "email" {
		return r
	}
	domain := strings.ToLower(strings.TrimSpace(r.Header.Get(bridgeSenderHeader)))
	if len(domain) > 253 || !senderDomain.MatchString(domain) {
		return r
	}
	sig := board.RequestSignalsFrom(r.Context())
	sig.Origin = "email:" + domain
	return r.WithContext(board.WithRequestSignals(r.Context(), sig))
}
