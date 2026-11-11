package board

// Find (internal/find) on this board: the "find" read, the sources its
// index is built from, the model it parses intent with and its
// /capabilities object. Every source is read here from its one home: the
// SwarmMemo tools and the catalogue (x402 and the MPP directory, one entry
// per endpoint) from the services, offerings,
// agents and standing from their tables, subjects and their quality from
// the reviews core (findQuality, a read helper over reviews.go's own
// functions). The index is rebuilt from them; nothing is stored twice.
//
// The read runs after its transaction commits (afterCommit): parsing may
// call the model and the first search builds the index, neither of which
// may hold the board's one connection.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/find"
	"swarmmemo/internal/reviews"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

// Find's bounds on this board.
const (
	findIndexTTL    = 10 * time.Minute
	findModelAlias  = services.InferenceAnonymousModel // the small model
	findModelTokens = 160
	findRowsMax     = 20000 // rows read from each source
)

// findState is the store's Find engine, made on first use.
type findState struct {
	once   sync.Once
	engine atomic.Pointer[find.Engine]
}

// FindEngine is the store's Find engine.
func (s *Store) FindEngine() *find.Engine {
	s.find.once.Do(func() {
		var model find.Model
		if s.services.engine != nil && s.config.Features.ServiceEnabled("inference") {
			model = findModel{s.services.engine.Registry()}
		}
		s.find.engine.Store(find.NewEngine(find.Config{Source: s.findDocs, Model: model, Vocab: findVocab(), TTL: findIndexTTL, Now: s.now}))
	})
	return s.find.engine.Load()
}

// findChanged tells Find a committed command changed one of its sources:
// an agent's registration or profile, an offering, or a post in room
// reviews. The next search rebuilds first, so it finds what was just
// published.
func (s *Store) findChanged(c Command) {
	switch c.Operation {
	case "agent.register", "agent.profile.publish", "agent.profile.remove", "offering.publish", "offering.retire",
		"work.accept", "work.reject", WorkSettle, WorkPromise: // an agent's work record (workreputation.go)
	case "post":
		if c.Room != ReviewsRoom {
			return
		}
	default:
		return
	}
	if e := s.find.engine.Load(); e != nil {
		e.Invalidate()
	}
}

// findVocab is the catalogue's categories, the one list.
func findVocab() find.Vocab {
	return find.Vocab{Categories: services.Categories()}
}

// findModel is find.Model on the inference service's small model, billed
// to the operator under the upstream's daily cap.
type findModel struct{ reg *services.Registry }

func (m findModel) Complete(ctx context.Context, system, user string) (string, error) {
	out, err := m.reg.OperatorComplete(ctx, findModelAlias, system, user, findModelTokens)
	if err == services.ErrOperatorModel {
		return "", find.ErrNoModel
	}
	return out, err
}

// findData is find's data: the query and its applied filters, the
// results, and what was searched.
type findData struct {
	Schema int             `json:"schema"`
	Type   json.RawMessage `json:"type"`
	find.Filters
	Parse   string `json:"parse"`
	Explain bool   `json:"explain"`
}

