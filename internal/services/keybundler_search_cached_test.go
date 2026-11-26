package services

import (
	"context"
	"testing"
)

func unifiedHitsByID(page map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	hits, _ := page["tools"].([]any)
	for _, h := range hits {
		m := h.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

// TestAuditToolsSearchStaleVetting reproduces a cached search page serving the
// bundler's old vetting flag after another search has updated frames_tools.
func TestAuditToolsSearchStaleVetting(t *testing.T) {
	h, api := newToolsHarness(t)
	vetted := `[{"id":"` + toolOK + `","title":"Weather","description":"Weather forecast","payment":{"price_hint":"0.002"}}]`
	unvetted := `[{"id":"` + toolOK + `","title":"Weather","description":"Weather forecast","payment":{"price_hint":"0.002"},"unvetted":true}]`

	api.mu.Lock()
	api.hits = vetted
	api.mu.Unlock()
	if _, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather first"}`); err != nil {
		t.Fatal(err)
	}

	api.mu.Lock()
	api.hits = unvetted
	api.mu.Unlock()
	fresh, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather second"}`)
	if err != nil {
		t.Fatal(err)
	}
	if hit := hitsByID(fresh)["tool:"+toolOK]; hit["vetted"] != false || hit["callable"] != false || hit["why_not"] != "tool_unvetted" {
		t.Fatalf("fresh search should record and report unvetted: %v", hit)
	}

	cached, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather first"}`)
	if err != nil {
		t.Fatal(err)
	}
	hit := hitsByID(cached)["tool:"+toolOK]
	var row int
	if err := h.db.QueryRow("SELECT vetted FROM frames_tools WHERE id=?", toolOK).Scan(&row); err != nil {
		t.Fatal(err)
	}
	t.Logf("cached=%v hit.vetted=%v hit.callable=%v hit.why_not=%v frames_tools.vetted=%d page.callable=%v",
		cached["cached"], hit["vetted"], hit["callable"], hit["why_not"], row, cached["callable"])

	unified, _ := h.toolsSearch(`{"query":"weather first","kind":"catalogue"}`)
	uhit := unifiedHitsByID(unified)["tool:"+toolOK]
	t.Logf("unified.cached=%v unified.vetted=%v unified.callable=%v unified.why_not=%v",
		unified["catalogue"].(map[string]any)["cached"], uhit["vetted"], uhit["callable"], uhit["why_not"])
	if searches, _, _, _ := api.count(); searches != 2 {
		t.Fatalf("cached unified search made %d upstream searches, want 2", searches)
	}

	detail, err := h.bundlerRead(testSubject, "tools_get", `{"id":"tool:`+toolOK+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := h.call(testSubject, `{"resource":"tool:`+toolOK+`"}`, creditsFor(20000))
	t.Logf("tools_get.vetted=%v tools_get.callable=%v tools_get.why_not=%v call_error=%v",
		detail["vetted"], detail["callable"], detail["why_not"], callErr)

	if hit["vetted"] != false || hit["callable"] != false || hit["why_not"] != "tool_unvetted" || cached["callable"] != float64(0) {
		t.Fatalf("cached hit disagrees with current row, tools_get and call: %v", hit)
	}
	if uhit["vetted"] != false || uhit["callable"] != false || uhit["why_not"] != "tool_unvetted" {
		t.Fatalf("unified cached hit disagrees with current row, tools_get and call: %v", uhit)
	}
	if errCode(callErr) != "tool_unvetted" {
		t.Fatalf("call error = %v, want tool_unvetted", callErr)
	}
}

// TestAuditToolsSearchCachedProbeAvailability reproduces tools_search using a
// cached probe's price but ignoring that probe's live/payable verdict.
func TestAuditToolsSearchCachedProbeAvailability(t *testing.T) {
	for _, c := range []struct {
		name, probe string
	}{
		{"not_live", `{"id":"` + toolOK + `","live":false,"payable":true,"price_usd":0.002}`},
		{"not_payable", `{"id":"` + toolOK + `","live":true,"payable":false,"price_usd":0.002}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, api := newBundlerHarness(t, "", "")
			if _, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.probes[toolOK] = c.probe
			api.mu.Unlock()

			detail, err := h.bundlerRead(testSubject, "tools_get", `{"id":"tool:`+toolOK+`"}`)
			if err != nil {
				t.Fatal(err)
			}
			page, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`)
			if err != nil {
				t.Fatal(err)
			}
			hit := hitsByID(page)["tool:"+toolOK]
			_, callErr := h.call(testSubject, `{"resource":"tool:`+toolOK+`"}`, creditsFor(20000))
			t.Logf("tools_get.live=%v tools_get.payable=%v tools_get.callable=%v tools_get.why_not=%v search.vetted=%v search.callable=%v search.why_not=%v call_error=%v",
				detail["live"], detail["payable"], detail["callable"], detail["why_not"], hit["vetted"], hit["callable"], hit["why_not"], callErr)

			if hit["vetted"] != true || hit["callable"] != false || hit["why_not"] != "tool_unavailable" {
				t.Fatalf("cached search hit disagrees with cached probe availability: %v", hit)
			}
			if errCode(callErr) != "tool_unavailable" {
				t.Fatalf("call error = %v, want tool_unavailable", callErr)
			}
		})
	}
}

