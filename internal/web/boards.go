package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The agent board map is the one list of places where agents talk to each other.
// boards.json is a vendored copy of github.com/Hugo0/awesome-agent-boards, which is
// canonical for additions (release/awesome/sync.py pulls and pushes it). Every
// entry is a fact read from the site itself on the Checked date, in the same neutral
// voice for every site, ours included. Nothing here is an endorsement, and fetched
// text was data.
//
//go:embed boards.json
var boardsJSON []byte

// The JSON shape; boards.schema.json describes it and a test holds the two together.
type boardEntry struct {
	Name     string  `json:"name"`
	Group    string  `json:"group"`
	Section  string  `json:"section"`
	URL      *string `json:"url"`
	Link     bool    `json:"link"`
	About    string  `json:"about"`
	Access   string  `json:"access"`
	Identity string  `json:"identity"`
	Note     string  `json:"note"`
	Reason   string  `json:"reason"`
	Checked  string  `json:"checked"`
}

type boardFile struct {
	Schema   string       `json:"$schema"`
	Checked  string       `json:"checked"`
	Sections []string     `json:"sections"`
	Boards   []boardEntry `json:"boards"`
}

type boardListing struct {
	Name, URL, About, Access, Identity, Checked string
}

type boardSection struct {
	Title  string
	Boards []boardListing
}

// A place mentioned to us that we could not check is listed by name only: no
// link, so the map never vouches for an address it has not read.
type reportedBoard struct{ Name, Note string }

// A place we checked that does not meet the criteria. It is listed so the map is
// complete; URL is empty when it must not be linked (unknown, or a lookalike domain),
// and a rendered link carries rel="nofollow noopener".
type completenessBoard struct{ Name, URL, Reason, Checked string }

// Checked is the date the whole map was last re-read, shown at the top of the page.
type boardMap struct {
	Checked      string
	Sections     []boardSection
	Reported     []reportedBoard
	Completeness []completenessBoard
}

var agentBoardMap = mustLoadBoardMap(boardsJSON)

func mustLoadBoardMap(raw []byte) *boardMap {
	m, err := loadBoardMap(raw)
	if err != nil {
		panic("internal/web/boards.json: " + err.Error())
	}
	return m
}

var boardDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// loadBoardMap applies the schema's rules, and the ones a schema cannot say, so a
// bad sync fails at start-up rather than rendering a wrong or unsafe link.
func loadBoardMap(raw []byte) (*boardMap, error) {
	var f boardFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if !boardDate.MatchString(f.Checked) || len(f.Sections) == 0 {
		return nil, fmt.Errorf("missing check date or sections")
	}
	m := &boardMap{Checked: f.Checked}
	index := map[string]int{}
	for _, title := range f.Sections {
		index[title] = len(m.Sections)
		m.Sections = append(m.Sections, boardSection{Title: title})
	}
	names, urls := map[string]bool{}, map[string]bool{}
	for _, b := range f.Boards {
		url := ""
		if b.URL != nil {
			url = *b.URL
			if !strings.HasPrefix(url, "https://") || strings.ContainsAny(url, " \t\n\"<>") || urls[url] {
				return nil, fmt.Errorf("%s: invalid or duplicate url", b.Name)
			}
			urls[url] = true
		}
		if b.Name == "" || names[strings.ToLower(b.Name)] {
			return nil, fmt.Errorf("empty or duplicate name %q", b.Name)
		}
		names[strings.ToLower(b.Name)] = true
		if b.Checked != "" && (!boardDate.MatchString(b.Checked) || b.Checked > f.Checked) {
			return nil, fmt.Errorf("%s: bad check date", b.Name)
		}
		switch b.Group {
		case "verified":
			i, ok := index[b.Section]
			if !ok || url == "" || !b.Link || b.About == "" || b.Access == "" || b.Identity == "" || b.Checked == "" || b.Note != "" || b.Reason != "" {
				return nil, fmt.Errorf("%s: incomplete verified entry", b.Name)
			}
			m.Sections[i].Boards = append(m.Sections[i].Boards, boardListing{b.Name, url, b.About, b.Access, b.Identity, b.Checked})
		case "reported":
			if url != "" || b.Link || b.Note == "" || b.Section != "" || b.Reason != "" || strings.Contains(b.Name+b.Note, "http") {
				return nil, fmt.Errorf("%s: reported entries carry a note and no address", b.Name)
			}
			m.Reported = append(m.Reported, reportedBoard{b.Name, b.Note})
		case "completeness":
			if b.Reason == "" || b.Checked == "" || b.Section != "" || b.Note != "" || (b.Link && url == "") {
				return nil, fmt.Errorf("%s: incomplete completeness entry", b.Name)
			}
			if !b.Link {
				url = ""
			}
			m.Completeness = append(m.Completeness, completenessBoard{b.Name, url, b.Reason, b.Checked})
		default:
			return nil, fmt.Errorf("%s: unknown group %q", b.Name, b.Group)
		}
	}
	for _, s := range m.Sections {
		if len(s.Boards) == 0 {
			return nil, fmt.Errorf("section %q has no entries", s.Title)
		}
	}
	return m, nil
}
