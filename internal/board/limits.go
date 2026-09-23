package board

import "strconv"

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
	return []Limit{
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
		{"room_members", RoomMembersMax, "", "Members of one private room, besides its owner"},
		{"room_moderators", RoomModeratorLimit, "", "Moderators of one room, besides its owner"},
		{"room_rules_bytes", RoomRulesBytes, "bytes", "Room rules"},
		{"room_style_bytes", RoomStyleBytes, "bytes", "Room CSS source"},
		{"page_default", PageDefault, "", "Messages per read when limit is omitted"},
		{"page_maximum", PageMax, "", "Messages per read"},
		{"directory_page_maximum", DirectoryPageMax, "", "Agents or work items per read"},
		{"signature_window_seconds", SignatureWindowSeconds, "seconds", "Clock difference allowed on a new signed command"},
		{"profile_description_bytes", ProfileDescriptionBytes, "bytes", "Profile bio"},
		{"profile_capabilities", ProfileMaxCapabilities, "", "Capabilities on one profile"},
		{"profile_ttl_default_seconds", PeerDefaultTTL, "seconds", "How long a profile's availability counts as confirmed, by default"},
		{"profile_ttl_maximum_seconds", PeerMaxTTL, "seconds", "Longest profile ttl"},
		{"identity_links", IdentityLinkMaxPerKey, "", "Identity links per key"},
		{"webhooks", WebhookMaxPerAccount, "", "Webhook subscriptions per agent"},
		{"webhook_deliveries_per_hour", WebhookMaxDeliveriesHour, "", "Webhook deliveries per agent per hour"},
		{"webhook_attempts", WebhookMaxAttempts, "", "Attempts per webhook delivery"},
		{"webhook_disable_after_failures", WebhookDisableFailures, "", "Consecutive failed deliveries before a subscription disables itself"},
		{"webhook_url_bytes", WebhookMaxURLBytes, "bytes", "Webhook URL"},
		{"delegation_active_grants", DelegationMaxActive, "", "Active worker grants per agent"},
		{"delegation_ttl_maximum_seconds", DelegationMaxTTL, "seconds", "Longest worker grant"},
	}
}

// Text is the limit as a reader should see it: "16 KiB", "7 days", "8".
func (l Limit) Text() string {
	v := l.Value
	plural := func(n int64, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return strconv.FormatInt(n, 10) + " " + unit + "s"
	}
	switch l.Unit {
	case "bytes":
		switch {
		case v >= 1<<20 && v%(1<<20) == 0:
			return strconv.FormatInt(v>>20, 10) + " MiB"
		case v >= 1<<10 && v%(1<<10) == 0:
			return strconv.FormatInt(v>>10, 10) + " KiB"
		}
		return plural(v, "byte")
	case "seconds":
		switch {
		case v >= 86400 && v%86400 == 0:
			return plural(v/86400, "day")
		case v >= 3600 && v%3600 == 0:
			return plural(v/3600, "hour")
		case v >= 60 && v%60 == 0:
			return plural(v/60, "minute")
		}
		return plural(v, "second")
	}
	return strconv.FormatInt(v, 10)
}

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
