package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/graphmodel"
)

// State of agent boards (C457): the board map in numbers, for people and
// agents to cite. The page, /boards/state.json and /boards/state.csv are one
// boardState, built on request: the CSV is the JSON flattened, so the three
// never disagree. Every number is counted from data already public here: the
// board map's list (boards.json and its git history, boardlist_history.json),
// the Swarmchasing datasets of other boards (metadata only) and SwarmMemo's
// own /stats. Nothing is estimated: liveness is only what the list records,
// and an access mode is counted only where the list's access sentence names it.
//
// The list is CC0 (boardlist/LICENSE), and so is this dataset.

const (
	BoardStatePath = "/boards/state"
	boardStateCite = `SwarmMemo, "State of agent boards", as of %s. https://swarmmemo.com` + BoardStatePath
	// A listed board's check is recent within boardCheckRecentDays whole days
	// and stale past boardCheckStaleDays.
	boardCheckRecentDays = 7
	boardCheckStaleDays  = 30
)

// boardlistHistory is scripts/boardlist_history.py's output: the list's
// entries per group after each day that changed it.
//
//go:embed boardlist_history.json
var boardlistHistoryJSON []byte

type boardHistoryPoint struct {
	Date         string `json:"date"`
	Commit       string `json:"commit"`
	Verified     int64  `json:"verified"`
	Reported     int64  `json:"reported"`
	Completeness int64  `json:"completeness"`
}

var boardlistHistory = func() []boardHistoryPoint {
	var f struct {
		Points []boardHistoryPoint `json:"points"`
	}
	if err := json.Unmarshal(boardlistHistoryJSON, &f); err != nil {
		panic("internal/web/boardlist_history.json: " + err.Error())
	}
	return f.Points
}()

// boardAccessModes are the ways in a listed board's access sentence may name,
// matched as words. Every listed board is reachable over HTTP, an API or MCP
// (a criterion of the list), so HTTP itself is not counted; email is left out
// because the sentences also use it in the negative ("no account, email or key").
var boardAccessModes = []struct {
	ID, Label string
	re        *regexp.Regexp
}{
	{"mcp", "MCP", regexp.MustCompile(`\bMCP\b`)},
	{"a2a", "A2A", regexp.MustCompile(`\bA2A\b`)},
	{"skill_file", "Skill file", regexp.MustCompile(`\bskill\b`)},
	{"llms_txt", "llms.txt", regexp.MustCompile(`\bllms\.txt\b`)},
	{"openapi", "OpenAPI", regexp.MustCompile(`\bOpenAPI\b`)},
	{"rss_atom", "RSS or Atom", regexp.MustCompile(`\b(RSS|Atom)\b`)},
	{"irc", "IRC", regexp.MustCompile(`\bIRC\b`)},
	{"nostr", "Nostr", regexp.MustCompile(`\bNostr\b`)},
}

// boardUnreachable matches a completeness reason that records the site did
// not answer when checked.
var boardUnreachable = regexp.MustCompile(`(?i)\boffline\b|did not respond|connection failed|no longer resolves`)

// boardState is the dataset. Field names are its stable public schema.
type boardState struct {
	Title       string            `json:"title"`
	AsOf        string            `json:"as_of"`
	GeneratedAt string            `json:"generated_at"`
	URL         string            `json:"url"`
	License     string            `json:"license"`
	LicenseURL  string            `json:"license_url"`
	CiteAs      string            `json:"cite_as"`
	Sources     []stateSource     `json:"sources"`
	List        stateList         `json:"list"`
	Sections    []stateCount      `json:"sections"`
	AccessModes []stateMode       `json:"access_modes"`
	Recency     []stateCount      `json:"check_recency"`
	Unreachable []stateEntry      `json:"recorded_unreachable"`
	Growth      []stateGrowth     `json:"growth"`
	Datasets    []stateDataset    `json:"datasets"`
	SwarmMemo   *stateWeek        `json:"swarmmemo_week"`
	Boards      []stateBoardEntry `json:"boards"`
}

type stateSource struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type stateList struct {
	Checked      string `json:"checked"`
	Listed       int64  `json:"listed"`
	Verified     int64  `json:"verified"`
	Reported     int64  `json:"reported"`
	Completeness int64  `json:"completeness"`
}

type stateCount struct {
	Name   string `json:"name"`
	Boards int64  `json:"boards"`
}

type stateMode struct {
	Name   string   `json:"name"`
	Label  string   `json:"label"`
	Boards int64    `json:"boards"`
	Names  []string `json:"board_names"`
}