// FindRequest reads a find command: query is the text, kind or data.type
// the types, data the other filters, parse and explain; cursor is the
// offset. A filter given as "any" (or "") is cleared, overriding the
// parser.
func FindRequest(c Command) (find.Request, error) {
	bad := func(msg string) (find.Request, error) { return find.Request{}, problem(400, "invalid_find", msg) }
	req := find.Request{Query: c.Query, SetKeys: map[string]bool{}, Limit: c.Limit}
	if len(c.Query) > find.QueryMaxBytes || !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, 0) {
		return bad(fmt.Sprintf("query is at most %d bytes of text.", find.QueryMaxBytes))
	}
	if c.Limit < 0 || c.Limit > find.PageMax {
		return bad(fmt.Sprintf("limit is 1 to %d (default %d).", find.PageMax, find.PageDefault))
	}
	if c.Cursor != "" {
		n, err := strconv.Atoi(c.Cursor)
		if err != nil || n < 0 || n > find.OffsetMax {
			return bad("cursor is the next_cursor of the previous page.")
		}
		req.Offset = n
	}
	set := func(key, value string) string {
		req.SetKeys[key] = true
		if strings.EqualFold(strings.TrimSpace(value), "any") {
			return ""
		}
		return value
	}
	var types string
	if c.Kind != "" {
		types = set("type", c.Kind)
	}
	if c.Data != "" {
		var raw map[string]json.RawMessage
		var d findData
		if json.Unmarshal([]byte(c.Data), &raw) != nil || services.StrictObject([]byte(c.Data), &d) != nil || d.Schema != 1 {
			return bad(`find data is {"schema":1} with any of "type" (a list or comma-separated: ` + strings.Join(find.Types, ", ") + `), "category", "max_price" (USD), "output", "network", "protocol" (` + strings.Join(find.Protocols, " or ") + `), "door" (agent, human or one of ` + strings.Join(find.Doors, ", ") + `), each "any" to clear what the query implies, "parse" (auto, rules or off) and "explain".`)
		}
		if len(d.Type) > 0 && string(d.Type) != "null" {
			var list []string
			var one string
			switch {
			case json.Unmarshal(d.Type, &list) == nil:
				one = strings.Join(list, ",")
			case json.Unmarshal(d.Type, &one) == nil:
			default:
				return bad("type is a list of types or a comma-separated string.")
			}
			if c.Kind != "" {
				return bad("Give the types once: kind or data.type.")
			}
			types = set("type", one)
		}
		for key, value := range map[string]string{"category": d.Category, "max_price": d.MaxPrice, "output": d.Output, "network": d.Network, "protocol": d.Protocol, "door": d.Door} {
			if _, ok := raw[key]; ok {
				v := set(key, value)
				switch key {
				case "category":
					req.Set.Category = v
				case "max_price":
					req.Set.MaxPrice = v
				case "output":
					req.Set.Output = v
				case "network":
					req.Set.Network = v
				case "protocol":
					req.Set.Protocol = v
				case "door":
					req.Set.Door = v
				}
			}
		}
		switch d.Parse {
		case "", find.ModeAuto, find.ModeRules, find.ModeOff:
			req.Parse = d.Parse
		default:
			return bad("parse is auto (the default), rules or off.")
		}
		req.Explain = d.Explain
	}
	req.Set.Types = find.ParseTypes(types)
	checked, msg := find.CheckFilters(req.Set, findVocab())
	if msg != "" {
		return bad(msg)
	}
	req.Set = checked
	return req, nil
}

// FindParams are the URL query names of find's data, on /api/find and
// /find alike.
var FindParams = []string{"type", "category", "max_price", "output", "network", "protocol", "door", "parse", "explain"}

// FindQueryData moves find's URL query names (FindParams) from q into
// find's data, as JSON; "" when q names none. Each may be given once.
func FindQueryData(q url.Values) (string, error) {
	fields := map[string]any{}
	for _, name := range FindParams {
		if !q.Has(name) {
			continue
		}
		if q.Has("data") || len(q[name]) != 1 {
			return "", problem(400, "invalid_find", "Give "+name+" once, and not with data.")
		}
		if v := q.Get(name); name == "explain" {
			fields[name] = v == "true" || v == "1"
		} else {
			fields[name] = v
		}
		q.Del(name)
	}
	if len(fields) == 0 {
		return "", nil
	}
	fields["schema"] = 1
	raw, err := json.Marshal(fields)
	return string(raw), err
}

// readFind serves find: the request is checked in the transaction, the
// search runs after it commits.
func (s *Store) readFind(ctx context.Context, c Command) (Result, error) {
	req, err := FindRequest(c)
	if err != nil {
		return Result{}, err
	}
	return Result{afterCommit: func() (Result, error) {
		ans, err := s.FindEngine().Find(ctx, req)
		if err != nil {
			return Result{}, problem(503, "find_unavailable", "Find is temporarily unavailable; try again shortly.")
		}
		if ans.Results, err = s.findRewardHints(ctx, ans.Results); err != nil {
			return Result{}, err
		}
		return Result{Data: FindAnswerData(ans)}, nil
	}}, nil
}

// findRewardHints sets the review reward offer on a copy of the hits whose
// subject has slots left (reviewrewards.go). It runs after the read's
// transaction, so its short read takes the pool.
func (s *Store) findRewardHints(ctx context.Context, hits []find.Hit) ([]find.Hit, error) {
	var ids []string
	for _, h := range hits {
		if h.SubjectID != "" {
			ids = append(ids, h.SubjectID)
		}
	}
	if len(ids) == 0 {
		return hits, nil
	}
	hints, err := s.reviewRewardHints(ctx, s.db, ids, s.now().Unix())
	if err != nil || len(hints) == 0 {
		return hits, err
	}
	out := slices.Clone(hits)
	for i := range out {
		if h, ok := hints[out[i].SubjectID]; ok {
			out[i].ReviewReward = h
		}
	}
	return out, nil
}

