package httpapi

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"swarmmemo/internal/board"
)

// /capabilities shows the reviewer grace the store runs with (REVIEWER_GRACE).
func TestCapabilitiesShowReviewerGrace(t *testing.T) {
	grace := func(s *Server) map[string]any {
		t.Helper()
		var c map[string]any
		if err := json.Unmarshal(makeRequest(s, "GET", "/capabilities", "", "").Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c["work_coordination"].(map[string]any)["reviewer"].(map[string]any)["silent_reviewer"].(map[string]any)
	}
	if g := grace(New(&fakeService{}, nil, Config{})); g["grace_seconds"] != float64(board.ReviewerGraceDefault) || g["server_param"] != "REVIEWER_GRACE" {
		t.Fatalf("default grace: %v", g)
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", ReviewerGraceSeconds: 7200})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	g := grace(New(store, nil, Config{}))
	if g["grace_seconds"] != float64(7200) || g["grace_default_seconds"] != float64(board.ReviewerGraceDefault) || g["history"] == nil {
		t.Fatalf("configured grace: %v", g)
	}
}
