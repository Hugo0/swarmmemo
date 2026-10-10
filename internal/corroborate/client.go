// Package corroborate is the client of the Corroborate sidecar
// (workers/corroborate): what it would cost an adversary to fake the
// evidence behind an EVM address set, scored by Corroborate's rule
// (saturate within a trust root, sum across roots, price each root at
// min(forge, rent) times its age curve) against a pinned registry revision
// (RFC0015 §3.4).
//
// Two callers share it: the public corroborate service (internal/services)
// and, once wallets can be linked, the standing run, which prices a linked
// address set's personhood roots with the same resolver (§3.2, Personhood
// row). An answer is a score, never a verdict: this package has no
// "is human" anywhere.
//
// The sidecar listens on loopback only; the client refuses any other URL,
// so a misconfiguration cannot turn it into an outbound request.
package corroborate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultURL is where the sidecar listens unless CORROBORATE_URL says
	// otherwise.
	DefaultURL = "http://127.0.0.1:8787"
	// MaxAddresses is the most addresses one set may hold.
	MaxAddresses = 10
	// RegistryGenesisBlock is the Sepolia block the registry was deployed
	// in: an as_of before it has no ontology.
	RegistryGenesisBlock = 11344158
	// Timeout bounds one resolve, under the board's 30-second bound on a
	// read that needs the network. The sidecar gives up at 20 seconds.
	Timeout = 25 * time.Second
	// responseMax bounds the sidecar's answer.
	responseMax = 256 << 10
)

// ErrUnavailable is any failure to get a score: the sidecar unreachable,
// busy, timed out, or unable to read any credential. RetryAfter says when
// to ask again. It is never answered with a zero score.
type ErrUnavailable struct {
	Reason     string
	RetryAfter int
}

func (e *ErrUnavailable) Error() string { return "corroborate: unavailable (" + e.Reason + ")" }

// ErrRequest is a request the sidecar refused as malformed (an as_of it
// cannot answer, for one); Code is its error code.
type ErrRequest struct {
	Code, Message string
}

func (e *ErrRequest) Error() string { return "corroborate: " + e.Code + ": " + e.Message }

// Credential is the credential that set a root's contribution.
type Credential struct {
	Adapter       string  `json:"adapter"`
	Name          string  `json:"name"`
	EvidenceClass string  `json:"evidence_class"`
	ObservedOn    string  `json:"observed_on"`
	ForgeCents    float64 `json:"forge_cents"`
	RentCents     float64 `json:"rent_cents"`
	Live          bool    `json:"live"`
	AgeCurve      string  `json:"age_curve"`
	HalfLifeDays  int64   `json:"half_life_days"`
	IssuedAt      *int64  `json:"issued_at"`
	AgeDays       *int64  `json:"age_days"`
	AgeWeight     float64 `json:"age_weight"`
	Source        string  `json:"source"`
}

// Root is one trust root's contribution: the strongest credential under it
// (saturated, not summed), in US cents.
type Root struct {
	Root              string      `json:"root"`
	ContributionCents float64     `json:"contribution_cents"`
	Saturated         bool        `json:"saturated"`
	Adapters          []string    `json:"adapters"`
	Strongest         *Credential `json:"strongest"`
}

// Check is a credential check that could not be read: not evidence of
// absence.
type Check struct {
	Adapter string `json:"adapter"`
	Address string `json:"address"`
}

// Caveat is one of the answer's caveats, in Corroborate's words.
type Caveat struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Registry names the ontology an answer was scored against: the registry
// contract, its chain, the revision and the block it was read at, and the
// SHA-256 of the pinned copy (empty for an as_of answer, which reads the
// registry as it stood at that block).
type Registry struct {
	Address   string `json:"address"`
	Chain     string `json:"chain"`
	ChainID   int64  `json:"chain_id"`
	Revision  int64  `json:"revision"`
	Block     int64  `json:"block"`
	BlockTime int64  `json:"block_time"`
	SHA256    string `json:"sha256"`
}

// Checks counts the credential checks made: held, and unreadable.
type Checks struct {
	Total       int `json:"total"`
	Held        int `json:"held"`
	Unavailable int `json:"unavailable"`
}

// Result is one resolve: the total cost to fake the set's evidence in US
// cents, its log score (log10(1 + cents)), each root's part, and what it
// was scored against.
type Result struct {
	Addresses        []string        `json:"addresses"`
	Score            float64         `json:"score"`
	TotalCents       float64         `json:"total_cents"`
	IndependentRoots int             `json:"independent_roots"`
	Roots            []Root          `json:"roots"`
	Checks           Checks          `json:"checks"`
	Unavailable      []Check         `json:"unavailable"`
	Caveats          []Caveat        `json:"caveats"`
	Registry         Registry        `json:"registry"`
	AsOf             json.RawMessage `json:"as_of"`
	ComputedAt       int64           `json:"computed_at"`
}

var addressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Client asks one sidecar. The zero value is not usable; use New.
type Client struct {
	url  string
	http *http.Client
	// Pin, when set, is the registry SHA-256 every pinned answer must carry:
	// a sidecar running another pin is refused as unavailable, so a registry
	// change cannot move what a caller computes without a change on its own
	// side. The standing run sets it from its parameters; the public service
	// reports whatever pin the sidecar runs.
	Pin string
}

// ValidURL reports whether raw is a sidecar URL the client will call:
// http://127.0.0.1:PORT or http://[::1]:PORT, with no path, user or query.
func ValidURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	return ip != nil && ip.IsLoopback() && err == nil && port > 0 && port < 65536
}

// New is a client of the sidecar at raw (DefaultURL when empty).
func New(raw string) (*Client, error) {
	if raw == "" {
		raw = DefaultURL
	}
	if !ValidURL(raw) {
		return nil, fmt.Errorf("CORROBORATE_URL must be http://127.0.0.1:PORT or http://[::1]:PORT (the sidecar listens on loopback only), not %q", raw)
	}
	u, _ := url.Parse(raw)
	host := u.Host
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &Client{
		url: "http://" + host + "/resolve",
		http: &http.Client{
			Timeout:       Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy: nil,
				// Only the configured loopback address, whatever the URL says.
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, network, host)
				},
				MaxIdleConns:          4,
				IdleConnTimeout:       60 * time.Second,
				ResponseHeaderTimeout: Timeout,
			},
		},
	}, nil
}

// Resolve scores an address set: 1 to MaxAddresses addresses, each 0x and
// 40 hex digits (the caller checks EIP-55 casing), and asOf a Sepolia
// registry block (0 for the pinned revision). The caller is responsible for
// the set: Corroborate never infers that two addresses belong together.
func (c *Client) Resolve(ctx context.Context, addresses []string, asOf int64) (*Result, error) {
	if len(addresses) == 0 || len(addresses) > MaxAddresses {
		return nil, &ErrRequest{Code: "invalid_request", Message: fmt.Sprintf("1 to %d addresses", MaxAddresses)}
	}
	for _, a := range addresses {
		if !addressRE.MatchString(a) {
			return nil, &ErrRequest{Code: "invalid_request", Message: "an address is 0x and 40 hex digits"}
		}
	}
	if asOf != 0 && asOf < RegistryGenesisBlock {
		return nil, &ErrRequest{Code: "invalid_request", Message: "as_of predates the registry"}
	}
	req := map[string]any{"addresses": addresses}
	if asOf != 0 {
		req["as_of"] = asOf
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hr)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
			return nil, &ErrUnavailable{Reason: "timeout", RetryAfter: 15}
		}
		return nil, &ErrUnavailable{Reason: "unreachable", RetryAfter: 30}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, responseMax+1))
	if err != nil || len(raw) > responseMax {
		return nil, &ErrUnavailable{Reason: "unreadable answer", RetryAfter: 30}
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusBadRequest:
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			return nil, &ErrUnavailable{Reason: "unreadable answer", RetryAfter: 30}
		}
		return nil, &ErrRequest{Code: e.Error, Message: e.Message}
	default:
		retry := 30
		if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 && n <= 3600 {
			retry = n
		}
		return nil, &ErrUnavailable{Reason: "sidecar " + strconv.Itoa(resp.StatusCode), RetryAfter: retry}
	}
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil || !r.valid() {
		return nil, &ErrUnavailable{Reason: "unreadable answer", RetryAfter: 30}
	}
	if c.Pin != "" && len(r.AsOf) == 0 && r.Registry.SHA256 != c.Pin {
		return nil, &ErrUnavailable{Reason: "sidecar runs another registry pin", RetryAfter: 300}
	}
	return &r, nil
}

// valid holds an answer to the shape a score needs: finite, non-negative,
// a named registry revision, and something actually read. Anything else is
// treated as no answer, never as a zero.
func (r *Result) valid() bool {
	if len(r.Addresses) == 0 || r.TotalCents < 0 || r.Score < 0 || r.Score > 20 || r.Registry.Revision <= 0 || r.Registry.Address == "" {
		return false
	}
	if r.Checks.Total == 0 || r.Checks.Unavailable >= r.Checks.Total {
		return false
	}
	if len(r.AsOf) > 0 && string(r.AsOf) == "null" {
		r.AsOf = nil
	}
	return true
}

// TODO(RFC0015 §3.2, wallet link slice): once `wallet` links verify (a
// CAIP-122 signature naming the fingerprint), the standing run resolves each
// account's linked address set here with Client.Pin set from the trust
// parameters, stores {roots, contributions, registry revision, block} as the
// proof's state, keeps the last value for at most proof_fresh_days while the
// sidecar is unavailable (personhood then reads "unknown", never failed), and
// prices one root once across a fleet (§3.2 "one root, one identity").
// Nothing calls it from the trust run yet: standing is unchanged by this slice.
