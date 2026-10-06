package services

// runs executes an agent's JavaScript or Python in a Cloudflare Dynamic
// Worker, through the runs loader (workers/runs-loader). One call is one run:
//
//	screen the code (moderation surface run.code): block or hold, nothing runs
//	admit it under the per-account and global daily caps
//	decide the network: asked for, allowed by config, the lever not pulled,
//	  the account not suspended, the global egress budget not spent
//	call the loader over HTTPS, HMAC both ways, to the configured host only
//	screen the egress log (run.egress): flag it; block suspends the account's network
//	sign a receipt over the code, input, output and egress hashes
//	record everything in runs_log (the run log the steward reviews)
//
// It is a Remote service: the call reserves the most the run can cost (its
// CPU limit and egress caps at the current price), runs after the command
// commits, and commits the CPU milliseconds and egress bytes it used.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"swarmmemo/internal/allowance"
)

// Runs bounds and defaults. A config may lower each limit, never raise it
// past the loader's hard bounds (runsloader.go).
const (
	// RunsArgsMax bounds run's args JSON: the code (at most RunsCodeBytes
	// once decoded) with room for JSON escaping, and the input.
	RunsArgsMax = 128 << 10
	// RunsMaxDuration bounds one call: both screens, the loader call and the
	// bookkeeping.
	RunsMaxDuration = 45 * time.Second
	// RunsScreenTimeout bounds one moderation screen; a screen that does not
	// answer in time holds the run.
	RunsScreenTimeout = 5 * time.Second
	// RunsConfigBytes and runsSecretBytes bound the files RUNS_CONFIG names.
	RunsConfigBytes    = 64 << 10
	runsSecretMinBytes = 32
	runsSecretMaxBytes = 4096
	// Caller-facing body bounds: the full output stays in the run log.
	runsBodyStdout = 4 << 10
	runsBodyStderr = 2 << 10
	runsBodyResult = 4 << 10
	runsBodyError  = 1 << 10
	// RunReceiptSchema names the receipt format.
	RunReceiptSchema = "swarmmemo-run/1"
	// Moderation surfaces (internal/moderation).
	RunsSurfaceCode   = "run.code"
	RunsSurfaceEgress = "run.egress"
)

// RunScreener is the moderation hook: internal/board adapts
// moderation.Engine.Screen to it (runScreener) when MODERATION is on.
// Subject is the caller's continuity account; content is JSON
// (runsCodeContent, runsEgressContent).
type RunScreener interface {
	Screen(ctx context.Context, surface, subject, content string) RunScreenDecision
}

// RunScreenDecision is a screen's answer. Action is "allow", "hold" or
// "block"; anything else counts as "hold".
type RunScreenDecision struct {
	Action string
	Reason string // one short sentence, shown to the caller

	timedOut bool // no answer in time: a hold for this call only, never recorded
}

// RunsConfig is RUNS_CONFIG: the loader, the secret, limits, egress policy,
// daily caps and the network lever. It holds no secret itself.
type RunsConfig struct {
	LoaderURL      string      `json:"loader_url"`
	SecretFile     string      `json:"secret_file"`
	ServiceID      string      `json:"service_id"`
	Languages      []string    `json:"languages"`
	Network        string      `json:"network"`          // "allowed" or "off" (the default)
	NetworkOffFile string      `json:"network_off_file"` // while this file exists, every run is network-off
	Screen         string      `json:"screen"`           // "required" (default): a screener must be wired; "static": the built-in rules
	Limits         RunsLimits  `json:"limits"`
	Egress         RunsEgress  `json:"egress"`
	Caps           RunsCaps    `json:"caps"`
	Screener       RunScreener `json:"-"`

	secret []byte
	url    *url.URL
	host   string // host:port
	clock  func() time.Time
	// dial, allowHTTP and httpGrace exist only for tests (export_runs_test.go).
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	allowHTTP bool
	httpGrace time.Duration
}

type RunsLimits struct {
	CPUMsDefault   int64 `json:"cpu_ms_default"`
	CPUMsMax       int64 `json:"cpu_ms_max"`
	WallMsDefault  int64 `json:"wall_ms_default"`
	WallMsMax      int64 `json:"wall_ms_max"`
	SubrequestsMax int64 `json:"subrequests_max"`
	StdoutBytes    int64 `json:"stdout_bytes"`
	StderrBytes    int64 `json:"stderr_bytes"`
	ResultBytes    int64 `json:"result_bytes"`
}

type RunsEgress struct {
	MaxRequests int64    `json:"max_requests"`
	MaxBytesOut int64    `json:"max_bytes_out"`
	MaxBytesIn  int64    `json:"max_bytes_in"`
	Ports       []int    `json:"ports"`
	DenyHosts   []string `json:"deny_hosts"`
	DenyCIDRs   []string `json:"deny_cidrs"`
	Resolve     *bool    `json:"resolve"`
}

type RunsCaps struct {
	AccountRunsDay       int64 `json:"account_runs_day"`
	AccountCPUMsDay      int64 `json:"account_cpu_ms_day"`
	GlobalRunsDay        int64 `json:"global_runs_day"`
	GlobalCPUMsDay       int64 `json:"global_cpu_ms_day"`
	GlobalEgressBytesDay int64 `json:"global_egress_bytes_day"`
}

var (
	runsDenyHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	runsCIDRRE     = regexp.MustCompile(`^[0-9a-f:.]+/[0-9]{1,3}$`)
)

// LoadRunsConfig reads and validates RUNS_CONFIG and its secret file.
func LoadRunsConfig(path string) (*RunsConfig, error) {
	body, err := readBounded(path, RunsConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("RUNS_CONFIG: %w", err)
	}
	return parseRunsConfig(body, false)
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return body, nil
}

