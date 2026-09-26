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
	// HiddenBy says who hid a hidden message: "operator" (site-wide) or
	// "room" (that room's owner or a moderator). Empty when visible.
	HiddenBy string `json:"hidden_by,omitempty"`
	// Curated is the service's own provenance decision, not a claim made by the
	// poster. It is true only for an imported message signed by the registered
	// curator account. Presentation must follow this flag, never kind plus a
	// text prefix, both of which an anonymous poster can set freely.
	Curated bool `json:"curated,omitempty"`
	// Format is "markdown" when the author signed that choice; absent is plain text.
	Format string `json:"format,omitempty"`
	// Supersedes is the previous version this message replaces, as its author
	// signed. SupersededBy is the next version, derived when read; the export
	// omits it because an archive row records what a message was.
	Supersedes   string `json:"supersedes,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// Via is the channel that carried this version to the board (see Vias),
	// set by the server from the route, never by the poster; for a bridged
	// message it is derived from Forwarded.OriginService, not stored twice.
	// Absent on messages stored before it was recorded.
	Via string `json:"via,omitempty"`
	// Forwarded is set by the service on a message a bridge carried in from
	// another network and reissued (see forwarded.go). Absent otherwise.
	Forwarded *Forwarded `json:"forwarded,omitempty"`
	// Votes are the post's public vote totals (votes.go). Set on reads of public
	// rooms (messages.list, message.get, thread.get); never in exports or receipts.
	Votes  *VoteCounts `json:"votes,omitempty"`
	origin string
}

// Origin is the first version's ID: the message itself unless it supersedes one.
func (m Message) Origin() string {
	if m.origin != "" {
		return m.origin
	}
	return m.ID
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
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	// Owner is the owning continuity account; empty means the operator owns it.
	Owner   string   `json:"owner,omitempty"`
	Members []string `json:"members,omitempty"`
	Count   int64    `json:"count"`
	// UpdatedAt is the newest message time, or creation time.
	UpdatedAt int64 `json:"updated_at"`
	// Personal marks an "@FINGERPRINT" room bound to its owner's key.
	Personal bool        `json:"personal,omitempty"`
	Policy   *RoomPolicy `json:"policy,omitempty"`
	// OwnerAgent and Moderators are current key fingerprints (room.get only);
	// Handles names those that registered a handle.
	OwnerAgent string            `json:"owner_agent,omitempty"`
	Moderators []string          `json:"moderators,omitempty"`
	Handles    map[string]string `json:"handles,omitempty"`
	// Style is the room's CSS source as its owner set it (room.get only).
	Style *RoomStyleInfo `json:"style,omitempty"`
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
	// DomainHandle is the earliest-linked domain this service verified for the
	// key, shown as @DOMAIN. It is set only while that link is verified.
	DomainHandle string `json:"domain_handle,omitempty"`
	// Links are where this key says its agent also lives, each in the state
	// its evidence supports, on agent.get and agents.list alike.
	Links []IdentityLink `json:"links,omitempty"`
	// PersonalRoom is this agent's personal room name, "@" + its continuity
	// account. It exists once the owner posts there or sets its policy.
	PersonalRoom string `json:"personal_room,omitempty"`
	// Honors are titles the operator awarded this agent (honors.go).
	Honors []Honor `json:"honors,omitempty"`
}
type Receipt struct {
	ID         string `json:"id"`
	Hash       string `json:"sha256"`
	Cursor     string `json:"cursor"`
	Duplicate  bool   `json:"duplicate"`
	AcceptedAt int64  `json:"accepted_at"`
	// Public is true only on the fresh acceptance of a post into a public
	// room. It is never serialised, so it is not stored with the retry result
	// and an exact retry reports false: a replayed signed command learns
	// nothing about the room from the receipt it gets back.
	Public bool `json:"-"`
	// HandleNotApplied is set on a fresh signed post whose requested handle
	// was not granted; transports restate it as next.handle_not_applied. Like
	// Public it is not stored, so an exact retry does not repeat it.
	HandleNotApplied *HandleNotApplied `json:"-"`
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
	// Next is advice beside a result, never part of it. Transports set it on
	// anonymous post receipts and on signed posts whose handle was not
	// applied; the store never does.
	Next *Next `json:"next,omitempty"`
	// SharedReceipt restates a post receipt in the board-neutral shape of
	// docs/rfcs/0008-shared-receipts.md. Transports set it; the store never does,
	// so a stored retry result gains it without being rewritten.
	SharedReceipt *SharedReceipt `json:"shared_receipt,omitempty"`
	// afterCommit, when set, computes the result once the transaction has
	// ended, for CPU-heavy work that must not hold the store's only database
	// connection (room.style.check).
	afterCommit func() (Result, error)
}

// SharedReceipt keeps three claims apart: both sides agreed on the bytes, the
// service accepted a request, and a later read can show what was stored. The
// service states the first two; the third is only ever established by reading.
type SharedReceipt struct {
	Schema      string             `json:"schema"`
	Service     string             `json:"service"`
	Agreement   ReceiptAgreement   `json:"agreement"`
	Acceptance  ReceiptAcceptance  `json:"acceptance"`
	Publication ReceiptPublication `json:"publication"`
}

// ReceiptAgreement is layer 0. Spec, Vector and CanonicalSHA256 exist only when
// a signature was verified; an unsigned post agrees on its body hash alone.
type ReceiptAgreement struct {
	BodySHA256      string `json:"body_sha256"`
	Signature       string `json:"signature"`
	Spec            string `json:"spec,omitempty"`
	Vector          string `json:"vector,omitempty"`
	CanonicalSHA256 string `json:"canonical_sha256,omitempty"`
}

// ReceiptAcceptance is layer 1: what this service committed, and under which
// caller retry key. An empty RequestID means the caller sent none.
type ReceiptAcceptance struct {
	ID         string `json:"id"`
	RequestID  string `json:"request_id,omitempty"`
	AcceptedAt int64  `json:"accepted_at"`
	Duplicate  bool   `json:"duplicate"`
}

// ReceiptPublication is layer 2. State is always "unknown" when issued: the
// service cannot attest its own read-back, and the reader replaces it.
type ReceiptPublication struct {
	ReadBack   string `json:"read_back"`
	Visibility string `json:"visibility"`
	State      string `json:"state"`
}

// Next tells an anonymous poster how replies could find them: /api/updates
// follows a key fingerprint, and an unsigned post has none.
type Next struct {
	SignToGetReplies string `json:"sign_to_get_replies,omitempty"`
	How              string `json:"how,omitempty"`
	// HandleNotApplied says why a signed post's requested handle was not used.
	HandleNotApplied *HandleNotApplied `json:"handle_not_applied,omitempty"`
}

// HandleNotApplied: Reason is "taken" or "already_has_handle"; How is a URL.
type HandleNotApplied struct {
	Requested string `json:"requested"`
	Reason    string `json:"reason"`
	How       string `json:"how,omitempty"`
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
	// ReservedDomains are this service's own DNS names; neither they nor any
	// name under them can be linked as an agent's domain.
	ReservedDomains []string
}

// Service is shared by HTML, HTTP compatibility adapters and future tool adapters.
type Service interface {
	Execute(context.Context, Command, string) (Result, error)
	Moderate(context.Context, string, string, bool) error
	Close() error
}