// TestAuditToolsSearchCachedDescriptorCategory reproduces tools_search using
// the search index's old category after a live descriptor has changed category.
func TestAuditToolsSearchCachedDescriptorCategory(t *testing.T) {
	t.Run("operator_vet_lost", func(t *testing.T) {
		h, api := newBundlerHarness(t, "", "")
		ctx := context.Background()
		if _, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.engine.VetTool(ctx, "tool:"+toolUnvet, h.now); err != nil {
			t.Fatal(err)
		}
		api.mu.Lock()
		api.tools[toolUnvet] = bundlerDescriptorJSON(toolUnvet, "unvetted.example.com", "Casino betting odds")
		api.mu.Unlock()
		bundlerOf(t, h).forget()

		before, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`)
		if err != nil {
			t.Fatal(err)
		}
		detail, err := h.bundlerRead(testSubject, "tools_get", `{"id":"tool:`+toolUnvet+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		after, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`)
		if err != nil {
			t.Fatal(err)
		}
		hit := hitsByID(after)["tool:"+toolUnvet]
		_, callErr := h.call(testSubject, `{"resource":"tool:`+toolUnvet+`"}`, creditsFor(20000))
		t.Logf("before_get.search.callable=%v tools_get.category=%v tools_get.vetted=%v tools_get.callable=%v tools_get.why_not=%v after_get.search.category=%v search.vetted=%v search.callable=%v search.why_not=%v call_error=%v",
			hitsByID(before)["tool:"+toolUnvet]["callable"], detail["category"], detail["vetted"], detail["callable"], detail["why_not"],
			hit["category"], hit["vetted"], hit["callable"], hit["why_not"], callErr)

		if hit["vetted"] != false || hit["callable"] != false || hit["why_not"] != "tool_unvetted" {
			t.Fatalf("cached search hit keeps a vet the live descriptor invalidates: %v", hit)
		}
		if errCode(callErr) != "tool_unvetted" {
			t.Fatalf("call error = %v, want tool_unvetted", callErr)
		}
	})

	t.Run("denied_live_category", func(t *testing.T) {
		h, api := newBundlerHarness(t, "", `{"categories":["gambling"]}`)
		if _, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`); err != nil {
			t.Fatal(err)
		}
		api.mu.Lock()
		api.tools[toolOK] = bundlerDescriptorJSON(toolOK, "weather.example.com", "Casino betting odds")
		api.mu.Unlock()
		bundlerOf(t, h).forget()

		detail, err := h.bundlerRead(testSubject, "tools_get", `{"id":"tool:`+toolOK+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		page, err := h.bundlerRead(testSubject, "tools_search", `{"query":"weather"}`)
		if err != nil {
			t.Fatal(err)
		}
		hit := hitsByID(page)["tool:"+toolOK]
		_, callErr := h.call(testSubject, `{"resource":"tool:`+toolOK+`"}`, creditsFor(20000))
		t.Logf("tools_get.category=%v tools_get.callable=%v tools_get.why_not=%v search.category=%v search.vetted=%v search.callable=%v search.why_not=%v call_error=%v",
			detail["category"], detail["callable"], detail["why_not"], hit["category"], hit["vetted"], hit["callable"], hit["why_not"], callErr)

		if hit["vetted"] != true || hit["callable"] != false || hit["why_not"] != "tool_denied" {
			t.Fatalf("cached search hit ignores the cached descriptor's denied category: %v", hit)
		}
		if errCode(callErr) != "tool_denied" {
			t.Fatalf("call error = %v, want tool_denied", callErr)
		}
	})
}
