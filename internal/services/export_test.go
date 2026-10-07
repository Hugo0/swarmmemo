package services

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

// FetchConfigForTest is a FETCH_CONFIG whose resolver answers from hosts
// (a name to its addresses), whose address decision is public (nil: the
// production one), and whose dial goes to target instead of the checked
// address and its port (an httptest server), with roots for its TLS.
func FetchConfigForTest(body []byte, hosts map[string][]net.IP, public func(net.IP) error, target string, roots *tls.Config) (*FetchConfig, error) {
	cfg, err := ParseFetchConfig(body)
	if err != nil {
		return nil, err
	}
	cfg.lookup = func(_ context.Context, host string) ([]net.IP, error) {
		ips, ok := hosts[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return ips, nil
	}
	cfg.public = public
	if target != "" {
		cfg.dialAddr = func(net.IP, string) string { return target }
	}
	cfg.tls = roots
	return cfg, nil
}

// SetFetchLookupForTest replaces cfg's resolver.
func SetFetchLookupForTest(cfg *FetchConfig, lookup func(context.Context, string) ([]net.IP, error)) {
	cfg.lookup = lookup
}

// SetFetchIntervalForTest shortens cfg's per-host spacing, which
// FETCH_CONFIG keeps at a second or more.
func SetFetchIntervalForTest(cfg *FetchConfig, d time.Duration) { cfg.interval = d }

// HTMLToTextForTest converts an HTML page, for the fuzz target and the
// converter's tests.
func HTMLToTextForTest(doc, base string) (title, text string) {
	u, _ := url.Parse(base)
	p := htmlToText(doc, u)
	return p.Title, p.Text
}

// RobotsAllowedForTest parses a robots.txt for SwarmMemoFetch and asks
// whether path is allowed.
func RobotsAllowedForTest(body, path string) bool {
	return parseRobots(body, FetchRobotsToken).allowed(path)
}

// QuoteForTest runs a built-in provider's argument parser and quote (memory
// put and delete, echo) on args, for the data fuzz target.
func QuoteForTest(method string, args json.RawMessage) (Quote, error) {
	c := Call{Method: method, Args: args, Price: Price{Base: 1, PerByte: 1, PerKiB: 1}}
	if method == "complete" {
		return newInference(Deps{}).Quote(c)
	}
	if method == "echo" {
		echo{}.ModeFor(c)
		return echo{}.Quote(c)
	}
	return (&memory{}).Quote(c)
}

// InferenceConfigForTest parses an INFERENCE_CONFIG body that may name plain
// http upstreams (httptest). With safe false requests go through an ordinary
// dialer, since httptest listens on loopback; with safe true they go through
// the production safe dialer, which must refuse it.
func InferenceConfigForTest(body []byte, safe bool) (*InferenceConfig, error) {
	cfg, err := parseInferenceConfig(body, true)
	if err != nil {
		return nil, err
	}
	if !safe {
		cfg.dial = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	}
	return cfg, nil
}

// InferenceReplyForTest runs an upstream reply parser (workers_ai when
// workers is true, else openai_compat), for the reply fuzz target.
func InferenceReplyForTest(raw []byte, workers bool) (output, finish string, in, out int64, reported bool, err error) {
	var r inferenceReply
	if workers {
		r, err = parseWorkersAIResponse(raw)
	} else {
		r, err = parseOpenAIResponse(raw)
	}
	return r.Output, r.FinishReason, r.InputTokens, r.OutputTokens, r.Reported, err
}

// PublicDataConfigForTest serves each catalogue host named in remap from a
// loopback test server over plain HTTP. With safe false requests go through
// an ordinary dialer; with safe true through the production safe dialer,
// which must refuse loopback.
func PublicDataConfigForTest(keyDir string, remap map[string]string, safe bool) *PublicDataConfig {
	c := &PublicDataConfig{KeyDir: keyDir, remap: remap}
	if !safe {
		c.dial = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	}
	return c
}

// PublicDataHosts is every host the catalogue may reach.
func PublicDataHosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, ds := range pdCatalogue {
		for _, h := range ds.Hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// PublicDataParseForTest parses fetch or bulk args: each request's dataset
// and its parameters as resolved.
func PublicDataParseForTest(method string, args json.RawMessage, now int64) ([]string, []map[string]any, error) {
	reqs, err := parsePublicData(method, args, now)
	if err != nil {
		return nil, nil, err
	}
	var ids []string
	var params []map[string]any
	for _, r := range reqs {
		ids = append(ids, r.ds.ID)
		params = append(params, r.params.echo())
	}
	return ids, params, nil
}

// PublicDataFetchForTest makes one fetch for dataset from host and path
// through the provider's own client, for the host-allowlist tests.
func PublicDataFetchForTest(cfg *PublicDataConfig, db *sql.DB, dataset, host, path string, now int64) error {
	p := newPublicData(Deps{DB: db, PublicData: cfg}).(*publicData)
	r := &pdRun{p: p, ds: pdByID[dataset], now: now}
	_, err := r.fetch(context.Background(), pdFetch{Key: "test:" + host + path, TTL: time.Minute, Host: host, Path: path,
		Parse: func(raw []byte) (any, error) { return string(raw), nil }})
	return err
}

// PublicDataDatasetsForTest is the datasets read's body.
func PublicDataDatasetsForTest(cfg *PublicDataConfig) map[string]any {
	return newPublicData(Deps{PublicData: cfg}).(*publicData).Datasets()
}

// InferenceQuoteForTest runs inference's argument parser and quote.
func InferenceQuoteForTest(cfg *InferenceConfig, args json.RawMessage) (Quote, error) {
	return newInference(Deps{Inference: cfg}).Quote(Call{Method: "complete", Args: args})
}

// ReceiverDedupeHeaderForTest is the dedupe_header create stores for raw
// create arguments, or the refusal.
func ReceiverDedupeHeaderForTest(raw json.RawMessage) (string, error) {
	s, err := parseReceiverCreate(raw)
	return s.dedupe, err
}

// FillReceiverWindowsForTest counts one delivery for each of n other
// receiver keys in the receiver's minute table, as table pressure would.
func FillReceiverWindowsForTest(e *Engine, n int, now int64) error {
	r, err := e.receiverProvider()
	if err != nil {
		return err
	}
	for i := range n {
		if err := r.admit(r.perRecv, fmt.Sprintf("pressure-%d", i), ReceiverPerMinute, now); err != nil {
			return err
		}
	}
	return nil
}

// RateEntriesMax is the bound of a rate table.
const RateEntriesMax = rateEntriesMax
