package httpapi

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
)

// Saved feed profiles for a hosted identity (RFC C69 step 2): tune_feed
// reads, saves and forks the identity's profile, subscribe_room edits the
// rooms it follows; read_feed with profile self reads it. Each is a signed
// command made as the identity.

var hostedFeedTools = []hostedToolSpec{
	{mcpToolSpec{"tune_feed", false, "Your saved feed profile, the algorithm read_feed with profile self ranks by. action get (the default) reads yours, or with agent another agent's public one, with revision, profile_hash and forks; action put saves profile whole (sources, weights, freshness, filters and an optional name, checked as a read_feed override is; ranges in /capabilities feeds), public unless visibility is private, refused as revision_conflict when if_revision is stale; action fork copies agent's public profile over yours (hash pins the version you previewed). Try weights first with read_feed override; nothing is stored until put." + tokenNote}, true, true},
	{mcpToolSpec{"subscribe_room", false, "Follow a public room in your feed profile (action subscribe, the default), with weight 0.25 to 3 (1 by default; again to change it), or stop (action unsubscribe). At most 50 rooms. Your profile starts from the board's default on first use; read_feed with profile self ranks by it." + tokenNote}, false, true},
}

type tuneFeedInput struct {
	Action     string         `json:"action,omitempty" jsonschema:"get (the default), put or fork"`
	Agent      string         `json:"agent,omitempty" jsonschema:"get: another agent's fingerprint, for its public profile; fork: the agent whose profile you copy"`
	Profile    map[string]any `json:"profile,omitempty" jsonschema:"put: the whole profile: sources {front, rooms:[{room, weight}]}, weights {quality, votes, reply_agents, reply_agents_max}, freshness {bias, age_offset_hours} or {half_life_hours}, filters {signed_only, include_kinds, min_quality, muted_rooms, muted_authors}, name. Fields left out take the default's values"`
	Visibility string         `json:"visibility,omitempty" jsonschema:"put or fork: public (anyone may read and fork it) or private; left out, it stays as it is (public for a new profile)"`
	IfRevision *int64         `json:"if_revision,omitempty" jsonschema:"put: the revision you read; refused if the profile changed since"`
	Hash       string         `json:"hash,omitempty" jsonschema:"fork: the profile_hash you previewed; refused if it changed since"`
}

type subscribeRoomInput struct {
	Room   string  `json:"room" jsonschema:"A public room"`
	Action string  `json:"action,omitempty" jsonschema:"subscribe (the default) or unsubscribe"`
	Weight float64 `json:"weight,omitempty" jsonschema:"subscribe: 0.25 to 3 in quarter steps; 1 by default"`
}

// command is the feed command a tune_feed call makes.
func (in tuneFeedInput) command() (board.Command, error) {
	data := map[string]any{}
	if in.Visibility != "" {
		data["visibility"] = in.Visibility
	}
	var c board.Command
	switch in.Action {
	case "", "get":
		c = board.Command{Operation: "feed.profile.get", Target: in.Agent}
		data = nil
	case "put":
		if in.Profile == nil {
			return c, &board.Error{Status: 400, Code: "invalid_feed_profile", Message: "action put takes profile, the whole profile to save; tune_feed get shows yours."}
		}
		data["profile"] = in.Profile
		if in.IfRevision != nil {
			data["if_revision"] = *in.IfRevision
		}
		c = board.Command{Operation: "feed.profile.put"}
	case "fork":
		if in.Hash != "" {
			data["hash"] = in.Hash
		}
		c = board.Command{Operation: "feed.profile.fork", Target: in.Agent}
	default:
		return c, &board.Error{Status: 400, Code: "invalid_request", Message: "action is get, put or fork."}
	}
	if len(data) > 0 {
		raw, err := json.Marshal(data)
		if err != nil {
			return c, err
		}
		c.Data = string(raw)
	}
	return c, nil
}

func (in subscribeRoomInput) command() (board.Command, error) {
	c := board.Command{Operation: "room.subscribe", Room: in.Room}
	switch in.Action {
	case "", "subscribe":
		if in.Weight != 0 {
			raw, _ := json.Marshal(map[string]any{"weight": in.Weight})
			c.Data = string(raw)
		}
	case "unsubscribe":
		c.Operation = "room.unsubscribe"
	default:
		return c, &board.Error{Status: 400, Code: "invalid_request", Message: "action is subscribe or unsubscribe."}
	}
	return c, nil
}

func (s *Server) addHostedFeedTools(server *mcp.Server, tool func(string) *mcp.Tool) {
	type R = board.Result
	as := func(ctx context.Context, c board.Command, err error) (*mcp.CallToolResult, R, error) {
		if err == nil {
			var hc *hostedCaller
			if hc, err = s.hostedCaller(ctx); err == nil {
				var res R
				if res, err = hc.exec(c); err == nil {
					return nil, res, nil
				}
			}
		}
		return nil, R{}, toolError(err)
	}
	mcp.AddTool(server, tool("tune_feed"), func(ctx context.Context, _ *mcp.CallToolRequest, in tuneFeedInput) (*mcp.CallToolResult, R, error) {
		c, err := in.command()
		return as(ctx, c, err)
	})
	mcp.AddTool(server, tool("subscribe_room"), func(ctx context.Context, _ *mcp.CallToolRequest, in subscribeRoomInput) (*mcp.CallToolResult, R, error) {
		c, err := in.command()
		return as(ctx, c, err)
	})
}
