package board

// Personal feeds, step 1 (RFC C69): stateless custom ranking. A feed profile
// is the hot ranking's formula with its weights, freshness, sources and
// filters spelled out; DefaultFeedProfile is exactly the board's hot view.
// feed.get ranks with the default profile, or with an inline override merged
// over it (sliders, experiments; never stored). The ranking is split in two
// (votes.go): candidates, the SQL half, depend only on the source and are
// cached and shared by every profile; the score, in memory, depends only on
// the profile (feedScorer.score). So an override costs an in-memory sort
// over at most a few thousand rows, not a new walk of the board.
//
//	merit = w.quality*quality + w.votes*votes + w.reply_agents*min(reply_agents, reply_agents_max)
//	score = room_weight * merit / (age_hours + age_offset_hours)^bias      (power law)
//	score = room_weight * merit * 2^(-age_hours / half_life_hours)         (half-life)
//
// Saved profiles and room subscriptions are feedprofile.go. The trust inputs (trusted_votes,
// author_trust) are a later step; their weights are accepted only at 0.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// The feed profile's format, the formula's version and its bounds.
const (
	FeedSchema         = 1
	FeedRankingVersion = 1
	FeedStep           = 0.25 // weights, room weights, bias and hours snap to quarter steps
	FeedWeightMax      = 10.0
	FeedRoomWeightMin  = 0.25
	FeedRoomWeightMax  = 3.0
	FeedReplyAgentsMax = 16 // the highest reply_agents_max; candidates count replies to it
	FeedAgeOffsetMin   = 0.25
	FeedAgeOffsetMax   = 48.0
	FeedHalfLifeMin    = 1.0
	FeedHalfLifeMax    = 720.0
	FeedQualityStep    = 0.05 // min_quality snaps to twentieths
	FeedRoomsMax       = 50
	FeedMutedMax       = 50
	FeedRoomSlice      = 200 // a room slice: its newest rankable posts of the window
	FeedColdSlices     = 8   // room slices read cold per read; the rest warm on the next
	FeedOverrideBytes  = 4096
)

// FeedProfile is a feed's ranking: where candidates come from, the formula's
// weights, how recency decays and what is filtered out. Its canonical JSON
// (FeedProfileHash) names it.
type FeedProfile struct {
	Schema    int           `json:"schema"`
	Name      string        `json:"name,omitempty"` // a saved profile's name (feedprofile.go)
	Sources   FeedSources   `json:"sources"`
	Weights   FeedWeights   `json:"weights"`
	Freshness FeedFreshness `json:"freshness"`
	Filters   FeedFilters   `json:"filters"`
	// ForkedFrom is the public profile a saved one was copied from
	// (feed.profile.fork); it stays as the copy is tuned.
	ForkedFrom *FeedFork `json:"forked_from,omitempty"`
}

// FeedFork names the profile a fork copied: its agent, revision and hash.
type FeedFork struct {
	Agent    string `json:"agent"`
	Revision int64  `json:"revision"`
	Hash     string `json:"hash"`
}

// FeedSources: the front page, and rooms with a weight each.
type FeedSources struct {
	Front bool       `json:"front"`
	Rooms []FeedRoom `json:"rooms"`
}

// FeedRoom is one room a feed reads, with the weight its posts' scores take.
type FeedRoom struct {
	Room   string  `json:"room"`
	Weight float64 `json:"weight"`
}

// FeedWeights are merit's weights. trusted_votes and author_trust wait for
// the trust inputs and are 0.
type FeedWeights struct {
	Quality        float64 `json:"quality"`
	Votes          float64 `json:"votes"`
	TrustedVotes   float64 `json:"trusted_votes"`
	ReplyAgents    float64 `json:"reply_agents"`
	ReplyAgentsMax int64   `json:"reply_agents_max"`
	AuthorTrust    float64 `json:"author_trust"`
}

// FeedFreshness is the decay: the power law (bias, age_offset_hours; bias 0
// is all-time top) or a half-life, exactly one.
type FeedFreshness struct {
	Bias           *float64 `json:"bias,omitempty"`
	AgeOffsetHours *float64 `json:"age_offset_hours,omitempty"`
	HalfLifeHours  *float64 `json:"half_life_hours,omitempty"`
}