func parseRunsConfig(body []byte, allowHTTP bool) (*RunsConfig, error) {
	cfg := &RunsConfig{allowHTTP: allowHTTP, clock: time.Now}
	if err := StrictObject(body, cfg); err != nil {
		return nil, errors.New("RUNS_CONFIG: not a strict JSON object with the documented keys (unknown, repeated or mistyped field)")
	}
	u, err := url.Parse(cfg.LoaderURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, errors.New("RUNS_CONFIG: loader_url must be an https URL with no credentials, query or fragment")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/run"
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	cfg.url, cfg.host = u, net.JoinHostPort(u.Hostname(), port)
	secret, err := readBounded(cfg.SecretFile, runsSecretMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("RUNS_CONFIG: secret_file: %w", err)
	}
	cfg.secret = bytes.TrimRight(secret, "\r\n")
	if len(cfg.secret) < runsSecretMinBytes {
		return nil, fmt.Errorf("RUNS_CONFIG: secret_file must hold at least %d bytes", runsSecretMinBytes)
	}
	if len(cfg.Languages) == 0 {
		return nil, errors.New("RUNS_CONFIG: languages must name javascript, python or both")
	}
	for _, l := range cfg.Languages {
		if l != "javascript" && l != "python" {
			return nil, fmt.Errorf("RUNS_CONFIG: unknown language %q", l)
		}
	}
	if cfg.Network == "" {
		// Off unless the operator allows it, after the loader's smoke test
		// shows raw connect() refused (deploy/RUNBOOK.md, "Runs loader").
		cfg.Network = "off"
	}
	if cfg.Network != "allowed" && cfg.Network != "off" {
		return nil, errors.New(`RUNS_CONFIG: network must be "allowed" or "off"`)
	}
	switch cfg.Screen {
	case "":
		cfg.Screen = "required"
	case "required", "static":
	default:
		return nil, errors.New(`RUNS_CONFIG: screen must be "required" or "static"`)
	}
	l := &cfg.Limits
	for _, d := range []struct {
		v        *int64
		def, max int64
		name     string
	}{
		{&l.CPUMsMax, 10_000, RunsCPUMsMax, "cpu_ms_max"},
		{&l.WallMsMax, 20_000, RunsWallMsMax, "wall_ms_max"},
		{&l.SubrequestsMax, 32, RunsSubrequestsMax, "subrequests_max"},
		{&l.StdoutBytes, 32 << 10, RunsOutputBytesMax, "stdout_bytes"},
		{&l.StderrBytes, 16 << 10, RunsOutputBytesMax, "stderr_bytes"},
		{&l.ResultBytes, 32 << 10, RunsOutputBytesMax, "result_bytes"},
		{&cfg.Egress.MaxRequests, 16, RunsEgressRequestsMax, "egress.max_requests"},
		{&cfg.Egress.MaxBytesOut, 256 << 10, RunsEgressBytesOutMax, "egress.max_bytes_out"},
		{&cfg.Egress.MaxBytesIn, 4 << 20, RunsEgressBytesInMax, "egress.max_bytes_in"},
		{&cfg.Caps.AccountRunsDay, 50, 1 << 20, "caps.account_runs_day"},
		{&cfg.Caps.AccountCPUMsDay, 60_000, 1 << 40, "caps.account_cpu_ms_day"},
		{&cfg.Caps.GlobalRunsDay, 1000, 1 << 30, "caps.global_runs_day"},
		{&cfg.Caps.GlobalCPUMsDay, 600_000, 1 << 40, "caps.global_cpu_ms_day"},
		{&cfg.Caps.GlobalEgressBytesDay, 1 << 30, 1 << 50, "caps.global_egress_bytes_day"},
	} {
		if *d.v == 0 {
			*d.v = d.def
		}
		if *d.v < 1 || *d.v > d.max {
			return nil, fmt.Errorf("RUNS_CONFIG: %s must be 1 to %d", d.name, d.max)
		}
	}
	if l.CPUMsDefault == 0 {
		l.CPUMsDefault = min(1000, l.CPUMsMax)
	}
	if l.WallMsDefault == 0 {
		l.WallMsDefault = min(10_000, l.WallMsMax)
	}
	if l.CPUMsDefault < 1 || l.CPUMsDefault > l.CPUMsMax || l.WallMsMax < RunsWallMsMin || l.WallMsDefault < RunsWallMsMin || l.WallMsDefault > l.WallMsMax {
		return nil, errors.New("RUNS_CONFIG: the default limits must sit inside the maximums")
	}
	e := &cfg.Egress
	if len(e.Ports) == 0 {
		e.Ports = []int{80, 443}
	}
	// Two deny_hosts places are kept for the service's own hosts (denyHosts).
	if len(e.Ports) > 16 || len(e.DenyHosts) > RunsDenyEntriesMax-2 || len(e.DenyCIDRs) > RunsDenyEntriesMax {
		return nil, errors.New("RUNS_CONFIG: at most 16 ports and 256 entries in each deny list")
	}
	for _, p := range e.Ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("RUNS_CONFIG: port %d", p)
		}
	}
	for _, h := range e.DenyHosts {
		if len(h) > 253 || !runsDenyHostRE.MatchString(h) {
			return nil, fmt.Errorf("RUNS_CONFIG: deny_hosts entry %q must be a lowercase host name", h)
		}
	}
	for _, c := range e.DenyCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil || !runsCIDRRE.MatchString(c) {
			return nil, fmt.Errorf("RUNS_CONFIG: deny_cidrs entry %q", c)
		}
	}
	if e.DenyHosts == nil {
		e.DenyHosts = []string{}
	}
	if e.DenyCIDRs == nil {
		e.DenyCIDRs = []string{}
	}
	if e.Resolve == nil {
		t := true
		e.Resolve = &t
	}
	return cfg, nil
}

