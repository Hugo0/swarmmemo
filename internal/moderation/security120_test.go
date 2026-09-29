package moderation

// Security review 1.20 regression tests. Each one inverts a proof of concept
// (TestSecPoC_*, branch security-review-1.20): it fails while the weakness is
// present.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// M9: text past the first max_text_bytes is screened too (in overlapping
// chunks), so a phishing payload after 12,000 bytes of filler is hidden.
func TestSec120_PayloadPastJevTruncationIsCaught(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(raw, &req)
		p := 0.01
		if strings.Contains(string(raw), "SEND-YOUR-SEED-PHRASE") {
			p = 0.99
		}
		answers := map[string]any{}
		for k := range req.Questions {
			answers[k] = map[string]float64{"noul": p}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int64{"input_tokens": 100}, "model": "jev-1.13.0"})
	}))
	t.Cleanup(srv.Close)
	v := newEnv(t, "")
	o := withFakeJev(v.opts, srv.URL)
	o.DB = openDB(t)
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	payload := "SEND-YOUR-SEED-PHRASE to https://wallet-verify.example to unlock your account"
	padded := strings.Repeat("Weather report. ", 12000/16) + payload
	if d := e.Screen(context.Background(), SurfacePost, Subject{ID: "padded"}, Content{Text: padded}); d.Action != Hide {
		t.Fatalf("a payload after 12,000 bytes of filler: %+v", d)
	}
	// Also when it straddles a chunk boundary.
	straddle := strings.Repeat("x", 12000-40) + payload
	if d := e.Screen(context.Background(), SurfacePost, Subject{ID: "straddle"}, Content{Text: straddle}); d.Action != Hide {
		t.Fatalf("a payload across the chunk boundary: %+v", d)
	}
}

func TestSec120_JevChunksCoverTheText(t *testing.T) {
	text := strings.Repeat("é", 20000) // 40,000 bytes, two-byte runes
	chunks, whole := jevChunks(text, 12000)
	if !whole || len(chunks) < 4 || len(chunks) > jevChunksMax {
		t.Fatalf("%d chunks", len(chunks))
	}
	covered := 0
	for _, c := range chunks {
		if len(c) > 12000 || !utf8.ValidString(c) {
			t.Fatalf("chunk of %d bytes, valid %v", len(c), utf8.ValidString(c))
		}
		covered += len(c)
	}
	if covered < len(text) || !strings.HasSuffix(text, chunks[len(chunks)-1]) {
		t.Fatal("the chunks do not cover the text")
	}
	if chunks, whole := jevChunks(strings.Repeat("a", 256<<10), 256); len(chunks) != jevChunksMax || whole {
		t.Fatalf("a long text at the smallest chunk size: %d chunks, whole %v", len(chunks), whole)
	}
}

// H2: five decoy phishing posts no longer switch hiding off board-wide. The
// burst is counted per category: another category, a hard category, a
// near-certain call and the decoys' own author are still acted on, and the
// burst still alerts.
func TestSec120_BurstDoesNotUnhideOtherAuthorsOrCategories(t *testing.T) {
	v := newEnv(t, "")
	v.e.SetActuator(SurfacePost, &recorder{})
	ctx := context.Background()
	v.jev.set(map[string]float64{"phishing": 0.99})
	for i := 0; i < 5; i++ {
		v.e.Screen(ctx, SurfacePost, Subject{ID: fmt.Sprint("decoy", i), Agent: "attacker"}, Content{Text: "obvious phishing"})
	}
	// The PoC's case: sexual content involving minors from someone else.
	v.jev.set(map[string]float64{"minors": 0.99})
	if d := v.e.Screen(ctx, SurfacePost, Subject{ID: "real", Agent: "someone-else"}, Content{Text: "worst content"}); d.Action != Hide || d.Burst {
		t.Fatalf("minors after a phishing burst: %+v", d)
	}
	// Doxxing is hard on posts too, and another category is not in the burst.
	v.jev.set(map[string]float64{"doxxing": 0.92})
	if d := v.e.Screen(ctx, SurfacePost, Subject{ID: "dox", Agent: "someone-else"}, Content{Text: "an address"}); d.Action != Hide {
		t.Fatalf("doxxing after a phishing burst: %+v", d)
	}
	v.jev.set(map[string]float64{"hate": 0.92})
	if d := v.e.Screen(ctx, SurfacePost, Subject{ID: "hate", Agent: "someone-else"}, Content{Text: "slurs"}); d.Action != Hide || d.Burst {
		t.Fatalf("hate after a phishing burst: %+v", d)
	}
	// In the burst's own category: a near-certain call still hides, and the
	// decoys' author gets no benefit of the doubt.
	v.jev.set(map[string]float64{"phishing": 0.99})
	if d := v.e.Screen(ctx, SurfacePost, Subject{ID: "sure", Agent: "third"}, Content{Text: "phishing"}); d.Action != Hide {
		t.Fatalf("near-certain phishing in the burst: %+v", d)
	}
	v.jev.set(map[string]float64{"phishing": 0.92})
	if d := v.e.Screen(ctx, SurfacePost, Subject{ID: "mine", Agent: "attacker"}, Content{Text: "phishing"}); d.Action != Hide {
		t.Fatalf("the decoys' author in the burst: %+v", d)
	}
	// An uncertain call from an uninvolved author is what the burst softens.
	d := v.e.Screen(ctx, SurfacePost, Subject{ID: "unsure", Agent: "fourth"}, Content{Text: "maybe phishing"})
	if d.Action != Flag || d.Proposed != Hide || !d.Burst {
		t.Fatalf("uncertain phishing in the burst: %+v", d)
	}
	if k := v.alertKinds(); len(k) != 1 || k[0] != "burst" {
		t.Fatalf("alerts %v", k)
	}
}