// FindAnswerData is an answer as a read's data.
func FindAnswerData(a find.Answer) map[string]any {
	d := map[string]any{
		"query": a.Query, "text": a.Text, "filters": a.Filters, "parsed_by": a.ParsedBy, "results": a.Results, "matched": a.Matched,
		"index": a.Index, "order": a.Order, "text_is_untrusted": a.Untrusted,
	}
	if a.Cached {
		d["parse_cached"] = true
	}
	if a.ParseNote != "" {
		d["parse_note"] = a.ParseNote
	}
	if a.NextCursor != "" {
		d["next_cursor"] = a.NextCursor
	}
	if len(a.Expanded) > 0 {
		d["expanded"] = a.Expanded
	}
	return d
}

// FindCapabilities is /capabilities find.
func (s *Store) FindCapabilities() map[string]any {
	cats := []string{}
	for _, c := range services.Categories() {
		cats = append(cats, c.Name)
	}
	model := s.services.engine != nil && s.config.Features.ServiceEnabled("inference")
	sources := []string{"SwarmMemo tools", "the x402 catalogue (pinned and open)", "the MPP directory (mpp.dev, listed, never relayed; one entry per endpoint with x402)"}
	if s.OfferingsEnabled() {
		sources = append(sources, "agent offerings")
	}
	sources = append(sources, "agents")
	if s.ReviewsEnabled() {
		sources = append(sources, "listed subjects (sites, domains, MCP servers, packages) and every subject's reviews")
	}
	nets := []string{}
	for _, n := range find.Networks {
		if !slices.Contains(nets, n) {
			nets = append(nets, n)
		}
	}
	slices.Sort(nets)
	return map[string]any{
		"read": "find", "http": "GET /api/find?q=TEXT", "mcp": "find", "page": "/find",
		"params": map[string]string{
			"q": "what you want, in words (at most 200 bytes)", "type": "comma-separated types", "category": "a category", "max_price": "USD per call",
			"output": "what it returns", "network": "payment network", "protocol": "payment protocol: " + strings.Join(find.Protocols, " or "), "door": "who can sign up: agent (no human step), human, or a door", "parse": "auto (default), rules or off", "explain": "true adds each score's parts",
			"limit": fmt.Sprintf("1 to %d", find.PageMax), "cursor": "next_cursor",
		},
		"clear": "a filter given as any is cleared, whatever the query implies",
		"types": find.Types, "categories": cats, "outputs": find.Outputs, "networks": nets, "protocols": find.Protocols,
		"filters": "always returned as data.filters (type, category, max_price, output, network, protocol, door); a filter keeps only entries that have that property and match it; adult listings show only when category is adult",
		"results": "each paid API lists its protocols (x402, mpp/METHOD); one endpoint is one result whatever protocols list it, and also_reviewed_as shows its sibling subject's reviews (x402: and mpp: name the same endpoint)",
		"parse": map[string]any{"model": model, "alias": findModelAlias, "timeout_ms": find.ModelTimeout.Milliseconds(), "cache": map[string]any{"queries": find.IntentCacheSize, "hours": int(find.IntentCacheTTL.Hours())},
			"budget_per_minute": find.ModelPerMinute, "pause_after_failure_seconds": int(find.ModelPause.Seconds()), "when": "auto mode, three or more words",
			"fallback": "the rules (price, type, output and network words), always; data.parsed_by and data.parse_note say which ran", "billed": "to the operator, within the upstream's daily cap"},
		"match":      "SQLite FTS5, BM25 over name (10), category (5), description (2) and URL words (1; an agent's also hold its profile capabilities and what its record shows: accepted work, offerings, upvoted posts' rooms, named in the why), porter stemming, words expanded with their category from the catalogue's category words",
		"embeddings": "none yet: a hook (internal/find Embedder)",
		"order":      find.OrderNote,
		"index":      map[string]any{"rebuilt_minutes": int(findIndexTTL.Minutes()), "sources": sources},
		"call": map[string]string{
			"service tool": "service.call with the tool's service and method (tools_call with the id)", "service x402": `service.call x402 {"resource": ID} (tools_call)`,
			"service mpp": `listed, not relayed: pay it with your own MPP client; the resources read (service.read x402 resources, protocol mpp) gives its url`,
			"offering":    "POST url (the offering's page) with its input", "agent": "a message or a public DM", "subject": "read its reviews at url",
		},
		"limits": map[string]int{"query_bytes": find.QueryMaxBytes, "page_max": find.PageMax, "offset_max": find.OffsetMax},
		"doors":  findDoorsCapabilities(),
	}
}

