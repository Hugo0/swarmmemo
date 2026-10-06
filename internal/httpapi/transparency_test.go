package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/tlog"
)

func TestTransparencyRoutes(t *testing.T) {
	s := realServer(t)
	store := s.service.(*board.Store)
	if w := get(s, "/api/log/checkpoint", ""); w.Code != 503 || !strings.Contains(w.Body.String(), "no_checkpoint") {
		t.Fatalf("before any checkpoint: %d %s", w.Code, w.Body)
	}
	post := func(text string) string {
		w := get(s, "/w/lobby/main?format=json&text="+url.QueryEscape(text), "application/json")
		var out struct {
			Receipt struct{ ID string } `json:"receipt"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Receipt.ID == "" {
			t.Fatalf("post: %d %s", w.Code, w.Body)
		}
		return out.Receipt.ID
	}
	id := post("hello log")
	if _, err := store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := get(s, "/api/log/checkpoint", "")
	var cp struct{ Checkpoint board.LogCheckpoint }
	if err := json.Unmarshal(w.Body.Bytes(), &cp); err != nil || w.Code != 200 || cp.Checkpoint.Size < 1 {
		t.Fatalf("checkpoint: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("checkpoint cache: %q", w.Header().Get("Cache-Control"))
	}
	note := get(s, "/api/log/checkpoint/note?size="+itoa(cp.Checkpoint.Size), "")
	if note.Body.String() != cp.Checkpoint.Note || !strings.HasPrefix(note.Header().Get("Content-Type"), "text/plain") || note.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("note: %s %v", note.Body, note.Header())
	}
	text, err := tlog.OpenNote(note.Body.Bytes(), cp.Checkpoint.VerifierKey)
	if err != nil {
		t.Fatal(err)
	}
	body, err := tlog.ParseCheckpoint(text)
	if err != nil {
		t.Fatal(err)
	}
	w = get(s, "/api/log/proof?message="+id, "")
	var proof board.LogInclusion
	if err = json.Unmarshal(w.Body.Bytes(), &proof); err != nil || w.Code != 200 {
		t.Fatalf("proof: %d %s", w.Code, w.Body)
	}
	hashes := make([]tlog.Hash, len(proof.Proof))
	for i, h := range proof.Proof {
		if hashes[i], err = tlog.ParseHash(h); err != nil {
			t.Fatal(err)
		}
	}
	if err = tlog.VerifyInclusion(proof.Leaf.Index, body.Size, tlog.LeafHash([]byte(proof.Leaf.Data)), hashes, body.Root); err != nil {
		t.Fatal(err)
	}
	post("second")
	if _, err = store.SignCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w = get(s, "/api/log/consistency?from="+itoa(body.Size), ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"proof"`) {
		t.Fatalf("consistency: %d %s", w.Code, w.Body)
	}
	if w = get(s, "/api/log/leaves?start=0", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"leaf_hash"`) || strings.Contains(w.Body.String(), "hello log") {
		t.Fatalf("leaves: %d %s", w.Code, w.Body)
	}
	if w = get(s, "/api/log/anchors", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"anchors":[]`) {
		t.Fatalf("anchors: %d %s", w.Code, w.Body)
	}
	for path, code := range map[string]int{
		"/api/log/proof":                      400,
		"/api/log/proof?leaf=0&message=" + id: 400,
		"/api/log/proof?leaf=-1":              400,
		"/api/log/proof?leaf=999":             404,
		"/api/log/proof?message=nope":         404,
		"/api/log/proof?bogus=1":              400,
		"/api/log/consistency":                400,
		"/api/log/consistency?from=999":       404,
		"/api/log/anchors/1.ots":              404,
		"/api/log/nothing":                    404,
		"/api/record/no-such-agent":           404,
		"/api/record/bad%20name":              400,
		"/api/log/checkpoint?size=12345":      404,
	} {
		if w = get(s, path, ""); w.Code != code {
			t.Errorf("%s: %d, want %d: %s", path, w.Code, code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: an error is cacheable", path)
		}
	}
	r := httptest.NewRequest("POST", "/api/log/checkpoint", nil)
	r.RemoteAddr = "198.51.100.8:12345"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}
	caps := get(s, "/capabilities", "").Body.String()
	if !strings.Contains(caps, `"transparency"`) || !strings.Contains(caps, cp.Checkpoint.VerifierKey) {
		t.Fatal("capabilities lack the transparency fragment")
	}
	if page := get(s, "/verify", "text/html"); page.Code != 200 || !strings.Contains(page.Body.String(), "/api/log/proof") {
		t.Fatalf("/verify: %d", page.Code)
	}
	if words := len(strings.Fields(get(s, "/verify.md", "").Body.String())); words > 150 {
		t.Fatalf("/verify is %d words; keep it under 150", words)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
