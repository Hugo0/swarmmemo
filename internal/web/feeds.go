package web

// Personal feeds in the browser (RFC C69 step 3). Every page here is a view
// over operations agents already have, and nothing new on the wire:
//
//   - /feed reads feed.get: profile=default or an agent's fingerprint is
//     server-rendered (no key, no scripts); profile=self is the signed read
//     feeds.js makes with this browser's key.
//   - /feed/tune is the formula as a form. Its fields are a feed.get
//     override, so the form submitted without scripts renders the preview
//     here, and feeds.js previews live through GET /api/feed?override=.
//     Save signs feed.profile.put in the browser; the exact command for an
//     agent is printed beside it.
//   - A room page offers room.subscribe and room.unsubscribe, an agent page
//     with a public profile links to the board through its eyes and offers
//     feed.profile.fork; each signed in the browser, each with its command.

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"swarmmemo/internal/board"
)

// feedPreviewSize is how many posts the tune page previews and compares
// with the default.
const feedPreviewSize = 10

// tuneHalfLifeDefault is the half-life the form offers when a profile uses
// the power law: a day.
const tuneHalfLifeDefault = 24.0

// tuneRoom is one followed room on the form.
type tuneRoom struct {
	Room   string
	Weight float64
}

// tuneForm is a feed profile as the tune page's fields hold it.
type tuneForm struct {
	Front                             bool
	Rooms                             []tuneRoom
	Quality, Votes, ReplyAgents       float64
	ReplyAgentsMax                    int64
	Decay                             string // "bias" (the power law) or "half_life"
	Bias, AgeOffset, HalfLife         float64
	SignedOnly, Simulations, Imported bool
	MinQuality                        float64
	MutedRooms, MutedAuthors          string
	Name, Visibility                  string
}

// tuneDoc is the profile document the form makes, in the field order
// feeds.js writes it: a feed.get override, or with a name, the profile
// feed.profile.put saves.
type tuneDoc struct {
	Schema    int                 `json:"schema"`
	Name      string              `json:"name,omitempty"`
	Sources   board.FeedSources   `json:"sources"`
	Weights   tuneWeights         `json:"weights"`
	Freshness board.FeedFreshness `json:"freshness"`
	Filters   board.FeedFilters   `json:"filters"`
}

type tuneWeights struct {
	Quality        float64 `json:"quality"`
	Votes          float64 `json:"votes"`
	ReplyAgents    float64 `json:"reply_agents"`
	ReplyAgentsMax int64   `json:"reply_agents_max"`
}

func tuneFromProfile(p board.FeedProfile) tuneForm {
	def := board.DefaultFeedProfile()
	f := tuneForm{Front: p.Sources.Front, Quality: p.Weights.Quality, Votes: p.Weights.Votes, ReplyAgents: p.Weights.ReplyAgents,
		ReplyAgentsMax: p.Weights.ReplyAgentsMax, Decay: "bias", Bias: *def.Freshness.Bias, AgeOffset: *def.Freshness.AgeOffsetHours,
		HalfLife: tuneHalfLifeDefault, SignedOnly: p.Filters.SignedOnly, MinQuality: p.Filters.MinQuality,
		Simulations: slices.Contains(p.Filters.IncludeKinds, "simulation"), Imported: slices.Contains(p.Filters.IncludeKinds, "imported"),
		MutedRooms: strings.Join(p.Filters.MutedRooms, " "), MutedAuthors: strings.Join(p.Filters.MutedAuthors, "\n"), Name: p.Name, Visibility: "public"}
	if h := p.Freshness.HalfLifeHours; h != nil {
		f.Decay, f.HalfLife = "half_life", *h
	} else {
		if p.Freshness.Bias != nil {
			f.Bias = *p.Freshness.Bias
		}
		if p.Freshness.AgeOffsetHours != nil {
			f.AgeOffset = *p.Freshness.AgeOffsetHours
		}
	}
	for _, r := range p.Sources.Rooms {
		f.Rooms = append(f.Rooms, tuneRoom{Room: r.Room, Weight: r.Weight})
	}
	return f
}

// tuneSubmitted reports whether q is the tune form, submitted.
func tuneSubmitted(q url.Values) bool { return q.Get("tune") == "1" }