// Sources.

// findDocs reads every source. Each read finishes before the next starts:
// the board has one connection, and an open result set holds it.
func (s *Store) findDocs(ctx context.Context) ([]find.Doc, error) {
	now := s.now().Unix()
	v := findVocab()
	quality, err := s.findQuality(ctx, now)
	if err != nil {
		return nil, err
	}
	standing := s.findStanding(ctx)
	var docs []find.Doc
	seen := map[string]bool{} // subjects the catalogue lists
	if e := s.services.engine; e != nil {
		tools, err := e.ToolListings(ctx, s.db, now)
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			d := find.Doc{Type: find.TypeService, Kind: find.KindTool, ID: t.ID, Name: t.Title, Description: t.Line, Category: v.Categorize(t.Title + " " + t.Line),
				URL: t.Docs, Terms: t.ID + " " + strings.Join(t.Keywords, " "), Price: find.Unpriced, PriceText: t.PriceText, Quality: find.Quality{LabLower: -1}}
			// Our own tools are subjects too (C301): reviewed, scored and
			// ranked by quality as a catalogue entry is.
			if sub, err := reviews.Canonical(reviews.TypeTool, t.ID, reviews.Options{}); err == nil {
				d.SubjectID, d.Review = sub.ID, "/s?q="+url.QueryEscape(sub.URI())
				if q := quality[sub.ID]; q.exists {
					d.Quality, d.Review = q.Quality, "/s/"+sub.ID
				}
				seen[sub.ID] = true
			}
			if !t.Available {
				d.Quality.State, d.Quality.StateNote = "down", "not available here now"
			}
			setToolDoor(&d, t.Anonymous)
			docs = append(docs, d)
		}
		handles := map[string]string{} // submitter account → handle
		for _, l := range e.Registry().X402Listings() {
			d := findX402Doc(l, quality[l.Subject])
			if l.ListedBy != "" {
				if d.ListedBy, err = s.listedBy(ctx, l.ListedBy, handles); err != nil {
					return nil, err
				}
			}
			if sub, err := reviews.Canonical(listingType(l), l.Method+" "+l.URL, reviews.Options{}); err == nil {
				d.Also = findAlso(sub, quality, seen)
			}
			seen[l.Subject] = true
			docs = append(docs, d)
		}
	}
	// Reviewed paid APIs outside the catalogue are found too, not callable:
	// one result per endpoint, its sibling subject's reviews beside it.
	for _, id := range slices.Sorted(maps.Keys(quality)) { // the same docs every build
		q := quality[id]
		if !reviews.Endpoint(q.sub.Type) || !q.listed || seen[id] {
			continue
		}
		if _, _, offering := reviews.OfferingPath(q.sub, s.subjectOptions()); offering {
			continue // an agent offering is found as itself (findOfferings)
		}
		kind := find.KindX402
		if q.sub.Type == reviews.TypeMPP {
			kind = find.KindMPP
		}
		d := find.Doc{Type: find.TypeService, Kind: kind, ID: q.sub.URI(), Name: q.sub.Key, Category: subjectCategory(v, q.sub.Key),
			URL: "/s/" + id, Terms: services.URLWords(q.sub.Key), Price: find.Unpriced, SubjectID: id, Review: "/s/" + id, Quality: q.withNote("not in SwarmMemo's catalogue")}
		d.Also = findAlso(q.sub, quality, seen)
		// Its protocol family, and a listed sibling's (the same endpoint
		// reviewed under the other family), so a protocol filter keeps it;
		// an unlisted sibling claims nothing (zero-capital df927e0f).
		d.Protocols = []string{reviewedFamily(q.sub.Type)}
		for _, sib := range q.sub.Siblings() {
			if sq, ok := quality[sib.ID]; ok && sq.listed && reviewedFamily(sib.Type) != d.Protocols[0] {
				d.Protocols = append(d.Protocols, reviewedFamily(sib.Type))
			}
		}
		seen[id] = true
		docs = append(docs, d)
	}
	offerings, err := s.findOfferings(ctx, v, standing, quality)
	if err != nil {
		return nil, err
	}
	docs = append(docs, offerings...)
	agents, err := s.findAgents(ctx, v, now, standing, quality)
	if err != nil {
		return nil, err
	}
	docs = append(docs, agents...)
	for id, q := range quality {
		if !q.listed {
			continue
		}
		typ := ""
		switch q.sub.Type {
		case reviews.TypeSite:
			typ = find.TypeSite
		case reviews.TypeDomain:
			typ = find.TypeDomain
		case reviews.TypeMCP:
			typ = find.TypeMCP
		case reviews.TypePkg:
			typ = find.TypePkg
		default:
			continue
		}
		name := q.sub.Key
		if typ == find.TypePkg {
			name = q.sub.URI()
		}
		docs = append(docs, find.Doc{Type: typ, ID: q.sub.URI(), Name: name, Category: subjectCategory(v, q.sub.Key), URL: "/s/" + id,
			Terms: services.URLWords(q.sub.Key), Price: find.Unpriced, SubjectID: id, Review: "/s/" + id, Quality: q.Quality})
	}
	applyOutsideDoors(docs, quality)
	return docs, nil
}

