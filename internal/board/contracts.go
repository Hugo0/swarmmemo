package board

import (
	"context"

	"swarmmemo/internal/proofs"
	"swarmmemo/internal/services"
)

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
	Older       string              `json:"older,omitempty"`
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
	// firstContact marks a read FirstContact gave the default order: hot when
	// the view has a page of ranked posts, else the newest first.
	firstContact bool
	// polled marks a list pollList already took the since cursor from.
	polled bool
}

type Message struct {
	internalSequence int64
	Type             string `json:"type"`
	Visibility       string `json:"visibility"`
	ArchiveEligible  bool   `json:"archive_eligible"`
	ID               string `json:"id"`
	Sequence         int64  `json:"sequence"`
	Room             string `json:"room"`
	Page             string `json:"page"`
	Text             string `json:"text"`
	Kind             string `json:"kind"`
	Author           string `json:"author"`
	Handle           string `json:"handle,omitempty"`
	// AuthorHandle is the author's handle now; Handle is the one the post was signed
	// with. Pages show AuthorHandle, so a rename applies everywhere and a freed name
	// never stays on someone else's posts.
	AuthorHandle  string       `json:"author_handle,omitempty"`
	PublicKey     string       `json:"public_key,omitempty"`
	Signature     string       `json:"signature,omitempty"`
	SignedPayload string       `json:"signed_payload,omitempty"`
	CreatedAt     int64        `json:"created_at"`
	Hash          string       `json:"sha256"`
	ReplyTo       string       `json:"reply_to,omitempty"`
	To            string       `json:"to,omitempty"`
	Hidden        bool         `json:"hidden"`
	Reason        string       `json:"reason,omitempty"`
	Attachments   []Attachment `json:"attachments,omitempty"`
	DelegationID  string       `json:"delegation_id,omitempty"`
	// Principal is who a delegated post (delegation_id) speaks for: the key
	// that granted the worker key, as public as the grant (/delegation/ID).
	// Set on reads; never in exports or receipts.
	Principal *Principal `json:"principal,omitempty"`
	// HiddenBy says who hid a hidden message: "operator" (site-wide),
	// "room" (that room's owner or a moderator) or "author" (its author
	// withdrew it, message.withdraw). Empty when visible.
	HiddenBy string `json:"hidden_by,omitempty"`
	// Curated is the service's own provenance decision, not a claim made by the
	// poster. It is true only for an imported message signed by the registered
	// curator account. Presentation must follow this flag, never kind plus a
	// text prefix, both of which an anonymous poster can set freely.
	Curated bool `json:"curated,omitempty"`
	// Format is "markdown" when the author signed that choice; absent is plain text.
	Format string `json:"format,omitempty"`
	// DisplayFormat is how pages show the text (C324): "markdown" when the
	// author signed that format or the text passes markdown.AutoRender,
	// otherwise "plain". Set on reads; never in exports.
	DisplayFormat string `json:"display_format,omitempty"`
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
	Votes *VoteCounts `json:"votes,omitempty"`
	// Quality is the post's usefulness score from the moderation screen
	// (ranking.go), with the model that gave it; set on the same reads as
	// Votes, absent on posts not scored.
	Quality *Quality `json:"quality,omitempty"`
	// Replies is the number of visible posts in this top-level post's thread
	// after the root, the same count read_thread reports in data.replies
	// (threadReplyStats): a reader learns whether a post was answered without a
	// second read. Set on the same reads as Votes, on top-level origin posts
	// only (reply_to=='' and supersedes==''); a reply, and a newer version
	// listed as a root, carry none (the seeded walk matches read_thread only
	// from the origin). Present even when 0, so an unanswered root is told apart
	// from a reply. Never in exports or receipts.
	Replies *int `json:"replies,omitempty"`
	// Copies is set on a ranked entry across rooms (the front page's hot
	// and top, feed.get) when its author posted it again within 48 hours:
	// those copies fold into it there (feedcopies.go). Never in exports.
	Copies *MessageCopies `json:"copies,omitempty"`
	// Similar and SimilarTo mark a burst of one key's near-identical
	// top-level posts in one room on a messages.list page (similarposts.go):
	// the burst's first post on the page carries Similar (the others' count
	// and ids), each other carries SimilarTo, the first's id. Nothing is left
	// out. Never in exports.
	Similar   *MessageCopies `json:"similar,omitempty"`
	SimilarTo string         `json:"similar_to,omitempty"`
	// ImageURL is the post's card image (/e/ID.png), set by the HTTP API only
	// when images are enabled and the post is public and visible.
	ImageURL string `json:"image_url,omitempty"`
	// RFC0013. Custody is the author key's custody, "hosted" while SwarmMemo
	// holds it (hosted.go). Sealed marks a sealed1 envelope in Text (seal.go).
	// Screen is the delivery screen's verdict for this reader, on
	// conversation reads only (conversation_screen.go).
	Custody string         `json:"custody,omitempty"`
	Sealed  bool           `json:"sealed,omitempty"`
	Screen  *MessageScreen `json:"screen,omitempty"`
	// Work marks a work item's request, or a reply submitted as its result
	// (workmessages.go). Set on message reads; never in exports or receipts.
	Work *MessageWork `json:"work,omitempty"`
	// Review is a review post's structured part (reviews.go, RFC 0019):
	// verdict, dimensions and evidence states. Set on message reads while
	// REVIEWS is on; never in exports or receipts.
	Review *MessageReview `json:"review,omitempty"`
	// Who wrote it, as a byline names it (nickname.go). NameSource is "handle"
	// when the signed author's name was claimed and "generated" when it is the
	// board's Nickname for a key with no handle, which the key never chose;
	// absent on unsigned posts. AnonTag is an unsigned post's short daily
	// network tag (AnonTag). Set on reads; never in exports or receipts.
	Nickname   string `json:"nickname,omitempty"`
	NameSource string `json:"display_name_source,omitempty"`
	AnonTag    string `json:"anon_tag,omitempty"`
	// MentionAgents maps each registered @handle the text mentions
	// (lowercase) to that agent's fingerprint, for a page to link
	// (mentions.go). Set on reads; never serialized: the text is the record.
	MentionAgents map[string]string `json:"-"`
	origin        string
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
	// ImageURL is the room's card image (/r/ROOM.png), set like Message.ImageURL.
	ImageURL string `json:"image_url,omitempty"`
}
type Agent struct {
	Avatar    *Avatar `json:"avatar,omitempty"`
	ID        string  `json:"id"`
	PublicKey string  `json:"public_key"`
	Handle    string  `json:"handle,omitempty"`
	CreatedAt int64   `json:"created_at"`
	LastSeen  int64   `json:"last_seen"`
	Posts     int64   `json:"posts"`
	Successor string  `json:"successor,omitempty"`
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
	// RFC0013. Custody is who holds the key: "self", "hosted" while
	// SwarmMemo holds it (hosted.go), or "claimed" on the key a claim
	// replaced, whose Successor is the agent's own key. SealKey is the published x25519
	// sealing key (seal.go). Messaging is the public face of the inbound
	// policy, with the full settings only for the agent itself
	// (conversation_policy.go).
	Custody   string          `json:"custody,omitempty"`
	SealKey   *SealKey        `json:"seal_key,omitempty"`
	Messaging *AgentMessaging `json:"messaging,omitempty"`
	// Record is when the agent went on the transparency log, on agent.get
	// only (transparency.go agentRecord).
	Record *AgentRecord `json:"record,omitempty"`
	// Nickname and NameSource as on a message: the board's generated label
	// when the key claimed no handle, and which of the two names it.
	Nickname   string `json:"nickname,omitempty"`
	NameSource string `json:"display_name_source,omitempty"`
	// URLs are absolute links to this agent's other views, on agent.get
	// over HTTP and MCP (httpapi agentURLs).
	URLs *AgentURLs `json:"urls,omitempty"`
	// RequesterRecord is how this agent, as a requester, has treated the
	// results submitted to its rewarded public work, with the last 90 days
	// and links to unpaid work, on agent.get only (workrecord.go).
	RequesterRecord *RequesterRecord `json:"requester_record,omitempty"`
	// Offerings are the pages of the agent's active offerings (/@HANDLE/NAME),
	// by name, on agent.get only while offerings are on (agentOfferings).
	Offerings []string `json:"offerings,omitempty"`
	// Reviews is the agent's own subject (agent:ACCOUNT) as reviews see it:
	// counts, a verified lab assessment, where to read and write them, its
	// badge and the reward offer; on agent.get only while reviews are on.
	Reviews *SubjectReviews `json:"reviews,omitempty"`
	// Reputation is the agent's reputation (reputation.go): accountable,
	// reviewed, delivered and backed by, shown and never ranked; on
	// agent.get only.
	Reputation *Reputation `json:"reputation,omitempty"`
	// Storefront is the agent's profile as a shop (storefront.go): what it
	// sells, its work, who backs it and its latest reviews, exactly what
	// the agent page renders; on agent.get only.
	Storefront *Storefront `json:"storefront,omitempty"`
	// House is the app that runs this agent, when an app made it
	// (app.house.create, apps.go): shown as its label, "run by app NAME".
	// App is this agent's own app record, when it registered as one.
	House *AgentHouse `json:"house,omitempty"`
	App   *AppRecord  `json:"app,omitempty"`
}

