package web

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/references"
)

func referenceFixture() ReferencePage {
	now := time.Unix(1788566400, 0).UTC()
	return ReferencePage{
		Now: now, APIURL: "/api/references?q=synthetic&limit=20", Query: "synthetic",
		Sources: []references.Source{{ID: "fixture", Name: "Synthetic human/agent source",
			FeedURL: "https://blog.cuttle.af/content-index.json", Adapter: "cuttle-worklog-index-v1",
			Status: "not_modified", AttributionBasis: "site_declared_human_agent_co_creation",
			LastAttemptedAt: now.Unix() - 60, LastSuccessfulAt: now.Unix() - 60}},
		References: []references.Reference{{ID: strings.Repeat("a", 64), SourceID: "fixture", ExternalID: "synthetic-worklog",
			URL: "https://blog.cuttle.af/synthetic-worklog/", Title: "Synthetic reference 雪",
			Excerpt: "Source-provided synthetic excerpt.", ExcerptAvailable: true,
			Authors:           []references.Author{{Name: "Synthetic human"}, {Name: "Synthetic agent; source-declared", URL: "https://blog.cuttle.af/about/"}},
			SourcePublishedAt: "2026-08-09 00:00:00", SourcePublicationTimezoneKnown: false,
			FirstObservedAt: now.Unix() - 200000, LastObservedAt: now.Unix() - 90000,
			ContentHash: strings.Repeat("b", 64), UntrustedContent: true}},
	}
}

func renderReferenceFixture(t *testing.T, data ReferencePage) string {
	t.Helper()
	var out bytes.Buffer
	if err := RenderReferences(&out, data); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestReferencesListIsExternalInertAndFreshnessIsNotFabricated(t *testing.T) {
	data := referenceFixture()
	data.NextURL = "/references?q=synthetic&cursor=fixture"
	body := renderReferenceFixture(t, data)
	for _, text := range []string{"External references", "not native posts or available jobs", "no training or HF export",
		"Source-declared authors", "Synthetic human", "Synthetic agent; source-declared", "Historical reference",
		"Last seen in source index", "Source last checked", "2026-08-09 00:00:00", "timezone not supplied",
		"Source-provided synthetic excerpt.", "Next references", `value="synthetic"`,
		`rel="noopener noreferrer nofollow ugc"`, `data-copy-label="Copy reference JSON URL"`} {
		if !strings.Contains(body, text) {
			t.Errorf("missing %q", text)
		}
	}
	if strings.Contains(body, "2026-08-09 00:00:00Z") {
		t.Fatal("invented timezone")
	}
	for _, forbidden := range []string{`id="feed"`, `id="compose"`, "work.claim", "identity-avatar", "new-messages", "Source series:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("unexpected native/prefix element %q", forbidden)
		}
	}
	if !strings.Contains(body, `href="/api/references?q=synthetic&amp;limit=20"`) {
		t.Fatal("JSON link not escaped/preserved")
	}
}

func TestReferencesDetailSeparatesCatalogProvenance(t *testing.T) {
	data := referenceFixture()
	data.Detail = true
	data.APIURL = "/api/references/" + data.References[0].ID
	body := renderReferenceFixture(t, data)
	for _, text := range []string{"Reference provenance", "Normalized catalog-record hash", "not an original-author signature",
		"External item ID", "First observed locally", "Source last successful fetch/check", data.References[0].ContentHash,
		`href="https://swarmmemo.com/references/` + data.References[0].ID + `"`} {
		if !strings.Contains(body, text) {
			t.Errorf("missing detail %q", text)
		}
	}
	if strings.Contains(body, `id="reference-query"`) || strings.Contains(body, "Reference details →") {
		t.Fatal("detail has redundant list controls")
	}
}

func TestReferencesEscapesFieldsAndNeverMakesSourceTextCopyable(t *testing.T) {
	data := referenceFixture()
	data.References[0].Title = `雪 <img src=x onerror=alert(1)>`
	data.References[0].Excerpt = `<script>alert("untrusted")</script>`
	data.References[0].Authors[0].Name = `<img src=author onerror=alert(2)>`
	data.Sources[0].Name = `<script>source</script>`
	data.Query = `"><img src=query>`
	body := renderReferenceFixture(t, data)
	for _, forbidden := range []string{`<img src=x`, `<script>alert`, `<img src=author`, `<script>source`, `<img src=query`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("unescaped %q", forbidden)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert") || !strings.Contains(body, "雪") {
		t.Fatal("escaped original text not retained")
	}
	if strings.Count(body, "data-copy-value=") != 1 || strings.Contains(body, `class="prose"`) {
		t.Fatal("source content must not enter service-authored copy-code enhancement")
	}
}

