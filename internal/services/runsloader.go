package services

// The runs loader's wire protocol (workers/runs-loader/src/protocol.ts) and
// the client that speaks it. Every constant here mirrors one there; a change
// is a change on both sides.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/safenet"
)

// Loader hard bounds (protocol.ts LIMITS).
const (
	RunsCodeBytes         = 64 << 10
	RunsInputBytes        = 16 << 10
	RunsCPUMsMax          = 30_000
	RunsWallMsMin         = 100
	RunsWallMsMax         = 25_000
	RunsSubrequestsMax    = 64
	RunsOutputBytesMax    = 64 << 10
	RunsErrorBytes        = 2 << 10
	RunsEgressRequestsMax = 64
	RunsEgressBytesOutMax = 1 << 20
	RunsEgressBytesInMax  = 8 << 20
	RunsEgressLogMax      = 128
	RunsDenyEntriesMax    = 256
	// RunsResponseBytes bounds the loader's answer; a longer one is refused.
	RunsResponseBytes = 512 << 10
	// runsResponseSkew bounds how far the answer's timestamp may be from ours.
	runsResponseSkew = 120
	runsDialTimeout  = 5 * time.Second
	// runsHTTPGrace is added to a run's wall limit for the HTTP call: the
	// loader waits up to 1.5 s for the tail event and then signs its answer.
	runsHTTPGrace = 8 * time.Second
)

const (
	runsTimestampHeader = "X-Runs-Timestamp"
	runsSignatureHeader = "X-Runs-Signature"
)

type loaderRequest struct {
	Schema   int             `json:"schema"`
	RunID    string          `json:"run_id"`
	Language string          `json:"language"`
	Code     string          `json:"code"`
	Input    json.RawMessage `json:"input"`
	Limits   loaderLimits    `json:"limits"`
	Network  loaderNetwork   `json:"network"`
}

type loaderLimits struct {
	CPUMs       int64 `json:"cpu_ms"`
	Subrequests int64 `json:"subrequests"`
	WallMs      int64 `json:"wall_ms"`
	StdoutBytes int64 `json:"stdout_bytes"`
	StderrBytes int64 `json:"stderr_bytes"`
	ResultBytes int64 `json:"result_bytes"`
}

type loaderNetwork struct {
	Enabled      bool     `json:"enabled"`
	MaxRequests  int64    `json:"max_requests"`
	MaxBytesOut  int64    `json:"max_bytes_out"`
	MaxBytesIn   int64    `json:"max_bytes_in"`
	Ports        []int    `json:"ports"`
	DenyHosts    []string `json:"deny_hosts"`
	DenyCIDRs    []string `json:"deny_cidrs"`
	AllowConnect bool     `json:"allow_connect"`
	Resolve      bool     `json:"resolve"`
}

// LoaderResponse is the loader's answer to one run.
type LoaderResponse struct {
	Schema     int           `json:"schema"`
	RunID      string        `json:"run_id"`
	Status     string        `json:"status"`
	Error      string        `json:"error"`
	ResultJSON string        `json:"result_json"`
	Stdout     string        `json:"stdout"`
	Stderr     string        `json:"stderr"`
	Truncated  RunTruncated  `json:"truncated"`
	CPUMs      int64         `json:"cpu_ms"`
	CPUSource  string        `json:"cpu_source"`
	WallMs     int64         `json:"wall_ms"`
	Outcome    string        `json:"outcome"`
	Network    string        `json:"network"`
	Egress     EgressSummary `json:"egress"`
}

type RunTruncated struct {
	Stdout bool `json:"stdout"`
	Stderr bool `json:"stderr"`
	Result bool `json:"result"`
}

// EgressSummary is a run's outbound traffic as the loader's gateway saw it.
type EgressSummary struct {
	Requests     int64         `json:"requests"`
	Blocked      int64         `json:"blocked"`
	BytesOut     int64         `json:"bytes_out"`
	BytesIn      int64         `json:"bytes_in"`
	Log          []EgressEntry `json:"log"`
	LogTruncated bool          `json:"log_truncated"`
}

// EgressEntry is one outbound attempt. The path is kept only as a hash.
type EgressEntry struct {
	Seq        int64  `json:"seq"`
	TMs        int64  `json:"t_ms"`
	Method     string `json:"method"`
	Host       string `json:"host"`
	Port       int64  `json:"port"`
	PathSHA256 string `json:"path_sha256"`
	BytesOut   int64  `json:"bytes_out"`
	BytesIn    int64  `json:"bytes_in"`
	Status     int64  `json:"status"`
	Ms         int64  `json:"ms"`
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason"`
	Pending    bool   `json:"pending"`
}