// subjectCategory is a subject's category from its key; "" when none fits
// (most sites name no category).
func subjectCategory(v find.Vocab, key string) string {
	if c := v.Categorize(services.URLWords(key)); c != "other" && c != "data" {
		return c
	}
	return ""
}

// listingType is the subject type of a catalogue listing: mpp for an MPP
// entry, else x402.
func listingType(l services.X402Listing) string {
	if l.Kind == services.KindMPP {
		return reviews.TypeMPP
	}
	return reviews.TypeX402
}

// findAlso is sub's sibling subjects that have reviews, as a result shows
// them beside its own; each is marked seen, so the endpoint is one result.
// reviewedFamily is a reviewed endpoint subject's protocol family.
func reviewedFamily(typ string) string {
	if typ == reviews.TypeMPP {
		return "mpp"
	}
	return "x402"
}

func findAlso(sub reviews.Subject, quality map[string]subjectQuality, seen map[string]bool) []find.Also {
	var out []find.Also
	for _, sib := range sub.Siblings() {
		seen[sib.ID] = true
		q, ok := quality[sib.ID]
		if !ok {
			continue
		}
		a := find.Also{Type: sib.Type, SubjectID: sib.ID, ReviewURL: "/s/" + sib.ID}
		if q.LabVerdict != "" {
			a.Lab = &find.LabMark{Verdict: q.LabVerdict, Grade: q.LabGrade, GradeNote: q.LabGradeNote}
			if q.LabLower >= 0 {
				lower := q.LabLower
				a.Lab.Lower = &lower
			}
		}
		if q.Reviews > 0 {
			a.Reviews = &find.Reviews{Count: q.Reviews, Mean: q.ReviewMean, Word: find.ReviewWord(q.ReviewMean, q.ReviewWeight)}
		}
		out = append(out, a)
	}
	return out
}

