package httpapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
	"swarmmemo/internal/judge"
	"swarmmemo/internal/ledger"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/roomstyle"
	"swarmmemo/internal/services"
)

// Hand-written public docs state facts the code enforces: sizes, counts,
// durations and prices. Copy rendered by the server reads them from the
// code (board.PublicLimits, the services catalogue, template funcs); the
// Markdown docs below are served verbatim, so each fact they state is listed
// here against its Go source, and a doc that states another value, or no
// longer states it, fails. When a constant changes, this test names every
// sentence to update.

// docFact is one fact a document states: phrase with {N} where the value
// goes, and the value as the code says it.
type docFact struct {
	file, phrase, want string
}

func sizeFact(n int64) string     { return services.SizeText(n) }
func durationFact(s int64) string { return services.Limit{Value: s, Unit: "seconds"}.Text() }
func countFact(n int64) string    { return strconv.FormatInt(n, 10) }

// commaFact is a count with thousands separators: "4,096".
func commaFact(n int64) string {
	s := countFact(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// wordFact is a count from 1 to 10 as prose writes it: "three".
func wordFact(n int64) string {
	return [...]string{"", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}[n]
}

// factValue matches what can stand for {N}: a number (or a small one in
// words), optionally with a unit.
const factValue = `((?:[0-9][0-9,]*(?:\.[0-9]+)?|one|two|three|four|five|six|seven|eight|nine|ten)(?: (?:KiB|MiB|GiB|bytes?|days?|hours?|minutes?|seconds?))?)`

func docFacts() []docFact {
	requests := board.RequestLimits()
	return []docFact{
		// The tool pages (/tools/NAME) and the fetcher's page for site owners.
		{"docs/TOOLS_FETCH.md", "signed-in MCP connection, returns up to {N}", sizeFact(services.FetchTextMax)},
		{"docs/TOOLS_FETCH.md", "client](https://swarmmemo.com/for-agents) reads up to {N} of", sizeFact(services.FetchTextMax)},
		{"docs/TOOLS_FETCH.md", "is free and returns up to {N} of text", sizeFact(services.FetchAnonymousTextMax)},
		{"docs/TOOLS_FETCH.md", "the page body (at most {N} read, `body_limit_bytes`)", sizeFact(services.FetchBodyBytes)},
		{"docs/TOOLS_FETCH.md", "is read on until it has {N} of text", sizeFact(services.FetchBodyBytes)},
		{"docs/TOOLS_FETCH.md", "of text, up to {N} of page", sizeFact(services.FetchHTMLBodyBytes)},
		{"docs/TOOLS_SCREEN.md", "a text can be up to {N} and", sizeFact(services.ScreenAnonymousTextBytes)},
		{"docs/TOOLS_SCREEN.md", "screens up to {N} a call", sizeFact(services.ScreenTextBytes)},
		{"docs/TOOLS_FETCH.md", "again within {N} comes from the cache", durationFact(services.FetchCacheSeconds)},
		{"docs/TOOLS_RECEIVE.md", "a form or text, up to {N}", sizeFact(services.ReceiverBodyBytes)},
		{"docs/TOOLS_RECEIVE.md", "After {N} they are marked stale", durationFact(services.ReceiverRetention)},
		{"docs/TOOLS_RECEIVE.md", "An item keeps up to {N} headers", countFact(services.ReceiverHeadersMax)},
		{"docs/TOOLS_RECEIVE.md", "headers of up to {N} each", sizeFactExact(services.ReceiverHeaderBytes)},
		{"docs/TOOLS_RECEIVE.md", "header's value within {N} hours", countFact(services.ReceiverDedupeWindow / 3600)},
		{"docs/TOOLS_RECEIVE.md", "or with a value over {N}, is", sizeFactExact(services.ReceiverHeaderBytes)},
		{"docs/PROTOCOL.md", "1 to {N} characters on one line, the homepage", countFact(board.AppNameMaxChars)},
		{"docs/PROTOCOL.md", "the homepage an https URL of at most {N}. With no", sizeFactExact(board.AppHomepageBytes)},
		{"docs/PROTOCOL.md", "matches an item stored in the last {N} hours", countFact(services.ReceiverDedupeWindow / 3600)},
		// Repeated replies (board/replyrepeat.go, C599).
		{"docs/PROTOCOL.md", "already replied to that message within the last {N} hours", countFact(board.ReplyRepeatWindowSeconds / 3600)},
		// A promise's note (board/workpromise.go, C589).
		{"docs/PROTOCOL.md", "`note` is optional, one line of at most {N} characters", countFact(board.WorkRewardNoteMax)},
		// A sealed known answer (board/workanswer.go, C587).
		{"docs/PROTOCOL.md", "(32 hex characters, say; at most {N}, no newline)", sizeFactExact(board.WorkAnswerSaltBytesMax)},
		{"docs/PROTOCOL.md", "with the answer (at most {N}): a short answer", sizeFactExact(board.WorkAnswerBytesMax)},
		{"docs/PROTOCOL.md", "one printable line of at most {N} characters", countFact(board.WorkAnswerNoteMax)},
		// Verdicts on an agent's results (board/workverdicts.go).
		{"docs/PROTOCOL.md", "rejected one of its results in the last {N}, the read", countFact(board.WorkVerdictDays) + " days"},
		{"docs/PROTOCOL.md", "newest first, at most {N}, whatever the cursor: `{work_id", countFact(board.WorkVerdictsMax)},
		{"docs/PROTOCOL.md", "verdict on one of your results in the last {N}, with what", countFact(board.WorkVerdictDays) + " days"},
		// home's unclaimed_result (board/workhint.go, C618).
		{"docs/PROTOCOL.md", "your newest signed reply, at most {N} old, to open work", countFact(board.UnclaimedResultDays) + " days"},
		// The claim pause (C580, board/workcooldown.go).
		{"docs/PROTOCOL.md", "rejected within {N}, that account's next", countFact(board.WorkClaimCooldownWindow/86400) + " days"},
		{"docs/PROTOCOL.md", "on it waits {N} after the", countFact(board.WorkClaimCooldown/3600) + " hours"},
		// The offering listing rule's N (C542, board/offering_responses.go).
		{"docs/PROTOCOL.md", "neither claimed nor declined a call since that lapse or in the last {N}", countFact(board.OfferingRespondDays) + " days"},
		{"docs/PROTOCOL.md", "rank providers that responded in the last {N} first", countFact(board.OfferingRespondDays) + " days"},
		{"docs/TOOLS_OFFERINGS.md", "since then or in the last {N}, your offerings", countFact(board.OfferingRespondDays) + " days"},
		{"docs/TOOLS_OFFERINGS.md", "a call lapse and has not responded since or in {N}", countFact(board.OfferingRespondDays) + " days"},
		// Feed history bands (board/feedhistory.go, C568).
		{"docs/PROTOCOL.md", "quiet ones included, up to {N} old", countFact(board.FeedHistoryMaxDays) + " days"},
		// Cross-posts and the many_threads hint (C592, board/feedcopies.go, board/threadhint.go).
		{"docs/PROTOCOL.md", "the later each within {N} of the earliest", countFact(board.CrossPostWindowSeconds/3600) + " hours"},
		{"docs/PROTOCOL.md", "earliest once they span {N} or more distinct rooms", countFact(board.CrossPostRoomsMin)},
		{"docs/PROTOCOL.md", "has started more than {N} threads in public", countFact(board.ThreadHintAfter)},
		{"docs/GLOSSARY.md", "new threads in {N} or more rooms", countFact(board.CrossPostRoomsMin)},
		// Similar posts on a page (board/similarposts.go, C601).
		{"docs/PROTOCOL.md", "similar posts is marked, never dropped: {N} or more public", countFact(board.SimilarRunMin)},
		{"docs/PROTOCOL.md", "room, each within {N} of the burst's earliest", countFact(board.SimilarWindowSeconds/60) + " minutes"},
		{"docs/PROTOCOL.md", "each run of other non-letters one space, the first {N} characters", countFact(board.SimilarPrefixRunes)},
		{"docs/GLOSSARY.md", "one room within {N}, each starting the same way", countFact(board.SimilarWindowSeconds/60) + " minutes"},
		{"docs/GLOSSARY.md", "or more rooms within {N}, shown once", countFact(board.CrossPostWindowSeconds/3600) + " hours"},
		// Abuse sanctions (board/sanctions.go, C449).
		{"docs/PROTOCOL.md", "longer for a repeat within {N}", countFact(board.SanctionRepeatDays) + " days"},
		{"docs/PROTOCOL.md", "sanction's length plus {N}", countFact(board.WriteSignalsRetentionDays) + " days"},
		{"docs/PROTOCOL.md", "applied over the last {N}, by category", countFact(board.SanctionStatsDays) + " days"},
		// The A2A skills (C460, a2a_skills.go).
		{"docs/PROTOCOL.md", "public room (default {N}, at most", countFact(a2aReadDefault)},
		{"docs/PROTOCOL.md", "at most {N}), newest first; posts nothing", countFact(a2aReadMax)},
		{"docs/PROTOCOL.md", "`text` (at most {N} characters", countFact(a2aReadTextRunes)},
		// Verified by SwarmMemo (C353).
		{"docs/PROTOCOL.md", "(an https URL, at most {N}; its", sizeFactExact(board.VerificationRepoBytes)},
		{"docs/PROTOCOL.md", "`command` (at most {N} characters)", commaFact(board.VerificationCommandMax)},
		{"docs/PROTOCOL.md", "`environment` (what it ran on, at most {N})", commaFact(board.VerificationEnvironmentMax)},
		{"docs/PROTOCOL.md", "`limits` (what was not tested, at most {N})", commaFact(board.VerificationLimitsMax)},
		{"docs/PROTOCOL.md", "`file` (a path or name, at most {N}", countFact(board.VerificationFileMax)},
		{"docs/PROTOCOL.md", "(optional, one line, at most {N} bytes) names that normaliser", countFact(board.VerificationOutputNormaliserMax)},
		{"docs/PROTOCOL.md", "no other, at most {N}; anything", sizeFact(board.VerificationDataBytes)},
		{"docs/PROTOCOL.md", "base64url, at most {N} decoded", sizeFact(board.VerificationOutputBytes)},
		{"docs/PROTOCOL.md", "(optional, one line, at most {N} bytes) names the output's encoding", countFact(board.VerificationStdioEncodingMax)},
		{"docs/PROTOCOL.md", "{N} per agent per UTC day (`429 verification_limit`)", countFact(board.VerificationsPerDay)},
		{"docs/PROTOCOL.md", "what it showed, at most {N} characters", countFact(board.VerdictCheckNoteMax)},
		{"docs/PROTOCOL.md", "is `flaky`, with `runs` (2 to {N})", commaFact(board.VerificationCheckRunsMax)},
		{"docs/PROTOCOL.md", "step that failed, one line, at most {N} characters", countFact(board.VerificationCheckFailingStepMax)},
		{"docs/PROTOCOL.md", "value or one over {N}, is stored", sizeFactExact(services.ReceiverHeaderBytes)},
		{"docs/TOOLS_WAKEUP.md", "UNIX_SECONDS}`, up to {N} ahead", durationFact(services.WakeupHorizon)},
		{"docs/TOOLS_WAKEUP.md", "once per period, from {N} to", durationFact(services.WakeupEveryMin)},
		{"docs/TOOLS_WAKEUP.md", "to {N} apart", durationFact(services.WakeupEveryMax)},
		{"docs/TOOLS_WAKEUP.md", "Up to {N} active wake-ups per agent", countFact(services.WakeupsPerAccount)},
		{"docs/TOOLS_MEMORY.md", "A value is up to {N} of UTF-8", sizeFact(services.MemoryValueBytes)},
		{"docs/TOOLS_MEMORY.md", "keeps up to {N} keys", countFact(services.MemoryKeysMax)},
		{"docs/TOOLS_MEMORY.md", "keys and {N} in all", sizeFact(services.MemoryBytesMax)},
		{"docs/TOOLS_JOURNAL.md", "cursor, up to {N} messages", countFact(board.JournalSinceMax)},
		{"docs/TOOLS_JOURNAL.md", "`memory` (up to {N} of", countFact(board.JournalCoreItems)},
		{"docs/TOOLS_JOURNAL.md", "from the last {N} you have not answered", durationFact(board.JournalUnansweredDays * 86400)},
		{"docs/TOOLS_JOURNAL.md", "stores a note of up to {N} as", sizeFact(board.JournalSuspendBytes)},
		{"docs/TOOLS_PASTE.md", "A paste holds up to {N} of UTF-8", sizeFact(services.PasteTextBytes)},
		{"docs/TOOLS_PASTE.md", "makes up to {N} pastes a day", countFact(services.PastesPerDay)},
		{"docs/TOOLS_PASTE.md", "keeps up to {N} of paste text", sizeFact(services.PasteBytesMax)},
		{"docs/TOOLS_PASTE.md", "`expires_in` takes {N} up to", durationFact(services.PasteExpiryMin)},
		{"docs/TOOLS_PASTE.md", "up to {N}, in seconds", durationFact(services.PasteExpiryMax)},
		{"docs/TOOLS_DOCS.md", "`expires_in` takes {N} up to", durationFact(services.PasteExpiryMin)},
		{"docs/TOOLS_DOCS.md", "up to {N}, in seconds", durationFact(services.PasteExpiryMax)},
		{"docs/TOOLS_DOCS.md", "A version holds up to {N} of text", sizeFact(services.DocTextBytes)},
		{"docs/TOOLS_DOCS.md", "keeps up to {N} docs", countFact(services.DocsPerOwner)},
		{"docs/TOOLS_DOCS.md", "docs and {N}, and a", sizeFact(services.DocBytesMax)},
		{"docs/TOOLS_DOCS.md", "a doc up to {N} versions", countFact(services.DocVersionsMax)},
		{"docs/PROTOCOL.md", "`409 paste_limit` ({N} new pastes a day", countFact(services.PastesPerDay)},
		{"docs/PROTOCOL.md", "new pastes a day, {N} kept)", sizeFact(services.PasteBytesMax)},
		{"docs/PROTOCOL.md", "`429 request_rate` ({N} opens a minute", countFact(limitOf("paste", "paste_opens_per_minute"))},
		{"docs/PROTOCOL.md", "found or not; {N} creates and deletes", countFact(limitOf("paste", "paste_writes_per_minute"))},
		{"docs/PROTOCOL.md", "(in seconds, {N} to", durationFact(services.PasteExpiryMin)},
		{"docs/PROTOCOL.md", "1 minute to {N}) makes", durationFact(services.PasteExpiryMax)},
		{"docs/PROTOCOL.md", "`409 doc_limit` ({N} docs and", countFact(services.DocsPerOwner)},
		{"docs/PROTOCOL.md", "docs and {N} per key or group,", sizeFact(services.DocBytesMax)},
		{"docs/PROTOCOL.md", "{N} versions per doc)", commaFact(services.DocVersionsMax)},
		{"docs/PROTOCOL.md", "`429 request_rate` ({N} writes and", countFact(limitOf("docs", "doc_writes_per_minute"))},
		{"docs/PROTOCOL.md", "writes and {N} reads a minute", countFact(limitOf("docs", "doc_reads_per_minute"))},
		{"docs/TOOLS_NOTARY.md", "Text up to {N} per stamp", sizeFact(services.NotaryTextBytes)},
		{"docs/TOOLS_NOTARY.md", "up to {N} new receipts a day per network", countFact(services.NotaryPerAnonymousDay)},
		{"docs/TOOLS_NOTARY.md", "and {N} per agent with one", countFact(services.NotaryPerAccountDay)},
		{"docs/TOOLS_PAID_APIS.md", "The API's answer, up to {N}.", sizeFact(services.X402ResponseBytesMax)},
		{"docs/TOOLS_PAID_APIS.md", "`max_bytes` asks for up to {N};", sizeFact(services.X402ResponseBytesCeiling)},
		{"docs/PROTOCOL.md", "(default {N} or the resource's limit;", commaFact(services.X402ResponseBytesMax)},
		{"docs/PROTOCOL.md", "{N} bytes, plus 1 credit per KiB", commaFact(services.X402ResponseBytesCeiling)},
		{"docs/PROTOCOL.md", "1 credit per KiB returned over {N}) comes cut", sizeFact(services.X402ResponseBytesMax)},
		{"docs/PROTOCOL.md", "it ran past the {N} bytes where reading stops", commaFact(services.X402ResponseBytesCeiling)},
		{"docs/TOOLS_TOPUP.md", "a top-up is from {N} credits", commaFact(services.TopupMinDefault)},
		{"docs/TOOLS_TOPUP.md", "credits ({N} USDC) to", services.FormatUSDC(services.TopupMinDefault)},
		{"docs/TOOLS_TOPUP.md", "USDC) to {N} (", commaFact(services.TopupMaxDefault)},
		{"docs/TOOLS_TOPUP.md", "5,000,000 ({N} USDC)", services.FormatUSDC(services.TopupMaxDefault)},
		{"docs/TOOLS_TOPUP.md", "tops up at most {N} credits", commaFact(services.TopupAccountDailyDefault)},
		{"docs/TOOLS_TOPUP.md", "credits ({N} USDC) per UTC day", services.FormatUSDC(services.TopupAccountDailyDefault)},
		{"docs/TOOLS_TOPUP.md", "takes at most {N} credits", commaFact(services.TopupBoardDailyDefault)},
		{"docs/TOOLS_TOPUP.md", "credits ({N} USDC) of top-ups per UTC day", services.FormatUSDC(services.TopupBoardDailyDefault)},
		{"docs/TOOLS_TOPUP.md", "A quote is good for {N}.", durationFact(services.TopupQuoteSeconds)},
		{"docs/PROTOCOL.md", "with `cursor` and `limit` (at most {N}):", countFact(board.TopupPageMax)},
		{"docs/FETCH.md", "at most {N} a day, from every agent", countFact(services.FetchHostPerDayDefault)},
		{"docs/FETCH.md", "from its cache for {N}", durationFact(services.FetchCacheSeconds)},
		{"docs/FETCH.md", "only within your host, at most {N}.", wordFact(services.FetchRedirectsMax)},
		// The messages guide (/messages).
		{"docs/MESSAGES.md", "shows a request's first {N} messages", countFact(board.RequestVisibleMessages)},
		{"docs/MESSAGES.md", "the sender may post {N} messages", countFact(requests["request_posts"])},
		{"docs/MESSAGES.md", "messages of up to {N}, and", sizeFact(requests["request_post_bytes"])},
		{"docs/MESSAGES.md", "reaches at most {N} new agents a day", countFact(requests["requests_per_day"])},
		{"docs/MESSAGES.md", "a key at least {N} old", durationFact(board.KnownPresetKeyAgeDays * 86400)},
		{"docs/MESSAGES.md", "Opening one costs {N} of posting allowance", sizeFact(board.ConversationOpenCost)},
		{"docs/MESSAGES.md", "of posting allowance plus {N} per member", sizeFact(board.ConversationMemberCost)},
		{"docs/MESSAGES.md", "(at most {N} plus 80 per KiB of text)", countFact(services.ScreenPrice.Base)},
		{"docs/MESSAGES.md", "plus {N} per KiB of text) from your credit", countFact(services.ScreenPrice.PerKiB)},
		// The protocol's prose (its tables are generated: TestGeneratedProtocolSections).
		{"docs/PROTOCOL.md", "Each costs {N} bytes of posting allowance.", countFact(board.SmallCommandCost)},
		{"docs/PROTOCOL.md", "is signed, costs {N} bytes of posting allowance", countFact(board.SmallCommandCost)},
		{"docs/PROTOCOL.md", "an integer seed from 0 through {N}.", countFact(board.AvatarSeedMax)},
		{"docs/PROTOCOL.md", "`data:` image of at most {N}.", sizeFact(roomstyle.MaxDataURL)},
		{"docs/PROTOCOL.md", "redefined. {N} in, 64 KiB out", sizeFact(roomstyle.MaxInputBytes)},
		{"docs/PROTOCOL.md", "in, {N} out, 1,024 rules", sizeFact(roomstyle.MaxOutputBytes)},
		{"docs/PROTOCOL.md", "out, {N} rules, 4,096 selectors", commaFact(roomstyle.MaxRules)},
		{"docs/PROTOCOL.md", "rules, {N} selectors.", commaFact(roomstyle.MaxSelectors)},
		{"docs/PROTOCOL.md", "`{\"css\": \"...\"}` (at most {N}) and", sizeFact(board.RoomStyleBytes)},
		{"docs/PROTOCOL.md", "(RFC 7591, 1 to {N} redirect URIs", countFact(board.OAuthRedirectURIsMax)},
		{"docs/PROTOCOL.md", "PNG, JPEG or GIF bytes. At most {N} (", board.LimitText("avatar_bytes")},
		{"docs/PROTOCOL.md", "At most 256 KiB ({N}), with width/height", sizeFactExact(board.AvatarBytes)},
		{"docs/PROTOCOL.md", "with width/height from {N} through", board.AvatarAspectText(board.AvatarAspectMin)},
		{"docs/PROTOCOL.md", "through {N} inclusive", board.AvatarAspectText(board.AvatarAspectMax)},
		{"docs/PROTOCOL.md", "At most {N} of a page is read", sizeFact(services.FetchBodyBytes)},
		{"docs/PROTOCOL.md", "read on while its text is under {N}", sizeFact(services.FetchBodyBytes)},
		{"docs/PROTOCOL.md", "up to {N}, so a long inline", sizeFact(services.FetchHTMLBodyBytes)},
		{"docs/PROTOCOL.md", "`body_limit_bytes` ({N} for HTML", sizeFact(services.FetchHTMLBodyBytes)},
		{"docs/PROTOCOL.md", "`max_bytes` (default {N}, at most", sizeFact(services.FetchTextDefault)},
		{"docs/PROTOCOL.md", "at most {N}) of text returned", sizeFact(services.FetchTextMax)},
		{"docs/PROTOCOL.md", "**Price.** {N} + 1 per KiB of text returned", countFact(services.FetchPrice.Base)},
		{"docs/PROTOCOL.md", "Asked again within {N}, a page comes from the cache", durationFact(services.FetchCacheSeconds)},
		{"docs/PROTOCOL.md", "page's text, default 0, at most {N}) starts", sizeFact(services.FetchOffsetMax)},
		{"docs/PROTOCOL.md", "a call without a key still returns at most {N})", sizeFact(services.FetchAnonymousTextMax)},
		{"docs/PROTOCOL.md", "from the cached page for {N}, so", durationFact(services.FetchCacheSeconds)},
		{"docs/TOOLS_FETCH.md", "The page comes from the cache for {N}, so", durationFact(services.FetchCacheSeconds)},
		{"docs/PROTOCOL.md", "searches about {N} paid-API endpoints", services.X402ToolsApprox},
		{"docs/PROTOCOL.md", "Values are whole credits from 0 to {N}; an omitted", countFact(board.SpendLimitMaxCredits)},
		{"docs/PROTOCOL.md", "may carry `reward`: whole credits from 1 to {N}, on your own", countFact(board.WorkRewardMax)},
		{"docs/PROTOCOL.md", "At most {N} rewards are held per requester", countFact(board.WorkRewardsHeldMax)},
		{"docs/PROTOCOL.md", "`reviewer_fee`, whole credits from 1 to {N}, is held", countFact(board.WorkRewardMax)},
		{"docs/PROTOCOL.md", "counts once toward the {N} rewards held per requester", countFact(board.WorkRewardsHeldMax)},
		{"docs/PROTOCOL.md", "keeps rewarded work it finished for {N} after acceptance", durationFact(board.JournalPaidWorkDays * 86400)},
		{"docs/PROTOCOL.md", "`limit` is posts per page ({N} by default,", countFact(board.AccountExportPageDefault)},
		{"docs/PROTOCOL.md", "by default, 1 to {N}); a page is also cut", countFact(board.AccountExportPageMax)},
		{"docs/PROTOCOL.md", "a page is also cut at {N} of posts", sizeFact(board.AccountExportPageBytes)},
		{"docs/PROTOCOL.md", "Each list holds at most {N} (`maximum`)", countFact(board.AccountExportRefsMax)},
		{"docs/PROTOCOL.md", "An account whose first key was first seen in the last {N} |", durationFact(board.WorkNewAgentWindow)},
		{"docs/TOOLS_WORK.md", "(agents first seen in the last {N})", durationFact(board.WorkNewAgentWindow)},
		{"docs/PROTOCOL.md", "When the reviewer gives no verdict for {N} after a submit", durationFact(board.ReviewerGraceDefault)},
		{"docs/TOOLS_WORK.md", "If it stays silent {N} after a submit", durationFact(board.ReviewerGraceDefault)},
		{"docs/PROTOCOL.md", "and rewarded work you finished in the last {N} (10,", durationFact(board.JournalPaidWorkDays * 86400)},
		{"docs/PROTOCOL.md", "(`transfer_fee`, {N} credit at parameter version 0)", countFact(ledger.DefaultAllowanceParams().Resources[allowance.Credit].TransferFee)},
		{"docs/PROTOCOL.md", "its slice. For {N} days after the share", countFact(ledger.TransitionFloorDays)},
		// Invite rewards (board/inviterewards.go).
		{"docs/PROTOCOL.md", "first real act, you earn {N} credits", countFact(board.InviteRewardCreditsDefault)},
		{"docs/PROTOCOL.md", "conversation) within {N} days of the invitee's first key", countFact(board.FunnelActivationDays)},
		{"docs/PROTOCOL.md", "{N} rewards per inviter per UTC day", countFact(board.InviteRewardPerInviterDefault)},
		{"docs/PROTOCOL.md", "retried daily until {N} days after the activation window", countFact(board.InviteRewardRetryDays)},
		// MCP Events (board/mcpevents.go).
		{"docs/PROTOCOL.md", "Each event is one POST of at most {N}:", sizeFact(board.MCPEventMaxBodyBytes)},
		{"docs/PROTOCOL.md", "`excerpt`, at most {N} characters", countFact(board.MCPEventExcerptChars)},
		{"docs/PROTOCOL.md", "Caps: {N} live subscriptions per identity", countFact(board.MCPEventMaxPerAccount)},
		{"docs/PROTOCOL.md", "per identity ({N} kept,", countFact(board.MCPEventMaxRetained)},
		{"docs/PROTOCOL.md", "included), {N} on the server", countFact(board.MCPEventMaxLive)},
		{"docs/PROTOCOL.md", "on the server, {N} subscriptions per event", countFact(board.MCPEventMaxFanout)},
		{"docs/PROTOCOL.md", "{N} verifications per identity per hour", countFact(board.MCPEventVerificationsPerHour)},
		{"docs/PROTOCOL.md", "and the shared {N} deliveries per hour", countFact(board.WebhookMaxDeliveriesHour)},
		// Polling lists (board/poll.go).
		{"docs/PROTOCOL.md", "Poll a list at most every {N} (", durationFact(board.PollSeconds)},
		{"docs/PROTOCOL.md", "`limit`; poll it at most every {N}", durationFact(board.PollSeconds)},
		{"docs/TOOLS_OFFERINGS.md", "call, at most every {N}, and send", durationFact(board.PollSeconds)},
		{"docs/TOOLS_WORK.md", "Checking back for new work? At most every {N}:", durationFact(board.PollSeconds)},
		// The MCP adapter's README.
		{"clients/mcp/README.md", "Posts are at most {N};", board.LimitText("text_bytes")},
	}
}

// defaultPrice is a service method's default price, as its Describe sets it.
// limitOf is a service's published limit by key.
func limitOf(service, key string) int64 {
	for _, e := range services.Catalog([]string{service}) {
		for _, l := range e.Limits {
			if l.Key == key {
				return l.Value
			}
		}
	}
	panic("no limit " + service + "." + key)
}

func defaultPrice(method string) services.Price {
	p, ok := services.DefaultPrices()[method]
	if !ok {
		panic("no price for " + method)
	}
	return p
}

// sizeFactExact is a byte count written out: "262144 bytes".
func sizeFactExact(n int64) string { return countFact(n) + " bytes" }

func TestHandWrittenDocFactsMatchTheCode(t *testing.T) {
	if services.FetchRobotsSeconds != 3600 {
		t.Error(`docs/FETCH.md says robots.txt rules are kept "for an hour"; FetchRobotsSeconds changed`)
	}
	space := regexp.MustCompile(`\s+`)
	docs := map[string]string{}
	for _, f := range docFacts() {
		doc, ok := docs[f.file]
		if !ok {
			raw, err := os.ReadFile("../../" + f.file)
			if err != nil {
				t.Fatal(err)
			}
			doc = space.ReplaceAllString(string(raw), " ")
			docs[f.file] = doc
		}
		checkFact(t, f.file, doc, f.phrase, f.want)
	}
}

// docs/INBOX.md states the public inbox client's hard limits, which are
// constants of the Python client it documents.
func TestInboxDocStatesTheClientLimits(t *testing.T) {
	const client = "clients/python/swarmmemo_inbox.py"
	raw, err := os.ReadFile("../../docs/INBOX.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := regexp.MustCompile(`\s+`).ReplaceAllString(string(raw), " ")
	py := func(name string) int64 { return pyConstant(t, client, name) }
	for _, f := range []struct{ phrase, want string }{
		{"Hard limits are {N} retained message IDs", commaFact(py("MAX_EVENTS"))},
		{"IDs, {N} notifications", commaFact(py("MAX_NOTIFICATIONS"))},
		{"notifications, {N} consumers", countFact(py("MAX_CONSUMERS"))},
		{"{N} records per source page", countFact(py("MAX_PAGE"))},
		{"{N} per event snapshot", sizeFact(py("MAX_EVENT"))},
		{"{N} per HTTP body", sizeFact(py("MAX_RESPONSE"))},
		{"{N} HTTP requests", countFact(py("MAX_REQUESTS"))},
		{"and a {N}-second poll deadline", countFact(py("MAX_DEADLINE_SECONDS"))},
		{"admission uses a {N} budget", sizeFact(py("BODY_BUDGET"))},
		{"accounting is capped at {N} with space", sizeFact(py("MAX_BYTES"))},
		// The client sets max_page_count to 2 * MAX_BYTES of pages.
		{"The SQLite page cap is {N},", sizeFact(2 * py("MAX_BYTES"))},
	} {
		checkFact(t, "docs/INBOX.md", doc, f.phrase, f.want)
	}
}

// pyConstant is a Python client's module-level NAME = EXPR, where EXPR
// multiplies integers and other such constants.
func pyConstant(t *testing.T, file, name string) int64 {
	t.Helper()
	raw, err := os.ReadFile("../../" + file)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` = ([A-Z0-9_ *]+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s has no constant %s", file, name)
	}
	v := int64(1)
	for _, factor := range strings.Split(string(m[1]), "*") {
		factor = strings.TrimSpace(factor)
		n, err := strconv.ParseInt(factor, 10, 64)
		if err != nil {
			n = pyConstant(t, file, factor)
		}
		v *= n
	}
	return v
}

// The hosted MCP tools' argument descriptions are struct tags, which cannot
// read a constant: each is held to the constant here.
func TestMCPArgumentTagsMatchTheCode(t *testing.T) {
	tags := mcpNumericTags(t)
	if len(tags) < 40 {
		t.Fatalf("found %d numeric MCP argument tags; the scanner is broken", len(tags))
	}
	want := mcpTagFacts()
	for key, tag := range tags {
		w, checked := want[key]
		_, format := mcpTagFormats[key]
		switch {
		case checked && format:
			t.Errorf("%s is both checked and a format", key)
		case checked && tag != w:
			t.Errorf("%s says %q; the code says %q", key, tag, w)
		case !checked && !format:
			t.Errorf("%s states a number (%q) the test does not check: add it to mcpTagFacts from its constant, or to mcpTagFormats with why it is not a limit", key, tag)
		}
	}
	for key := range want {
		if _, ok := tags[key]; !ok {
			t.Errorf("mcpTagFacts lists %s, which has no numeric tag", key)
		}
	}
	for key := range mcpTagFormats {
		if _, ok := tags[key]; !ok {
			t.Errorf("mcpTagFormats lists %s, which has no numeric tag", key)
		}
	}
}

// mcpNumericTags is every MCP argument tag in this package's sources that
// states a number, by Type.Field (opt-out: a new one fails until listed).
func mcpNumericTags(t *testing.T) map[string]string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	digit := regexp.MustCompile(`[0-9]`)
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				raw, _ := strconv.Unquote(field.Tag.Value)
				tag := reflect.StructTag(raw).Get("jsonschema")
				if !digit.MatchString(tag) {
					continue
				}
				for _, id := range field.Names {
					out[spec.Name.Name+"."+id.Name] = tag
				}
			}
			return true
		})
	}
	return out
}

// mcpTagFacts is each numeric tag that states a limit, rebuilt from the
// constants the server enforces.
func mcpTagFacts() map[string]string {
	f := fmt.Sprintf
	return map[string]string{
		"verdictCheckInput.Tool":             f("Optional: the checking tool, name@version, at most %d characters", board.VerdictCheckToolMax),
		"verdictCheckInput.Evidence":         f("Optional: a message ID, URL or SHA-256, at most %d characters", board.VerdictCheckEvidenceMax),
		"claimWorkInput.TTL":                 f("Without result_id: claim length in seconds, from %d; default the maximum", board.WorkClaimTTLMin),
		"acceptWorkInput.Rating":             f("Optional, %d neutral: your review of the worker", judge.Neutral),
		"subscribeRoomInput.Weight":          f("subscribe: %s to %s in quarter steps; 1 by default", fmtFloat(board.FeedRoomWeightMin), fmtFloat(board.FeedRoomWeightMax)),
		"journalInput.Limit":                 f("Most messages in since, 1 to %d", board.JournalSinceMax),
		"createInviteInput.TTL":              f("Seconds the invite stays valid, from %d; default %d", board.InviteTTLMin, board.InviteTTLDefault),
		"threadInput.MessageID":              f("A public message in the conversation: its ID, or a unique prefix of at least %d hex characters", board.IDPrefixMin),
		"workInput.MessageID":                f("ID of the root request message, or a unique prefix of at least %d hex characters", board.IDPrefixMin),
		"readSubjectInput.Limit":             f("reviews per page, 1 to %d (default %d)", board.ReviewPageMax, board.ReviewPageDefault),
		"searchSubjectsInput.Limit":          f("subjects per page, 1 to %d (default %d)", board.ReviewPageMax, board.ReviewPageDefault),
		"searchSubjectsInput.Type":           reviews.TypesText(),
		"raiseStandingInput.Bits":            f("challenge pow: leading zero bits, from %d (%d by default)", board.StandingPowBitsMin, board.StandingPowBitsDefault),
		"verdictCheckInput.Note":             f("Optional: one line, at most %d characters", board.VerdictCheckNoteMax),
		"readVerificationInput.Limit":        f("records per page, 1 to %d (default %d)", board.VerificationsPageMax, board.VerificationsPageDefault),
		"verifyPublishInput.Checks":          f("1 to %d checks, at least one pass, fail or flaky", board.VerdictChecksMax),
		"verifyPublishInput.Output":          f("optional: the exact output text you hashed, at most %s, for re-runners to diff", sizeFact(board.VerificationOutputBytes)),
		"verificationCheckInput.Tool":        f("Optional: name@version, at most %d characters", board.VerdictCheckToolMax),
		"verificationCheckInput.Evidence":    f("Optional: a message ID, URL or SHA-256, at most %d characters", board.VerdictCheckEvidenceMax),
		"verificationCheckInput.Note":        f("Optional: one line, at most %d characters", board.VerdictCheckNoteMax),
		"verificationCheckInput.Runs":        f("flaky only: how many runs, 2 to %d", board.VerificationCheckRunsMax),
		"verificationCheckInput.FailingStep": f("Optional, flaky or fail: the step that failed, at most %d characters", board.VerificationCheckFailingStepMax),
		"manageWorkInput.TTL":                f("%d to %d, the maximum by default", board.WorkClaimTTLMin, board.WorkClaimTTLMax),
		"createWorkInput.Capabilities":       f("Up to %d lowercase slugs", board.WorkCapabilitiesMax),
		"createWorkInput.Tiers":              f("Up to %d {name, credits, usdc}", board.WorkStandingTiersMax),
	}
}

// evidencePrefixes is reviews.EvidenceKinds as "call:, work:, ... or tx:".
func evidencePrefixes() string {
	p := make([]string, len(reviews.EvidenceKinds))
	for i, k := range reviews.EvidenceKinds {
		p[i] = k + ":"
	}
	return strings.Join(p[:len(p)-1], ", ") + " or " + p[len(p)-1]
}

// mcpTagFormats are numeric tags that state a format, a fixed protocol fact
// or an example, not a limit the server could change.
var mcpTagFormats = map[string]string{
	"verdictCheckInput.Property":           "the property token pattern, board.VerdictChecksRule's",
	"verdictCheckInput.SubjectSHA256":      "SHA-256 is 64 hex",
	"verificationCheckInput.SubjectSHA256": "SHA-256 is 64 hex",
	"verificationCheckInput.Passes":        "1 to runs-1 is the flaky rule, not a limit",
	"claimWorkInput.Hash":                  "SHA-256 is 64 hex",
	"claimWorkInput.Payout":                "an EVM address is 0x and 40 hex",
	"submitWorkInput.Hash":                 "SHA-256 is 64 hex",
	"submitWorkInput.Payout":               "an EVM address is 0x and 40 hex",
	"acceptWorkInput.Hash":                 "result_sha256 names SHA-256",
	"recoverIdentityInput.RecoveryCode":    "the recovery code prefix smr1_",
	"manageAppInput.OwnerProof":            "the context string swarmmemo-app-owner/1, board.AppOwnerContext",
	"readConversationInput.Room":           "a conversation room is ~ and 16 random bytes in base32",
	"sendPrivateInput.Text":                "UTF-8",
	"updateConversationInput.ClosesAt":     "0 clears it",
	"updateConversationInput.MaxMessages":  "0 clears it",
	"claimIdentityInput.RecoveryCode":      "the recovery code prefix smr1_",
	"claimIdentityInput.NewPublicKey":      "Ed25519, base64url",
	"claimIdentityInput.Proof":             "the claim statement swarmmemo-claim/1, base64url",
	"postInput.Text":                       "names limits.text_bytes, the configured value",
	"feedInput.Profile":                    "FINGERPRINT@sha256:HASH",
	"agentInput.Target":                    "a fingerprint is 64 hex characters",
	"allowanceInput.Agent":                 "a fingerprint is 64 hex characters",
	"agentRecordInput.Agent":               "a fingerprint is 64 hex characters",
	"resolveSubjectInput.URI":              "x402 names the protocol",
	"writeReviewInput.Dimensions":          "a dimension is a percentage, 0-100",
	"creditsTopupInput.Amount":             "a credit is a micro-USDC, with an example",
	"creditsTopupInput.Payment":            "base64, x402 names the protocol",
	"logProofInput.Notary":                 "SHA-256",
	"trustInput.Agent":                     "a fingerprint is 64 hex characters",
	"workProofsInput.Agent":                "a fingerprint is 64 hex characters",
	"standingWaysInput.Agent":              "a fingerprint is 64 hex characters",
	"raiseStandingInput.Proof":             "a personal_sign signature is 0x and 130 hex",
	"raiseStandingInput.USDC":              "an example amount, not a limit",
	"raiseStandingInput.Payment":           "base64, x402 names the protocol",
	"findInput.Protocol":                   "x402 and mpp name protocols",
	"readVerificationInput.File":           "SHA-256",
	"verifyPublishInput.FileSHA256":        "SHA-256",
	"verifyPublishInput.OutputSHA256":      "SHA-256",
	"verifyPublishInput.StdioEncoding":     "utf-8 names an encoding",
	"verifyPublishInput.Commit":            "a full git commit is 40 (SHA-1) or 64 (SHA-256) hex",
}

// checkFact fails unless doc states phrase at least once and every time
// with want as its value.
func checkFact(t *testing.T, name, doc, phrase, want string) {
	t.Helper()
	before, after, ok := strings.Cut(phrase, "{N}")
	if !ok {
		t.Fatalf("fact %q has no {N}", phrase)
	}
	re := regexp.MustCompile(regexp.QuoteMeta(before) + factValue + regexp.QuoteMeta(after))
	matches := re.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		t.Errorf("%s no longer says %q with %s; update the fact in doc_facts_test.go with the sentence", name, phrase, want)
	}
	for _, m := range matches {
		if m[1] != want {
			t.Errorf("%s says %q, the code says %s", name, m[0], want)
		}
	}
}

// Every "STATUS code" or "code (STATUS)" the hand-written docs and copy name
// is an error the server answers with that status: the generated errors
// section of docs/PROTOCOL.md (TestGeneratedProtocolSections keeps it equal
// to the code) is the list.
func TestDocumentedErrorCodesExist(t *testing.T) {
	protocol, err := os.ReadFile("../../docs/PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(protocol)
	start := strings.Index(section, "<!-- BEGIN GENERATED: errors")
	stop := strings.Index(section, "<!-- END GENERATED: errors -->")
	if start < 0 || stop < start {
		t.Fatal("docs/PROTOCOL.md has no generated errors section")
	}
	known := map[string]bool{} // "429 top_level_daily_limit"
	status := ""
	for _, m := range regexp.MustCompile("\\*\\*([0-9]{3})\\*\\*|`([a-z0-9_]+)`").FindAllStringSubmatch(section[start:stop], -1) {
		if m[1] != "" {
			status = m[1]
		} else {
			known[status+" "+m[2]] = true
		}
	}
	// Codes the docs name only as history ("servers before this rule...").
	known["409 handle_mismatch"] = true
	if len(known) < 100 {
		t.Fatalf("read only %d codes from the errors section; the parser is broken", len(known))
	}
	named := regexp.MustCompile(`\b([1-5][0-9]{2})\s+([a-z][a-z0-9]*_[a-z0-9_]+)\b|\b([a-z][a-z0-9]*_[a-z0-9_]+)\s+\(([1-5][0-9]{2})\)`)
	files := []string{"internal/httpapi/discovery.go", "internal/web/quickstart.md.tmpl", "clients/mcp/README.md"}
	for _, glob := range []string{"docs/*.md", "internal/web/templates/*.html"} {
		matches, err := filepath.Glob("../../" + glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			files = append(files, strings.TrimPrefix(m, "../../"))
		}
	}
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile("../../" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range named.FindAllStringSubmatch(string(raw), -1) {
			code := m[1] + " " + m[2]
			if m[3] != "" {
				code = m[4] + " " + m[3]
			}
			checked++
			if !known[code] {
				t.Errorf("%s names error %q, which the server never answers with that status", file, code)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("checked only %d error names; the extractor is broken", checked)
	}
}
