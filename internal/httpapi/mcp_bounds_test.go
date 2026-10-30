package httpapi

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services"
	"swarmmemo/internal/web"
)

// mcpSchemaBounds is every numeric bound an MCP input schema may carry, by
// TOOL.argument.keyword, each the constant the server enforces it with and
// where. A bound in a schema that is not here, or a row no schema carries,
// fails: a schema cannot state a number the code does not enforce.
func mcpSchemaBounds() map[string]float64 {
	return map[string]float64{
		// readLimitRange (board/readlimits.go) refuses past PageMax.
		"read_messages.limit.maximum":    board.PageMax,
		"read_feed.limit.maximum":        board.PageMax,
		"read_thread.limit.maximum":      board.PageMax,
		"list_pages.limit.maximum":       board.PageMax,
		"read_agent_posts.limit.maximum": board.PageMax,
		// readWork (board/work.go) and agents.list (board/identity.go).
		"read_work_history.limit.maximum": board.DirectoryPageMax,
		"find_agents.limit.maximum":       board.DirectoryPageMax,
		"find_work.limit.maximum":         board.DirectoryPageMax,
		// conversations.list (board/conversation_model.go).
		"list_conversations.limit.maximum": board.ConversationListMax,
		// offering.list and offering.calls (board/offerings.go, offering_calls.go).
		"find_offerings.limit.maximum": board.OfferingCallsPageMax,
		"offering_calls.limit.maximum": board.OfferingCallsPageMax,
		// find (board/find.go).
		"find.limit.maximum": find.PageMax,
		// updates.get data wait (board/updates.go).
		"read_updates.wait.maximum": board.UpdatesWaitMax,
		// work.claim ttl (board/work.go), room.invite.create ttl (board/invites.go).
		"claim_work.ttl.maximum":    float64(board.WorkClaimTTLMax),
		"create_invite.ttl.maximum": float64(board.InviteTTLMax),
		// standing.challenge pow bits (board/standingroots.go).
		"raise_standing.bits.maximum":       board.StandingPowBitsMax,
		"raise_standing.solution.maxLength": board.StandingPowSolutionMax,
		// app.register and app.house.create (board/apps.go, board/hosted.go).
		"manage_app.name.maxLength":     board.AppNameMaxChars,
		"manage_app.homepage.maxLength": board.AppHomepageBytes,
		"manage_app.handle.maxLength":   board.HandleMaxChars,
		// room.subscribe weight (board/feedprofile.go).
		"subscribe_room.weight.maximum": board.FeedRoomWeightMax,
		// work.accept rating (board/workreputation.go) and checks (board/checks.go).
		"accept_work.rating.minimum":   0,
		"accept_work.rating.maximum":   board.WorkRatingMax,
		"accept_work.checks.maxItems":  board.VerdictChecksMax,
		"reject_work.checks.maxItems":  board.VerdictChecksMax,
		"reject_work.reason.maxLength": board.ReasonBytes,
		// updates.dispose and updates.mark_read (board/inbox_dispose.go, inbox_unread.go).
		"dispose_updates.ids.maxItems":   board.InboxDisposeMax,
		"mark_updates_read.ids.maxItems": board.InboxMarkReadMax,
		// review evidence (reviews.MaxEvidence).
		"write_review.evidence.maxItems": reviews.MaxEvidence,
		// handles (board.ValidHandle).
		"post_message.handle.maxLength":    board.HandleMaxChars,
		"create_identity.handle.maxLength": board.HandleMaxChars,
		"set_handle.handle.maxLength":      board.HandleMaxChars,
		// offering.decline reason (board/offering_calls.go).
		"decline_offering_call.reason.maxLength": board.OfferingDeclineReasonBytes,
		// journal.suspend text (board/journal.go), hosted token label
		// (board/hosted.go), feedback text (board/funnel_feedback.go).
		"journal_suspend.text.maxLength": board.JournalSuspendBytes,
		"manage_tokens.label.maxLength":  board.HostedTokenLabelBytes,
		"send_feedback.text.maxLength":   board.FeedbackTextBytes,
	}
}

// anonymousCallBounds are the bounds every anonymous service call tool's
// max_cost and request_id carry (callSchema; anonymousCallCommand and the
// store enforce them).
var anonymousCallBounds = map[string]float64{
	services.CallFieldMaxCost + ".minimum":     0,
	services.CallFieldRequestID + ".minLength": services.AnonymousRequestIDMin,
	services.CallFieldRequestID + ".maxLength": board.RequestIDBytes,
}

