package httpapi

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmmemo/internal/board"
	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
)

// mcpBound is one MCP tool argument's numeric bounds in its input schema,
// read from the constants the server enforces them with (C295-3). A client
// sees the bound before it calls; the board refuses past it.
type mcpBound struct {
	Minimum, Maximum    *float64
	MaxLength, MaxItems *int
}

func boundMax[N int | int64 | float64](n N) mcpBound {
	v := float64(n)
	return mcpBound{Maximum: &v}
}

func boundRange[N int | int64 | float64](lo, hi N) mcpBound {
	a, b := float64(lo), float64(hi)
	return mcpBound{Minimum: &a, Maximum: &b}
}

func boundLength(n int) mcpBound { return mcpBound{MaxLength: &n} }

func boundItems(n int) mcpBound { return mcpBound{MaxItems: &n} }

// mcpArgBounds is every bounded argument, by TOOL.argument. Only bounds the
// server refuses past are listed: a read that clamps its limit (updates.get,
// journal.get, the review reads) keeps serving a page, so its schema says
// nothing a client could be refused for. A limit's zero (left out) means the
// default, so limits carry a maximum only.
var mcpArgBounds = map[string]mcpBound{
	"read_messages.limit":          boundMax(board.PageMax),
	"read_feed.limit":              boundMax(board.PageMax),
	"read_thread.limit":            boundMax(board.PageMax),
	"list_pages.limit":             boundMax(board.PageMax),
	"read_agent_posts.limit":       boundMax(board.PageMax),
	"read_work_history.limit":      boundMax(board.DirectoryPageMax),
	"find_agents.limit":            boundMax(board.DirectoryPageMax),
	"find_work.limit":              boundMax(board.DirectoryPageMax),
	"list_conversations.limit":     boundMax(board.ConversationListMax),
	"find_offerings.limit":         boundMax(board.OfferingCallsPageMax),
	"offering_calls.limit":         boundMax(board.OfferingCallsPageMax),
	"find.limit":                   boundMax(find.PageMax),
	"read_updates.wait":            boundMax(board.UpdatesWaitMax),
	"claim_work.ttl":               boundMax(board.WorkClaimTTLMax),
	"create_invite.ttl":            boundMax(board.InviteTTLMax),
	"raise_standing.bits":          boundMax(board.StandingPowBitsMax),
	"subscribe_room.weight":        boundMax(board.FeedRoomWeightMax),
	"accept_work.rating":           boundRange(0, board.WorkRatingMax),
	"accept_work.checks":           boundItems(board.VerdictChecksMax),
	"reject_work.checks":           boundItems(board.VerdictChecksMax),
	"dispose_updates.ids":          boundItems(board.InboxDisposeMax),
	"mark_updates_read.ids":        boundItems(board.InboxMarkReadMax),
	"write_review.evidence":        boundItems(reviews.MaxEvidence),
	"post_message.handle":          boundLength(board.HandleMaxChars),
	"create_identity.handle":       boundLength(board.HandleMaxChars),
	"set_handle.handle":            boundLength(board.HandleMaxChars),
	"reject_work.reason":           boundLength(board.ReasonBytes),
	"decline_offering_call.reason": boundLength(board.OfferingDeclineReasonBytes),
	"journal_suspend.text":         boundLength(board.JournalSuspendBytes),
	"manage_tokens.label":          boundLength(board.HostedTokenLabelBytes),
	"manage_app.handle":            boundLength(board.HandleMaxChars),
	"manage_app.name":              boundLength(board.AppNameMaxChars),
	"manage_app.homepage":          boundLength(board.AppHomepageBytes),
	"send_feedback.text":           boundLength(board.FeedbackTextBytes),
	"raise_standing.solution":      boundLength(board.StandingPowSolutionMax),
}

// boundToolSchemas adds mcpArgBounds to the input schemas tools/list
// answers. The SDK keeps validating calls against the inferred schemas, so a
// call past a bound still reaches the board and gets its refusal, with the
// error code an agent branches on ({"ok":false,"error":{...}}), not a bare
// schema-validation error; the listed bound tells a client the same number
// before it calls. TestMCPSchemaBoundsAreTheEnforcedConstants holds every
// row to a listed argument and to the constant the board enforces.
func boundToolSchemas() mcp.Middleware {
	byTool := map[string]map[string]mcpBound{}
	for key, b := range mcpArgBounds {
		tool, arg, _ := strings.Cut(key, ".")
		if byTool[tool] == nil {
			byTool[tool] = map[string]mcpBound{}
		}
		byTool[tool][arg] = b
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			r, ok := res.(*mcp.ListToolsResult)
			if !ok || err != nil {
				return res, err
			}
			tools := slices.Clone(r.Tools)
			for i, t := range tools {
				bounds := byTool[t.Name]
				schema, ok := t.InputSchema.(*jsonschema.Schema)
				if len(bounds) == 0 || !ok {
					continue
				}
				// The registered schema is the SDK's, shared by every call: copy it.
				schema = schema.CloneSchemas()
				for arg, b := range bounds {
					if p := schema.Properties[arg]; p != nil {
						p.Minimum, p.Maximum, p.MaxLength, p.MaxItems = b.Minimum, b.Maximum, b.MaxLength, b.MaxItems
					}
				}
				copied := *t
				copied.InputSchema = schema
				tools[i] = &copied
			}
			r.Tools = tools
			return r, nil
		}
	}
}

// fmtFloat is a float constant as copy states it: 0.5, 1.5, 3.
func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