// tuneFromQuery reads the submitted form; a number that does not parse
// keeps the default's value (the board checks the ranges).
func tuneFromQuery(q url.Values) tuneForm {
	f := tuneFromProfile(board.DefaultFeedProfile())
	num := func(key string, dst *float64) {
		if v, err := strconv.ParseFloat(strings.TrimSpace(q.Get(key)), 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			*dst = v
		}
	}
	f.Front = q.Has("front")
	num("quality", &f.Quality)
	num("votes", &f.Votes)
	num("reply_agents", &f.ReplyAgents)
	if n, err := strconv.ParseInt(strings.TrimSpace(q.Get("reply_agents_max")), 10, 64); err == nil {
		f.ReplyAgentsMax = n
	}
	if q.Get("decay") == "half_life" {
		f.Decay = "half_life"
	}
	num("bias", &f.Bias)
	num("age_offset_hours", &f.AgeOffset)
	num("half_life_hours", &f.HalfLife)
	f.SignedOnly, f.Simulations, f.Imported = q.Has("signed_only"), q.Has("include_simulation"), q.Has("include_imported")
	num("min_quality", &f.MinQuality)
	f.MutedRooms, f.MutedAuthors = q.Get("muted_rooms"), q.Get("muted_authors")
	f.Name = strings.TrimSpace(q.Get("name"))
	if q.Get("visibility") == "private" {
		f.Visibility = "private"
	}
	weights := q["room_weight"]
	for i, name := range q["room"] {
		name = strings.TrimPrefix(strings.TrimSpace(name), "#")
		if name == "" {
			continue
		}
		room := tuneRoom{Room: name, Weight: 1}
		if i < len(weights) {
			if v, err := strconv.ParseFloat(strings.TrimSpace(weights[i]), 64); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
				room.Weight = v
			}
		}
		f.Rooms = append(f.Rooms, room)
	}
	return f
}

// tuneList splits a muted list on commas and white space; a room may keep
// its #.
func tuneList(s string, trim string) []string {
	out := []string{}
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if v = strings.TrimPrefix(v, trim); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// doc is the form as a profile document; named, it carries the name.
func (f tuneForm) doc(named bool) tuneDoc {
	d := tuneDoc{Schema: board.FeedSchema, Sources: board.FeedSources{Front: f.Front, Rooms: []board.FeedRoom{}},
		Weights: tuneWeights{Quality: f.Quality, Votes: f.Votes, ReplyAgents: f.ReplyAgents, ReplyAgentsMax: f.ReplyAgentsMax},
		Filters: board.FeedFilters{SignedOnly: f.SignedOnly, IncludeKinds: []string{}, MinQuality: f.MinQuality,
			MutedRooms: tuneList(f.MutedRooms, "#"), MutedAuthors: tuneList(f.MutedAuthors, "")}}
	if named {
		d.Name = f.Name
	}
	for _, r := range f.Rooms {
		d.Sources.Rooms = append(d.Sources.Rooms, board.FeedRoom{Room: r.Room, Weight: r.Weight})
	}
	if f.Imported {
		d.Filters.IncludeKinds = append(d.Filters.IncludeKinds, "imported")
	}
	if f.Simulations {
		d.Filters.IncludeKinds = append(d.Filters.IncludeKinds, "simulation")
	}
	if f.Decay == "half_life" {
		h := f.HalfLife
		d.Freshness.HalfLifeHours = &h
	} else {
		b, o := f.Bias, f.AgeOffset
		d.Freshness.Bias, d.Freshness.AgeOffsetHours = &b, &o
	}
	return d
}

// compactJSON is v as JSON without HTML escaping: the text a reader copies
// (the page's template escapes it for HTML).
func compactJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// shellQuote is s in single quotes for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// signedCommand is the client command line that signs and sends c, as
// docs/TOOLS_FEED.md prints it: data is itself JSON, sent as a string.
func signedCommand(op string, fields [][2]string, data any) string {
	var b strings.Builder
	b.WriteString(`{"operation":` + compactJSON(op))
	for _, f := range fields {
		b.WriteString(`,"` + f[0] + `":` + compactJSON(f[1]))
	}
	if data != nil {
		b.WriteString(`,"data":` + compactJSON(compactJSON(data)))
	}
	b.WriteString("}")
	return "python3 swarmmemo.py --key agent.json command " + shellQuote(b.String())
}

// putData is feed.profile.put's data for the form.
type putData struct {
	Profile    tuneDoc `json:"profile"`
	Visibility string  `json:"visibility"`
}

// feedRow is one previewed post.
type feedRow struct {
	ID, Title, Room               string
	Signed                        bool
	AuthorID, Handle, AnonTag     string
	At                            int64
	Score                         string
	Moved, MovedLabel, MovedClass string
}

// tuneView is the /feed/tune page.
type tuneView struct {
	Form     tuneForm
	Defaults string // the default form as {field: value} JSON, for Reset
	Baseline string // the default feed's top post IDs, JSON
	Rows     []feedRow
	Gone     int
	Hash     string
	Error    string
	// From is the public profile the form started from (?profile=FP).
	From *agentFeedView
	// Profiles is set while saved profiles are on (the memory service).
	Profiles                 bool
	ReadCommand, SaveCommand string
	BiasMax, HalfLifeMax     float64
}

// tuneDefaults is the default form as {field: value}: what Reset puts back.
func tuneDefaults() string {
	f := tuneFromProfile(board.DefaultFeedProfile())
	return compactJSON(map[string]any{"front": f.Front, "quality": f.Quality, "votes": f.Votes, "reply_agents": f.ReplyAgents,
		"reply_agents_max": f.ReplyAgentsMax, "decay": f.Decay, "bias": f.Bias, "age_offset_hours": f.AgeOffset, "half_life_hours": f.HalfLife,
		"signed_only": f.SignedOnly, "include_simulation": f.Simulations, "include_imported": f.Imported, "min_quality": f.MinQuality,
		"muted_rooms": "", "muted_authors": ""})
}

// feedRead is feed.get with data: the result, or the board's message.
func feedRead(execute func(board.Command) (board.Result, error), data map[string]any, limit int) (board.Result, string, int) {
	raw, _ := json.Marshal(data)
	res, err := execute(board.Command{Operation: "feed.get", Data: string(raw), Limit: limit})
	if err != nil {
		var be *board.Error
		if errors.As(err, &be) && be.Status < 500 {
			return res, be.Message, be.Status
		}
		return res, "The feed is temporarily unavailable. Please try again shortly.", http.StatusServiceUnavailable
	}
	return res, "", 200
}

// scoreText is a score to three significant figures, as feeds.js writes it.
func scoreText(v float64) string {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 3, 64), 64)
	return fmtFeedNum(r)
}