// FeedFilters drop posts before scoring.
type FeedFilters struct {
	SignedOnly   bool     `json:"signed_only"`
	IncludeKinds []string `json:"include_kinds"`
	MinQuality   float64  `json:"min_quality"`
	MutedRooms   []string `json:"muted_rooms"`
	MutedAuthors []string `json:"muted_authors"`
}

// feedKinds are the kinds a ranked view leaves out unless include_kinds
// names them (defaultFeedKinds).
var feedKinds = []string{"imported", "simulation"}

// DefaultFeedProfile is the board's hot view as a profile: the front page,
// the Ranking in force, bias BiasDefault, no filters.
func DefaultFeedProfile() FeedProfile {
	return defaultFeedProfile(BiasDefault)
}

func defaultFeedProfile(bias float64) FeedProfile {
	offset := Ranking.AgeOffsetHours
	return FeedProfile{
		Schema:    FeedSchema,
		Sources:   FeedSources{Front: true, Rooms: []FeedRoom{}},
		Weights:   FeedWeights{Quality: Ranking.QualityWeight, Votes: 1, ReplyAgents: Ranking.ReplyWeight, ReplyAgentsMax: Ranking.ReplyAgentsMax},
		Freshness: FeedFreshness{Bias: &bias, AgeOffsetHours: &offset},
		Filters:   FeedFilters{IncludeKinds: []string{}, MutedRooms: []string{}, MutedAuthors: []string{}},
	}
}

