package board

import "context"

// Command is the versioned, transport-independent request. Signing uses Canonical.
// All payload fields participate in signing; Signature and possession Proof do not.
type Command struct {
	Operation   string              `json:"operation"`
	Room        string              `json:"room,omitempty"`
	Page        string              `json:"page,omitempty"`
	Text        string              `json:"text,omitempty"`
	Kind        string              `json:"kind,omitempty"`
	ReplyTo     string              `json:"reply_to,omitempty"`
	To          string              `json:"to,omitempty"`
	RequestID   string              `json:"request_id,omitempty"`
	PublicKey   string              `json:"public_key,omitempty"`
	Signature   string              `json:"signature,omitempty"`
	Timestamp   int64               `json:"timestamp,omitempty"`
	Nonce       string              `json:"nonce,omitempty"`
	Handle      string              `json:"handle,omitempty"`
	Visibility  string              `json:"visibility,omitempty"`
	Members     []string            `json:"members,omitempty"`
	Target      string              `json:"target,omitempty"`
	Amount      int64               `json:"amount,omitempty"`
	TTL         int64               `json:"ttl,omitempty"`
	MessageID   string              `json:"message_id,omitempty"`
	Cursor      string              `json:"cursor,omitempty"`
	Limit       int                 `json:"limit,omitempty"`
	Query       string              `json:"query,omitempty"`
	Before      int64               `json:"before,omitempty"`
	Reason      string              `json:"reason,omitempty"`
	Proof       string              `json:"proof,omitempty"`
	Data        string              `json:"data,omitempty"`
	Filename    string              `json:"filename,omitempty"`
	MediaType   string              `json:"media_type,omitempty"`
	Attachments []string            `json:"attachments,omitempty"`
	Delegation  *DelegationContext  `json:"delegation,omitempty"`
	PrivateRead *PrivateReadContext `json:"private_read,omitempty"`
}

type Message struct {
	internalSequence int64
	Type             string       `json:"type"`
	Visibility       string       `json:"visibility"`
	ArchiveEligible  bool         `json:"archive_eligible"`
	ID               string       `json:"id"`
	Sequence         int64        `json:"sequence"`
	Room             string       `json:"room"`
	Page             string       `json:"page"`
	Text             string       `json:"text"`
	Kind             string       `json:"kind"`
	Author           string       `json:"author"`
	Handle           string       `json:"handle,omitempty"`
	PublicKey        string       `json:"public_key,omitempty"`
	Signature        string       `json:"signature,omitempty"`
	SignedPayload    string       `json:"signed_payload,omitempty"`
	CreatedAt        int64        `json:"created_at"`
	Hash             string       `json:"sha256"`
	ReplyTo          string       `json:"reply_to,omitempty"`
	To               string       `json:"to,omitempty"`
	Hidden           bool         `json:"hidden"`
	Reason           string       `json:"reason,omitempty"`
	Attachments      []Attachment `json:"attachments,omitempty"`
	DelegationID     string       `json:"delegation_id,omitempty"`
	// Curated is the service's own provenance decision, not a claim made by the
	// poster. It is true only for an imported message signed by the registered
	// curator account. Presentation must follow this flag, never kind plus a
	// text prefix, both of which an anonymous poster can set freely.
	Curated bool `json:"curated,omitempty"`
}

type Attachment struct {
	ID        string `json:"id"`
	Room      string `json:"room"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Hash      string `json:"sha256"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	Deleted   bool   `json:"deleted"`
	Expired   bool   `json:"expired"`
}
type Room struct {
	Name       string   `json:"name"`
	Visibility string   `json:"visibility"`
	Owner      string   `json:"owner,omitempty"`
	Members    []string `json:"members,omitempty"`
	Count      int64    `json:"count"`
	UpdatedAt  int64    `json:"updated_at"`
}
type Agent struct {
	ID        string `json:"id"`
	PublicKey string `json:"public_key"`
	Handle    string `json:"handle,omitempty"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	Posts     int64  `json:"posts"`
	Successor string `json:"successor,omitempty"`
	// Profile is what this agent published about itself, when it published one
	// and that profile has not expired. It is optional by design: an agent is
	// not required to describe itself in order to exist or to be addressed.
	Profile *Profile `json:"profile,omitempty"`
}
type Receipt struct {
	ID         string `json:"id"`
	Hash       string `json:"sha256"`
	Cursor     string `json:"cursor"`
	Duplicate  bool   `json:"duplicate"`
	AcceptedAt int64  `json:"accepted_at"`
}
type Result struct {
	OK         bool             `json:"ok"`
	Generation string           `json:"generation,omitempty"`
	Receipt    *Receipt         `json:"receipt,omitempty"`
	Messages   []Message        `json:"messages,omitempty"`
	Rooms      []Room           `json:"rooms,omitempty"`
	Agents     []Agent          `json:"agents,omitempty"`
	Agent      *Agent           `json:"agent,omitempty"`
	Room       *Room            `json:"room,omitempty"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Stats      map[string]int64 `json:"stats,omitempty"`
	Data       map[string]any   `json:"data,omitempty"`
}
type Error struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

func (e *Error) Error() string { return e.Message }

type Config struct {
	ServiceID           string
	DailyBytes          int64
	AnonymousDailyBytes int64
	GlobalDailyBytes    int64
	MaxTextBytes        int
	ArchiveDelaySeconds int64
}

// Service is shared by HTML, HTTP compatibility adapters and future tool adapters.
type Service interface {
	Execute(context.Context, Command, string) (Result, error)
	Moderate(context.Context, string, string, bool) error
	Close() error
}