// denyHosts is the configured deny list plus the service's own hosts: a run
// never reaches the board it was called from, nor the loader.
func (cfg *RunsConfig) denyHosts() []string {
	out := slices.Clone(cfg.Egress.DenyHosts)
	for _, h := range []string{strings.ToLower(cfg.ServiceID), strings.ToLower(cfg.url.Hostname())} {
		if h != "" && runsDenyHostRE.MatchString(h) && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// networkLever reports whether the steward's network-off lever is pulled.
func (cfg *RunsConfig) networkLever() bool {
	if cfg.NetworkOffFile == "" {
		return false
	}
	_, err := os.Stat(cfg.NetworkOffFile)
	return err == nil
}

// ---------------------------------------------------------------- the provider

type runs struct {
	cfg    *RunsConfig
	db     *sql.DB
	client *runsClient
	key    ed25519.PrivateKey // the notary key (Deps.NotaryKey); nil refuses every run
}

func newRuns(d Deps) Provider {
	return &runs{cfg: d.Runs, db: d.DB, client: newRunsClient(d.Runs), key: d.NotaryKey}
}

func (*runs) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS runs_code (sha256 TEXT PRIMARY KEY, language TEXT NOT NULL, code TEXT NOT NULL, first_seen INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS runs_log (
 run_id TEXT PRIMARY KEY, account TEXT NOT NULL, request_key TEXT NOT NULL, created_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0,
 language TEXT NOT NULL, code_sha256 TEXT NOT NULL, input TEXT NOT NULL, input_sha256 TEXT NOT NULL,
 network_requested INTEGER NOT NULL, network TEXT NOT NULL DEFAULT 'off', network_note TEXT NOT NULL DEFAULT '',
 cpu_ms_limit INTEGER NOT NULL, wall_ms_limit INTEGER NOT NULL, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
 output TEXT NOT NULL DEFAULT '', output_sha256 TEXT NOT NULL DEFAULT '', egress TEXT NOT NULL DEFAULT '', egress_sha256 TEXT NOT NULL DEFAULT '',
 cpu_ms INTEGER NOT NULL DEFAULT 0, cpu_source TEXT NOT NULL DEFAULT '', wall_ms INTEGER NOT NULL DEFAULT 0,
 egress_bytes INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0,
 code_decision TEXT NOT NULL DEFAULT '', egress_decision TEXT NOT NULL DEFAULT '', flagged INTEGER NOT NULL DEFAULT 0,
 receipt TEXT NOT NULL DEFAULT '', UNIQUE(account,request_key));
CREATE INDEX IF NOT EXISTS runs_log_account ON runs_log(account,created_at);
CREATE INDEX IF NOT EXISTS runs_log_flagged ON runs_log(created_at) WHERE flagged=1;
CREATE TABLE IF NOT EXISTS runs_usage (day INTEGER NOT NULL, account TEXT NOT NULL, runs INTEGER NOT NULL DEFAULT 0,
 cpu_ms INTEGER NOT NULL DEFAULT 0, egress_bytes INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(day,account));
CREATE TABLE IF NOT EXISTS runs_reviews (code_sha256 TEXT PRIMARY KEY, state TEXT NOT NULL CHECK(state IN ('held','approved','blocked')),
 reason TEXT NOT NULL DEFAULT '', account TEXT NOT NULL, created_at INTEGER NOT NULL, decided_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS runs_network_blocks (account TEXT PRIMARY KEY, reason TEXT NOT NULL, run_id TEXT NOT NULL, created_at INTEGER NOT NULL,
 lifted_at INTEGER NOT NULL DEFAULT 0);
`
}

func (*runs) Describe() Descriptor {
	return Descriptor{
		ID: "runs",
		Summary: `Runs JavaScript or Python in a Cloudflare Dynamic Worker: args {"language","code","input","cpu_ms","wall_ms","network":{...}}; the code exports run(input). ` +
			`Price: base per run, per_byte per CPU millisecond, per_kib per KiB of egress. Code and input are screened before the run. Network is off unless asked for, logged per request, and screened; the result carries a signed receipt. A run whose answer was lost (loader_error) is charged at its limits.`,
		Title: "Runs", Topic: "Code runs",
		Line: "Run a short JavaScript or Python function in a sandbox and get its result with a signed receipt; the network is off unless you ask.",
		Limits: []Limit{
			{"run_args_bytes", RunsArgsMax, "bytes", "Arguments of one run"},
			{"run_code_bytes", RunsCodeBytes, "bytes", "Code of one run, UTF-8"},
			{"run_input_bytes", RunsInputBytes, "bytes", "Input of one run, JSON"},
		},
		Mode: Remote,
		Methods: []Method{
			{Name: "run", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: RunsArgsMax, Price: Price{Base: 10, PerByte: 1, PerKiB: 1},
				Line:           "Run code once; the result carries stdout, stderr and a signed receipt.",
				PriceNote:      "base per run + per_byte per CPU millisecond + per_kib per KiB of egress, in credit; charged what it used, and at its limits if its answer was lost",
				ExampleMaxCost: 1100,
				Args: []Arg{
					{"language", "string", true, "javascript or python"},
					{"code", "string", true, "defines run(input); up to " + SizeText(RunsCodeBytes)},
					{"input", "any", false, "JSON passed to run, up to " + SizeText(RunsInputBytes)},
					{"cpu_ms", "integer", false, "CPU limit in milliseconds"},
					{"wall_ms", "integer", false, "wall-clock limit in milliseconds"},
					{"network", "object", false, `{"max_requests","max_bytes_out","max_bytes_in"}; off when absent`},
				},
				Example: json.RawMessage(`{"language":"javascript","code":"export function run(input) { return input.n * 2 }","input":{"n":21},"cpu_ms":1000}`)},
			{Name: "log", Signed: true, ArgsMax: 128, Line: "Your whole run: output, egress log and receipt.",
				Args: []Arg{{"run", "string", true, "the run id"}}, Example: json.RawMessage(`{"run":"RUN_ID"}`)},
		},
		MaxDuration: RunsMaxDuration,
	}
}

type runsArgs struct {
	Language string          `json:"language"`
	Code     *string         `json:"code"`
	Input    json.RawMessage `json:"input"`
	CPUMs    json.RawMessage `json:"cpu_ms"`
	WallMs   json.RawMessage `json:"wall_ms"`
	Network  *runsNetArgs    `json:"network"`
}

type runsNetArgs struct {
	MaxRequests json.RawMessage `json:"max_requests"`
	MaxBytesOut json.RawMessage `json:"max_bytes_out"`
	MaxBytesIn  json.RawMessage `json:"max_bytes_in"`
}

type runPlan struct {
	language    string
	code        string
	input       json.RawMessage
	cpuMs       int64
	wallMs      int64
	network     bool // asked for and allowed by config
	asked       bool // asked for
	maxRequests int64
	maxBytesOut int64
	maxBytesIn  int64
}

// egressMax is the most egress this run can be charged for.
func (p runPlan) egressMax() int64 {
	if !p.network {
		return 0
	}
	return p.maxBytesOut + p.maxBytesIn
}

func (r *runs) plan(raw json.RawMessage) (runPlan, error) {
	var a runsArgs
	if err := StrictObject(raw, &a); err != nil {
		return runPlan{}, err
	}
	cfg := r.cfg
	if cfg == nil {
		return runPlan{}, refusal("service_unavailable")
	}
	bad := refusal("invalid_service_data")
	if a.Code != nil && len(*a.Code) > RunsCodeBytes {
		return runPlan{}, tooLarge("invalid_service_data", len(*a.Code), RunsCodeBytes)
	}
	if !slices.Contains(cfg.Languages, a.Language) || a.Code == nil || *a.Code == "" {
		return runPlan{}, bad
	}
	p := runPlan{language: a.Language, code: *a.Code, input: a.Input, cpuMs: cfg.Limits.CPUMsDefault, wallMs: cfg.Limits.WallMsDefault}
	if len(p.input) == 0 {
		p.input = json.RawMessage("null")
	}
	if len(p.input) > RunsInputBytes {
		return runPlan{}, tooLarge("invalid_service_data", len(p.input), RunsInputBytes)
	}
	var ok bool
	if a.CPUMs != nil {
		if p.cpuMs, ok = Integer(a.CPUMs, cfg.Limits.CPUMsMax); !ok || p.cpuMs < 1 {
			return runPlan{}, bad
		}
	}
	if a.WallMs != nil {
		if p.wallMs, ok = Integer(a.WallMs, cfg.Limits.WallMsMax); !ok || p.wallMs < RunsWallMsMin {
			return runPlan{}, bad
		}
	}
	p.asked = a.Network != nil
	if n := a.Network; n != nil && cfg.Network == "allowed" {
		p.network = true
		p.maxRequests, p.maxBytesOut, p.maxBytesIn = cfg.Egress.MaxRequests, cfg.Egress.MaxBytesOut, cfg.Egress.MaxBytesIn
		for _, f := range []struct {
			raw json.RawMessage
			dst *int64
		}{{n.MaxRequests, &p.maxRequests}, {n.MaxBytesOut, &p.maxBytesOut}, {n.MaxBytesIn, &p.maxBytesIn}} {
			if f.raw != nil {
				if *f.dst, ok = Integer(f.raw, *f.dst); !ok {
					return runPlan{}, bad
				}
			}
		}
	}
	return p, nil
}

// runsCost is the price of a run: Base + PerByte × CPU ms + PerKiB × egress KiB.
func runsCost(p Price, cpuMs, egressBytes int64) int64 {
	return p.Base + p.PerByte*max(cpuMs, 0) + p.PerKiB*((max(egressBytes, 0)+1023)/1024)
}

func (r *runs) Quote(c Call) (Quote, error) {
	if c.Method != "run" {
		return Quote{}, refusal("invalid_service_data")
	}
	p, err := r.plan(c.Args)
	if err != nil {
		return Quote{}, err
	}
	if len(r.key) != ed25519.PrivateKeySize {
		// No receipt could be signed: refuse before anything is reserved.
		return Quote{}, refusal("service_unavailable")
	}
	if r.cfg.Screen == "required" && r.cfg.Screener == nil {
		// Fail closed: no run without the moderation screen the config asks for.
		return Quote{}, refusal("service_unavailable")
	}
	return Quote{Resource: allowance.Credit, Max: runsCost(c.Price, p.cpuMs, p.egressMax())}, nil
}

// runIDFor names a run after its call: the same account and request key
// always give the same run ID, so the loader refuses a replay.
func runIDFor(account, requestKey string) string {
	sum := sha256.Sum256([]byte("run\x00" + account + "\x00" + requestKey))
	return hex.EncodeToString(sum[:16])
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- screening

// runsCodeContent is what the code screen sees: the code and the run's
// input together, since a small interpreter or fetch proxy passes a screen
// of its code alone and takes its real payload as input (security review
// 1.20, M8).
type runsCodeContent struct {
	Language string          `json:"language"`
	Network  bool            `json:"network"`
	Code     string          `json:"code"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// RunScreenKey names what a code screen decided on, and what a review of it
// approves or blocks: the code alone when the input is empty (null), else
// the code and the input together, so an approval never carries over to
// another input.
func RunScreenKey(code string, input json.RawMessage) string {
	if in := bytes.TrimSpace(input); len(in) == 0 || string(in) == "null" {
		return sha256Of([]byte(code))
	}
	h := sha256.New()
	fmt.Fprintf(h, "run-screen\x00%d\x00", len(code))
	h.Write([]byte(code))
	h.Write([]byte{0})
	h.Write(input)
	return hex.EncodeToString(h.Sum(nil))
}

// RunScreenText is the text a code screen reads: the code, then the input.
func RunScreenText(code string, input json.RawMessage) string {
	if in := bytes.TrimSpace(input); len(in) == 0 || string(in) == "null" {
		return code
	}
	return code + "\n\n// run input (JSON):\n" + string(input)
}

type runsEgressContent struct {
	RunID  string        `json:"run_id"`
	Egress EgressSummary `json:"egress"`
}

func (r *runs) screener() RunScreener {
	if r.cfg.Screener != nil {
		return r.cfg.Screener
	}
	return StaticRunScreener{}
}

// screen asks the screener, bounded by RunsScreenTimeout; no answer is a hold.
func (r *runs) screen(ctx context.Context, surface, subject string, content any) RunScreenDecision {
	body, _ := json.Marshal(content)
	ctx, cancel := context.WithTimeout(ctx, RunsScreenTimeout)
	defer cancel()
	out := make(chan RunScreenDecision, 1)
	go func() { out <- r.screener().Screen(ctx, surface, subject, string(body)) }()
	var d RunScreenDecision
	select {
	case d = <-out:
	case <-ctx.Done():
		d = RunScreenDecision{Action: "hold", Reason: "The moderation screen did not answer in time; retry later.", timedOut: true}
	}
	if d.Action != "allow" && d.Action != "block" {
		d.Action = "hold"
	}
	d.Reason = truncateString(d.Reason, 200)
	return d
}

// StaticRunScreener is the built-in stand-in for the moderation module
// (screen "static"): it blocks code carrying well-known miner markers, and
// flags egress that reached for a mining pool or a metadata address.
type StaticRunScreener struct{}

var staticMinerMarkers = []string{"stratum+tcp://", "stratum+ssl://", "stratum2+tcp://", "xmrig", "coinhive", "cryptonight", "randomx", "minexmr", "nicehash", "coin-hive"}

func (StaticRunScreener) Screen(ctx context.Context, surface, subject, content string) RunScreenDecision {
	switch surface {
	case RunsSurfaceCode:
		var c runsCodeContent
		_ = json.Unmarshal([]byte(content), &c)
		low := strings.ToLower(RunScreenText(c.Code, c.Input))
		for _, m := range staticMinerMarkers {
			if strings.Contains(low, m) {
				return RunScreenDecision{Action: "block", Reason: "The code carries a cryptocurrency-miner marker."}
			}
		}
	case RunsSurfaceEgress:
		var e runsEgressContent
		_ = json.Unmarshal([]byte(content), &e)
		hold := false
		for _, x := range e.Egress.Log {
			switch x.Reason {
			case "mining_pool":
				return RunScreenDecision{Action: "block", Reason: "The run reached for a mining pool."}
			case "metadata", "private_address", "raw_tcp":
				hold = true
			}
		}
		if hold {
			return RunScreenDecision{Action: "hold", Reason: "The run reached for an internal address or raw TCP."}
		}
	}
	return RunScreenDecision{Action: "allow"}
}

// codeDecision applies a standing review of this exact code and input first
// (a steward's approval or an earlier block; RunScreenKey), else screens the
// code with its input and records a hold or block for review.
func (r *runs) codeDecision(ctx context.Context, p runPlan, account string, now int64) (RunScreenDecision, error) {
	key := RunScreenKey(p.code, p.input)
	var state, reason string
	err := r.db.QueryRowContext(ctx, "SELECT state, reason FROM runs_reviews WHERE code_sha256=?", key).Scan(&state, &reason)
	switch {
	case err == nil && state == "approved":
		return RunScreenDecision{Action: "allow", Reason: "approved on review"}, nil
	case err == nil && state == "blocked":
		return RunScreenDecision{Action: "block", Reason: reason}, nil
	case err == nil:
		return RunScreenDecision{Action: "hold", Reason: reason}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return RunScreenDecision{}, err
	}
	d := r.screen(ctx, RunsSurfaceCode, account, runsCodeContent{Language: p.language, Network: p.network, Code: p.code, Input: p.input})
	if d.Action != "allow" && !d.timedOut {
		state := map[string]string{"hold": "held", "block": "blocked"}[d.Action]
		if _, err = r.db.ExecContext(ctx, "INSERT OR IGNORE INTO runs_reviews(code_sha256,state,reason,account,created_at) VALUES(?,?,?,?,?)", key, state, d.Reason, account, now); err != nil {
			return RunScreenDecision{}, err
		}
	}
	return d, nil
}

// ---------------------------------------------------------------- caps

// admit counts a run against today's caps, reserving its CPU limit; the
// second result names the cap that refused it. settle later replaces the
// reserved CPU with the actual and adds the egress.
func (r *runs) admit(ctx context.Context, account string, day, cpuLimit int64) (bool, string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	var aRuns, aCPU, gRuns, gCPU int64
	for _, row := range []struct {
		acct      string
		runs, cpu *int64
	}{{account, &aRuns, &aCPU}, {"", &gRuns, &gCPU}} {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO runs_usage(day,account) VALUES(?,?)", day, row.acct); err != nil {
			return false, "", err
		}
		if err = tx.QueryRowContext(ctx, "SELECT runs, cpu_ms FROM runs_usage WHERE day=? AND account=?", day, row.acct).Scan(row.runs, row.cpu); err != nil {
			return false, "", err
		}
	}
	c := r.cfg.Caps
	switch {
	case aRuns >= c.AccountRunsDay:
		return false, "account_runs_day", nil
	case aCPU+cpuLimit > c.AccountCPUMsDay:
		return false, "account_cpu_ms_day", nil
	case gRuns >= c.GlobalRunsDay:
		return false, "global_runs_day", nil
	case gCPU+cpuLimit > c.GlobalCPUMsDay:
		return false, "global_cpu_ms_day", nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs_usage SET runs=runs+1, cpu_ms=cpu_ms+? WHERE day=? AND account IN (?,'')", cpuLimit, day, account); err != nil {
		return false, "", err
	}
	return true, "", tx.Commit()
}

// settleCaps adjusts today's counters once the run is known: the CPU
// reserved at admission becomes the actual, and egress is added. A run that
// was never sent to the loader gives its admission back.
func (r *runs) settleCaps(ctx context.Context, account string, day, cpuLimit, cpu, egress int64, ran bool) error {
	runsDelta := int64(0)
	if !ran {
		runsDelta, cpu = -1, 0
	}
	_, err := r.db.ExecContext(ctx, "UPDATE runs_usage SET runs=runs+?, cpu_ms=cpu_ms+?, egress_bytes=egress_bytes+? WHERE day=? AND account IN (?,'')", runsDelta, cpu-cpuLimit, egress, day, account)
	return err
}

// networkFor decides whether this run gets the network, and says why not.
func (r *runs) networkFor(ctx context.Context, p runPlan, account string, day int64) (bool, string, error) {
	if !p.network {
		if p.asked {
			return false, "config", nil
		}
		return false, "", nil
	}
	if r.cfg.networkLever() {
		return false, "lever", nil
	}
	var n int
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM runs_network_blocks WHERE account=? AND lifted_at=0", account).Scan(&n); err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "account_suspended", nil
	}
	var egress int64
	if err := r.db.QueryRowContext(ctx, "SELECT COALESCE((SELECT egress_bytes FROM runs_usage WHERE day=? AND account=''),0)", day).Scan(&egress); err != nil {
		return false, "", err
	}
	if egress+p.egressMax() > r.cfg.Caps.GlobalEgressBytesDay {
		return false, "global_egress_bytes_day", nil
	}
	return true, "", nil
}

// ---------------------------------------------------------------- receipts

// RunReceiptPayload is what a run receipt signs: this struct's JSON, fields
// in this order, no spaces. The receipt carries the exact bytes as payload,
// so a verifier checks the signature over them and never re-serialises.
type RunReceiptPayload struct {
	Schema       string `json:"schema"`
	ServiceID    string `json:"service_id"`
	KeyID        string `json:"key_id"`
	RunID        string `json:"run_id"`
	Time         int64  `json:"time"`
	Language     string `json:"language"`
	Network      bool   `json:"network"`
	Status       string `json:"status"`
	CPUMs        int64  `json:"cpu_ms"`
	EgressBytes  int64  `json:"egress_bytes"`
	CodeSHA256   string `json:"code_sha256"`
	InputSHA256  string `json:"input_sha256"`
	OutputSHA256 string `json:"output_sha256"`
	EgressSHA256 string `json:"egress_sha256"`
}

// RunReceipt is a signed run receipt. The key is the notary's (the notary key
// file), published by service.read notary key and GET /api/notary/key once the
// notary is enabled.
type RunReceipt struct {
	Schema    string `json:"schema"`
	RunID     string `json:"run_id"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// VerifyRunReceipt checks a receipt offline against a public key (base64url):
// the signature over the payload bytes, the payload's canonical form, and
// that the repeated fields agree.
func VerifyRunReceipt(publicKey string, r RunReceipt) bool {
	var p RunReceiptPayload
	id, ok := verifyPayload(publicKey, r.Payload, r.Signature, &p)
	return ok && p.Schema == RunReceiptSchema && r.Schema == RunReceiptSchema && p.RunID == r.RunID && p.KeyID == r.KeyID && p.KeyID == id
}

func (r *runs) signReceipt(ctx context.Context, p RunReceiptPayload) (RunReceipt, error) {
	key := r.key
	if len(key) != ed25519.PrivateKeySize {
		return RunReceipt{}, refusal("service_unavailable")
	}
	pub := key.Public().(ed25519.PublicKey)
	p.Schema, p.ServiceID, p.KeyID = RunReceiptSchema, r.cfg.ServiceID, sha256Of(pub)
	if p.ServiceID == "" {
		p.ServiceID = "swarmmemo.com"
	}
	payload, signature := signPayload(key, p)
	return RunReceipt{Schema: RunReceiptSchema, RunID: p.RunID, KeyID: p.KeyID, PublicKey: base64.RawURLEncoding.EncodeToString(pub),
		Payload: payload, Signature: signature}, nil
}

// ---------------------------------------------------------------- the run

// runOutput is the stored output document; its SHA-256 is the receipt's
// output hash.
type runOutput struct {
	Status     string       `json:"status"`
	Error      string       `json:"error"`
	ResultJSON string       `json:"result_json"`
	Stdout     string       `json:"stdout"`
	Stderr     string       `json:"stderr"`
	Truncated  RunTruncated `json:"truncated"`
}

func canonicalJSON(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}

func (r *runs) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if r.cfg == nil || r.db == nil || r.client == nil || len(r.key) != ed25519.PrivateKeySize {
		return Result{}, refusal("service_unavailable")
	}
	p, err := r.plan(c.Args)
	if err != nil {
		return Result{}, err
	}
	account := c.Subject.ID
	now := r.cfg.clock().Unix()
	day := now / 86400
	runID := runIDFor(account, c.RequestKey)
	codeHash, inputHash := sha256Of([]byte(p.code)), sha256Of(p.input)
	if _, err = r.db.ExecContext(ctx, "INSERT OR IGNORE INTO runs_code(sha256,language,code,first_seen) VALUES(?,?,?,?)", codeHash, p.language, p.code, now); err != nil {
		return Result{}, err
	}
	if _, err = r.db.ExecContext(ctx, `INSERT INTO runs_log(run_id,account,request_key,created_at,language,code_sha256,input,input_sha256,network_requested,cpu_ms_limit,wall_ms_limit,status)
VALUES(?,?,?,?,?,?,?,?,?,?,?,'screening')`, runID, account, c.RequestKey, now, p.language, codeHash, string(p.input), inputHash, boolInt(p.asked), p.cpuMs, p.wallMs); err != nil {
		return Result{}, err
	}
	rec := runRecord{RunID: runID, Language: p.language, CodeSHA256: codeHash, InputSHA256: inputHash, Network: "off"}

	// Before the run: the code screen.
	d, err := r.codeDecision(ctx, p, account, now)
	if err != nil {
		return Result{}, err
	}
	rec.CodeDecision = d
	if d.Action != "allow" {
		rec.Status = map[string]string{"hold": "held", "block": "blocked"}[d.Action]
		return r.finishWithout(ctx, rec, "code "+d.Action+": "+d.Reason)
	}
	ok, capName, err := r.admit(ctx, account, day, p.cpuMs)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		rec.Status, rec.Note = "cap_reached", capName
		return r.finishWithout(ctx, rec, "daily cap "+capName)
	}
	network, note, err := r.networkFor(ctx, p, account, day)
	if err != nil {
		_ = r.settleCaps(ctx, account, day, p.cpuMs, 0, 0, false)
		return Result{}, err
	}
	rec.Note = note
	if network {
		rec.Network = "on"
	}

	// The run.
	req := loaderRequest{Schema: 1, RunID: runID, Language: p.language, Code: p.code, Input: p.input,
		Limits: loaderLimits{CPUMs: p.cpuMs, Subrequests: min(r.cfg.Limits.SubrequestsMax, max(p.maxRequests, 1)), WallMs: p.wallMs,
			StdoutBytes: r.cfg.Limits.StdoutBytes, StderrBytes: r.cfg.Limits.StderrBytes, ResultBytes: r.cfg.Limits.ResultBytes},
		Network: loaderNetwork{Ports: r.cfg.Egress.Ports, DenyHosts: r.cfg.denyHosts(), DenyCIDRs: r.cfg.Egress.DenyCIDRs, Resolve: *r.cfg.Egress.Resolve}}
	if network {
		req.Network.Enabled, req.Network.MaxRequests, req.Network.MaxBytesOut, req.Network.MaxBytesIn = true, p.maxRequests, p.maxBytesOut, p.maxBytesIn
	}
	resp, err := r.client.run(ctx, req)
	if err != nil {
		detail := err.Error()
		var le *LoaderError
		if errors.As(err, &le) && le.Kind == "auth" {
			slog.Error("runs: loader authentication failed", "run", runID, "detail", le.Detail)
		}
		if le != nil && le.MayHaveRun() {
			// The loader had the run and its answer is lost or not to be
			// believed: the run may have used its CPU and its network. It is
			// settled at its maximum, never refunded (security review 1.20,
			// H4), with a receipt and a flag.
			return r.finishLost(ctx, c, p, rec, network, account, day, now, detail)
		}
		// Nothing ran. The ledger refunds the call, but the run stays counted
		// against today's caps at its full CPU limit: a failure must never
		// become a way around the caps.
		_, _ = r.db.ExecContext(ctx, "UPDATE runs_log SET status='loader_error', error=?, network=?, network_note=?, code_decision=?, finished_at=? WHERE run_id=?",
			truncateString(detail, 500), rec.Network, rec.Note, decisionText(rec.CodeDecision), r.cfg.clock().Unix(), runID)
		return Result{}, refusal("upstream_failed")
	}

	// Metering: CPU from the runtime's tail event (the limit when unknown),
	// egress as the gateway counted it, never more than the quote.
	cpu := min(max(resp.CPUMs, 0), p.cpuMs)
	if resp.CPUSource != "tail" {
		cpu = p.cpuMs
	}
	egress := int64(0)
	if network {
		egress = min(resp.Egress.BytesOut+resp.Egress.BytesIn, p.egressMax())
	}
	used := min(runsCost(c.Price, cpu, egress), runsCost(c.Price, p.cpuMs, p.egressMax()))
	if err = r.settleCaps(ctx, account, day, p.cpuMs, cpu, egress, true); err != nil {
		return Result{}, err
	}

	// After the run: the egress screen.
	rec.EgressDecision = RunScreenDecision{Action: "allow"}
	if network {
		rec.EgressDecision = r.screen(ctx, RunsSurfaceEgress, account, runsEgressContent{RunID: runID, Egress: resp.Egress})
		if rec.EgressDecision.Action == "block" {
			if _, err = r.db.ExecContext(ctx, `INSERT INTO runs_network_blocks(account,reason,run_id,created_at) VALUES(?,?,?,?)
 ON CONFLICT(account) DO UPDATE SET reason=excluded.reason, run_id=excluded.run_id, created_at=excluded.created_at, lifted_at=0`, account, rec.EgressDecision.Reason, runID, now); err != nil {
				return Result{}, err
			}
		}
	}
	rec.Flagged = rec.EgressDecision.Action != "allow"
	out := runOutput{Status: resp.Status, Error: resp.Error, ResultJSON: resp.ResultJSON, Stdout: resp.Stdout, Stderr: resp.Stderr, Truncated: resp.Truncated}
	outBytes, egressBytes := canonicalJSON(out), canonicalJSON(resp.Egress)
	rec.Status, rec.CPUMs, rec.WallMs, rec.EgressBytes, rec.Egress = resp.Status, cpu, resp.WallMs, egress, resp.Egress
	rec.OutputSHA256, rec.EgressSHA256 = sha256Of(outBytes), sha256Of(egressBytes)
	receipt, err := r.signReceipt(ctx, RunReceiptPayload{RunID: runID, Time: r.cfg.clock().Unix(), Language: p.language, Network: network, Status: resp.Status,
		CPUMs: cpu, EgressBytes: egress, CodeSHA256: codeHash, InputSHA256: inputHash, OutputSHA256: rec.OutputSHA256, EgressSHA256: rec.EgressSHA256})
	if err != nil {
		return Result{}, err
	}
	rec.Receipt = &receipt
	receiptJSON, _ := json.Marshal(receipt)
	if _, err = r.db.ExecContext(ctx, `UPDATE runs_log SET status=?, error=?, output=?, output_sha256=?, egress=?, egress_sha256=?, cpu_ms=?, cpu_source=?, wall_ms=?,
 egress_bytes=?, cost=?, network=?, network_note=?, code_decision=?, egress_decision=?, flagged=?, receipt=?, finished_at=? WHERE run_id=?`,
		resp.Status, resp.Error, string(outBytes), rec.OutputSHA256, string(egressBytes), rec.EgressSHA256, cpu, resp.CPUSource, resp.WallMs,
		egress, used, rec.Network, rec.Note, decisionText(rec.CodeDecision), decisionText(rec.EgressDecision), boolInt(rec.Flagged), string(receiptJSON), r.cfg.clock().Unix(), runID); err != nil {
		return Result{}, err
	}
	if rec.Flagged {
		slog.Warn("runs: egress flagged", "run", runID, "account", account, "action", rec.EgressDecision.Action, "reason", rec.EgressDecision.Reason)
	}
	return Result{Body: rec.body(out), Used: used, Public: rec.public()}, nil
}

// finishLost ends a run the loader had but whose answer was lost (a timeout
// after the request was sent, an answer too large, unsigned or for another
// run). Its CPU counts at the limit and its egress at the most it could
// have used, against the caller's charge and today's global egress cap; the
// egress screen sees what is known (nothing) and the run is flagged for
// review; the call is charged the quote and gets a signed receipt with
// status loader_error.
func (r *runs) finishLost(ctx context.Context, c Call, p runPlan, rec runRecord, network bool, account string, day, now int64, detail string) (Result, error) {
	cpu, egress := p.cpuMs, int64(0)
	if network {
		egress = p.egressMax()
	}
	used := runsCost(c.Price, cpu, egress)
	if err := r.settleCaps(ctx, account, day, p.cpuMs, cpu, egress, true); err != nil {
		return Result{}, err
	}
	rec.EgressDecision = RunScreenDecision{Action: "allow"}
	if network {
		rec.EgressDecision = r.screen(ctx, RunsSurfaceEgress, account, runsEgressContent{RunID: rec.RunID, Egress: EgressSummary{Log: []EgressEntry{}}})
		if rec.EgressDecision.Action == "allow" {
			rec.EgressDecision = RunScreenDecision{Action: "flag", Reason: "egress unknown: the loader's answer was lost; counted at its maximum"}
		}
		if rec.EgressDecision.Action == "block" {
			if _, err := r.db.ExecContext(ctx, `INSERT INTO runs_network_blocks(account,reason,run_id,created_at) VALUES(?,?,?,?)
 ON CONFLICT(account) DO UPDATE SET reason=excluded.reason, run_id=excluded.run_id, created_at=excluded.created_at, lifted_at=0`, account, rec.EgressDecision.Reason, rec.RunID, now); err != nil {
				return Result{}, err
			}
		}
	}
	rec.Flagged = rec.EgressDecision.Action != "allow"
	summary := EgressSummary{Log: []EgressEntry{}}
	out := runOutput{Status: "loader_error", Error: "the loader's answer was lost; the run is charged at its limits"}
	outBytes, egressBytes := canonicalJSON(out), canonicalJSON(summary)
	rec.Status, rec.CPUMs, rec.EgressBytes, rec.Egress = "loader_error", cpu, egress, summary
	rec.OutputSHA256, rec.EgressSHA256 = sha256Of(outBytes), sha256Of(egressBytes)
	receipt, err := r.signReceipt(ctx, RunReceiptPayload{RunID: rec.RunID, Time: r.cfg.clock().Unix(), Language: p.language, Network: network, Status: rec.Status,
		CPUMs: cpu, EgressBytes: egress, CodeSHA256: rec.CodeSHA256, InputSHA256: rec.InputSHA256, OutputSHA256: rec.OutputSHA256, EgressSHA256: rec.EgressSHA256})
	if err != nil {
		return Result{}, err
	}
	rec.Receipt = &receipt
	receiptJSON, _ := json.Marshal(receipt)
	if _, err = r.db.ExecContext(ctx, `UPDATE runs_log SET status=?, error=?, output=?, output_sha256=?, egress=?, egress_sha256=?, cpu_ms=?, cpu_source='limit', wall_ms=?,
 egress_bytes=?, cost=?, network=?, network_note=?, code_decision=?, egress_decision=?, flagged=?, receipt=?, finished_at=? WHERE run_id=?`,
		rec.Status, truncateString(detail, 500), string(outBytes), rec.OutputSHA256, string(egressBytes), rec.EgressSHA256, cpu, p.wallMs,
		egress, used, rec.Network, rec.Note, decisionText(rec.CodeDecision), decisionText(rec.EgressDecision), boolInt(rec.Flagged), string(receiptJSON), r.cfg.clock().Unix(), rec.RunID); err != nil {
		return Result{}, err
	}
	slog.Warn("runs: loader answer lost, charged at the limits", "run", rec.RunID, "account", account, "network", network, "detail", truncateString(detail, 200))
	return Result{Body: rec.body(out), Used: used, Public: rec.public()}, nil
}

// finishWithout ends a run that never reached the loader: nothing is
// charged, and the call records why.
func (r *runs) finishWithout(ctx context.Context, rec runRecord, why string) (Result, error) {
	if _, err := r.db.ExecContext(ctx, "UPDATE runs_log SET status=?, error=?, network_note=?, code_decision=?, finished_at=? WHERE run_id=?",
		rec.Status, truncateString(why, 500), rec.Note, decisionText(rec.CodeDecision), r.cfg.clock().Unix(), rec.RunID); err != nil {
		return Result{}, err
	}
	return Result{Body: rec.body(runOutput{Status: rec.Status}), Used: 0, Public: rec.public()}, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func decisionText(d RunScreenDecision) string {
	if d.Action == "" {
		return ""
	}
	return truncateString(d.Action+": "+d.Reason, 300)
}

// runRecord is what the caller and the public record are told.
type runRecord struct {
	RunID, Language, Status, Network, Note string
	CodeSHA256, InputSHA256                string
	OutputSHA256, EgressSHA256             string
	CPUMs, WallMs, EgressBytes             int64
	Egress                                 EgressSummary
	CodeDecision, EgressDecision           RunScreenDecision
	Flagged                                bool
	Receipt                                *RunReceipt
}

func (rec runRecord) public() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"run_id": rec.RunID, "status": rec.Status, "language": rec.Language, "network": rec.Network, "cpu_ms": rec.CPUMs,
		"egress_bytes": rec.EgressBytes, "egress_requests": rec.Egress.Requests, "egress_blocked": rec.Egress.Blocked, "flagged": rec.Flagged,
		"code_sha256": rec.CodeSHA256, "output_sha256": rec.OutputSHA256,
	})
	return b
}

// body is the caller's answer, within StoredBodyBytes: the output's head
// (the whole of it is in the run log, read with the log method), the
// counts, the moderation outcome and the receipt.
func (rec runRecord) body(out runOutput) json.RawMessage {
	stdout, stderr, errText := out.Stdout, out.Stderr, out.Error
	cut := out.Truncated
	limit := func(s *string, n int, flag *bool) {
		if len(*s) > n {
			*s = truncateString(*s, n)
			if flag != nil {
				*flag = true
			}
		}
	}
	limit(&stdout, runsBodyStdout, &cut.Stdout)
	limit(&stderr, runsBodyStderr, &cut.Stderr)
	limit(&errText, runsBodyError, nil)
	var result json.RawMessage
	if out.ResultJSON != "" && len(out.ResultJSON) <= runsBodyResult {
		result = json.RawMessage(out.ResultJSON)
	} else if out.ResultJSON != "" {
		cut.Result = true
	}
	for {
		m := map[string]any{
			"run_id": rec.RunID, "status": rec.Status, "language": rec.Language, "network": rec.Network,
			"result": result, "stdout": stdout, "stderr": stderr, "truncated": cut,
			"cpu_ms": rec.CPUMs, "wall_ms": rec.WallMs,
			"egress":  map[string]int64{"requests": rec.Egress.Requests, "blocked": rec.Egress.Blocked, "bytes_out": rec.Egress.BytesOut, "bytes_in": rec.Egress.BytesIn},
			"flagged": rec.Flagged,
			"log":     map[string]any{"operation": "service.read", "target": "runs", "data": map[string]any{"schema": 1, "method": "log", "args": map[string]string{"run": rec.RunID}}},
		}
		if errText != "" {
			m["error"] = errText
		}
		if rec.Note != "" {
			m["network_note"] = rec.Note
		}
		if rec.CodeDecision.Action != "" && rec.CodeDecision.Action != "allow" {
			m["moderation"] = map[string]string{"surface": RunsSurfaceCode, "action": rec.CodeDecision.Action, "reason": rec.CodeDecision.Reason}
		} else if rec.Flagged {
			m["moderation"] = map[string]string{"surface": RunsSurfaceEgress, "action": rec.EgressDecision.Action, "reason": rec.EgressDecision.Reason}
		}
		if rec.Receipt != nil {
			m["receipt"] = rec.Receipt
		}
		b, _ := json.Marshal(m)
		if len(b) <= StoredBodyBytes-256 || (stdout == "" && stderr == "" && result == nil) {
			return b
		}
		// JSON escaping inflated the output: halve it and try again.
		result, cut.Result = nil, cut.Result || result != nil
		limit(&stdout, len(stdout)/2, &cut.Stdout)
		limit(&stderr, len(stderr)/2, &cut.Stderr)
	}
}

// ---------------------------------------------------------------- the log read

// Read is service.read runs log {"run":RUN_ID}: the caller's own full run
// record, owner only.
func (r *runs) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	if c.Method != "log" || !c.Subject.Signed {
		return nil, refusal("invalid_service_data")
	}
	var a struct {
		Run string `json:"run"`
	}
	if err := StrictObject(c.Args, &a); err != nil || !runIDRE.MatchString(a.Run) {
		return nil, refusal("invalid_service_data")
	}
	var row struct {
		createdAt, finishedAt, networkRequested, cpuLimit, wallLimit, cpu, wall, egressBytes, cost, flagged int64
		language, codeHash, code, input, inputHash, network, note, status, errText, output, outputHash      string
		egress, egressHash, cpuSource, codeDecision, egressDecision, receipt                                string
	}
	err := q.QueryRowContext(ctx, `SELECT l.created_at,l.finished_at,l.network_requested,l.cpu_ms_limit,l.wall_ms_limit,l.cpu_ms,l.wall_ms,l.egress_bytes,l.cost,l.flagged,
 l.language,l.code_sha256,COALESCE(c.code,''),l.input,l.input_sha256,l.network,l.network_note,l.status,l.error,l.output,l.output_sha256,
 l.egress,l.egress_sha256,l.cpu_source,l.code_decision,l.egress_decision,l.receipt
 FROM runs_log l LEFT JOIN runs_code c ON c.sha256=l.code_sha256 WHERE l.run_id=? AND l.account=?`, a.Run, c.Subject.ID).Scan(
		&row.createdAt, &row.finishedAt, &row.networkRequested, &row.cpuLimit, &row.wallLimit, &row.cpu, &row.wall, &row.egressBytes, &row.cost, &row.flagged,
		&row.language, &row.codeHash, &row.code, &row.input, &row.inputHash, &row.network, &row.note, &row.status, &row.errText, &row.output, &row.outputHash,
		&row.egress, &row.egressHash, &row.cpuSource, &row.codeDecision, &row.egressDecision, &row.receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refusal("call_not_found")
	}
	if err != nil {
		return nil, err
	}
	raw := func(s string) json.RawMessage {
		if s == "" {
			return nil
		}
		return json.RawMessage(s)
	}
	b, err := json.Marshal(map[string]any{
		"run_id": a.Run, "status": row.status, "error": row.errText, "language": row.language, "created_at": row.createdAt, "finished_at": row.finishedAt,
		"code": row.code, "code_sha256": row.codeHash, "input": raw(row.input), "input_sha256": row.inputHash,
		"network_requested": row.networkRequested == 1, "network": row.network, "network_note": row.note,
		"limits": map[string]int64{"cpu_ms": row.cpuLimit, "wall_ms": row.wallLimit},
		"cpu_ms": row.cpu, "cpu_source": row.cpuSource, "wall_ms": row.wall, "egress_bytes": row.egressBytes, "cost": row.cost,
		"output": raw(row.output), "output_sha256": row.outputHash, "egress": raw(row.egress), "egress_sha256": row.egressHash,
		"moderation": map[string]string{"code": row.codeDecision, "egress": row.egressDecision}, "flagged": row.flagged == 1,
		"receipt": raw(row.receipt),
	})
	return b, err
}