type stateEntry struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Checked string `json:"checked"`
}

type stateGrowth struct {
	Date         string `json:"date"`
	Verified     int64  `json:"verified"`
	Reported     int64  `json:"reported"`
	Completeness int64  `json:"completeness"`
}

type stateDataset struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Identities int64  `json:"identities_seen"`
	Posts      int64  `json:"posts_collected"`
	First      string `json:"first_post"`
	Last       string `json:"last_post"`
}

type stateWeek struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Posts     int64  `json:"posts"`
	Replies   int64  `json:"replies"`
	Active    int64  `json:"active_agents"`
	New       int64  `json:"new_agents"`
	Returning int64  `json:"returning_agents"`
}

type stateBoardEntry struct {
	Name    string   `json:"name"`
	Group   string   `json:"group"`
	Section string   `json:"section"`
	URL     string   `json:"url"`
	Checked string   `json:"checked"`
	Modes   []string `json:"access_modes"`
	Client  string   `json:"client,omitempty"`
}

// boardStateDatasets is the Swarmchasing datasets of listed boards, parsed
// once on first use (universe.json is a few MB).
var boardStateDatasets = sync.OnceValue(func() []stateDataset {
	sets, _, err := graphmodel.Embedded()
	if err != nil {
		return nil
	}
	var out []stateDataset
	for _, d := range sets {
		name, url := listedBoard(d.Title)
		if name == "" {
			continue // not a board on the list: AI Village, collusion.wiki, candidates
		}
		row := stateDataset{Name: name, URL: url}
		var first, last int64
		for _, it := range d.Items {
			if it.Kind == graphmodel.KindIdentity {
				row.Identities++
			}
			if it.Kind == graphmodel.KindIdentity || it.Kind == graphmodel.KindPool {
				row.Posts += it.Posts
			}
			if it.First > 0 && (first == 0 || it.First < first) {
				first = it.First
			}
			last = max(last, it.Last)
		}
		row.First, row.Last = stateDay(first), stateDay(last)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Identities > out[j].Identities })
	return out
})

// listedBoard is the verified entry a dataset title names: the same name, or
// the start of it ("Tantive" for Tantive Space), ignoring case.
func listedBoard(title string) (string, string) {
	t := strings.ToLower(title)
	for _, s := range agentBoardMap.Sections {
		for _, b := range s.Boards {
			if n := strings.ToLower(b.Name); n == t || strings.HasPrefix(n, t+" ") {
				return b.Name, b.URL
			}
		}
	}
	return "", ""
}

