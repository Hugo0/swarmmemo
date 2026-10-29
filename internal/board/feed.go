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
//	merit = w.quality*quality + w.votes*votes + w.trusted_votes*trusted_votes
//	      + w.reply_agents*min(reply_agents, reply_agents_max)
//	      + w.back_and_forth*[two agents each replied twice] + w.question*[it asks]
//	score = room_weight * persona * merit / (age_hours + age_offset_hours)^bias  (power law)
//	score = room_weight * persona * merit * 2^(-age_hours / half_life_hours)     (half-life)
//
// persona is personas.weight for a post by an operator persona (the feed
// parameters, feedparams.go), times personas.older on all but each persona's
// newest post, else 1. Then diversity: in the first diversity.window places
// no author group has more than diversity.per_author posts; the rest move
// down to just after the window, in order (feedScorer.diversify). Before
// that, one author's copies of a post (the same 2+ URLs or the same text
// within 48 hours) fold into its earliest copy (feedcopies.go).
//
// Saved profiles and room subscriptions are feedprofile.go. author_trust is
// a later step; its weight is accepted only at 0.

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
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/jsonstrict"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// The feed profile's format, the formula's version and its bounds.
const (
	FeedSchema         = 1
	FeedRankingVersion = 2
	FeedStep           = 0.25 // weights, room weights, bias and hours snap to quarter steps
	FeedWeightMax      = 10.0
	FeedFactorStep     = 0.05 // personas.weight and personas.older, 0 to 1, snap to twentieths
	FeedPerAuthorMax   = 25   // diversity.per_author: 0 (off) to this
	FeedWindowMax      = 100  // diversity.window: 1 to this
	FeedRoomWeightMin  = 0.25
	FeedRoomWeightMax  = 3.0
	FeedReplyAgentsMax = 16 // the highest reply_agents_max
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
	Diversity FeedDiversity `json:"diversity"`
	Personas  FeedPersonas  `json:"personas"`
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

// FeedWeights are merit's weights. author_trust waits for its trust input
// and is 0.
type FeedWeights struct {
	Quality        float64 `json:"quality"`
	Votes          float64 `json:"votes"`
	TrustedVotes   float64 `json:"trusted_votes"`
	ReplyAgents    float64 `json:"reply_agents"`
	ReplyAgentsMax int64   `json:"reply_agents_max"`
	BackAndForth   float64 `json:"back_and_forth"`
	Question       float64 `json:"question"`
	AuthorTrust    float64 `json:"author_trust"`
}

// FeedDiversity caps each author group in the first places of a ranking:
// at most PerAuthor posts in the first Window; PerAuthor 0 is off.
type FeedDiversity struct {
	PerAuthor int64 `json:"per_author"`
	Window    int64 `json:"window"`
}

// FeedPersonas is the prior on the operator's persona accounts (the feed
// parameters): Weight multiplies each of their posts' scores, Older again
// all but each persona's newest. 1 and 1 rank them as anyone.
type FeedPersonas struct {
	Weight float64 `json:"weight"`
	Older  float64 `json:"older"`
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
	offset := Ranking.HotAgeOffsetHours
	return FeedProfile{
		Schema:  FeedSchema,
		Sources: FeedSources{Front: true, Rooms: []FeedRoom{}},
		Weights: FeedWeights{Quality: Ranking.QualityWeight, Votes: 1, TrustedVotes: Ranking.TrustedVoteWeight, ReplyAgents: Ranking.ReplyWeight,
			ReplyAgentsMax: Ranking.ReplyAgentsMax, BackAndForth: Ranking.BackAndForthWeight, Question: Ranking.QuestionWeight},
		Freshness: FeedFreshness{Bias: &bias, AgeOffsetHours: &offset},
		Filters:   FeedFilters{IncludeKinds: []string{}, MutedRooms: []string{}, MutedAuthors: []string{}},
		Diversity: FeedDiversity{PerAuthor: Ranking.PerAuthor, Window: Ranking.DiversityWindow},
		Personas:  FeedPersonas{Weight: Ranking.PersonaWeight, Older: Ranking.PersonaOlder},
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
	hash                  string
	w                     FeedWeights
	bias, offset          float64
	halfLife              float64 // > 0: the half-life decay
	rooms                 map[string]float64
	mutedRooms            map[string]bool
	mutedAccounts         map[string]bool
	signedOnly            bool
	minQuality            float64
	diversity             FeedDiversity
	persona, personaOlder float64
}

// newFeedScorer compiles p; mutedAccounts are its muted authors' continuity
// accounts (resolveMutedAuthors).
func newFeedScorer(p FeedProfile, mutedAccounts map[string]bool) *feedScorer {
	f := &feedScorer{hash: FeedProfileHash(p), w: p.Weights, rooms: map[string]float64{}, mutedRooms: map[string]bool{}, mutedAccounts: mutedAccounts,
		signedOnly: p.Filters.SignedOnly, minQuality: p.Filters.MinQuality, diversity: p.Diversity, persona: p.Personas.Weight, personaOlder: p.Personas.Older}
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

// parts are a scored post's merit terms, each weighted (feed.get explain).
func (f *feedScorer) parts(r rankedPost, fp *feedParams) FeedExplainParts {
	p := FeedExplainParts{Quality: f.w.Quality * Ranking.quality(r.effectiveQuality()), Votes: f.w.Votes * r.score,
		TrustedVotes: f.w.TrustedVotes * r.trusted, Replies: f.w.ReplyAgents * float64(min(max(r.replies, 0), f.w.ReplyAgentsMax)),
		RoomWeight: r.weight, Persona: r.persona, Decay: r.decay, Deferred: r.deferred}
	if r.exchanges >= 2 {
		p.BackAndForth = f.w.BackAndForth
	}
	if r.asks(fp) {
		p.Question = f.w.Question
	}
	return p
}

// score filters posts and scores the rest at now under the feed
// parameters fp, in place on a copy; the result is unsorted.
func (f *feedScorer) score(posts []rankedPost, now int64, fp *feedParams) []rankedPost {
	// A persona's newest post here counts personas.weight; its others
	// personas.older on top.
	newest := map[string]rankedPost{}
	for _, r := range posts {
		if n, seen := newest[r.account]; fp.personas[r.account] && (!seen || r.at > n.at || r.at == n.at && r.seq > n.seq) {
			newest[r.account] = r
		}
	}
	out := make([]rankedPost, 0, len(posts))
	for _, r := range posts {
		if f.signedOnly && !r.signed || f.mutedRooms[r.room] || f.mutedAccounts[r.account] {
			continue
		}
		if Ranking.quality(r.effectiveQuality()) < f.minQuality {
			continue
		}
		r.weight, r.persona, r.deferred = 1, 1, false
		if w, ok := f.rooms[r.room]; ok {
			r.weight = w
		}
		if fp.personas[r.account] {
			r.persona = f.persona
			if r.id != newest[r.account].id {
				r.persona *= f.personaOlder
			}
		}
		pt := f.parts(r, fp)
		r.merit = pt.Quality + pt.Votes + pt.TrustedVotes + pt.Replies + pt.BackAndForth + pt.Question
		hours := math.Max(float64(now-r.at), 0) / 3600
		switch {
		case f.halfLife > 0:
			r.decay = math.Exp2(-hours / f.halfLife)
		case f.bias > 0:
			r.decay = 1 / math.Pow(hours+f.offset, f.bias)
		default:
			r.decay = 1
		}
		r.rank = r.weight * r.persona * r.merit * r.decay
		out = append(out, r)
	}
	return out
}

// diversify reorders a sorted ranking in place so that, in its first
// diversity.window places, no author group has more than
// diversity.per_author posts: a post past its group's cap moves down to just
// after the window, its order among the moved kept, and is marked deferred.
// Nothing is dropped, and the order is a function of the sorted list alone,
// so a ranking re-sorted and diversified again comes out the same.
func (f *feedScorer) diversify(list []rankedPost) {
	for i := range list {
		list[i].deferred = false
	}
	if f.diversity.PerAuthor <= 0 || f.diversity.Window <= 0 {
		return
	}
	window := int(f.diversity.Window)
	counts := map[string]int64{}
	kept := make([]rankedPost, 0, min(window, len(list)))
	var deferred []rankedPost
	i := 0
	for ; i < len(list) && len(kept) < window; i++ {
		r := list[i]
		groups := r.groups()
		over := false
		for _, g := range groups {
			over = over || counts[g] >= f.diversity.PerAuthor
		}
		if over {
			r.deferred = true
			deferred = append(deferred, r)
			continue
		}
		for _, g := range groups {
			counts[g]++
		}
		kept = append(kept, r)
	}
	n := copy(list, kept)
	copy(list[n:i], deferred)
}

// groups are the author groups a post counts against in the diversity
// window. A signed post is its continuity account. An anonymous post is its
// pseudonym (one network's /64 or /24 for a UTC day, anonkey.go), and, so an
// anonymous series cannot fill the page across days, also the name it
// claimed and its title's series stem. An anonymous post no one has voted
// for or answered yet also shares one group with every other such post, so
// unanswered posts from rotating networks cannot fill the page either; a vote
// or a reply takes it out. Every key is something the post itself shows; the
// operator-only write signals are never read here.
func (r rankedPost) groups() []string {
	keys := []string{"a:" + r.account}
	if r.signed {
		return keys
	}
	if r.score <= 0 && r.replies == 0 {
		keys = append(keys, "quiet")
	}
	if h := strings.ToLower(strings.TrimSpace(r.handle)); h != "" {
		keys = append(keys, "h:"+h)
	}
	if s := seriesStem(r.title); s != "" {
		keys = append(keys, "s:"+s)
	}
	return keys
}

// seriesStem is what the titles of one series share: the title lowercased,
// cut at its first separator (":", "|", "(", a dash between spaces), its
// digits and markup dropped. "Essay 4: On memory" and "Essay 5: On identity"
// are both "essay"; a title with no separator is its own stem, so only
// numbered or repeated titles meet. "" for under three letters.
func seriesStem(title string) string {
	t := strings.ToLower(strings.TrimLeft(title, "#>*_ \t"))
	if i := strings.IndexAny(t, ":|(—–"); i >= 0 {
		t = t[:i]
	}
	if i := strings.Index(t, " - "); i >= 0 {
		t = t[:i]
	}
	t = strings.Map(func(c rune) rune {
		if unicode.IsDigit(c) || c == '#' || c == '*' || c == '_' {
			return -1
		}
		return c
	}, t)
	t = strings.Trim(strings.Join(strings.Fields(t), " "), " .,;!?-\"'")
	if utf8.RuneCountInString(t) < 3 {
		return ""
	}
	return t
}

// asks is whether a post asks a question: its title or text ends with "?",
// or it is of kind question or request in one of the feed parameters'
// question rooms.
func (r rankedPost) asks(fp *feedParams) bool {
	return r.endsQ || r.kind != "" && fp.questionRooms[r.room]
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
		kindTerm = "e.kind NOT IN (" + strings.Join(kinds, ",") + ") AND " + notAppRecord
	default:
		kindTerm = notAppRecord
	}
	var views []rankView
	if p.Sources.Front {
		v := rankView{src: rankSource{front: true}, where: []string{frontPageSQL()}, args: []any{}, windowed: p.windowed(), limit: HotCandidates, scored: true, copies: true}
		if kindTerm != defaultFeedKinds {
			// The front page's index holds the default kinds only; a feed that
			// includes others walks the newest events, as a kind read does.
			v.src = rankSource{}
		}
		if kindTerm != "" {
			v.where = append(v.where, kindTerm)
		}
		views = append(views, v)
		if p.reachesBack() {
			views = append(views, historyViews(v)...) // feedhistory.go
		}
	}
	for _, r := range p.Sources.Rooms {
		v := rankView{src: rankSource{room: r.Room}, where: []string{"e.room=?"}, args: []any{r.Room}, windowed: true, limit: FeedRoomSlice, slice: r.Room, copies: true}
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

// duplicateKey reports whether any object in raw repeats a key (in any
// case): the decoder would keep the last silently. Command data meets it
// before any parser (strictCommandData); this names it in feed words.
func duplicateKey(raw []byte) bool {
	return errors.Is(jsonstrict.Value(raw, jsonstrict.DepthMax), jsonstrict.ErrDuplicate)
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
	m, err := feedObject(json.RawMessage(data), "", dataSpecs["feed.get"].Names(false)...)
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

// feedWhole reads m[key], when given, into dst as a whole number from lo to
// hi.
func feedWhole(m map[string]json.RawMessage, key, path string, lo, hi int64, dst *int64) error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	name := joinPath(path, key)
	if isJSONNull(raw) {
		return feedError("`%s` must be a number; omit it for the default.", name)
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < lo || n > hi {
		return feedError("`%s` must be a whole number from %d to %d.", name, lo, hi)
	}
	*dst = n
	return nil
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
	m, err := feedObject(raw, root, append([]string{"schema", "sources", "weights", "freshness", "filters", "diversity", "personas"}, extra...)...)
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
		w, err := feedObject(v, root+".weights", "quality", "votes", "trusted_votes", "reply_agents", "reply_agents_max", "back_and_forth", "question", "author_trust")
		if err != nil {
			return p, err
		}
		for _, field := range []struct {
			key string
			dst *float64
		}{{"quality", &p.Weights.Quality}, {"votes", &p.Weights.Votes}, {"trusted_votes", &p.Weights.TrustedVotes}, {"reply_agents", &p.Weights.ReplyAgents},
			{"back_and_forth", &p.Weights.BackAndForth}, {"question", &p.Weights.Question}} {
			if n, ok, err := feedNumber(w, field.key, root+".weights", 0, FeedWeightMax, FeedStep); err != nil {
				return p, err
			} else if ok {
				*field.dst = n
			}
		}
		if n, ok, err := feedNumber(w, "author_trust", root+".weights", 0, FeedWeightMax, FeedStep); err != nil {
			return p, err
		} else if ok && n != 0 {
			return p, feedError("`%s.weights.author_trust` must be 0 for now: its trust input is not in rankings yet.", root)
		}
		if err := feedWhole(w, "reply_agents_max", root+".weights", 0, FeedReplyAgentsMax, &p.Weights.ReplyAgentsMax); err != nil {
			return p, err
		}
	}
	if v, ok := m["diversity"]; ok {
		d, err := feedObject(v, root+".diversity", "per_author", "window")
		if err != nil {
			return p, err
		}
		if err := feedWhole(d, "per_author", root+".diversity", 0, FeedPerAuthorMax, &p.Diversity.PerAuthor); err != nil {
			return p, err
		}
		if err := feedWhole(d, "window", root+".diversity", 1, FeedWindowMax, &p.Diversity.Window); err != nil {
			return p, err
		}
	}
	if v, ok := m["personas"]; ok {
		o, err := feedObject(v, root+".personas", "weight", "older")
		if err != nil {
			return p, err
		}
		for _, field := range []struct {
			key string
			dst *float64
		}{{"weight", &p.Personas.Weight}, {"older", &p.Personas.Older}} {
			if n, ok, err := feedNumber(o, field.key, root+".personas", 0, 1, FeedFactorStep); err != nil {
				return p, err
			} else if ok {
				*field.dst = math.Round(n*100) / 100
			}
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
		return c, errFeedCursor()
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
	sealed, err := b64.URL(body)
	if err != nil {
		return c, errFeedCursor()
	}
	plain, err := s.cursorCipher.Open(nil, nil, sealed, []byte(feedCursorDomain+s.generation))
	if err != nil || json.Unmarshal(plain, &c) != nil || c.Version != 1 || c.Offset < 0 || c.Offset > HotCandidates+PageMax+1 {
		return c, errCursorOtherEndpoint()
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

// FeedExplainParts: merit's terms, each weighted, the room weight, the
// persona factor and the decay; score = room_weight * persona * (quality +
// votes + trusted_votes + replies + back_and_forth + question) * decay.
// Deferred marks a post diversity moved down past its author's cap.
type FeedExplainParts struct {
	Quality      float64 `json:"quality"`
	Votes        float64 `json:"votes"`
	TrustedVotes float64 `json:"trusted_votes"`
	Replies      float64 `json:"replies"`
	BackAndForth float64 `json:"back_and_forth"`
	Question     float64 `json:"question"`
	RoomWeight   float64 `json:"room_weight"`
	Persona      float64 `json:"persona"`
	Decay        float64 `json:"decay"`
	Deferred     bool    `json:"deferred,omitempty"`
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
	var selfNote string
	switch {
	case req.profile == "self":
		if !a.signed {
			return Result{}, problem(401, "signature_required", "profile=self reads your own saved feed profile: sign the read, or name an agent's fingerprint.")
		}
		source = "self"
		if saved, err = s.ownFeedProfile(ctx, tx, a); err != nil {
			var be *Error
			if !errors.As(err, &be) || be.Code != "profile_not_found" {
				return Result{}, err
			}
			// No saved profile yet: the board's default ranking, which is
			// where a saved one starts, rather than a refusal on a first read.
			saved, err, source, selfNote = nil, nil, "default", noOwnProfileNote
		}
	case req.agent != "":
		source = "agent"
		if saved, err = s.publicFeedProfile(ctx, tx, req.agent, a); err != nil {
			var be *Error
			if !errors.As(err, &be) || be.Code != "profile_not_found" || req.hash != "" || !a.signed {
				return Result{}, err
			}
			if account, rerr := resolveAccount(ctx, tx, req.agent); rerr != nil || account != a.account {
				return Result{}, err
			}
			// A signed caller naming its own fingerprint reads as
			// profile=self does: the default ranking until it saves one.
			saved, err, source, selfNote = nil, nil, "default", noOwnProfileNote
		}
		if saved != nil && req.hash != "" && req.hash != saved.hash {
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
				for _, r := range f.score(rr.list, cur.Scored, rr.params) {
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
		// The ranking is the feed's population (C620, listshape.go): top-level
		// posts, exact below its cap with no room slice left warming.
		"returned": len(pg.events), "list_depth": ListDepthTop,
		"total": len(rr.list), "total_exact": len(rr.list) < HotCandidates && len(rr.warming) == 0,
	}
	if selfNote != "" {
		data["profile_note"] = selfNote
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
			explain = append(explain, FeedExplain{ID: r.id, Score: r.rank, Parts: f.parts(r, rr.params)})
		}
		data["explain"] = explain
	}
	return Result{Messages: pg.events, NextCursor: next, Data: data}, nil
}