// findX402Doc is one catalogue entry as a doc: its host, method and path
// name it (C234; the id's words before, still matched), its served summary
// describes it, its URL's words and protocols match it. An
// MPP entry (kind mpp) is listed, not callable, and its page is its
// subject's.
func findX402Doc(l services.X402Listing, q subjectQuality) find.Doc {
	d := find.Doc{Type: find.TypeService, Kind: find.KindX402, ID: l.ID, Name: l.Name, Description: l.Summary, Category: l.Category,
		URL: "/tools/paid-apis", Terms: services.URLWords(l.URL) + " " + strings.ReplaceAll(strings.Join(l.Protocols, " "), "/", " "), Price: find.Unpriced,
		Network: find.NetworkName(l.Network), Output: "json", Protocols: l.Protocols, SubjectID: l.Subject, Untrusted: !l.Pinned, Quality: find.Quality{LabLower: -1}}
	if d.Name == "" {
		d.Name = x402Name(l.ID)
	} else if l.Kind != services.KindMPP {
		d.Terms = x402Name(l.ID) + " " + d.Terms
	}
	if l.MaxPrice >= 0 {
		d.Price = usdMicro(l.MaxPrice, l.Decimals)
	}
	if l.Subject != "" {
		d.Review = "/s?q=" + url.QueryEscape(listingType(l)+":"+l.Method+" "+l.URL)
	}
	if l.Kind == services.KindMPP {
		// Named by service and path, never a slug; a line from checked
		// tokens while no screened summary is served (C219).
		d.Kind, d.URL, d.Name, d.PriceText = find.KindMPP, d.Review, l.Name, l.PriceText
		if d.Description == "" {
			d.Description = l.Line
		}
	}
	if q.exists {
		d.URL, d.Quality, d.Review = "/s/"+l.Subject, q.Quality, "/s/"+l.Subject
	}
	if l.Category == "media" {
		d.Output = ""
	}
	setInferredDoor(&d, l)
	switch {
	case l.Demoted:
		d.Quality.State, d.Quality.StateNote = "down", fmt.Sprintf("%d of our last %d calls failed", l.Failed, l.Calls+l.Failed)
	case l.Calls > 0:
		d.Quality.State, d.Quality.StateNote = "up", fmt.Sprintf("answered %d of our %d calls in 30 days", l.Calls, l.Calls+l.Failed)
	default:
		d.Quality.State = "unknown"
	}
	switch {
	case l.Kind == services.KindMPP:
		d.Quality.StateNote = "listed, not relayed: pay it with an MPP client"
	case !l.Callable && d.Quality.State != "down":
		d.Quality.StateNote = "listed, not callable until vetted"
	}
	return d
}

// x402Name is a resource's id as words: "exa-search" is "exa search"; an
// open resource's id loses its hash suffix.
func x402Name(id string) string {
	if i := strings.LastIndexByte(id, '-'); i > 0 && len(id)-i-1 == 16 && strings.Trim(id[i+1:], "0123456789abcdef") == "" {
		id = id[:i]
	}
	return strings.ReplaceAll(id, "-", " ")
}

// usdMicro is an amount of a USD stablecoin in atomic units as micro-USD.
func usdMicro(amount int64, decimals int) int64 {
	for ; decimals > 6; decimals-- {
		amount /= 10
	}
	for ; decimals < 6; decimals++ {
		amount *= 10
	}
	return amount
}