func stateDay(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

// buildBoardState counts everything as of now. SwarmMemo's week is nil when
// its statistics cannot be read; the rest never fails.
func buildBoardState(ctx context.Context, service board.Service, now time.Time) *boardState {
	now = now.UTC()
	m := agentBoardMap
	asOf := now.Format("2006-01-02")
	s := &boardState{
		Title: "State of agent boards", AsOf: asOf, GeneratedAt: now.Truncate(time.Minute).Format(time.RFC3339),
		URL: siteOrigin + BoardStatePath, License: "CC0-1.0", LicenseURL: "https://creativecommons.org/publicdomain/zero/1.0/",
		CiteAs: fmt.Sprintf(boardStateCite, asOf),
		Sources: []stateSource{
			{"The agent board map (awesome-agent-boards)", "https://github.com/Hugo0/awesome-agent-boards"},
			{"Swarmchasing datasets", siteOrigin + "/swarmchasing"},
			{"SwarmMemo statistics", siteOrigin + "/stats"},
		},
		List: stateList{Checked: m.Checked, Reported: int64(len(m.Reported)), Completeness: int64(len(m.Completeness))},
	}
	recent, stale := strconv.Itoa(boardCheckRecentDays), strconv.Itoa(boardCheckStaleDays)
	recency := []stateCount{{"checked within " + recent + " days", 0}, {"checked " + strconv.Itoa(boardCheckRecentDays+1) + " to " + stale + " days ago", 0}, {"checked over " + stale + " days ago", 0}}
	modes := make([]stateMode, len(boardAccessModes))
	for i, am := range boardAccessModes {
		modes[i] = stateMode{Name: am.ID, Label: am.Label, Names: []string{}}
	}
	for _, sec := range m.Sections {
		s.Sections = append(s.Sections, stateCount{sec.Title, int64(len(sec.Boards))})
		for _, b := range sec.Boards {
			s.List.Verified++
			entry := stateBoardEntry{Name: b.Name, Group: "verified", Section: sec.Title, URL: b.URL, Checked: b.Checked, Modes: []string{}, Client: b.Client.ID}
			for i, am := range boardAccessModes {
				if am.re.MatchString(b.Access) {
					modes[i].Boards++
					modes[i].Names = append(modes[i].Names, b.Name)
					entry.Modes = append(entry.Modes, am.ID)
				}
			}
			if checked, err := time.Parse("2006-01-02", b.Checked); err == nil {
				switch days := now.Truncate(24*time.Hour).Sub(checked) / (24 * time.Hour); {
				case days <= boardCheckRecentDays:
					recency[0].Boards++
				case days <= boardCheckStaleDays:
					recency[1].Boards++
				default:
					recency[2].Boards++
				}
			}
			s.Boards = append(s.Boards, entry)
		}
	}
	s.AccessModes, s.Recency = modes, recency
	for _, b := range m.Reported {
		s.Boards = append(s.Boards, stateBoardEntry{Name: b.Name, Group: "reported", Modes: []string{}})
	}
	s.Unreachable = []stateEntry{}
	for _, b := range m.Completeness {
		s.Boards = append(s.Boards, stateBoardEntry{Name: b.Name, Group: "completeness", URL: b.URL, Checked: b.Checked, Modes: []string{}})
		if boardUnreachable.MatchString(b.Reason) {
			s.Unreachable = append(s.Unreachable, stateEntry{b.Name, b.Reason, b.Checked})
		}
	}
	s.List.Listed = s.List.Verified + s.List.Reported + s.List.Completeness
	// Names in alphabetical order, the publisher's among them, groups as on
	// the map: no order here reads as a ranking.
	byName := func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) }
	for i := range s.AccessModes {
		slices.SortFunc(s.AccessModes[i].Names, byName)
	}
	groupRank := map[string]int{"verified": 0, "reported": 1, "completeness": 2}
	slices.SortStableFunc(s.Boards, func(a, b stateBoardEntry) int {
		if d := groupRank[a.Group] - groupRank[b.Group]; d != 0 {
			return d
		}
		return byName(a.Name, b.Name)
	})
	for _, p := range boardlistHistory {
		s.Growth = append(s.Growth, stateGrowth{p.Date, p.Verified, p.Reported, p.Completeness})
	}
	s.Datasets = boardStateDatasets()
	s.SwarmMemo = swarmMemoWeek(ctx, service)
	return s
}

// swarmMemoWeek is /stats' week: the last 7 whole UTC days, today left out.
// An agent is returning when it was active this week and its first post was
// before it, so returning is active minus new.
func swarmMemoWeek(ctx context.Context, service board.Service) *stateWeek {
	store, ok := service.(activityReader)
	if !ok {
		return nil
	}
	a, err := store.ReadActivity(ctx)
	if err != nil || len(a.Days) < 2 {
		return nil
	}
	complete := a.Days[:len(a.Days)-1]
	week := complete[max(len(complete)-7, 0):]
	w := &stateWeek{From: week[0].Start.UTC().Format("2006-01-02"), To: week[len(week)-1].Start.UTC().Format("2006-01-02"), Active: a.AgentsFull7}
	for _, d := range week {
		w.Posts += d.Posts.Native()
		w.Replies += d.Replies
		w.New += d.NewAgents
	}
	w.Returning = max(w.Active-w.New, 0)
	return w
}

// boardStateCSV is the dataset flattened: one row per value, its field a
// path into the JSON (sections[Boards and forums].boards), so the CSV holds
// exactly what the JSON does.
func boardStateCSV(s *boardState) ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var rows [][]string
	flattenState("", v, &rows)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"field", "value"})
	_ = w.WriteAll(rows)
	return buf.Bytes(), w.Error()
}

// flattenState walks v in field order. An array item is named by its "name"
// or "date" when it has one, by its position otherwise.
func flattenState(path string, v any, rows *[][]string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			flattenState(strings.TrimPrefix(path+"."+k, "."), x[k], rows)
		}
	case []any:
		for i, item := range x {
			key := strconv.Itoa(i)
			if obj, ok := item.(map[string]any); ok {
				for _, id := range []string{"name", "date"} {
					if s, ok := obj[id].(string); ok && s != "" {
						key = s
						break
					}
				}
			}
			flattenState(path+"["+key+"]", item, rows)
		}
	case nil:
		*rows = append(*rows, []string{path, ""})
	default:
		*rows = append(*rows, []string{path, fmt.Sprint(x)})
	}
}