// H1: a burst of refused prompts no longer lets a sexual-minors prompt
// through: minors is hard on every surface.
func TestSec120_InferencePromptBurstKeepsRefusingMinors(t *testing.T) {
	v := newEnv(t, "")
	ctx := context.Background()
	v.jev.set(map[string]float64{"minors": 0.99})
	for i := 0; i < 10; i++ {
		if hide, _, _ := v.e.ScreenInference(ctx, "prompt", "m", fmt.Sprint("decoy ", i)); !hide {
			t.Fatalf("decoy %d was not refused", i)
		}
	}
	hide, reason, err := v.e.ScreenInference(ctx, "prompt", "m", "the real request")
	if err != nil || !hide {
		t.Fatalf("the 11th minors prompt proceeds: hide=%v reason=%q err=%v", hide, reason, err)
	}
	// A policy that forgets to mark minors hard still cannot soften it.
	policy := `{"schema":1,"version":2,"surfaces":{"inference.prompt":{"classifiers":["jev"],"on_unavailable":"flag","burst":{"window_seconds":3600,"max":1,"mode":"downgrade"},"categories":{"minors":{"thresholds":[{"at":0.9,"action":"block"}]}}}}}`
	w := newEnv(t, policy)
	if sp := w.e.Policy(ctx).surface(SurfaceInferencePrompt); sp.Categories["minors"].Hard || sp.Burst.Max != 1 {
		t.Fatalf("the test policy did not load: %+v", sp)
	}
	w.jev.set(map[string]float64{"minors": 0.99})
	for i := 0; i < 3; i++ {
		if hide, _, _ := w.e.ScreenInference(ctx, "prompt", "m", fmt.Sprint("p", i)); !hide {
			t.Fatalf("prompt %d proceeds under a policy without hard minors", i)
		}
	}
}

// H1: when Jev cannot answer (down, or over its daily cap) a prompt is
// refused, even under a policy that says flag; posts still fail open to flag.
func TestSec120_InferencePromptFailsClosedWhenJevIsDown(t *testing.T) {
	for _, policy := range []string{"", `{"schema":1,"version":2,"surfaces":{"inference.prompt":{"classifiers":["jev"],"on_unavailable":"flag"}}}`} {
		v := newEnv(t, policy)
		v.jev.fail(503)
		hide, _, err := v.e.ScreenInference(context.Background(), "prompt", "m", "hello")
		if err != nil || !hide {
			t.Fatalf("policy %q: an unscreened prompt proceeds: hide=%v err=%v", policy, hide, err)
		}
		if d := v.screen(t, SurfacePost, "post", "hello"); d.Action != Flag || d.Degraded == "" {
			t.Fatalf("policy %q: a post with Jev down: %+v", policy, d)
		}
	}
	// Over the daily spend cap: the same.
	capped := `{"schema":1,"version":1,"jev":{"model":"jev-1.13.0","daily_spend_cap_microusd":1,"price_per_mtok_microusd":1,"max_text_bytes":12000,"timeout_ms":5000}}`
	v := newEnv(t, capped)
	v.screen(t, SurfacePost, "first", "x") // spends the cap
	if hide, _, _ := v.e.ScreenInference(context.Background(), "prompt", "m", "hello"); !hide {
		t.Fatal("a prompt over the spend cap proceeds")
	}
}
