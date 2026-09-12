// Package references validates a bounded offline publication. It never contacts
// sources, reads the private catalog, or converts references into board events.
package references

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("references_unavailable")

type Config struct {
	RegistryPath, SuppressionPath, SnapshotPath string
	OwnerUID                                    uint32
}

type Reader struct {
	config Config
	now    func() time.Time
}

type Author struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}
type Source struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	FeedURL          string `json:"feed_url"`
	Adapter          string `json:"adapter"`
	LastAttemptedAt  int64  `json:"last_attempted_at"`
	LastSuccessfulAt int64  `json:"last_successful_at"`
	Status           string `json:"status"`
	AttributionBasis string `json:"attribution_basis"`
}
type Reference struct {
	ID                             string   `json:"id"`
	SourceID                       string   `json:"source_id"`
	ExternalID                     string   `json:"external_id"`
	URL                            string   `json:"url"`
	Title                          string   `json:"title"`
	Excerpt                        string   `json:"excerpt"`
	ExcerptAvailable               bool     `json:"excerpt_available"`
	ExcerptTruncated               bool     `json:"excerpt_truncated"`
	Authors                        []Author `json:"authors"`
	SourcePublishedAt              string   `json:"source_published_at"`
	SourcePublicationTimezoneKnown bool     `json:"source_publication_timezone_known"`
	FirstObservedAt                int64    `json:"first_observed_at"`
	LastObservedAt                 int64    `json:"last_observed_at"`
	ContentHash                    string   `json:"content_hash"`
	NativeIdentity                 bool     `json:"native_identity"`
	ClaimableJob                   bool     `json:"claimable_job"`
	HuggingFaceEligible            bool     `json:"hugging_face_eligible"`
	UntrustedContent               bool     `json:"untrusted_content"`
}
type Snapshot struct {
	Version           int         `json:"version"`
	State             string      `json:"state"`
	RegistrySHA256    string      `json:"registry_sha256"`
	SuppressionSHA256 string      `json:"suppression_sha256"`
	GeneratedAt       int64       `json:"generated_at"`
	ValidUntil        int64       `json:"valid_until"`
	Sources           []Source    `json:"sources"`
	References        []Reference `json:"references"`
	Digest            string      `json:"-"`
	reader            *Reader
	seal              string
	notBefore         int64
}

// New checks configuration only. Missing runtime files never prevent startup.
// All-empty paths disable the optional feature; partially configured paths fail.
func New(config Config) (*Reader, error) {
	paths := []string{config.RegistryPath, config.SuppressionPath, config.SnapshotPath}
	if paths[0] == "" && paths[1] == "" && paths[2] == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) || seen[path] {
			return nil, ErrUnavailable
		}
		seen[path] = true
	}
	return &Reader{config: config, now: time.Now}, nil
}

func (r *Reader) Load(ctx context.Context) (*Snapshot, error) {
	if r == nil || ctx == nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	registry, err := readFixed(ctx, r.config.RegistryPath, r.config.OwnerUID, 1<<20)
	if err != nil {
		return nil, ErrUnavailable
	}
	suppression, err := readFixed(ctx, r.config.SuppressionPath, r.config.OwnerUID, 1<<20)
	if err != nil {
		return nil, ErrUnavailable
	}
	projection, err := readFixed(ctx, r.config.SnapshotPath, r.config.OwnerUID, 8<<20)
	if err != nil {
		return nil, ErrUnavailable
	}
	snapshot, err := validate(registry, suppression, projection, r.now().Unix())
	if err != nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	finalNow := r.now().Unix()
	if finalNow < snapshot.notBefore || finalNow >= snapshot.ValidUntil {
		return nil, ErrUnavailable
	}
	snapshot.reader, snapshot.seal = r, snapshot.Digest
	return snapshot, nil
}

// Fence must run after rendering and before writing headers/body. It does not
// authorize cached responses or recall already delivered bytes, and is not an
// ABA history detector. Exported snapshot data must remain unchanged after Load.
func (r *Reader) Fence(ctx context.Context, snapshot *Snapshot) error {
	if r == nil || snapshot == nil || snapshot.reader != r || snapshot.Digest != snapshot.seal {
		return ErrUnavailable
	}
	raw, err := Canonical(snapshot)
	if err != nil || hash(raw) != snapshot.seal {
		return ErrUnavailable
	}
	current, err := r.Load(ctx)
	if err != nil || current.Digest != snapshot.seal || current.RegistrySHA256 != snapshot.RegistrySHA256 || current.SuppressionSHA256 != snapshot.SuppressionSHA256 {
		return ErrUnavailable
	}
	finalNow := r.now().Unix()
	if finalNow < current.notBefore || finalNow >= current.ValidUntil {
		return ErrUnavailable
	}
	return nil
}

func canonicalJSON(raw []byte) (any, error) {
	value, err := parseJSON(raw)
	if err != nil {
		return nil, ErrUnavailable
	}
	encoded, err := encodeParsedJSON(value)
	if err != nil || !bytes.Equal(raw, encoded) {
		return nil, ErrUnavailable
	}
	return value, nil
}

func decodeSnapshot(raw []byte) (*Snapshot, error) {
	var snapshot Snapshot
	if json.Unmarshal(raw, &snapshot) != nil {
		return nil, ErrUnavailable
	}
	snapshot.Digest = hash(raw)
	return &snapshot, nil
}
