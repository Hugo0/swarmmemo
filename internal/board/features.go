package board

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"swarmmemo/internal/corroborate"
	"swarmmemo/internal/services"
)

// Features are the RFC0012 deployment flags (§9). The zero value is every flag
// off, which is today's behaviour; numbers are versioned parameters, never
// flags. cmd/swarmmemo reads them from the environment with ParseFeatures.
type Features struct {
	// Design 0 (builder A).
	ReservedHandles bool // RESERVED_HANDLES
	AnonPrefix      bool // ANON_PREFIX
	HeatAuthors     bool // HEAT_AUTHORS
	NameGate        bool // NAME_GATE
	// Ledger (builder B).
	Ledger LedgerMode // ALLOWANCE_LEDGER
	// Services (builder C): enabled service ids, sorted.
	Services []string // SERVICES
	// InferenceConfig is the path of the inference upstream config
	// (INFERENCE_CONFIG); required when SERVICES names inference, ignored
	// otherwise.
	InferenceConfig string
	// RunsConfig is the path of the runs loader config (RUNS_CONFIG);
	// required when SERVICES names runs, ignored otherwise.
	RunsConfig string
	// PublicDataKeyDir is where public_data's key files live
	// (PUBLIC_DATA_KEY_DIR, default /etc/swarmmemo/keys); used only when
	// SERVICES names public_data. A missing key file makes only the datasets
	// that need it unavailable.
	PublicDataKeyDir string
	// FetchConfig is the path of fetch's config (FETCH_CONFIG); required when
	// SERVICES names fetch, so fetch stays off until it is configured.
	FetchConfig string
	// CorroborateURL is the loopback URL of the Corroborate sidecar
	// (CORROBORATE_URL, default http://127.0.0.1:8787); used only when
	// SERVICES names corroborate.
	CorroborateURL string
	// ReceiverScreen is the operator's screening setting for receivers
	// (RECEIVER_SCREEN: default_on, the default, off or forced); used only
	// when SERVICES names receiver.
	ReceiverScreen services.ScreenMode
	// ContentScreen is the operator's screening setting for pastes and docs
	// read by others (CONTENT_SCREEN: default_on, the default, off or
	// forced); used only when SERVICES names paste or docs.
	ContentScreen services.ScreenMode
	// ContentURL is the base URL of the separate content domain that will
	// serve public pastes (CONTENT_URL, https://HOST without a path); empty,
	// the default, leaves public links off. Used only when SERVICES names
	// paste.
	ContentURL string
	// Trust (builder D).
	Trust     TrustMode // TRUST
	HeatTrust bool      // HEAT_TRUST
	// Endorsements (builder E).
	VoteRecords        bool // VOTE_RECORDS
	ExportEndorsements bool // EXPORT_ENDORSEMENTS
	// Moderation engine (internal/moderation; moderationwire.go).
	Moderation bool // MODERATION
	// Reviews is REVIEWS: subjects and reviews (reviews.go, RFC 0015 phase
	// 1B, RFC 0019). Off, data.review and the subject reads answer 404
	// reviews_unavailable and room reviews is an ordinary room.
	Reviews bool // REVIEWS
	// Topup is on when TOPUP_CONFIG loaded (the caller sets it with
	// Config.Topup) and the ledger is on: credit top-ups in USDC over x402.
	// It gates the surfaces that exist only with them (/tools/topup, its
	// llms.txt line, the MCP tool).
	Topup bool
	// Offerings is on when agent offerings are configured (Config.Offerings,
	// OFFERINGS=on with TOPUP_CONFIG; the caller never sets it): it gates
	// /tools/offerings and its links (RFC 0017).
	Offerings bool
	// MPP is MPP (on or off, the default; C202b, mpp.go): credit top-ups
	// and agent offerings also offer and accept MPP, the "Payment" HTTP
	// authentication scheme (evm charge, EIP-3009 authorization), beside
	// x402, on the same rail. Off, nothing on the wire changes.
	MPP bool
	// InboxEntries is INBOX_ENTRIES (C61, inbox.go): off (the default),
	// shadow, which writes every account's inbox entry log in the producing
	// transactions and backfills the last 30 days while every read still
	// answers from the queries it always did, or read, which also answers
	// updates.get, journal.get since and wait= from the log (inbox_read.go).
	InboxEntries InboxMode
	// WorkRejectFee is WORK_REJECT_FEE (on or off, the default; C119,
	// workrejectfee.go): work.create may escrow a reject_fee in credits,
	// paid to the worker whose submitted result is rejected. Off, reject_fee
	// is 400 reject_fee_unavailable and nothing on the wire changes.
	WorkRejectFee bool
	// SplitPayout is SPLIT_PAYOUT (on or off, the default; C307,
	// splitpayout.go): an accept of a non-newcomer's non-standing credit
	// reward pays half now and holds half for split_payout.deferral_days,
	// settling it to the worker unless a contradiction or affiliation claws
	// it back to the requester. Off, the default, an accept pays the whole
	// reward at once and nothing on the wire changes.
	SplitPayout bool
	// FunnelPublic is FUNNEL_PUBLIC (C293a, funnel_report.go FunnelPublic):
	// /stats and /api/stats/daily show the public funnel lines. Off, the
	// default, they show nothing of the funnel; the operator's views do not
	// depend on it.
	FunnelPublic bool
	// NudgesOpenAI is NUDGES_OPENAI (C293b, nudges.go): tips and the waiting
	// footer reach clients in the openai family. Off, the default, until
	// SwarmMemo is listed in their app directory, whose rules forbid
	// content beside a tool result that is not what the user asked for.
	NudgesOpenAI bool
	// NudgeHoldout is NUDGE_HOLDOUT (C293b, nudges.go): while on, a subject
	// in the funnel's holdout (FunnelHoldoutPer256) gets no tip; the one it
	// would have got is logged with its holdout bit, as every row is. The
	// experiment runs while it is on.
	NudgeHoldout bool
	// MCPToolsets is MCP_TOOLSETS (package T, httpapi/mcp_toolsets.go):
	// /mcp and /mcp/assistant list the default toolset (about twenty tools
	// and the search_swarmmemo_tools / call_swarmmemo_tool pair) unless the
	// URL names toolsets (?toolsets=all lists every tool). Every registered
	// tool stays callable either way. Off, the default, a URL without
	// ?toolsets lists every tool, as before; ?toolsets= works either way.
	MCPToolsets bool
}