func TestReferencesUnavailableAndMissingDiscardAllSuppliedData(t *testing.T) {
	for _, missing := range []bool{false, true} {
		data := referenceFixture()
		data.Missing, data.Unavailable = missing, !missing
		data.Query, data.SourceFilter, data.NextURL, data.APIURL = "query-canary", "filter-canary", "/w/leak", "/w/api-canary"
		data.Sources[0].Name = "source-canary"
		data.References[0].Title = "title-canary"
		data.References[0].Excerpt = "body-canary"
		data.References[0].Authors[0].Name = "author-canary"
		body := renderReferenceFixture(t, data)
		if strings.Contains(body, "canary") || strings.Contains(body, `id="reference-query"`) || strings.Contains(body, "data-copy-value=") {
			t.Fatal("unavailable view leaked supplied metadata")
		}
		if missing && !strings.Contains(body, "Reference unavailable.") {
			t.Fatal("missing-detail distinction lost")
		}
		if !missing && !strings.Contains(body, "References temporarily unavailable.") {
			t.Fatal("whole-view unavailable distinction lost")
		}
	}
}

func TestReferencesEmptyAndMetadataOnlyViewsAreHonest(t *testing.T) {
	data := referenceFixture()
	data.References = nil
	data.Query = ""
	body := renderReferenceFixture(t, data)
	if !strings.Contains(body, "No external references yet.") || strings.Contains(body, "temporarily unavailable") {
		t.Fatal("empty ready confused with failure")
	}
	data.Query = "nothing"
	if !strings.Contains(renderReferenceFixture(t, data), "No references match.") {
		t.Fatal("empty search is not identified")
	}
	data = referenceFixture()
	data.References[0].ExcerptAvailable = false
	data.References[0].Excerpt = ""
	body = renderReferenceFixture(t, data)
	if !strings.Contains(body, "No excerpt is available") || strings.Contains(body, `class="reference-excerpt"`) {
		t.Fatal("metadata-only view implies an excerpt")
	}
}

func TestReferencesHistoricalBoundaryAndTruncationLabel(t *testing.T) {
	for _, age := range []int64{86400, 86401} {
		data := referenceFixture()
		data.References[0].LastObservedAt = data.Now.Unix() - age
		data.References[0].ExcerptTruncated = true
		body := renderReferenceFixture(t, data)
		if strings.Contains(body, "Historical reference") != (age > 86400) {
			t.Fatal("historical threshold differs from contract")
		}
		if !strings.Contains(body, "Excerpt shortened for display") {
			t.Fatal("missing truncation disclosure")
		}
	}
}

func TestReferencesRejectUnsafeLinksAndContradictoryFlagsBeforeWriting(t *testing.T) {
	mutations := []func(*ReferencePage){
		func(p *ReferencePage) { p.APIURL = "https://example.org/api/references" },
		func(p *ReferencePage) { p.APIURL = "/api/references/../command" },
		func(p *ReferencePage) { p.APIURL = "/w/lobby/main?text=write" },
		func(p *ReferencePage) { p.APIURL = "/api/references?operation=post" },
		func(p *ReferencePage) { p.NextURL = "/references?q=one&q=two" },
		func(p *ReferencePage) { p.NextURL = "//example.org/references" },
		func(p *ReferencePage) { p.NextURL = "/references/%2e%2e/w/lobby" },
		func(p *ReferencePage) { p.References[0].URL = "javascript:alert(1)" },
		func(p *ReferencePage) { p.References[0].Authors[0].URL = "https://example.org/?token=secret" },
		func(p *ReferencePage) { p.Sources[0].FeedURL = "https://user:password@example.org/" },
		func(p *ReferencePage) { p.References[0].ClaimableJob = true },
		func(p *ReferencePage) { p.References[0].NativeIdentity = true },
		func(p *ReferencePage) { p.References[0].HuggingFaceEligible = true },
		func(p *ReferencePage) { p.References[0].UntrustedContent = false },
		func(p *ReferencePage) { p.References[0].ExcerptAvailable = false },
		func(p *ReferencePage) { p.References[0].Excerpt = strings.Repeat("雪", 513) },
		func(p *ReferencePage) { p.References[0].SourceID = "not-supplied" },
		func(p *ReferencePage) { p.Detail = true; p.References = nil },
	}
	for i, mutate := range mutations {
		data := referenceFixture()
		mutate(&data)
		var out bytes.Buffer
		if err := RenderReferences(&out, data); !errors.Is(err, errReferenceView) || out.Len() != 0 {
			t.Errorf("mutation %d did not fail before output: %v", i, err)
		}
	}
}

type referenceFailWriter struct{}

func (referenceFailWriter) Write([]byte) (int, error) { return 0, errors.New("fixture_writer_failed") }

func TestReferencesReturnsWriterFailure(t *testing.T) {
	if err := RenderReferences(referenceFailWriter{}, referenceFixture()); err == nil || !strings.Contains(err.Error(), "fixture_writer_failed") {
		t.Fatal("writer failure not propagated")
	}
}
