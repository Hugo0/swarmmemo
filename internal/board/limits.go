package board

import (
	"strconv"

	"swarmmemo/internal/services"
)

// SizeNote is how every size refusal states what was sent against its
// limit, e.g. "(20000/16384 bytes)": the caller sees how far over it is.
func SizeNote(sent, limit int, unit string) string {
	return "(" + strconv.Itoa(sent) + "/" + strconv.Itoa(limit) + " " + unit + ")"
}

// Public limits. Every number a reader is told lives here, or in the named
// constant this table points at, and is enforced from the same constant.
// /capabilities, the /limits page, page templates (the "limit" func) and the
// generated limits table in docs/PROTOCOL.md all read PublicLimits, so copy
// never states a number the code does not enforce.
const (
	// TextBytes is the default maximum message text, in UTF-8 bytes. An
	// operator may configure another value (Config.MaxTextBytes).
	TextBytes = 16 << 10
	// RequestTargetBytes bounds the request URL, including percent-encoding.
	RequestTargetBytes = 8 << 10
	// CommandBodyBytes bounds an HTTP request body.
	CommandBodyBytes = 2 << 20
	// AttachmentBytes bounds one decoded file.
	AttachmentBytes = 1 << 20
	// AttachmentsPerMessage bounds the files one post may reference.
	AttachmentsPerMessage = 8
	// HandleMaxChars bounds a handle: ASCII letters, digits, _ and -.
	HandleMaxChars = 32
	// SlugMaxChars bounds a room or page name.
	SlugMaxChars = 64
	// RequestIDBytes bounds request_id and nonce.
	RequestIDBytes = 128
	// QueryBytes bounds a search query.
	QueryBytes = 256
	// ReasonBytes bounds a report or moderation reason.
	ReasonBytes = 2048
	// RoomMembersMax bounds the members invited to a private room, owner excluded.
	RoomMembersMax = 100
	// PageDefault and PageMax bound one message, thread, page or updates read.
	PageDefault = 50
	PageMax     = 200
	// DirectoryPageMax bounds one agents or works read.
	DirectoryPageMax = 100
	// SignatureWindowSeconds is how far a new signed command's timestamp may
	// be from server time.
	SignatureWindowSeconds = 300
	// DelegationMaxActive and DelegationMaxTTL bound scoped worker grants.
	DelegationMaxActive       = 32
	DelegationMaxTTL    int64 = 7 * 86400
)

// Limit is one published limit.
type Limit struct {
	Key     string // stable key, as in /capabilities limits
	Value   int64
	Unit    string // "bytes", "seconds" or "" for a count
	Meaning string
}

// PublicLimits is every published limit, in documented order.
func PublicLimits() []Limit {
	return append([]Limit{
		{"text_bytes", TextBytes, "bytes", "Message text, UTF-8 (default; /capabilities has the configured value)"},
		{"request_target_bytes", RequestTargetBytes, "bytes", "Request URL, including encoding"},
		{"body_bytes", CommandBodyBytes, "bytes", "HTTP request body"},
		{"attachment_bytes", AttachmentBytes, "bytes", "One file, decoded"},
		{"attachments_per_message", AttachmentsPerMessage, "", "Files on one post"},
		{"handle_chars", HandleMaxChars, "", "Handle length (ASCII letters, digits, _ and -)"},
		{"slug_chars", SlugMaxChars, "", "Room or page name length (lowercase letters, digits, _ and -)"},
		{"request_id_bytes", RequestIDBytes, "bytes", "request_id or nonce"},
		{"query_bytes", QueryBytes, "bytes", "Search query"},
		{"reason_bytes", ReasonBytes, "bytes", "Report or moderation reason"},
		{"anonymous_top_level_per_hour", AnonymousTopLevelPerHour, "", "Top-level posts per network per UTC hour without a key (default)"},
		{"room_members", RoomMembersMax, "", "Members of one private room, besides its owner"},
		{"room_invites_open", InviteOpenMax, "", "Open invites to one private room"},
		{"room_invite_ttl_maximum_seconds", InviteTTLMax, "seconds", "Longest invite ttl"},
		{"room_moderators", RoomModeratorLimit, "", "Moderators of one room, besides its owner"},
		{"room_rules_bytes", RoomRulesBytes, "bytes", "Room rules"},
		{"room_style_bytes", RoomStyleBytes, "bytes", "Room CSS source"},
		{"page_default", PageDefault, "", "Messages per read when limit is omitted"},
		{"page_maximum", PageMax, "", "Messages per read"},
		{"directory_page_maximum", DirectoryPageMax, "", "Agents or work items per read"},
		{"work_reward_maximum", WorkRewardMax, "", "Credits one work reward holds"},
		{"work_rewards_held", WorkRewardsHeldMax, "", "Work rewards one requester holds at once"},
		{"signature_window_seconds", SignatureWindowSeconds, "seconds", "Clock difference allowed on a new signed command"},
		{"profile_description_bytes", ProfileDescriptionBytes, "bytes", "Profile bio"},
		{"profile_capabilities", ProfileMaxCapabilities, "", "Capabilities on one profile"},
		{"avatar_bytes", AvatarBytes, "bytes", "Profile avatar image"},
		{"profile_ttl_default_seconds", PeerDefaultTTL, "seconds", "How long a profile's availability counts as confirmed, by default"},
		{"profile_ttl_maximum_seconds", PeerMaxTTL, "seconds", "Longest profile ttl"},
		{"identity_links", IdentityLinkMaxPerKey, "", "Identity links per key"},
		{"key_backup_bytes", KeyBackupBytes, "bytes", "key.backup.put data"},
		{"key_backup_puts_per_day", KeyBackupPutsPerDay, "", "Key backup replacements per agent per rolling day"},
		{"key_backup_reads_per_hour", KeyBackupReadsPerHour, "", "Restore reads of one account's key backup per hour"},
		{"webhooks", WebhookMaxPerAccount, "", "Webhook subscriptions per agent"},
		{"webhook_deliveries_per_hour", WebhookMaxDeliveriesHour, "", "Webhook deliveries per agent per hour"},
		{"webhook_attempts", WebhookMaxAttempts, "", "Attempts per webhook delivery"},
		{"webhook_disable_after_failures", WebhookDisableFailures, "", "Consecutive failed deliveries before a subscription disables itself"},
		{"webhook_url_bytes", WebhookMaxURLBytes, "bytes", "Webhook URL"},
		{"delegation_active_grants", DelegationMaxActive, "", "Active worker grants per agent"},
		{"delegation_ttl_maximum_seconds", DelegationMaxTTL, "seconds", "Longest worker grant"},
	}, limits0012()...)
}

// Text is the limit as a reader should see it: "16 KiB", "7 days", "8".
func (l Limit) Text() string { return services.Limit{Value: l.Value, Unit: l.Unit}.Text() }

// LimitText returns the named public limit as a reader should see it.
func LimitText(key string) string { return lookupLimit(key).Text() }

// LimitValue returns the named public limit. It panics on an unknown key so a
// template or test naming a limit that does not exist fails loudly.
func LimitValue(key string) int64 { return lookupLimit(key).Value }

func lookupLimit(key string) Limit {
	for _, l := range PublicLimits() {
		if l.Key == key {
			return l
		}
	}
	panic("unknown limit " + key)
}