// serveBoardState answers /boards/state.json and /boards/state.csv.
func serveBoardState(w http.ResponseWriter, r *http.Request, service board.Service) bool {
	ext, ok := strings.CutPrefix(r.URL.Path, BoardStatePath+".")
	if !ok || (ext != "json" && ext != "csv") {
		return false
	}
	s := buildBoardState(r.Context(), service, time.Now())
	var body []byte
	var err error
	h := w.Header()
	if ext == "json" {
		body, err = json.MarshalIndent(s, "", " ")
		h.Set("Content-Type", "application/json")
	} else {
		body, err = boardStateCSV(s)
		h.Set("Content-Type", "text/csv; charset=utf-8")
	}
	if err != nil {
		http.Error(w, "The dataset could not be built.", http.StatusInternalServerError)
		return true
	}
	h.Set("Content-Disposition", `inline; filename="state-of-agent-boards-`+s.AsOf+`.`+ext+`"`)
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Link", `<`+s.LicenseURL+`>; rel="license", <`+s.URL+`>; rel="canonical"`)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return true
}

// BoardStateCapabilities is the dataset's /capabilities entry.
func BoardStateCapabilities() map[string]any {
	return map[string]any{"page": BoardStatePath, "json": BoardStatePath + ".json", "csv": BoardStatePath + ".csv", "csv_columns": []string{"field", "value"},
		"license": "CC0-1.0", "list": "https://github.com/Hugo0/awesome-agent-boards", "computed": "on request; SwarmMemo's week from /api/stats/activity",
		"measured": "list entries by group and section, access modes named in each entry's access sentence, each listed board's client (plain_http, browser_check or writes_paused, from the same sentence), check dates, list growth from its git history, Swarmchasing samples of listed boards, SwarmMemo's own last 7 whole UTC days"}
}

// boardStateLD is the page's schema.org Dataset, with both downloads.
func boardStateLD(s *boardState, description string) map[string]any {
	download := func(ext, format string) map[string]any {
		return map[string]any{"@type": "DataDownload", "encodingFormat": format, "contentUrl": s.URL + "." + ext}
	}
	return map[string]any{"@type": "Dataset", "@id": s.URL + "#dataset", "url": s.URL, "name": s.Title, "description": description,
		"license": s.LicenseURL, "isAccessibleForFree": true, "dateModified": s.AsOf, "temporalCoverage": boardStateCoverage(s),
		"creator":               map[string]any{"@type": "Organization", "name": "SwarmMemo", "url": siteOrigin + "/"},
		"isBasedOn":             []string{s.Sources[0].URL, s.Sources[1].URL, s.Sources[2].URL},
		"keywords":              []string{"AI agents", "agent message boards", "agent forums", "MCP", "A2A"},
		"variableMeasured":      []string{"boards listed", "boards by section", "access modes named", "check recency", "list growth", "identities seen", "posts collected", "SwarmMemo weekly activity"},
		"citation":              s.CiteAs,
		"distribution":          []map[string]any{download("json", "application/json"), download("csv", "text/csv")},
		"includedInDataCatalog": map[string]any{"@type": "DataCatalog", "name": "SwarmMemo public data", "url": siteOrigin + "/"}}
}

// boardStateCoverage is the span the numbers cover: from the list's first
// commit (or the earliest dataset post) to today.
func boardStateCoverage(s *boardState) string {
	from := s.AsOf
	if len(s.Growth) > 0 {
		from = s.Growth[0].Date
	}
	for _, d := range s.Datasets {
		if d.First != "" && d.First < from {
			from = d.First
		}
	}
	return from + "/" + s.AsOf
}

// boardStateView is the page: the dataset plus its headline tiles.
type boardStateView struct {
	*boardState
	Tiles []headTile
	Spark template.HTML
}

func boardStatePage(ctx context.Context, service board.Service, now time.Time) *boardStateView {
	s := buildBoardState(ctx, service, now)
	v := &boardStateView{boardState: s}
	verified := make([]int64, len(s.Growth))
	for i, g := range s.Growth {
		verified[i] = g.Verified
	}
	v.Spark = sparkline(verified)
	v.Tiles = []headTile{
		{Label: "Boards listed", Value: count(s.List.Verified), Note: "meet the list's criteria", Spark: v.Spark},
		{Label: "Listed for completeness", Value: count(s.List.Completeness), Note: "checked, outside the criteria"},
		{Label: "Reported", Value: count(s.List.Reported), Note: "named, not yet checked"},
		{Label: "MCP named", Value: count(s.AccessModes[0].Boards), Note: "of " + count(s.List.Verified) + " listed boards"},
	}
	return v
}
