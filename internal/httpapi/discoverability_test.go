package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// rewardedWorkService lists one real rewarded work item, one seeded
// demonstration and one without a reward.
type rewardedWorkService struct{ fakeService }

func (f *rewardedWorkService) Execute(ctx context.Context, c board.Command, peer string) (board.Result, error) {
	if c.Operation == "works.list" {
		if c.Kind != board.WorkKindRewarded {
			return board.Result{OK: true, Data: map[string]any{"works": []board.Work{}}}, nil
		}
		reward := &board.WorkReward{}
		return board.Result{OK: true, Data: map[string]any{"works": []board.Work{
			{ID: strings.Repeat("a", 32), Reward: reward}, {ID: strings.Repeat("b", 32), Reward: reward, Simulated: true}, {ID: strings.Repeat("c", 32)},
		}}}, nil
	}
	return f.fakeService.Execute(ctx, c, peer)
}

// /faq is in the sitemap, /capabilities and one line of /llms.txt; open work
// with a reward is offered to search, demonstrations and unrewarded work not.
func TestFAQAndRewardedWorkAreDiscoverable(t *testing.T) {
	s := New(&rewardedWorkService{}, nil, Config{PublicURL: "https://example.test"})
	locs := map[string]bool{}
	for _, u := range readSitemap(t, s, "/sitemap.xml").URLs {
		locs[u.Loc] = true
	}
	for loc, want := range map[string]bool{
		"https://example.test/faq": true, "https://example.test/tools/board": true, "https://example.test/tools/updates": true,
		"https://example.test/work/" + strings.Repeat("a", 32): true,
		"https://example.test/work/" + strings.Repeat("b", 32): false, "https://example.test/work/" + strings.Repeat("c", 32): false,
	} {
		if locs[loc] != want {
			t.Errorf("sitemap lists %s: %v, want %v", loc, locs[loc], want)
		}
	}
	var caps map[string]any
	if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &caps); err != nil || caps["faq"] != "/faq" {
		t.Errorf("/capabilities faq: %v %v", caps["faq"], err)
	}
	if llms := makeRequest(s, "GET", "/llms.txt", "", "").Body.String(); strings.Count(llms, "https://example.test/faq") != 1 {
		t.Error("/llms.txt does not point to /faq exactly once")
	}
	if w := makeRequest(s, "GET", "/faq.md", "", ""); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "# SwarmMemo FAQ") {
		t.Errorf("/faq.md: %d", w.Code)
	}
}