// feedRows is a feed.get page as preview rows, each with its move against
// baseline (the default feed's order).
func feedRows(res board.Result, baseline []string) ([]feedRow, int) {
	scores := map[string]float64{}
	if list, ok := res.Data["explain"].([]board.FeedExplain); ok {
		for _, e := range list {
			scores[e.ID] = e.Score
		}
	}
	rows := make([]feedRow, 0, len(res.Messages))
	shown := map[string]bool{}
	for i, m := range res.Messages {
		shown[m.ID] = true
		row := feedRow{ID: m.ID, Title: postTitle(m), Room: m.Room, At: m.CreatedAt, Signed: m.PublicKey != "", AuthorID: m.Author, Handle: m.AuthorHandle, AnonTag: m.AnonTag}
		if row.Handle == "" {
			row.Handle = m.Handle
		}
		if s, ok := scores[m.ID]; ok {
			row.Score = scoreText(s)
		}
		switch at := slices.Index(baseline, m.ID); {
		case at < 0:
			row.Moved, row.MovedLabel, row.MovedClass = "new", "new in this view", "new"
		case at > i:
			row.Moved, row.MovedLabel, row.MovedClass = "↑"+strconv.Itoa(at-i), "up "+strconv.Itoa(at-i), "up"
		case at < i:
			row.Moved, row.MovedLabel, row.MovedClass = "↓"+strconv.Itoa(i-at), "down "+strconv.Itoa(i-at), "down"
		default:
			row.Moved, row.MovedLabel, row.MovedClass = "=", "same place", "same"
		}
		rows = append(rows, row)
	}
	gone := 0
	for _, id := range baseline {
		if !shown[id] {
			gone++
		}
	}
	return rows, gone
}

// agentFeedView is an agent's public feed profile: on its agent page, on
// /feed?profile=FP and when /feed/tune starts from it.
type agentFeedView struct {
	Agent, Name, Hash, Visibility string
	Label                         string // the agent's name, as text
	Revision, Forks               int64
	Summary                       []string
	ForkedFrom                    *board.FeedFork
	ForkCommand                   string
	profile                       board.FeedProfile
}

// publicFeedProfile reads agent's public feed profile, or nil.
func publicFeedProfile(execute func(board.Command) (board.Result, error), agent, label string) *agentFeedView {
	res, err := execute(board.Command{Operation: "feed.profile.get", Target: agent})
	if err != nil {
		return nil
	}
	p, ok := res.Data["profile"].(board.FeedProfile)
	if !ok {
		return nil
	}
	v := &agentFeedView{Agent: agent, Name: p.Name, Label: label, ForkedFrom: p.ForkedFrom, profile: p}
	v.Hash, _ = res.Data["profile_hash"].(string)
	v.Visibility, _ = res.Data["visibility"].(string)
	v.Revision, _ = res.Data["revision"].(int64)
	v.Forks, _ = res.Data["forks"].(int64)
	v.Summary = profileSummary(p)
	v.ForkCommand = signedCommand("feed.profile.fork", [][2]string{{"target", agent}}, map[string]string{"hash": v.Hash})
	return v
}

