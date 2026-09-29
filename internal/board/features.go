package board

import (
	"fmt"
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
}

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
	if len(errs) > 0 {
		return Features{}, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return f, nil
}