// hostedCallBounds are a hosted call tool's (hostedCallSchema): its
// request_id is signed, so any non-empty one up to RequestIDBytes.
var hostedCallBounds = map[string]float64{
	services.CallFieldMaxCost + ".minimum":     0,
	services.CallFieldRequestID + ".minLength": 1,
	services.CallFieldRequestID + ".maxLength": board.RequestIDBytes,
}

var schemaBoundKeywords = []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "minItems", "maxItems"}

// schemaBounds collects every bound keyword under props, by path.
func schemaBounds(prefix string, props map[string]any, out map[string]float64) {
	for name, raw := range props {
		p, _ := raw.(map[string]any)
		path := prefix + name
		for _, k := range schemaBoundKeywords {
			if v, ok := p[k].(float64); ok {
				out[path+"."+k] = v
			}
		}
		if nested, ok := p["properties"].(map[string]any); ok {
			schemaBounds(path+".", nested, out)
		}
		if items, ok := p["items"].(map[string]any); ok {
			if nested, ok := items["properties"].(map[string]any); ok {
				schemaBounds(path+"[].", nested, out)
			}
		}
	}
}

// Every numeric bound in a listed MCP input schema is the constant the
// server enforces, and every bound the code declares is listed.
func TestMCPSchemaBoundsAreTheEnforcedConstants(t *testing.T) {
	// pinServer lists the widest profiles; the hosted server adds the tools
	// behind flags pinServer leaves off (inbox entries, reviews).
	_, hosted := hostedServerWith(t, board.Features{InboxEntries: board.InboxRead, Reviews: true})
	want := mcpSchemaBounds()
	got := map[string]float64{}
	for _, s := range []*Server{pinServer(t), hosted} {
		for _, path := range []string{"/mcp", mcpProfileCore, web.AssistantMCPPath} {
			_, tools := toolsListAnswer(t, s, path)
			for _, tool := range tools {
				name, _ := tool["name"].(string)
				props, _ := dig(tool, "inputSchema", "properties").(map[string]any)
				bounds := map[string]float64{}
				schemaBounds("", props, bounds)
				calls := anonymousCallBounds
				if rid, _ := props[services.CallFieldRequestID].(map[string]any); rid != nil && rid["description"] == hostedRequestIDNote {
					calls = hostedCallBounds
				}
				_, isCall := props[services.CallFieldMaxCost]
				for k, v := range bounds {
					if w, ok := calls[k]; ok && isCall {
						if v != w {
							t.Errorf("%s %s.%s is %v; the code enforces %v", path, name, k, v, w)
						}
						continue
					}
					got[name+"."+k] = v
				}
			}
		}
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, ok := want[k]
		switch {
		case !ok:
			t.Errorf("tools/list has %s = %v, which mcpSchemaBounds does not list: add it with the constant that enforces it", k, got[k])
		case got[k] != w:
			t.Errorf("tools/list has %s = %v; the code enforces %v", k, got[k], w)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("mcpSchemaBounds lists %s, which no tools/list schema carries", k)
		}
	}
	// The table the server reads and this one name the same arguments.
	declared := map[string]bool{}
	for key := range mcpArgBounds {
		declared[key] = true
	}
	listed := map[string]bool{}
	for k := range want {
		listed[k[:strings.LastIndex(k, ".")]] = true
	}
	if !reflect.DeepEqual(declared, listed) {
		t.Errorf("mcpArgBounds names %v; mcpSchemaBounds %v", declared, listed)
	}
}

// A tag that no longer states a number is outside TestMCPArgumentTagsMatchTheCode;
// the evidence kinds it names still come from reviews.EvidenceKinds.
func TestMCPEvidenceTagNamesTheKinds(t *testing.T) {
	tags := map[string]string{}
	f := reflect.TypeOf(writeReviewInput{})
	for i := range f.NumField() {
		tags[f.Field(i).Name] = f.Field(i).Tag.Get("jsonschema")
	}
	// It may go on to say which call: ids count (C495: an offering call's oc_ID).
	if want := "your own " + evidencePrefixes() + " ids"; tags["Evidence"] != want && !strings.HasPrefix(tags["Evidence"], want+" (") {
		t.Errorf("writeReviewInput.Evidence says %q; the kinds are %q", tags["Evidence"], want)
	}
}