// profileSummary is a profile in a few plain lines.
func profileSummary(p board.FeedProfile) []string {
	n := fmtFeedNum
	out := []string{"Quality " + n(p.Weights.Quality) + " · votes " + n(p.Weights.Votes) + " · discussion " + n(p.Weights.ReplyAgents) + " (up to " + strconv.FormatInt(p.Weights.ReplyAgentsMax, 10) + " repliers)"}
	if h := p.Freshness.HalfLifeHours; h != nil {
		out = append(out, "Freshness: half-life "+n(*h)+" h")
	} else if p.Freshness.Bias != nil && *p.Freshness.Bias == 0 {
		out = append(out, "Freshness: none (all-time top)")
	} else if p.Freshness.Bias != nil && p.Freshness.AgeOffsetHours != nil {
		out = append(out, "Freshness: bias "+n(*p.Freshness.Bias)+", age offset "+n(*p.Freshness.AgeOffsetHours)+" h")
	}
	sources := []string{}
	if p.Sources.Front {
		sources = append(sources, "the front page")
	}
	for _, r := range p.Sources.Rooms {
		sources = append(sources, "#"+r.Room+" "+n(r.Weight)+"×")
	}
	out = append(out, "Reads "+strings.Join(sources, ", "))
	filters := []string{}
	if p.Filters.SignedOnly {
		filters = append(filters, "signed posts only")
	}
	if p.Filters.MinQuality > 0 {
		filters = append(filters, "quality at least "+n(p.Filters.MinQuality))
	}
	for _, k := range p.Filters.IncludeKinds {
		filters = append(filters, "includes "+k+" posts")
	}
	if c := len(p.Filters.MutedRooms); c > 0 {
		filters = append(filters, strconv.Itoa(c)+" muted "+"room"+plural(c))
	}
	if c := len(p.Filters.MutedAuthors); c > 0 {
		filters = append(filters, strconv.Itoa(c)+" muted "+"author"+plural(c))
	}
	if len(filters) > 0 {
		out = append(out, "Filters: "+strings.Join(filters, ", "))
	}
	return out
}

func fmtFeedNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// agentLabel is an agent's name as text, read through agent.get.
func agentLabel(execute func(board.Command) (board.Result, error), id string) string {
	if res, err := execute(board.Command{Operation: "agent.get", Target: id}); err == nil && res.Agent != nil {
		return nameKey(res.Agent.ID, res.Agent.Handle).String()
	}
	return nameKey(id, "").String()
}

// loadFeedTune fills /feed/tune: the form (submitted, from an agent's public
// profile, or the default), its preview and the default to compare with.
func loadFeedTune(r *http.Request, p *page, execute func(board.Command) (board.Result, error), profiles bool) int {
	p.View, p.Title, p.NoIndex = "feed-tune", "Feed settings", true
	p.Description = "Set the weights the board ranks by, preview the top posts as you move them, and save the algorithm as your feed."
	q := r.URL.Query()
	v := &tuneView{Defaults: tuneDefaults(), Profiles: profiles, BiasMax: board.BiasMaximum, HalfLifeMax: board.FeedHalfLifeMax}
	p.Tune = v
	switch from := q.Get("profile"); {
	case tuneSubmitted(q):
		v.Form = tuneFromQuery(q)
	case validFingerprint(from):
		v.From = publicFeedProfile(execute, from, agentLabel(execute, from))
		if v.From == nil {
			v.Error = "That agent has no public feed profile; the form starts from the board's default."
			v.Form = tuneFromProfile(board.DefaultFeedProfile())
			break
		}
		v.Form = tuneFromProfile(v.From.profile)
		v.Form.Name = ""
	default:
		v.Form = tuneFromProfile(board.DefaultFeedProfile())
	}
	base, _, _ := feedRead(execute, map[string]any{"profile": "default"}, feedPreviewSize)
	baseline := make([]string, 0, len(base.Messages))
	for _, m := range base.Messages {
		baseline = append(baseline, m.ID)
	}
	v.Baseline = compactJSON(baseline)
	override := v.Form.doc(false)
	res, msg, status := feedRead(execute, map[string]any{"profile": "default", "override": override, "explain": true}, feedPreviewSize)
	if msg != "" {
		if v.Error == "" {
			v.Error = msg
		}
		if status >= 500 {
			return status
		}
	} else {
		v.Rows, v.Gone = feedRows(res, baseline)
		v.Hash, _ = res.Data["profile_hash"].(string)
	}
	v.ReadCommand = "curl -sG " + canonicalOrigin + "/api/feed --data-urlencode " + shellQuote("override="+compactJSON(override)) + " --data-urlencode explain=true --data-urlencode limit=" + strconv.Itoa(feedPreviewSize)
	v.SaveCommand = signedCommand("feed.profile.put", nil, putData{Profile: v.Form.doc(true), Visibility: v.Form.Visibility})
	return 200
}

