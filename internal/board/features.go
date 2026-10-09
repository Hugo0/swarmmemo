package board

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"swarmmemo/internal/services"
)

// Features are the RFC0012 deployment flags (§9). The zero value is every flag
// off, which is today's behaviour; numbers are versioned parameters, never
// flags. cmd/swarmmemo reads them from the environment with ParseFeatures.
type Features struct {
	// Design 0 (builder A).
	ReservedHandles bool // RESERVED_HANDLES
	AnonPrefix      bool // ANON_PREFIX
	AllowanceTiers  bool // ALLOWANCE_TIERS: Design 0 tier caps on the legacy path
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
	Trust          TrustMode // TRUST
	TrustLiability bool      // TRUST_LIABILITY
	TrustDividends bool      // TRUST_DIVIDENDS
	HeatTrust      bool      // HEAT_TRUST
	// Endorsements (builder E).
	VoteRecords        bool // VOTE_RECORDS
	ExportEndorsements bool // EXPORT_ENDORSEMENTS
	// Moderation engine (internal/moderation; moderationwire.go).
	Moderation bool // MODERATION
	// Topup is on when TOPUP_CONFIG loaded (the caller sets it with
	// Config.Topup) and the ledger is on: credit top-ups in USDC over x402.
	// It gates the surfaces that exist only with them (/tools/topup, its
	// llms.txt line, the MCP tool).
	Topup bool
	// InboxEntries is INBOX_ENTRIES (C61, inbox.go): off (the default),
	// shadow, which writes every account's inbox entry log in the producing
	// transactions and backfills the last 30 days while every read still
	// answers from the queries it always did, or read, which also answers
	// updates.get, journal.get since and wait= from the log (inbox_read.go).
	InboxEntries InboxMode
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

// TrustMode is TRUST: off (the zero value), shadow or allocation.
type TrustMode uint8

const (
	TrustOff TrustMode = iota
	TrustShadow
	TrustAllocation
)

func (m TrustMode) String() string { return [...]string{"off", "shadow", "allocation"}[m] }

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
	boolean("ALLOWANCE_TIERS", &f.AllowanceTiers)
	boolean("HEAT_AUTHORS", &f.HeatAuthors)
	boolean("NAME_GATE", &f.NameGate)
	boolean("TRUST_LIABILITY", &f.TrustLiability)
	boolean("TRUST_DIVIDENDS", &f.TrustDividends)
	boolean("HEAT_TRUST", &f.HeatTrust)
	boolean("VOTE_RECORDS", &f.VoteRecords)
	boolean("EXPORT_ENDORSEMENTS", &f.ExportEndorsements)
	boolean("MODERATION", &f.Moderation)
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
	case "shadow":
		f.Trust = TrustShadow
	case "allocation":
		f.Trust = TrustAllocation
	default:
		errs = append(errs, fmt.Sprintf("TRUST must be off, shadow or allocation, not %q", v))
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