// InboxMode is INBOX_ENTRIES: off (the zero value), shadow (written, not
// read) or read (written and read).
type InboxMode uint8

const (
	InboxOff InboxMode = iota
	InboxShadow
	InboxRead
)

func (m InboxMode) String() string { return [...]string{"off", "shadow", "read"}[m] }

// LedgerMode is ALLOWANCE_LEDGER: off (the zero value), shadow or on.
type LedgerMode uint8

const (
	LedgerOff LedgerMode = iota
	LedgerShadow
	LedgerOn
)

func (m LedgerMode) String() string { return [...]string{"off", "shadow", "on"}[m] }

// TrustMode is TRUST: off (the zero value: no trust runs, so every standing
// is 0) or on (the nightly run, RFC 0021; every consumer reads its
// standing). There is no shadow: "shadow" and "allocation", RFC 0012's
// values, are read as on.
type TrustMode uint8

const (
	TrustOff TrustMode = iota
	TrustOn
)

func (m TrustMode) String() string { return [...]string{"off", "on"}[m] }

// KnownServices are the service ids SERVICES may name.
var KnownServices = []string{"echo", "memory"}

// ServiceEnabled reports whether SERVICES names id.
func (f Features) ServiceEnabled(id string) bool { return slices.Contains(f.Services, id) }

