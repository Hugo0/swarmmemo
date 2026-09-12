package web

import (
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/references"
)

// ReferencePage contains already authorized reference data, not a board query.
// The HTTP caller must buffer this output and perform its final file/time fence
// before writing any headers or bytes. This renderer does no authorization I/O.
type ReferencePage struct {
	Sources                              []references.Source
	References                           []references.Reference
	Query, SourceFilter, NextURL, APIURL string
	Detail, Unavailable, Missing         bool
	Now                                  time.Time
}

type referenceItemView struct {
	references.Reference
	Source     references.Source
	Historical bool
}

type referenceView struct {
	Items                                []referenceItemView
	Sources                              []references.Source
	Query, SourceFilter, NextURL, APIURL string
	Detail, Unavailable, Missing         bool
}

var errReferenceView = errors.New("invalid_reference_view")

func referenceID(id string) bool {
	return len(id) == 64 && strings.Trim(id, "0123456789abcdef") == ""
}

func referenceLocalURL(raw, prefix string) bool {
	if len(raw) > 8192 || strings.ContainsAny(raw, "\\\x00\t\r\n #") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.RawPath != "" ||
		!(u.Path == prefix || (strings.HasPrefix(u.Path, prefix+"/") && referenceID(strings.TrimPrefix(u.Path, prefix+"/")))) {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for name, values := range query {
		if len(values) != 1 || (name != "q" && name != "source" && name != "limit" && name != "cursor") {
			return false
		}
	}
	return true
}

func referenceExternalURL(raw string, optional bool) bool {
	if raw == "" {
		return optional
	}
	if len(raw) > 4096 || strings.ContainsAny(raw, "\\\x00\t\r\n #?") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

// RenderReferences uses only the supplied values and the embedded public shell.
// In particular, it never calls board.Service, accesses policy/catalog files,
// fetches an external URL, or writes HTTP headers. Missing/unavailable states
// discard all supplied rows, source labels, filters and links defensively.
func RenderReferences(w io.Writer, data ReferencePage) error {
	if w == nil {
		return errReferenceView
	}
	p := page{View: "references", Path: "/references", Title: "External references",
		Description: "Source-provided excerpts and original links. External references, not native posts or available jobs."}
	v := &referenceView{Detail: data.Detail, Missing: data.Missing, Unavailable: data.Unavailable}
	p.ReferencesView = v
	if data.Unavailable || data.Missing {
		p.NoIndex = true
		if data.Unavailable {
			p.Title = "References temporarily unavailable"
			p.Description = "External references are temporarily unavailable. The bulletin remains independent."
		} else {
			p.Title = "Reference unavailable"
			p.Description = "This external reference is not available."
		}
		return templates.ExecuteTemplate(w, "page.html", p)
	}
	if data.Now.IsZero() || len(data.Sources) > 50 || len(data.References) > 50 ||
		len(data.Query) > 256 || !utf8.ValidString(data.Query) || strings.ContainsRune(data.Query, 0) ||
		len(data.SourceFilter) > 64 || !referenceLocalURL(data.APIURL, "/api/references") ||
		(data.NextURL != "" && !referenceLocalURL(data.NextURL, "/references")) ||
		(data.Detail && (len(data.References) != 1 || data.NextURL != "")) {
		return errReferenceView
	}
	sources := make(map[string]references.Source, len(data.Sources))
	for _, source := range data.Sources {
		if source.ID == "" || len(source.ID) > 64 || len(source.Name) > 256 ||
			!referenceExternalURL(source.FeedURL, false) || (source.Status != "ok" && source.Status != "not_modified") {
			return errReferenceView
		}
		if _, exists := sources[source.ID]; exists {
			return errReferenceView
		}
		sources[source.ID] = source
	}
	seen := make(map[string]bool, len(data.References))
	for _, item := range data.References {
		source, exists := sources[item.SourceID]
		if !exists || !referenceID(item.ID) || seen[item.ID] || !referenceID(item.ContentHash) ||
			len(item.Title) > 512 || !utf8.ValidString(item.Title) ||
			len(item.Excerpt) > 2048 || !utf8.ValidString(item.Excerpt) || utf8.RuneCountInString(item.Excerpt) > 512 ||
			!referenceExternalURL(item.URL, false) || len(item.Authors) > 20 ||
			item.NativeIdentity || item.ClaimableJob || item.HuggingFaceEligible || !item.UntrustedContent ||
			(!item.ExcerptAvailable && (item.Excerpt != "" || item.ExcerptTruncated)) {
			return errReferenceView
		}
		for _, author := range item.Authors {
			if len(author.Name) > 512 || !referenceExternalURL(author.URL, true) {
				return errReferenceView
			}
		}
		seen[item.ID] = true
		v.Items = append(v.Items, referenceItemView{Reference: item, Source: source,
			Historical: data.Now.Unix()-item.LastObservedAt > 86400})
	}
	v.Sources = data.Sources
	v.Query, v.SourceFilter, v.NextURL, v.APIURL = data.Query, data.SourceFilter, data.NextURL, data.APIURL
	if data.Detail {
		p.Path += "/" + data.References[0].ID
		p.Title = data.References[0].Title + " — external reference"
	}
	return templates.ExecuteTemplate(w, "page.html", p)
}
