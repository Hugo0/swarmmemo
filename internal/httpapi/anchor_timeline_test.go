package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/ots"
)

// TestAnchorTimelineRoutes: /api/log/anchors and a proof show each anchor's
// timeline, a proof against a pending anchor is not cached for a day, and
// /capabilities states the expected timeline.
func TestAnchorTimelineRoutes(t *testing.T) {
	s := realServer(t)
	store := s.service.(*board.Store)
	cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/digest" {
			http.NotFound(w, r)
			return
		}
		digest, _ := io.ReadAll(io.LimitReader(r.Body, 64))
		uri := []byte("https://calendar.invalid")
		var out bytes.Buffer
		(&ots.Timestamp{Msg: digest, Attestations: []ots.Attestation{{Tag: ots.TagPending, Payload: append([]byte{byte(len(uri))}, uri...)}}}).Serialize(&out)
		w.Write(out.Bytes())
	}))
	defer cal.Close()
	if w := get(s, "/w/lobby/main?format=json&text="+url.QueryEscape("bracket me"), "application/json"); w.Code != 200 {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.AnchorCheckpoints(context.Background(), &ots.Client{Calendars: []string{cal.URL}}, 3); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Anchors  []board.LogAnchor `json:"anchors"`
		Timeline string            `json:"timeline"`
	}
	w := get(s, "/api/log/anchors", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Anchors) != 1 || list.Timeline != board.AnchorTimeline {
		t.Fatalf("anchors: %d %s", w.Code, w.Body)
	}
	a := list.Anchors[0]
	if a.State != "pending" || a.CheckpointAt == 0 || a.SubmittedAt < a.CheckpointAt || a.NextCheckAt <= a.SubmittedAt || a.ConfirmedAt != 0 ||
		!strings.HasSuffix(a.OTS, ".ots") || !strings.Contains(a.Note, "size=") {
		t.Fatalf("anchor: %+v", a)
	}
	w = get(s, "/api/log/proof?leaf=0&size="+itoa(a.Size), "")
	var proof board.LogInclusion
	if err := json.Unmarshal(w.Body.Bytes(), &proof); err != nil || proof.Anchor == nil || proof.Anchor.Size != a.Size || proof.Anchor.NextCheckAt != a.NextCheckAt {
		t.Fatalf("proof: %d %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("a proof with a pending anchor is cached: %q", cc)
	}
	if caps := get(s, "/capabilities", "").Body.String(); !strings.Contains(caps, `"anchor_timeline"`) || !strings.Contains(caps, "next_check_at") {
		t.Fatal("capabilities lack the anchor timeline")
	}
	if llms := get(s, "/llms.txt", "").Body.String(); !strings.Contains(llms, "confirmed_at") {
		t.Fatal("llms.txt lacks the anchor timeline")
	}
}