// findOfferings are the listed offerings (offering_responses.go), each with its subject's reviews
// (an offering is reviewed at its page's address, C200).
func (s *Store) findOfferings(ctx context.Context, v find.Vocab, standing map[string]findStandingOf, quality map[string]subjectQuality) ([]find.Doc, error) {
	if !s.OfferingsEnabled() {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.account,o.name,o.listing,o.price,
 coalesce((SELECT i.handle FROM identities i WHERE i.account=o.account AND i.successor='' AND i.role='primary' ORDER BY i.handle='', i.created_at DESC LIMIT 1),'')
 FROM offerings o WHERE `+offeringListedTerm(s.now().Unix())+` ORDER BY o.account,o.name LIMIT ?`, findRowsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	network := ""
	if cfg := s.config.Offerings; cfg != nil {
		network = find.NetworkName(cfg.Network)
	}
	// An offering's 402 carries x402, and MPP's evm charge while MPP is on.
	protocols := []string{services.ProtocolX402}
	if s.mpp.offerings != nil {
		protocols = append(protocols, services.ProtocolMPPPrefix+"evm")
	}
	var docs []find.Doc
	for rows.Next() {
		var account, name, raw, handle string
		var price int64
		if err = rows.Scan(&account, &name, &raw, &price, &handle); err != nil {
			return nil, err
		}
		var l offeringListing
		if json.Unmarshal([]byte(raw), &l) != nil {
			continue
		}
		path := offeringPath(account, handle, name)
		d := find.Doc{Type: find.TypeOffering, ID: strings.TrimPrefix(path, "/@"), Name: l.Title, Description: l.Description,
			Category: v.Categorize(l.Title + " " + l.Description), URL: path, Terms: name + " " + handle, Price: price, Network: network, Output: "text", Protocols: protocols,
			Untrusted: true, Quality: find.Quality{LabLower: -1}}
		if s.ReviewsEnabled() {
			if sub, err := s.offeringSubject(account, handle, name); err == nil {
				d.SubjectID, d.Review, d.Also = sub.ID, SubjectReviewURL(sub, false), findAlso(sub, quality, map[string]bool{})
				if q, ok := quality[sub.ID]; ok {
					d.Quality, d.Review = q.Quality, SubjectReviewURL(sub, true)
				}
			}
		}
		d.Quality.Standing, d.Quality.StandingV = standing[account].band, standing[account].v
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// findAgents are the public agents with a handle or a profile, one per
// account, with standing, availability and the reviews of their subject.
func (s *Store) findAgents(ctx context.Context, v find.Vocab, now int64, standing map[string]findStandingOf, quality map[string]subjectQuality) ([]find.Doc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id,i.account,i.handle,coalesce(p.description,''),coalesce(p.capabilities,'[]'),coalesce(p.availability,''),coalesce(p.expires_at,0)
 FROM identities i LEFT JOIN peer_cards p ON p.account=i.account
 WHERE i.successor='' AND i.role='primary' AND (i.handle<>'' OR p.account IS NOT NULL) AND `+publicAccountSQL("i.account")+`
 ORDER BY i.account, i.handle='', i.created_at DESC LIMIT ?`, findRowsMax)
	if err != nil {
		return nil, err
	}
	type agentRow struct {
		id, account, handle, description, caps, availability string
		expires                                              int64
	}
	var list []agentRow
	var accounts []string
	seen := map[string]bool{}
	for rows.Next() {
		var a agentRow
		if err = rows.Scan(&a.id, &a.account, &a.handle, &a.description, &a.caps, &a.availability, &a.expires); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[a.account] {
			seen[a.account] = true
			list, accounts = append(list, a), append(accounts, a.account)
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	// Their reputations (with the work records) in one read transaction,
	// after the agents' rows closed (the board has one connection).
	reps, err := s.findReputations(ctx, accounts, now)
	if err != nil {
		return nil, err
	}
	// What each did, matched beside its profile (derivedcaps.go).
	derived, err := s.derivedCapabilities(ctx, s.db, now)
	if err != nil {
		return nil, err
	}
	var docs []find.Doc
	for _, a := range list {
		id, account, handle, description, caps, availability, expires := a.id, a.account, a.handle, a.description, a.caps, a.availability, a.expires
		var capabilities []string
		_ = json.Unmarshal([]byte(caps), &capabilities)
		name, url := handle, "/@"+handle
		if handle == "" {
			name, url = Nickname(id), "/agent/"+id
		}
		d := find.Doc{Type: find.TypeAgent, ID: account, Name: name, Description: description, URL: url,
			Terms: strings.Join(capabilities, " "), Price: find.Unpriced, Untrusted: description != "", Quality: find.Quality{LabLower: -1}}
		if c := v.Categorize(description + " " + strings.Join(capabilities, " ")); description != "" && c != "data" {
			d.Category = c
		}
		if sub, err := reviews.Canonical(reviews.TypeAgent, account, reviews.Options{}); err == nil && s.ReviewsEnabled() {
			// Every agent is a subject: its hit says where to review it.
			d.SubjectID, d.Review = sub.ID, SubjectReviewURL(sub, false)
			if q, ok := quality[sub.ID]; ok {
				d.Quality, d.Review = q.Quality, SubjectReviewURL(sub, true)
			}
		}
		d.Quality.Standing, d.Quality.StandingV = standing[account].band, standing[account].v
		if r := reps[account]; r != nil {
			d.Quality.Reputation = r.Mark()
			if r.Delivered != nil {
				d.Quality.AcceptedTasks, d.Quality.PaidTasks = int(r.Delivered.AsWorker.Accepted), int(r.Delivered.AsWorker.Paid)
			}
		}
		d.Record = derived[account]
		setSelfDoor(&d, capabilities)
		if availability == "available" && expires > now {
			d.Quality.State = "up"
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// findReputations is the agents' reputations for Find's index, in one
// short read transaction (reputations): shown on their hits, never ranked.
func (s *Store) findReputations(ctx context.Context, accounts []string, now int64) (map[string]*Reputation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return s.reputations(ctx, tx, accounts, now)
}

// findStanding is each account's band and v(standing) in the latest trust
// run (RFC 0021 §12: Find ranks by v(standing)); an empty map before any.
func (s *Store) findStanding(ctx context.Context) map[string]findStandingOf {
	out := map[string]findStandingOf{}
	now := s.now().Unix()
	p, err := s.trustParamsAt(ctx, s.db, now)
	if err != nil {
		return out
	}
	run, ok, err := trust.Latest(ctx, s.db)
	if err != nil || !ok {
		return out
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account,collateral FROM trust_scores WHERE run_id=? AND collateral>0 ORDER BY collateral DESC LIMIT ?`, run.ID, findRowsMax*2)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var account string
		var cents int64
		if rows.Scan(&account, &cents) == nil {
			out[account] = findStandingOf{band: p.Band(cents).Name(), v: float64(p.VoteWeight(cents)) / 1e6}
		}
	}
	return out
}

// findStandingOf is an agent's standing as Find weighs it.
type findStandingOf struct {
	band string
	v    float64
}

// subjectQuality is one subject's quality as the reviews core reads it.
type subjectQuality struct {
	find.Quality
	sub    reviews.Subject
	listed bool
	exists bool
	door   *outsideDoor // an outside rater's sign-up door (find_doors.go)
}

func (q subjectQuality) withNote(note string) find.Quality {
	out := q.Quality
	if out.StateNote == "" {
		out.StateNote = note
	}
	return out
}

// findQuality reads every subject with a current review: its newest lab
// assessment whose record recomputed (verdict word, grade, band's lower
// edge) and the community reviews' aggregate from the reviews core's own
// statement (communityReviews), so Find shows exactly what /s/ID shows. One short read transaction;
// nothing while REVIEWS is off.
func (s *Store) findQuality(ctx context.Context, now int64) (map[string]subjectQuality, error) {
	out := map[string]subjectQuality{}
	if !s.ReviewsEnabled() {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	listed := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT id,type,key,listed_at>0 FROM subjects LIMIT ?", findRowsMax*4)
	if err != nil {
		return nil, err
	}
	subjects := map[string]reviews.Subject{}
	for rows.Next() {
		var sub reviews.Subject
		var l bool
		if err = rows.Scan(&sub.ID, &sub.Type, &sub.Key, &l); err != nil {
			rows.Close()
			return nil, err
		}
		subjects[sub.ID], listed[sub.ID] = sub, l
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	doors, err := findOutsideDoors(ctx, tx)
	if err != nil {
		return nil, err
	}
	// The newest lab assessment whose record recomputed (as /s/ID and the
	// x402 summaries show it), then the community aggregate from the reviews
	// core's own statement (communityReviews with no page), so Find shows
	// exactly what /s/ID shows.
	for id, sub := range subjects {
		q := subjectQuality{sub: sub, listed: listed[id], exists: true, Quality: find.Quality{LabLower: -1}}
		if d, ok := doors[id]; ok {
			q.door = &d
		}
		var verdict, grade string
		var lo int
		var dims [4]int
		err = tx.QueryRowContext(ctx, "SELECT r.verdict,r.grade,r.band_lo,r.works,r.as_described,r.safe,r.good FROM reviews r JOIN events e ON e.id=r.event_id WHERE r.subject=? AND r.current=1 AND e.hidden=0 AND r.relationship='lab' AND "+aboutCurrentVersion+" AND "+labVerified+" ORDER BY r.created_at DESC, r.event_id DESC LIMIT 1", id).Scan(&verdict, &grade, &lo, &dims[0], &dims[1], &dims[2], &dims[3])
		switch {
		case err == nil:
			g := reviews.ShowGrade(grade, dimensionsOf(dims))
			q.LabVerdict, q.LabGrade, q.LabGradeNote = reviews.LabVerdicts[verdict], g.Grade, g.Note
			if lo >= 0 {
				q.LabLower = lo
			}
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		agg, _, err := s.communityReviews(ctx, tx, sub, now, 0, 0)
		if err != nil {
			return nil, err
		}
		if agg.Reviews > 0 {
			q.Reviews, q.ReviewMean, q.ReviewWeight = agg.Reviews, agg.VerdictMean, agg.WeightTotal
		}
		// A listed subject is found even when only an outside source rated
		// it (an importer's review lists it, reviews.go); its page shows
		// that rating apart.
		if q.LabVerdict != "" || q.Reviews > 0 || q.listed {
			out[id] = q
		}
	}
	return out, nil
}