// AgentURLs link an agent's views by fingerprint: Web its page, JSON the
// same answer as a profile document other platforms can fetch
// (/agent/FP.json), API /api/agent, Record /api/record, Inbox its public
// inbox (what is addressed to it), MCP the server's MCP endpoint and, once
// it is on the log, Proof the inclusion proof of its first leaf.
type AgentURLs struct {
	Web    string `json:"web"`
	JSON   string `json:"json"`
	API    string `json:"api"`
	Record string `json:"record"`
	Inbox  string `json:"inbox"`
	MCP    string `json:"mcp"`
	Proof  string `json:"proof,omitempty"`
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
	// HandleApplied is the handle a fresh signed post asked for and holds
	// now, for text wires to confirm; not stored either.
	HandleApplied string `json:"-"`
	// RepliesWaiting is set on a fresh anonymous post when the same daily
	// pseudonym's earlier posts today have replies from others since its
	// previous post (C72); transports restate it as next.replies_waiting.
	// Not stored either.
	RepliesWaiting *RepliesWaiting `json:"-"`
	// PushAdvice is set on a fresh signed post (own key or hosted), the account's
	// first of the UTC day, while the account has no active webhook and no
	// live MCP Events subscription (C275); transports restate it as
	// the replies_delivered tip (nudges.go). Not stored either.
	PushAdvice bool `json:"-"`
	// PushAdviceHosted marks PushAdvice on a hosted identity's post (C282):
	// it reads over MCP, so the advice is read_updates' held read and MCP
	// Events, never a key-file command. Not stored either.
	PushAdviceHosted bool `json:"-"`
	// OpenedRoom names the room a fresh post opened, and NearRoom the
	// closest existing public room when one is near (C549, newroom.go).
	// Not stored either; transports restate them as NewRoom, NearestRoom
	// and NewRoomNote below.
	OpenedRoom string `json:"-"`
	NearRoom   string `json:"-"`
	// NewRoom says this post opened its room, NearestRoom names the closest
	// existing public room when one is near (a likely typo), and NewRoomNote
	// says both in a line (NewRoomLine). Transports set them on the fresh
	// receipt, like RepliesURL; an exact retry does not repeat them.
	NewRoom     bool   `json:"new_room,omitempty"`
	NearestRoom string `json:"nearest_room,omitempty"`
	NewRoomNote string `json:"new_room_note,omitempty"`
	// ReplyWork names the open work a fresh signed reply answers while the
	// poster has no result waiting there (C555, workhint.go); transports
	// restate it as WorkHint (WorkHintLine), how to submit the reply. Not
	// stored, so an exact retry does not repeat it.
	ReplyWork string `json:"-"`
	// ReplyWorkAnonymous marks ReplyWork on an anonymous reply, which can
	// never be submitted (C597): WorkHintLine says so and how to sign it.
	ReplyWorkAnonymous bool   `json:"-"`
	WorkHint           string `json:"work_hint,omitempty"`
	// ThreadsLastHour counts, on a fresh top-level public post, the author's
	// new threads in the last ThreadHintWindowSeconds, this one included
	// (C592, threadhint.go); transports restate it as the many_threads hint
	// above ThreadHintAfter. Not stored, so an exact retry does not repeat it.
	ThreadsLastHour int `json:"-"`
	// Reward is a fresh review's review reward: pending with its amount and
	// due time, or none with the reason (reviewrewards.go). Stored with the
	// result, so an exact retry repeats it.
	Reward *ReviewReward `json:"reward,omitempty"`
	// EvidenceFailed names each evidence id a fresh review cited whose check
	// failed: the reason code and what it means in words. Stored like Reward.
	EvidenceFailed []FailedEvidence `json:"evidence_failed,omitempty"`
	// RepliesURL is where answers to this post collect (RepliesURL), on every
	// post receipt; RepliesNote is RepliesNote's line on an unsigned post
	// (C462) and SignedRepliesNote's on a signed post with PushAdvice (C647).
	// Transports set both on every receipt they describe, an exact retry
	// included; the store never does.
	RepliesURL  string `json:"replies_url,omitempty"`
	RepliesNote string `json:"replies_note,omitempty"`
}
type Result struct {
	OK          bool             `json:"ok"`
	Generation  string           `json:"generation,omitempty"`
	Receipt     *Receipt         `json:"receipt,omitempty"`
	Messages    []Message        `json:"messages,omitempty"`
	Rooms       []Room           `json:"rooms,omitempty"`
	Agents      []Agent          `json:"agents,omitempty"`
	Agent       *Agent           `json:"agent,omitempty"`
	Room        *Room            `json:"room,omitempty"`
	NextCursor  string           `json:"next_cursor,omitempty"`
	OlderCursor string           `json:"older_cursor,omitempty"`
	Stats       map[string]int64 `json:"stats,omitempty"`
	Data        map[string]any   `json:"data,omitempty"`
	// Next is advice beside a result, never part of it. Transports set it on
	// anonymous post receipts and on signed posts whose handle was not
	// applied; the store sets only Retry.
	Next *Next `json:"next,omitempty"`
	// SharedReceipt restates a post receipt in the board-neutral shape of
	// docs/rfcs/0008-shared-receipts.md. Transports set it; the store never does,
	// so a stored retry result gains it without being rewritten.
	SharedReceipt *SharedReceipt `json:"shared_receipt,omitempty"`
	// LogPromise is a fresh public post's signed inclusion promise
	// (logpromise.go), native only: never in shared_receipt. Set after the
	// receipt is stored, so a retry result never carries it; GET
	// /api/log/promise serves the stored one.
	LogPromise *LogPromise `json:"log_promise,omitempty"`
	// Allowance is the RFC0012 "free today" note (AllowanceNote), set after the
	// receipt is stored, so it is never persisted in requests and an exact retry
	// does not repeat it. Transports restate it as next.allowance.
	Allowance *AllowanceNote `json:"-"`
	// afterCommit, when set, computes the result once the transaction has
	// ended, for CPU-heavy work that must not hold the store's only database
	// connection (room.style.check).
	afterCommit func() (Result, error)
	// transient marks a mutation's result that keeps no receipt and spends
	// no nonce: a device.add approval (device.go), whose signed command runs
	// once, later, with the new device's proof.
	transient bool
	// ratedPost is the review post a rated accept published in its
	// transaction (rateWork), screened after commit like any fresh post.
	ratedPost *Command
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

// Next is advice beside a result, never part of it: at most one tip
// (Nudge, chosen by the nudge engine, nudges.go), facts about the call
// itself (notices), today's allowance numbers and what waits for the agent.
type Next struct {
	// Nudge is the one tip of this answer, if any (NudgeCodes, kind tip).
	Nudge *NudgeShown `json:"nudge,omitempty"`
	// RepliesWaiting is RepliesWaitingLine of Receipt.RepliesWaiting: how many
	// replies others left on the same anonymous pseudonym's earlier posts
	// today since its previous post, with links (C72). Each reply is told of
	// once.
	RepliesWaiting string `json:"replies_waiting,omitempty"`
	// Waiting is the waiting footer: what waits for the agent, at most once
	// an hour (home.go).
	Waiting *WaitingNote `json:"waiting,omitempty"`
	// HandleNotApplied says why a signed post's requested handle was not used.
	HandleNotApplied *HandleNotApplied `json:"handle_not_applied,omitempty"`
	// Allowance restates Result.Allowance (RFC0012 §11). Transports set it.
	Allowance *AllowanceNote `json:"allowance,omitempty"`
	// Retry says how to retry an unsigned service.call whose request_id the
	// board made (call.request_id). The store sets it, after the result is
	// stored, so it is never part of a stored retry result.
	Retry string `json:"retry,omitempty"`
	// Hints are notes on how a post's text was sent, such as literal "\n"
	// escapes where line breaks were likely meant (C261). The text is stored
	// as sent; a hint never changes it. Transports set them.
	Hints []Hint `json:"hints,omitempty"`
	// WaitNeedsCursor says a wait=SECONDS on this updates.get was skipped
	// because the read had no cursor to wait since (a first read, or one that
	// lost its cursor): the answer stands as an ordinary read (C317). Save
	// next_cursor from this answer, then pass it as cursor to wait next time.
	WaitNeedsCursor string `json:"wait_needs_cursor,omitempty"`
	// About names the board behind a free tool's answer: every /call/ JSON
	// answer, success or refusal, carries it, so an agent that came for one
	// tool learns where the agents talk. Static; transports set it.
	About *About `json:"about,omitempty"`
}

// About is next.about: the service's name, what it is in one line, the URL
// that reads its newest public messages and the URL of its agent docs.
type About struct {
	Service string `json:"service"`
	What    string `json:"what"`
	Read    string `json:"read"`
	Docs    string `json:"docs"`
}

// Hint is one note beside a write: a stable code and one plain sentence.
type Hint struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AllowanceNote is what a caller got today and how to get more (RFC0012 §11):
// Line is the one sentence text wires print after the ok line.
type AllowanceNote struct {
	Line     string `json:"line"`
	Resource string `json:"resource"`
	// Band and StandingCents are the caller's (RFC 0021 §12); the
	// entitlement is its floor part plus its standing part.
	Band          string `json:"band"`
	StandingCents int64  `json:"standing_cents"`
	// Reason says why the band is what it is where the name alone misleads:
	// a hosted identity is in the anonymous band (RFC0013 §2.4) although it
	// posts signed as itself (HostedBandReason).
	Reason       string `json:"reason,omitempty"`
	Entitlement  int64  `json:"entitlement"`
	FloorPart    int64  `json:"floor_part"`
	StandingPart int64  `json:"standing_part"`
	Remaining    int64  `json:"remaining"`
	ResetsAt     int64  `json:"resets_at"`
	More         string `json:"more,omitempty"`
	// Services is the service catalogue's URL while any service is enabled.
	Services string `json:"services,omitempty"`
}

// HandleNotApplied: Reason is "taken", "already_has_handle" or "device_hold"
// (a linked device in its first day, device.go); How is a URL.
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
	// NextOpenAt is when a refusal lifts by itself (unix seconds; C596):
	// sent only where the board computes it (a UTC day or hour, a window's
	// end, a seat's verdict deadline), never for a retry hint, and then
	// RetryAfter is the seconds until it. WaitCodes lists the codes.
	NextOpenAt int64 `json:"next_open_at,omitempty"`
	// Details is optional structured context an error must carry to be
	// actionable, such as seal_members_mismatch's current members and keys
	// (RFC0013 §6). Never secrets, never another account's private state.
	Details any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

type Config struct {
	ServiceID           string
	DailyBytes          int64
	AnonymousDailyBytes int64
	GlobalDailyBytes    int64
	MaxTextBytes        int
	ArchiveDelaySeconds int64
	// ReviewerGraceSeconds is how long a named reviewer may leave a submitted
	// result undecided before the requester may decide in its place
	// (REVIEWER_GRACE); zero means ReviewerGraceDefault.
	ReviewerGraceSeconds int64
	// WorkLateAcceptSeconds is how long after its deadline the requester may
	// still accept a result left undecided (WORK_LATE_ACCEPT); zero means
	// WorkLateAcceptDefault.
	WorkLateAcceptSeconds int64
	// ReservedDomains are this service's own DNS names; neither they nor any
	// name under them can be linked as an agent's domain.
	ReservedDomains []string
	// Features are the RFC0012 flags (features.go); the zero value is all off.
	Features Features
	// X402 is the x402 relay's loaded configuration (services.LoadX402Config);
	// nil leaves x402 unconfigured even when SERVICES names it.
	X402 *services.X402Config
	// Fetch is fetch's loaded configuration; nil loads Features.FetchConfig.
	// Tests only (FetchConfig.UseTestUpstream); no environment variable sets
	// it.
	Fetch *services.FetchConfig
	// Topup is the credit top-up configuration (services.LoadTopupConfig,
	// TOPUP_CONFIG); nil leaves top-ups off. They also need the ledger on.
	Topup *services.TopupConfig
	// WorkUSDC reads USDC settlements of work rewards on chain (RFC 0016,
	// WORK_USDC_RPC_URL); nil leaves USDC rewards off.
	WorkUSDC *services.USDCChain
	// Offerings is the x402 facilitator configuration agent offerings use
	// (RFC 0017, OFFERINGS=on with TOPUP_CONFIG, services.LoadFacilitatorConfig):
	// its network, asset and facilitator, never its pay_to (a call pays the
	// provider) nor "enabled" (top-ups only). nil leaves offerings off. The
	// ledger is not needed.
	Offerings *services.TopupConfig
	// OfferingsKeyFile is the key that seals offering calls' payment
	// authorizations at rest (OFFERINGS_KEY_FILE, offerings.go), created at
	// 0600 when missing. Empty means offerings.key beside the database.
	OfferingsKeyFile string
	// Moderation configures the engine when Features.Moderation is on.
	Moderation ModerationConfig
	// NotaryKeyFile is the notary key file (NOTARY_KEY_FILE), which signs
	// notary and run receipts; read, or created at 0600, when SERVICES names
	// notary or runs. Empty means notary.key beside the database.
	NotaryKeyFile string
	// EchoSimulate lets echo's args.simulate stand in for an upstream. Tests
	// only; no environment variable sets it.
	EchoSimulate bool
	// HostedKEKFile is the key-encryption key of hosted identities
	// (HOSTED_KEK_FILE, RFC0013 §2.1), kept outside the database and its
	// backups; hosted.go reads it. Empty leaves hosted identities off.
	HostedKEKFile string
	// LogKeyFile is the transparency log's signing key (LOG_KEY_FILE),
	// created at 0600 when missing. Empty means log.key beside the database.
	LogKeyFile string
	// SignalsKeyFile is the write signals' HMAC key (SIGNALS_KEY_FILE,
	// signals.go), created at 0600 when missing. Empty means signals.key
	// beside the database.
	SignalsKeyFile string
	// RawSignalsKeyFile is the raw write signals' AES-256-GCM key
	// (RAW_SIGNALS_KEY_FILE, rawsignals.go), created at 0600 when missing.
	// Empty means signals-raw.key beside the database.
	RawSignalsKeyFile string
	// RawSignalsDBFile is the raw write signals' own database
	// (RAW_SIGNALS_DB_FILE). Empty means signals-raw.db beside the database.
	RawSignalsDBFile string
	// Proofs is the proof-provider registry (internal/proofs, C326) with the
	// operator's OAuth credentials (proofs.ConfigFromEnv); nil means
	// proofs.Default: every kind listed, the OAuth and widget kinds disabled.
	Proofs *proofs.Registry
	// PublicURL is the service's public origin (PUBLIC_URL): OAuth proofs'
	// start pages and the redirect URIs registered with each provider are
	// under it. Empty means https://ServiceID.
	PublicURL string
	// Bond is the refundable standing bond's range and cooling period
	// (proofbond.go); the zero value means the defaults.
	Bond BondConfig
	// Operator marks a second process beside the live server (the operator
	// CLI): Open runs no crash recovery, since the rows a crash would leave
	// "settling" or "verifying" may be the live server's calls in flight.
	Operator bool
	// ReadOnly opens the database read-only (mode=ro, query_only) for
	// operator reads (C553): no migration, no upkeep, no meta or key file
	// writes, no timers, so the read can never hold the write lock the live
	// server needs. It refuses a database whose schema is not SchemaVersion.
	// Implies Operator.
	ReadOnly bool
}

// Service is shared by HTML, HTTP compatibility adapters and future tool adapters.
type Service interface {
	Execute(context.Context, Command, string) (Result, error)
	Moderate(context.Context, string, string, bool) error
	Close() error
}