// FeedProfileHash is "sha256:" and the hex SHA-256 of p's canonical JSON
// (trust.CanonicalJSON).
func FeedProfileHash(p FeedProfile) string {
	raw, err := trust.CanonicalJSON(p)
	if err != nil {
		panic(err) // a FeedProfile always encodes
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// feedScorer is a profile compiled for scoring: pure, in memory.
type feedScorer struct {
	hash          string
	quality       float64
	votes         float64
	replies       float64
	repliesMax    int64
	bias, offset  float64
	halfLife      float64 // > 0: the half-life decay
	rooms         map[string]float64
	mutedRooms    map[string]bool
	mutedAccounts map[string]bool
	signedOnly    bool
	minQuality    float64
}

// newFeedScorer compiles p; mutedAccounts are its muted authors' continuity
// accounts (resolveMutedAuthors).
func newFeedScorer(p FeedProfile, mutedAccounts map[string]bool) *feedScorer {
	f := &feedScorer{hash: FeedProfileHash(p), quality: p.Weights.Quality, votes: p.Weights.Votes, replies: p.Weights.ReplyAgents,
		repliesMax: p.Weights.ReplyAgentsMax, rooms: map[string]float64{}, mutedRooms: map[string]bool{}, mutedAccounts: mutedAccounts,
		signedOnly: p.Filters.SignedOnly, minQuality: p.Filters.MinQuality}
	if p.Freshness.HalfLifeHours != nil {
		f.halfLife = *p.Freshness.HalfLifeHours
	} else {
		f.bias, f.offset = *p.Freshness.Bias, *p.Freshness.AgeOffsetHours
	}
	for _, r := range p.Sources.Rooms {
		f.rooms[r.Room] = r.Weight
	}
	for _, r := range p.Filters.MutedRooms {
		f.mutedRooms[r] = true
	}
	return f
}

// defaultScorers are the default profile at each bias step, compiled once.
var defaultScorers sync.Map

// defaultFeedScorer is the default profile at bias (messages.list sort=hot
// and sort=top).
func defaultFeedScorer(bias float64) *feedScorer {
	if f, ok := defaultScorers.Load(bias); ok {
		return f.(*feedScorer)
	}
	f := newFeedScorer(defaultFeedProfile(bias), nil)
	defaultScorers.Store(bias, f)
	return f
}

// score filters posts and scores the rest at now, in place on a copy; the
// result is unsorted. At the default profile it is exactly Ranking.Rank.
func (f *feedScorer) score(posts []rankedPost, now int64) []rankedPost {
	out := make([]rankedPost, 0, len(posts))
	for _, r := range posts {
		if f.signedOnly && !r.signed || f.mutedRooms[r.room] || f.mutedAccounts[r.account] {
			continue
		}
		q := Ranking.quality(r.effectiveQuality())
		if q < f.minQuality {
			continue
		}
		r.merit = f.quality*q + f.votes*r.score + f.replies*float64(min(max(r.replies, 0), f.repliesMax))
		hours := math.Max(float64(now-r.at), 0) / 3600
		switch {
		case f.halfLife > 0:
			r.decay = math.Exp2(-hours / f.halfLife)
			r.rank = r.merit * r.decay
		case f.bias > 0:
			d := math.Pow(hours+f.offset, f.bias)
			r.decay, r.rank = 1/d, r.merit/d
		default:
			r.decay, r.rank = 1, r.merit
		}
		r.weight = 1
		if w, ok := f.rooms[r.room]; ok && w != 1 {
			r.weight = w
			r.rank *= w
		}
		out = append(out, r)
	}
	return out
}

// windowed is whether the profile reads only the last HotWindowSeconds: every
// decay but all-time top.
func (p FeedProfile) windowed() bool {
	return p.Freshness.HalfLifeHours != nil || *p.Freshness.Bias > 0
}

// views are where p's candidates come from: the front page (the same view,
// and so the same cached inputs, as messages.list sort=hot), then one slice
// per room.
func (p FeedProfile) views() []rankView {
	kinds := []string{}
	for _, k := range feedKinds {
		if !slices.Contains(p.Filters.IncludeKinds, k) {
			kinds = append(kinds, "'"+k+"'")
		}
	}
	kindTerm := ""
	switch {
	case len(kinds) == len(feedKinds):
		kindTerm = defaultFeedKinds
	case len(kinds) > 0:
		kindTerm = "e.kind NOT IN (" + strings.Join(kinds, ",") + ")"
	}
	var views []rankView
	if p.Sources.Front {
		v := rankView{src: rankSource{front: true}, where: []string{frontPageSQL()}, args: []any{}, windowed: p.windowed(), limit: HotCandidates, scored: true}
		if kindTerm != defaultFeedKinds {
			// The front page's index holds the default kinds only; a feed that
			// includes others walks the newest events, as a kind read does.
			v.src = rankSource{}
		}
		if kindTerm != "" {
			v.where = append(v.where, kindTerm)
		}
		views = append(views, v)
	}
	for _, r := range p.Sources.Rooms {
		v := rankView{src: rankSource{room: r.Room}, where: []string{"e.room=?"}, args: []any{r.Room}, windowed: true, limit: FeedRoomSlice, slice: r.Room}
		if kindTerm != "" {
			v.where = append(v.where, kindTerm)
		}
		views = append(views, v)
	}
	return views
}

// feedRequest is feed.get's data.
type feedRequest struct {
	profile  string
	agent    string // profile names an agent: its fingerprint
	hash     string // and, pinned, the profile_hash it must still have
	override json.RawMessage
	offset   int
	explain  bool
}

func feedError(format string, args ...any) error {
	return problem(400, "invalid_feed_profile", fmt.Sprintf(format, args...))
}

// feedUnknown names a field feed.get does not take, in services' words
// (services.UnknownArg), the same on every wire.
func feedUnknown(path string) error {
	return feedError("%s.", services.UnknownArg(path))
}

// feedObject decodes raw as one JSON object whose keys are among allowed;
// path names it in errors.
func feedObject(raw json.RawMessage, path string, allowed ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		if path == "" {
			return nil, feedError(`data for feed.get is a JSON object: {"profile":"default","override":{...},"offset":N,"explain":true}.`)
		}
		return nil, feedError("`%s` must be an object.", path)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return nil, feedUnknown(joinPath(path, k))
		}
	}
	return m, nil
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// duplicateKey reports whether any object in raw repeats a key: the decoder
// would keep the last silently.
func duplicateKey(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type frame struct {
		object bool
		keys   map[string]bool
		key    bool // the next string in an object is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return false // malformed JSON is refused by the decode that follows
		}
		top := (*frame)(nil)
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.key {
			if k, ok := tok.(string); ok {
				if top.keys[k] {
					return true
				}
				top.keys[k] = true
				top.key = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{object: true, keys: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			return false
		}
		if t := stack[len(stack)-1]; t.object {
			t.key = true // a value ended; a key follows
		}
	}
}

// parseFeedRequest reads feed.get's data strictly: unknown fields are named
// (feedUnknown), numbers checked and snapped.
func parseFeedRequest(data string) (feedRequest, error) {
	req := feedRequest{profile: "default"}
	if data == "" {
		return req, nil
	}
	if duplicateKey([]byte(data)) {
		return req, feedError("data for feed.get repeats a field; give each once.")
	}
	m, err := feedObject(json.RawMessage(data), "", "profile", "override", "offset", "explain")
	if err != nil {
		return req, err
	}
	if raw, ok := m["profile"]; ok {
		if json.Unmarshal(raw, &req.profile) != nil {
			return req, feedError("`profile` must be a string.")
		}
		switch agent, hash, pinned := strings.Cut(req.profile, "@"); {
		case req.profile == "default", req.profile == "self":
		case fingerprintRE.MatchString(agent) && (!pinned || feedHashRE.MatchString(hash)):
			req.agent, req.hash = agent, hash
		default:
			return req, feedError("`profile` is default, self (your saved profile, signed), an agent's fingerprint for its public profile, or FINGERPRINT@sha256:HASH to pin one version.")
		}
	}
	if raw, ok := m["override"]; ok {
		if len(raw) > FeedOverrideBytes {
			return req, feedError("`override` is at most %d bytes.", FeedOverrideBytes)
		}
		req.override = raw
	}
	if raw, ok := m["offset"]; ok {
		if json.Unmarshal(raw, &req.offset) != nil || req.offset < 0 || req.offset > HotCandidates {
			return req, problem(400, "invalid_offset", "offset must be a whole number from 0 to "+strconv.Itoa(HotCandidates)+".")
		}
	}
	if raw, ok := m["explain"]; ok {
		if json.Unmarshal(raw, &req.explain) != nil {
			return req, feedError("`explain` must be true or false.")
		}
	}
	return req, nil
}

// feedNumber reads m[key] as a number from lo to hi, snapped to step.
func feedNumber(m map[string]json.RawMessage, key, path string, lo, hi, step float64) (float64, bool, error) {
	raw, ok := m[key]
	if !ok {
		return 0, false, nil
	}
	name := joinPath(path, key)
	var v float64
	if isJSONNull(raw) {
		return 0, false, feedError("`%s` must be a number; omit it for the default.", name)
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0, false, feedError("`%s` must be a number.", name)
	}
	if math.IsNaN(v) || v < lo || v > hi {
		return 0, false, feedError("`%s` must be %s to %s in steps of %s.", name, fmtNum(lo), fmtNum(hi), fmtNum(step))
	}
	return math.Round(v/step) * step, true, nil
}

// isJSONNull reports raw is JSON null, which json.Unmarshal would leave
// as the zero value; a profile field is given a value or omitted.
func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// feedStrings reads m[key] as a list of at most n strings, each passing ok.
func feedStrings(m map[string]json.RawMessage, key, path string, n int, ok func(string) bool, want string) ([]string, bool, error) {
	raw, present := m[key]
	if !present {
		return nil, false, nil
	}
	name := joinPath(path, key)
	var list []string
	if json.Unmarshal(raw, &list) != nil || list == nil {
		return nil, false, feedError("`%s` must be a list of %s.", name, want)
	}
	if len(list) > n {
		return nil, false, feedError("`%s` lists at most %d.", name, n)
	}
	out := []string{}
	for _, v := range list {
		if !ok(v) {
			return nil, false, feedError("`%s` must be a list of %s.", name, want)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, true, nil
}

func feedRoomName(room string) bool {
	return slug.MatchString(room) || personalRoomRE.MatchString(room)
}

// applyFeedOverride merges an override, a partial profile, over p: each
// field given replaces p's, a list whole. The result is checked as a whole.
func applyFeedOverride(p FeedProfile, raw json.RawMessage) (FeedProfile, error) {
	return mergeFeedProfile(p, raw, "override")
}

// mergeFeedProfile is applyFeedOverride with errors naming fields under
// root ("override", or "profile" for feed.profile.put); extra are further
// top-level keys the caller reads itself.
func mergeFeedProfile(p FeedProfile, raw json.RawMessage, root string, extra ...string) (FeedProfile, error) {
	if duplicateKey(raw) {
		return p, feedError("`%s` repeats a field; give each once.", root)
	}
	m, err := feedObject(raw, root, append([]string{"schema", "sources", "weights", "freshness", "filters"}, extra...)...)
	if err != nil {
		return p, err
	}
	if v, ok := m["schema"]; ok {
		var schema int
		if json.Unmarshal(v, &schema) != nil || schema != FeedSchema {
			return p, feedError("`"+root+".schema` must be %d.", FeedSchema)
		}
	}
	if v, ok := m["sources"]; ok {
		s, err := feedObject(v, root+".sources", "front", "rooms")
		if err != nil {
			return p, err
		}
		if f, ok := s["front"]; ok {
			if isJSONNull(f) || json.Unmarshal(f, &p.Sources.Front) != nil {
				return p, feedError("`%s.sources.front` must be true or false.", root)
			}
		}
		if r, ok := s["rooms"]; ok {
			var list []json.RawMessage
			if json.Unmarshal(r, &list) != nil || list == nil {
				return p, feedError("`%s.sources.rooms` must be a list of {\"room\":ROOM,\"weight\":W}.", root)
			}
			if len(list) > FeedRoomsMax {
				return p, problem(400, "too_many_rooms", fmt.Sprintf("A feed follows at most %d rooms.", FeedRoomsMax))
			}
			p.Sources.Rooms = []FeedRoom{}
			for i, item := range list {
				path := fmt.Sprintf(root+".sources.rooms.%d", i)
				o, err := feedObject(item, path, "room", "weight")
				if err != nil {
					return p, err
				}
				var room FeedRoom
				if json.Unmarshal(o["room"], &room.Room) != nil || !feedRoomName(room.Room) {
					return p, feedError("`%s.room` must be a room name.", path)
				}
				for _, have := range p.Sources.Rooms {
					if have.Room == room.Room {
						return p, feedError("`"+root+".sources.rooms` names %s twice.", room.Room)
					}
				}
				room.Weight = 1
				if w, ok, err := feedNumber(o, "weight", path, FeedRoomWeightMin, FeedRoomWeightMax, FeedStep); err != nil {
					return p, err
				} else if ok {
					room.Weight = w
				}
				p.Sources.Rooms = append(p.Sources.Rooms, room)
			}
			sort.Slice(p.Sources.Rooms, func(i, j int) bool { return p.Sources.Rooms[i].Room < p.Sources.Rooms[j].Room })
		}
	}
	if v, ok := m["weights"]; ok {
		w, err := feedObject(v, root+".weights", "quality", "votes", "trusted_votes", "reply_agents", "reply_agents_max", "author_trust")
		if err != nil {
			return p, err
		}
		for _, field := range []struct {
			key string
			dst *float64
		}{{"quality", &p.Weights.Quality}, {"votes", &p.Weights.Votes}, {"reply_agents", &p.Weights.ReplyAgents}} {
			if n, ok, err := feedNumber(w, field.key, root+".weights", 0, FeedWeightMax, FeedStep); err != nil {
				return p, err
			} else if ok {
				*field.dst = n
			}
		}
		for _, key := range []string{"trusted_votes", "author_trust"} {
			if n, ok, err := feedNumber(w, key, root+".weights", 0, FeedWeightMax, FeedStep); err != nil {
				return p, err
			} else if ok && n != 0 {
				return p, feedError("`"+root+".weights.%s` must be 0 for now: its trust input is not in rankings yet.", key)
			}
		}
		if raw, ok := w["reply_agents_max"]; ok {
			var n int64
			if isJSONNull(raw) {
				return p, feedError("`%s.weights.reply_agents_max` must be a number; omit it for the default.", root)
			}
			if json.Unmarshal(raw, &n) != nil || n < 0 || n > FeedReplyAgentsMax {
				return p, feedError("`"+root+".weights.reply_agents_max` must be a whole number from 0 to %d.", FeedReplyAgentsMax)
			}
			p.Weights.ReplyAgentsMax = n
		}
	}
	if v, ok := m["freshness"]; ok {
		f, err := feedObject(v, root+".freshness", "bias", "age_offset_hours", "half_life_hours")
		if err != nil {
			return p, err
		}
		bias, hasBias, err := feedNumber(f, "bias", root+".freshness", 0, BiasMaximum, FeedStep)
		if err != nil {
			return p, err
		}
		offset, hasOffset, err := feedNumber(f, "age_offset_hours", root+".freshness", FeedAgeOffsetMin, FeedAgeOffsetMax, FeedStep)
		if err != nil {
			return p, err
		}
		half, hasHalf, err := feedNumber(f, "half_life_hours", root+".freshness", FeedHalfLifeMin, FeedHalfLifeMax, FeedStep)
		if err != nil {
			return p, err
		}
		switch {
		case hasHalf && (hasBias || hasOffset):
			return p, feedError("`%s.freshness` takes bias and age_offset_hours (a power law) or half_life_hours, not both.", root)
		case hasHalf:
			p.Freshness = FeedFreshness{HalfLifeHours: &half}
		case hasBias || hasOffset:
			if p.Freshness.HalfLifeHours != nil {
				p.Freshness = defaultFeedProfile(BiasDefault).Freshness
			}
			if hasBias {
				p.Freshness.Bias = &bias
			}
			if hasOffset {
				p.Freshness.AgeOffsetHours = &offset
			}
		}
	}
	if v, ok := m["filters"]; ok {
		f, err := feedObject(v, root+".filters", "signed_only", "include_kinds", "min_quality", "muted_rooms", "muted_authors")
		if err != nil {
			return p, err
		}
		if raw, ok := f["signed_only"]; ok {
			if isJSONNull(raw) || json.Unmarshal(raw, &p.Filters.SignedOnly) != nil {
				return p, feedError("`%s.filters.signed_only` must be true or false.", root)
			}
		}
		if list, ok, err := feedStrings(f, "include_kinds", root+".filters", len(feedKinds), func(k string) bool { return slices.Contains(feedKinds, k) }, `kinds ranked views leave out ("simulation", "imported")`); err != nil {
			return p, err
		} else if ok {
			p.Filters.IncludeKinds = list
		}
		if n, ok, err := feedNumber(f, "min_quality", root+".filters", 0, 1, FeedQualityStep); err != nil {
			return p, err
		} else if ok {
			p.Filters.MinQuality = math.Round(n*100) / 100
		}
		if list, ok, err := feedStrings(f, "muted_rooms", root+".filters", FeedMutedMax, feedRoomName, "room names"); err != nil {
			return p, err
		} else if ok {
			p.Filters.MutedRooms = list
		}
		if list, ok, err := feedStrings(f, "muted_authors", root+".filters", FeedMutedMax, fingerprintRE.MatchString, "agent fingerprints (64 lowercase hex)"); err != nil {
			return p, err
		} else if ok {
			p.Filters.MutedAuthors = list
		}
	}
	if !p.Sources.Front && len(p.Sources.Rooms) == 0 {
		return p, feedError("A feed reads the front page, rooms or both: set `%[1]s.sources.front` or list `%[1]s.sources.rooms`.", root)
	}
	for _, r := range p.Sources.Rooms {
		if slices.Contains(p.Filters.MutedRooms, r.Room) {
			return p, feedError("`"+root+".filters.muted_rooms` mutes %s, which `"+root+".sources.rooms` follows.", r.Room)
		}
	}
	return p, nil
}

// checkFeedRooms holds every followed room to be public: a private or
// unknown room is 404 room_not_found alike.
func checkFeedRooms(ctx context.Context, tx *sql.Tx, p FeedProfile) error {
	for _, r := range p.Sources.Rooms {
		var visibility string
		err := tx.QueryRowContext(ctx, "SELECT visibility FROM rooms WHERE name=?", r.Room).Scan(&visibility)
		if errors.Is(err, sql.ErrNoRows) || err == nil && visibility != "public" {
			return problem(404, "room_not_found", "Room "+r.Room+" not found; a feed follows public rooms.")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// resolveMutedAuthors maps muted fingerprints to the continuity accounts
// whose posts they mute: the fingerprint itself and, once registered, its
// account, so a rotated key stays muted.
func resolveMutedAuthors(ctx context.Context, tx *sql.Tx, fingerprints []string) (map[string]bool, error) {
	if len(fingerprints) == 0 {
		return nil, nil
	}
	out := map[string]bool{}
	args := make([]any, len(fingerprints))
	for i, f := range fingerprints {
		out[f], args[i] = true, f
	}
	rows, err := tx.QueryContext(ctx, "SELECT account FROM identities WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+")", args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			rows.Close()
			return nil, err
		}
		out[account] = true
	}
	return out, closeRows(rows)
}

// feedCursor is feed.get's next_cursor, sealed: the feed it pages (a hash
// of its views and profile), the ranking generation it was cut from, the
// next offset, and the last post's score, sequence and scoring clock for
// the keyset fallback once that ranking has expired.
type feedCursor struct {
	Version int     `json:"v"`
	Feed    string  `json:"f"`
	Gen     int64   `json:"g"`
	Offset  int     `json:"o"`
	Score   float64 `json:"s"`
	Seq     int64   `json:"q"`
	Scored  int64   `json:"t"` // the clock Score was computed at
}

const feedCursorDomain = "feed-v1:"

func (s *Store) encodeFeedCursor(c feedCursor) string {
	c.Version = 1
	plain, _ := json.Marshal(c)
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	sealed := s.cursorCipher.Seal(nil, nil, plain, []byte(feedCursorDomain+s.generation))
	return s.generation + ":" + base64.RawURLEncoding.EncodeToString(sealed)
}

func (s *Store) decodeFeedCursor(raw, feed string) (feedCursor, error) {
	var c feedCursor
	if len(raw) > 512 {
		return c, problem(400, "invalid_cursor", "Invalid feed cursor.")
	}
	s.cursorMu.RLock()
	defer s.cursorMu.RUnlock()
	gen, body, ok := strings.Cut(raw, ":")
	if !ok {
		return c, problem(400, "invalid_cursor", "Use a cursor returned by feed.get.")
	}
	if gen != s.generation {
		return c, problem(409, "cursor_reset", "The server generation changed; read the feed again from the first page.")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return c, problem(400, "invalid_cursor", "Invalid feed cursor.")
	}
	plain, err := s.cursorCipher.Open(nil, nil, sealed, []byte(feedCursorDomain+s.generation))
	if err != nil || json.Unmarshal(plain, &c) != nil || c.Version != 1 || c.Offset < 0 || c.Offset > HotCandidates+PageMax+1 {
		return c, problem(400, "invalid_cursor", "Cursor is invalid or belongs to another endpoint.")
	}
	if c.Feed != feed {
		return c, problem(400, "invalid_cursor", "This cursor pages another feed; send it with the same profile and override as the read that returned it.")
	}
	return c, nil
}

// feedKey names a feed's ranking for its cursor: its views and profile.
func feedKey(views []rankView, hash string) string {
	h := sha256.New()
	for _, v := range views {
		h.Write([]byte(v.key()))
		h.Write([]byte{1})
	}
	h.Write([]byte(hash))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// FeedExplain is one ranked post's score and its parts (feed.get explain).
type FeedExplain struct {
	ID    string           `json:"id"`
	Score float64          `json:"score"`
	Parts FeedExplainParts `json:"parts"`
}

// FeedExplainParts: merit's terms, the room weight and the decay factor;
// score = room_weight * (quality + votes + replies) * decay.
type FeedExplainParts struct {
	Quality    float64 `json:"quality"`
	Votes      float64 `json:"votes"`
	Replies    float64 `json:"replies"`
	RoomWeight float64 `json:"room_weight"`
	Decay      float64 `json:"decay"`
}

// readFeed is feed.get: a ranked page of the default profile, or of an
// inline override merged over it. Pages go by offset into the ranking the
// first page was cut from, pinned for RankSnapshotTTL; next_cursor carries
// that offset and, once the ranking has expired, resumes by keyset over a
// fresh one ((score, seq) below the last post's): a feed cursor never
// expires, though a post may repeat across the seam (dedupe by id).
// Everything runs on the read's tx.
func (s *Store) readFeed(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	req, err := parseFeedRequest(c.Data)
	if err != nil {
		return Result{}, err
	}
	profile := DefaultFeedProfile()
	source := "default"
	var saved *savedFeedProfile
	var skipped []string
	switch {
	case req.profile == "self":
		if !a.signed {
			return Result{}, problem(401, "signature_required", "profile=self reads your own saved feed profile: sign the read, or name an agent's fingerprint.")
		}
		source = "self"
		if saved, err = s.ownFeedProfile(ctx, tx, a); err != nil {
			return Result{}, err
		}
	case req.agent != "":
		source = "agent"
		if saved, err = s.publicFeedProfile(ctx, tx, req.agent, a); err != nil {
			return Result{}, err
		}
		if req.hash != "" && req.hash != saved.hash {
			return Result{}, &Error{Status: 409, Code: "profile_changed", Message: "That profile changed since " + req.hash + "; its profile_hash is now " + saved.hash + " (revision " + strconv.FormatInt(saved.revision, 10) + "). Read it again, or pin the new hash."}
		}
	}
	if saved != nil {
		// A room a saved profile follows may since have gone private or
		// away: the feed reads the rest and says which it skipped.
		if profile, skipped, err = publicFeedRooms(ctx, tx, saved.profile); err != nil {
			return Result{}, err
		}
	}
	if req.override != nil {
		if profile, err = applyFeedOverride(profile, req.override); err != nil {
			return Result{}, err
		}
	}
	if c.Cursor != "" && req.offset != 0 {
		return Result{}, problem(400, "invalid_cursor", "Give offset or cursor, not both.")
	}
	if err = checkFeedRooms(ctx, tx, profile); err != nil {
		return Result{}, err
	}
	muted, err := resolveMutedAuthors(ctx, tx, profile.Filters.MutedAuthors)
	if err != nil {
		return Result{}, err
	}
	f := newFeedScorer(profile, muted)
	views := profile.views()
	key := feedKey(views, f.hash)
	var cur *feedCursor
	if c.Cursor != "" {
		decoded, err := s.decodeFeedCursor(c.Cursor, key)
		if err != nil {
			return Result{}, err
		}
		cur = &decoded
	}
	limit := limitValue(c.Limit)
	ctx, cancel := context.WithTimeout(ctx, rankBudget)
	defer cancel()
	rr, err := s.ranking(ctx, tx, views, f, now, cur != nil || req.offset > 0)
	if err != nil {
		return Result{}, rankTimeout(err)
	}
	offset, resumed := req.offset, ""
	if cur != nil {
		offset, resumed = cur.Offset, "snapshot"
		if cur.Gen != rr.gen {
			// The ranking the cursor was cut from has expired: resume after
			// its last post in this one, or, when that post is gone, below
			// its (score, seq). Recency lowers every score as time passes, so
			// the post itself is the better mark while it ranks.
			offset, resumed = len(rr.list), "keyset"
			found := false
			for i, r := range rr.list {
				if r.seq == cur.Seq {
					offset, found = i+1, true
					break
				}
			}
			if !found {
				// Rescored at the cursor's clock, the posts ranked above
				// its mark are the ones its pages already gave.
				offset = 0
				for _, r := range f.score(rr.list, cur.Scored) {
					if r.rank > cur.Score || r.rank == cur.Score && r.seq >= cur.Seq {
						offset++
					}
				}
			}
		}
	}
	pg, err := s.rankedPage(ctx, tx, rr.list, offset, limit, now)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{
		"sort": "feed", "profile": profile, "profile_source": source, "overridden": req.override != nil,
		"profile_hash": f.hash, "ranking_version": FeedRankingVersion,
		"offset": offset, "next_offset": pg.next, "has_more": pg.hasMore, "warming": append([]string{}, rr.warming...),
	}
	if saved != nil {
		data["profile_agent"], data["profile_revision"], data["profile_visibility"] = saved.agent, saved.revision, saved.visibility
		data["saved_hash"] = saved.hash
		if len(skipped) > 0 {
			data["skipped_rooms"] = skipped
		}
	}
	if resumed != "" {
		data["resumed_from"] = resumed
	}
	next := ""
	if pg.hasMore && len(pg.page) > 0 {
		last := pg.page[len(pg.page)-1]
		next = s.encodeFeedCursor(feedCursor{Feed: key, Gen: rr.gen, Offset: pg.next, Score: last.rank, Seq: last.seq, Scored: rr.scored})
	} else if pg.hasMore {
		next = s.encodeFeedCursor(feedCursor{Feed: key, Gen: rr.gen, Offset: pg.next, Score: math.MaxFloat64, Seq: math.MaxInt64, Scored: rr.scored})
	}
	data["next_cursor"] = next
	if req.explain {
		explain := make([]FeedExplain, 0, len(pg.page))
		shown := map[string]bool{}
		for _, m := range pg.events {
			shown[m.ID] = true
		}
		for _, r := range pg.page {
			if !shown[r.id] {
				continue
			}
			q := Ranking.quality(r.effectiveQuality())
			explain = append(explain, FeedExplain{ID: r.id, Score: r.rank, Parts: FeedExplainParts{
				Quality: f.quality * q, Votes: f.votes * r.score, Replies: f.replies * float64(min(max(r.replies, 0), f.repliesMax)),
				RoomWeight: r.weight, Decay: r.decay}})
		}
		data["explain"] = explain
	}
	return Result{Messages: pg.events, NextCursor: next, Data: data}, nil
}