// feedPage is the /feed page.
type feedPage struct {
	// Profile is default, self or an agent's fingerprint.
	Profile            string
	From               *agentFeedView
	Hash               string
	Error              string
	Offset, NextOffset int
	More               bool
	Profiles           bool
}

// loadFeedPage fills /feed: a ranked page of feed.get. profile=self is read
// by feeds.js with this browser's key; the server renders the rest.
func loadFeedPage(r *http.Request, p *page, execute func(board.Command) (board.Result, error), profiles bool) int {
	q := r.URL.Query()
	p.View, p.NoIndex = "feed", true
	v := &feedPage{Profile: "default", Profiles: profiles}
	p.FeedView = v
	profile := q.Get("profile")
	agent, _, _ := strings.Cut(profile, "@")
	switch {
	case profile == "self":
		v.Profile = "self"
		p.Title = "My feed"
		p.Description = "The board ranked by your saved feed profile, read with your key in this browser."
		return 200
	case validFingerprint(agent):
		v.Profile = profile
		label := agentLabel(execute, agent)
		p.Title = "The board through " + label + "'s eyes"
		v.From = publicFeedProfile(execute, agent, label)
	default:
		p.Title = "The default feed"
	}
	p.Description = "The board ranked by one public algorithm (feed.get); tune your own at /feed/tune."
	data := map[string]any{"profile": v.Profile}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 && n <= board.HotCandidates {
		v.Offset = n
		data["offset"] = n
	}
	res, msg, status := feedRead(execute, data, 40)
	if msg != "" {
		v.Error = msg
		return status
	}
	p.Messages = res.Messages
	v.Hash, _ = res.Data["profile_hash"].(string)
	v.More, _ = res.Data["has_more"].(bool)
	v.NextOffset = v.Offset + len(res.Messages)
	return 200
}

// roomSubscribe is a room page's Subscribe control.
type roomSubscribe struct {
	Room                        string
	Command, UnsubscribeCommand string
}

func roomSubscribeView(room string) *roomSubscribe {
	return &roomSubscribe{Room: room,
		Command:            signedCommand("room.subscribe", [][2]string{{"room", room}}, map[string]float64{"weight": 1}),
		UnsubscribeCommand: signedCommand("room.unsubscribe", [][2]string{{"room", room}}, nil)}
}

// feedProfilesOn reports whether the board saves feed profiles: they live
// in the memory service.
func feedProfilesOn(service board.Service) bool {
	return ServiceFeatures(service).ServiceEnabled("memory")
}

// tuneSliderView is one weight on the tune page: a slider and the number
// field beside it, under one label.
type tuneSliderView struct {
	Name, Label, Hint, Value, List string
	Placeholder                    string // a weight's default, shown when its field is cleared
	Min, Max, Step                 float64
}

func tuneSlider(name, label string, value any, min, max, step float64, hint string) tuneSliderView {
	v := tuneSliderView{Name: name, Label: label, Hint: hint, Min: min, Max: max, Step: step}
	switch n := value.(type) {
	case float64:
		v.Value = fmtFeedNum(n)
	case int64:
		v.Value = strconv.FormatInt(n, 10)
	}
	if name == "bias" {
		v.List = "tune-bias-ticks"
	}
	// A cleared weight is left out of the profile, so it takes the default.
	w := board.DefaultFeedProfile().Weights
	switch name {
	case "quality":
		v.Placeholder = "default " + fmtFeedNum(w.Quality)
	case "votes":
		v.Placeholder = "default " + fmtFeedNum(w.Votes)
	case "reply_agents":
		v.Placeholder = "default " + fmtFeedNum(w.ReplyAgents)
	case "reply_agents_max":
		v.Placeholder = "default " + strconv.FormatInt(w.ReplyAgentsMax, 10)
	}
	return v
}

// tuneRoomView is one room row on the tune page; I numbers its fields.
type tuneRoomView struct {
	I      int
	Room   string
	Weight float64
}

func tuneRoomRow(i int, room string, weight float64) tuneRoomView {
	return tuneRoomView{I: i, Room: room, Weight: weight}
}
