package services

import (
	"net"
	"time"
)

// RunsConfigForTest parses a RUNS_CONFIG body that may name a plain-http
// loader (httptest). With safe false the loader is dialed with an ordinary
// dialer, since httptest listens on loopback; with safe true it goes through
// the production safe dialer, which must refuse it. grace replaces the HTTP
// grace over the run's wall limit, so a timeout test is quick.
func RunsConfigForTest(body []byte, safe bool, clock func() time.Time, screener RunScreener, grace time.Duration) (*RunsConfig, error) {
	cfg, err := parseRunsConfig(body, true)
	if err != nil {
		return nil, err
	}
	if !safe {
		cfg.dial = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	}
	if clock != nil {
		cfg.clock = clock
	}
	cfg.Screener = screener
	cfg.httpGrace = grace
	return cfg, nil
}

// RunOutputForTest is the canonical output document a receipt hashes.
func RunOutputForTest(status, errText, resultJSON, stdout, stderr string, t RunTruncated) []byte {
	return canonicalJSON(runOutput{Status: status, Error: errText, ResultJSON: resultJSON, Stdout: stdout, Stderr: stderr, Truncated: t})
}