var (
	runIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// Hosts and methods come from the run's own requests: any printable
	// ASCII within the loader's bounds. A strict pattern here would let a run
	// make its own answer unparseable, and so unbillable.
	egressMethodRE = regexp.MustCompile(`^[\x21-\x7e]{1,16}$`)
	egressHostRE   = regexp.MustCompile(`^[\x21-\x7e]{0,253}$`)
	egressReasonRE = regexp.MustCompile(`^[a-z_]{0,64}$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	outcomeRE      = regexp.MustCompile(`^[A-Za-z_]{0,32}$`)
	runStatuses    = map[string]bool{"ok": true, "error": true, "cpu_exceeded": true, "subrequests_exceeded": true, "memory_exceeded": true, "timeout": true, "bad_output": true}
)

var errLoaderResponse = errors.New("runs: malformed loader response")

// ParseLoaderResponse parses the loader's answer strictly: the exact keys,
// every string within its cap, every number inside the loader's own bounds.
// It never trusts a count it can check.
func ParseLoaderResponse(raw []byte) (LoaderResponse, error) {
	var r LoaderResponse
	if len(raw) > RunsResponseBytes {
		return r, errLoaderResponse
	}
	if err := StrictObject(raw, &r); err != nil {
		return LoaderResponse{}, errLoaderResponse
	}
	e := r.Egress
	switch {
	case r.Schema != 1, !runIDRE.MatchString(r.RunID), !runStatuses[r.Status],
		r.CPUSource != "tail" && r.CPUSource != "limit", r.Network != "on" && r.Network != "off",
		len(r.Error) > RunsErrorBytes, len(r.Stdout) > RunsOutputBytesMax, len(r.Stderr) > RunsOutputBytesMax,
		len(r.ResultJSON) > RunsOutputBytesMax, r.ResultJSON != "" && !json.Valid([]byte(r.ResultJSON)),
		!outcomeRE.MatchString(r.Outcome),
		r.CPUMs < 0 || r.CPUMs > 10*RunsCPUMsMax, r.WallMs < 0 || r.WallMs > 10*RunsWallMsMax,
		e.Requests < 0 || e.Requests > RunsEgressRequestsMax, e.Blocked < 0 || e.Blocked > 1<<20,
		e.BytesOut < 0 || e.BytesOut > RunsEgressBytesOutMax,
		e.BytesIn < 0 || e.BytesIn > RunsEgressRequestsMax*(RunsEgressBytesInMax+1),
		len(e.Log) > RunsEgressLogMax,
		r.Network == "off" && (e.Requests > 0 || e.BytesOut > 0 || e.BytesIn > 0):
		return LoaderResponse{}, errLoaderResponse
	}
	for _, s := range []string{r.Error, r.Stdout, r.Stderr, r.ResultJSON} {
		if !utf8.ValidString(s) {
			return LoaderResponse{}, errLoaderResponse
		}
	}
	for _, x := range e.Log {
		if x.Seq < 0 || x.TMs < 0 || x.TMs > 10*RunsWallMsMax*10 || !egressMethodRE.MatchString(x.Method) || !egressHostRE.MatchString(x.Host) ||
			x.Port < 0 || x.Port > 65535 || (x.PathSHA256 != "" && !sha256HexRE.MatchString(x.PathSHA256)) ||
			x.BytesOut < 0 || x.BytesOut > RunsEgressBytesOutMax || x.BytesIn < 0 || x.BytesIn > RunsEgressBytesInMax+1 ||
			x.Status < 0 || x.Status > 999 || x.Ms < 0 || x.Ms > 10*RunsWallMsMax*10 ||
			(x.Verdict != "allowed" && x.Verdict != "blocked") || !egressReasonRE.MatchString(x.Reason) {
			return LoaderResponse{}, errLoaderResponse
		}
	}
	return r, nil
}

// signRuns is the MAC both directions use (auth.ts): hex HMAC-SHA256 over
// LABEL "." TIMESTAMP "." BODY, LABEL "req" or "res".
func signRuns(secret []byte, label string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(label + "." + timestamp + "."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// SignRunsRequest is exported for the fake loader in tests.
func SignRunsRequest(secret []byte, label string, timestamp int64, body []byte) string {
	return signRuns(secret, label, strconv.FormatInt(timestamp, 10), body)
}

// LoaderError is a failed loader call: nothing ran, or what ran cannot be
// trusted. Kind is "transport", "status", "auth", "response" or "mismatch".
// Sent is whether the whole request reached the wire.
type LoaderError struct {
	Kind   string
	Status int
	Detail string
	Sent   bool
}

func (e *LoaderError) Error() string {
	return fmt.Sprintf("runs loader: %s (%d) %s", e.Kind, e.Status, e.Detail)
}

// MayHaveRun reports whether the loader may have run the code, network and
// all: the request was sent and the loader did not refuse it up front (a 401
// for our signature, or another 4xx before the run). Such a failure is
// charged at the run's maximum, never refunded (security review 1.20, H4).
func (e *LoaderError) MayHaveRun() bool {
	switch {
	case !e.Sent:
		return false
	case e.Kind == "status":
		return e.Status >= 500
	case e.Kind == "auth":
		return e.Status != http.StatusUnauthorized
	}
	return true
}

// runsClient calls the one configured loader URL, over the safe dialer, and
// only ever dials the configured host.
type runsClient struct {
	url    string
	host   string // host:port
	secret []byte
	http   *http.Client
	now    func() time.Time
	grace  time.Duration
}

func newRunsClient(cfg *RunsConfig) *runsClient {
	if cfg == nil {
		return nil
	}
	c := &runsClient{url: cfg.url.String(), host: cfg.host, secret: cfg.secret, now: cfg.clock, grace: runsHTTPGrace}
	if cfg.httpGrace > 0 {
		c.grace = cfg.httpGrace
	}
	dial := cfg.dial
	if dial == nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safenet.Dial(ctx, network, addr, runsDialTimeout)
		}
	}
	c.http = &http.Client{
		// A redirect would be a second address; it is never followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr != c.host {
					return nil, fmt.Errorf("runs: refusing to dial %s, not the loader host", addr)
				}
				return dial(ctx, network, addr)
			},
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 5 * time.Second,
			DisableKeepAlives:   false,
			MaxIdleConns:        4,
			IdleConnTimeout:     60 * time.Second,
		},
	}
	return c
}

// run sends one run and returns the verified, parsed answer and its exact
// bytes. The deadline is the run's wall limit plus runsHTTPGrace.
func (c *runsClient) run(ctx context.Context, req loaderRequest) (LoaderResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return LoaderResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.Limits.WallMs)*time.Millisecond+c.grace)
	defer cancel()
	ts := strconv.FormatInt(c.now().Unix(), 10)
	// Whether the whole request was written: after that the loader may have
	// run it, and a failure is no longer free (LoaderError.MayHaveRun).
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			sent.Store(true)
		}
	}})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return LoaderResponse{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(runsTimestampHeader, ts)
	hr.Header.Set(runsSignatureHeader, signRuns(c.secret, "req", ts, body))
	resp, err := c.http.Do(hr)
	if err != nil {
		return LoaderResponse{}, &LoaderError{Sent: sent.Load(), Kind: "transport", Detail: transportDetail(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, RunsResponseBytes+1))
	if err != nil {
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "transport", Status: resp.StatusCode, Detail: transportDetail(err)}
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "auth", Status: resp.StatusCode, Detail: "the loader refused our signature (check the shared secret and the clock)"}
	case resp.StatusCode != http.StatusOK:
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "status", Status: resp.StatusCode, Detail: snippet(raw)}
	case len(raw) > RunsResponseBytes:
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "response", Status: resp.StatusCode, Detail: "answer larger than the cap"}
	}
	rts := resp.Header.Get(runsTimestampHeader)
	n, err := strconv.ParseInt(rts, 10, 64)
	if err != nil || n < 0 || abs(c.now().Unix()-n) > runsResponseSkew ||
		!hmac.Equal([]byte(resp.Header.Get(runsSignatureHeader)), []byte(signRuns(c.secret, "res", rts, raw))) {
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "auth", Status: resp.StatusCode, Detail: "the answer's signature does not verify"}
	}
	out, err := ParseLoaderResponse(raw)
	if err != nil {
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "response", Status: resp.StatusCode, Detail: err.Error()}
	}
	if out.RunID != req.RunID || (out.Network == "on") != req.Network.Enabled {
		return LoaderResponse{}, &LoaderError{Sent: true, Kind: "mismatch", Status: resp.StatusCode, Detail: "the answer is for another run or another network setting"}
	}
	return out, nil
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func transportDetail(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return truncateUTF8(err.Error(), 200)
}

func snippet(raw []byte) string {
	return truncateUTF8(string(bytes.ToValidUTF8(raw, nil)), 200)
}