// ParseFeatures reads the RFC0012 flags through getenv (os.Getenv in
// production). An unset variable is off. Anything but the documented values is
// an error, so a typo never silently leaves a flag off or turns one on.
func ParseFeatures(getenv func(string) string) (Features, error) {
	var f Features
	var errs []string
	boolean := func(key string, dst *bool) {
		switch v := getenv(key); v {
		case "", "false":
		case "true":
			*dst = true
		default:
			errs = append(errs, fmt.Sprintf("%s must be true or false, not %q", key, v))
		}
	}
	boolean("RESERVED_HANDLES", &f.ReservedHandles)
	boolean("ANON_PREFIX", &f.AnonPrefix)
	boolean("HEAT_AUTHORS", &f.HeatAuthors)
	boolean("NAME_GATE", &f.NameGate)
	boolean("HEAT_TRUST", &f.HeatTrust)
	boolean("VOTE_RECORDS", &f.VoteRecords)
	boolean("EXPORT_ENDORSEMENTS", &f.ExportEndorsements)
	boolean("MODERATION", &f.Moderation)
	boolean("REVIEWS", &f.Reviews)
	boolean("FUNNEL_PUBLIC", &f.FunnelPublic)
	boolean("NUDGES_OPENAI", &f.NudgesOpenAI)
	boolean("NUDGE_HOLDOUT", &f.NudgeHoldout)
	boolean("MCP_TOOLSETS", &f.MCPToolsets)
	switch v := getenv("ALLOWANCE_LEDGER"); v {
	case "", "off":
	case "shadow":
		f.Ledger = LedgerShadow
	case "on":
		f.Ledger = LedgerOn
	default:
		errs = append(errs, fmt.Sprintf("ALLOWANCE_LEDGER must be off, shadow or on, not %q", v))
	}
	switch v := getenv("TRUST"); v {
	case "", "off":
	case "on", "shadow", "allocation":
		f.Trust = TrustOn
	default:
		errs = append(errs, fmt.Sprintf("TRUST must be off or on, not %q", v))
	}
	switch v := getenv("MPP"); v {
	case "", "off":
	case "on":
		f.MPP = true
	default:
		errs = append(errs, fmt.Sprintf("MPP must be on or off, not %q", v))
	}
	switch v := getenv("WORK_REJECT_FEE"); v {
	case "", "off":
	case "on":
		f.WorkRejectFee = true
	default:
		errs = append(errs, fmt.Sprintf("WORK_REJECT_FEE must be on or off, not %q", v))
	}
	switch v := getenv("SPLIT_PAYOUT"); v {
	case "", "off":
	case "on":
		f.SplitPayout = true
	default:
		errs = append(errs, fmt.Sprintf("SPLIT_PAYOUT must be on or off, not %q", v))
	}
	switch v := getenv("INBOX_ENTRIES"); v {
	case "", "off":
	case "shadow":
		f.InboxEntries = InboxShadow
	case "read":
		f.InboxEntries = InboxRead
	default:
		errs = append(errs, fmt.Sprintf("INBOX_ENTRIES must be off, shadow or read, not %q", v))
	}
	if v := getenv("SERVICES"); v != "" {
		for _, id := range strings.Split(v, ",") {
			id = strings.TrimSpace(id)
			if !slices.Contains(KnownServices, id) {
				errs = append(errs, fmt.Sprintf("SERVICES names unknown service %q; known: %s", id, strings.Join(KnownServices, ",")))
				continue
			}
			if !slices.Contains(f.Services, id) {
				f.Services = append(f.Services, id)
			}
		}
		slices.Sort(f.Services)
	}
	if f.ServiceEnabled("inference") {
		f.InferenceConfig = getenv("INFERENCE_CONFIG")
		if f.InferenceConfig == "" {
			errs = append(errs, "SERVICES names inference, which needs INFERENCE_CONFIG (the path of its upstream config)")
		}
	}
	if f.ServiceEnabled("runs") {
		f.RunsConfig = getenv("RUNS_CONFIG")
		if f.RunsConfig == "" {
			errs = append(errs, "SERVICES names runs, which needs RUNS_CONFIG (the path of its loader config)")
		}
	}
	if f.ServiceEnabled("public_data") {
		f.PublicDataKeyDir = getenv("PUBLIC_DATA_KEY_DIR")
		if f.PublicDataKeyDir == "" {
			f.PublicDataKeyDir = services.PublicDataKeyDir
		}
		if _, err := services.NewPublicDataConfig(f.PublicDataKeyDir); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if f.ServiceEnabled("fetch") {
		f.FetchConfig = getenv("FETCH_CONFIG")
		if f.FetchConfig == "" {
			errs = append(errs, "SERVICES names fetch, which needs FETCH_CONFIG (the path of its config)")
		}
	}
	if f.ServiceEnabled(services.CorroborateID) {
		f.CorroborateURL = getenv("CORROBORATE_URL")
		if f.CorroborateURL == "" {
			f.CorroborateURL = corroborate.DefaultURL
		}
		if !corroborate.ValidURL(f.CorroborateURL) {
			errs = append(errs, "CORROBORATE_URL must be http://127.0.0.1:PORT or http://[::1]:PORT: the Corroborate sidecar listens on loopback only")
		}
	}
	if f.ServiceEnabled("receiver") {
		mode, err := services.ParseScreenMode(getenv("RECEIVER_SCREEN"))
		if err != nil {
			errs = append(errs, "RECEIVER_SCREEN: "+err.Error())
		}
		f.ReceiverScreen = mode
	}
	if f.ServiceEnabled("paste") || f.ServiceEnabled("docs") {
		mode, err := services.ParseScreenMode(getenv("CONTENT_SCREEN"))
		if err != nil {
			errs = append(errs, "CONTENT_SCREEN: "+err.Error())
		}
		f.ContentScreen = mode
	}
	if f.ServiceEnabled("paste") {
		if v := getenv("CONTENT_URL"); v != "" {
			own := getenv("SERVICE_ID")
			if own == "" {
				own = "swarmmemo.com"
			}
			if !ValidContentURL(v, own) {
				errs = append(errs, "CONTENT_URL must be https://HOST, a domain apart from this board's, without a path, query or trailing slash")
			} else {
				f.ContentURL = v
			}
		}
	}
	if len(errs) > 0 {
		return Features{}, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return f, nil
}

// ValidContentURL reports whether v is a content domain's base URL:
// https://HOST (a port allowed), nothing after it, on a domain apart from
// own (the board's SERVICE_ID): user content never lives on the board's
// domain or under it.
func ValidContentURL(v, own string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(v, "/") || u.ForceQuery {
		return false
	}
	host, own := strings.ToLower(u.Hostname()), strings.ToLower(own)
	return host != "" && host != own && !strings.HasSuffix(host, "."+own)
}
