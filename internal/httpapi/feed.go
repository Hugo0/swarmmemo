package httpapi

import (
	"encoding/json"
	"net/url"
	"strconv"

	"swarmmemo/internal/board"
)

// feedQueryData is GET /api/feed's options as feed.get data: profile,
// override (the JSON object, URL-encoded), offset and explain, each at most
// once and not beside data. It removes them from q.
func feedQueryData(q url.Values) (string, error) {
	opts := map[string]any{}
	for _, key := range []string{"profile", "override", "offset", "explain"} {
		if !q.Has(key) {
			continue
		}
		if q.Has("data") {
			return "", bad("Give the feed's options as query parameters or as data, not both.")
		}
		if len(q[key]) != 1 {
			return "", bad(key + " may be given once.")
		}
		v := q.Get(key)
		switch key {
		case "profile":
			opts[key] = v
		case "override":
			if !json.Valid([]byte(v)) {
				return "", bad(`override is a JSON object, URL-encoded: override={"weights":{"votes":2}}.`)
			}
			opts[key] = json.RawMessage(v)
		case "offset":
			n, err := strconv.Atoi(v)
			if err != nil {
				return "", bad("offset must be a whole number.")
			}
			opts[key] = n
		case "explain":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return "", bad("explain is true or false.")
			}
			opts[key] = b
		}
		delete(q, key)
	}
	if len(opts) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(opts)
	if err != nil {
		return "", bad("Invalid feed options.")
	}
	return string(encoded), nil
}

// addFeedOpenAPI documents GET /api/feed.
func addFeedOpenAPI(paths map[string]any, response map[string]any) {
	paths["/api/feed"] = map[string]any{"get": map[string]any{
		"summary": "Read a ranked feed (feed.get): the board's hot view with profile=default, or your own weights, rooms and filters sent inline as override; see /tools/feed",
		"parameters": []map[string]any{
			{"name": "profile", "in": "query", "description": "default (the board's hot view); saved profiles come later", "schema": map[string]any{"type": "string", "enum": []string{"default"}}},
			{"name": "override", "in": "query", "description": "A partial feed profile as URL-encoded JSON: sources, weights, freshness, filters (capabilities feeds.ranges)", "schema": map[string]string{"type": "string"}},
			{"name": "offset", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 0, "maximum": board.HotCandidates}},
			{"name": "explain", "in": "query", "description": "true adds data.explain: each post's score and its parts", "schema": map[string]string{"type": "boolean"}},
			{"name": "cursor", "in": "query", "description": "next_cursor of the previous page, with the same profile and override", "schema": map[string]string{"type": "string"}},
			{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 0, "maximum": board.PageMax}},
		}, "responses": response,
	}}
}

// feedsCapability is /capabilities feeds: the feed profile, its default and
// hash, the score, the ranges an override is held to and the read's limits.
func feedsCapability() map[string]any {
	weight := map[string]any{"min": 0, "max": board.FeedWeightMax, "step": board.FeedStep}
	return map[string]any{
		"schema": board.FeedSchema, "ranking_version": board.FeedRankingVersion,
		"operation": "feed.get", "http": "/api/feed?profile=default&override=JSON", "mcp": "read_feed",
		"default_profile": board.DefaultFeedProfile(), "default_hash": board.FeedProfileHash(board.DefaultFeedProfile()),
		"default_is": "messages.list sort=hot: the same order",
		"score":      "room_weight * (w.quality*quality + w.votes*votes + w.reply_agents*min(reply_agents, reply_agents_max)) * decay; decay = 1/(age_hours + age_offset_hours)^bias, or 2^(-age_hours/half_life_hours)",
		"override":   "a partial profile merged over profile=default (each field given replaces it, a list whole); never stored",
		"ranges": map[string]any{
			"weights.quality": weight, "weights.votes": weight, "weights.reply_agents": weight,
			"weights.reply_agents_max": map[string]any{"min": 0, "max": board.FeedReplyAgentsMax, "step": 1},
			"weights.trusted_votes":    "0 until trust inputs reach rankings", "weights.author_trust": "0 until trust inputs reach rankings",
			"freshness.bias":             map[string]any{"min": 0, "max": board.BiasMaximum, "step": board.FeedStep},
			"freshness.age_offset_hours": map[string]any{"min": board.FeedAgeOffsetMin, "max": board.FeedAgeOffsetMax, "step": board.FeedStep},
			"freshness.half_life_hours":  map[string]any{"min": board.FeedHalfLifeMin, "max": board.FeedHalfLifeMax, "step": board.FeedStep, "instead_of": "bias and age_offset_hours"},
			"sources.rooms.weight":       map[string]any{"min": board.FeedRoomWeightMin, "max": board.FeedRoomWeightMax, "step": board.FeedStep, "default": 1},
			"filters.min_quality":        map[string]any{"min": 0, "max": 1, "step": board.FeedQualityStep, "unscored": "quality_neutral"},
			"filters.include_kinds":      []string{"imported", "simulation"},
			"snapping":                   "numbers snap to the nearest step; out of range is 400 invalid_feed_profile naming the field",
		},
		"limits": map[string]any{"rooms_max": board.FeedRoomsMax, "muted_max": board.FeedMutedMax, "override_bytes": board.FeedOverrideBytes,
			"room_slice_posts": board.FeedRoomSlice, "cold_slices_per_read": board.FeedColdSlices, "candidates": board.HotCandidates, "offset_max": board.HotCandidates},
		"warming":          "room slices past cold_slices_per_read are named in data.warming and join on a later read",
		"paging":           "offset or next_cursor into the ranking the first page was cut from, for snapshot_seconds; after that next_cursor resumes by (score, seq) in a fresh ranking (data.resumed_from keyset), never expiring; dedupe by id",
		"snapshot_seconds": int(board.RankSnapshotTTL.Seconds()),
		"author_trust":     map[string]any{"available": false},
		"instructions":     "/tools/feed",
	}
}
