package moderation

// Security review of screen.text (branch screen-service, 5a09344): proofs of
// concept. Each test asserts the safe behaviour, so it fails while the
// finding it names is open.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// payloadJev answers injection 0.99 for a chunk that holds "PAYLOAD" and
// 0.01 otherwise. tokens < 0 reports the request body's length over -tokens
// (bytes a token) as its input tokens; otherwise it reports tokens.
type payloadJev struct {
	srv      *httptest.Server
	requests atomic.Int64
	tokens   int64
}

func newPayloadJev(t *testing.T, tokens int64) *payloadJev {
	f := &payloadJev{tokens: tokens}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			State struct {
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			} `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if json.Unmarshal(raw, &req) != nil {
			w.WriteHeader(400)
			return
		}
		answers := map[string]any{}
		for k := range req.Questions {
			p := 0.01
			if k == "injection" && strings.Contains(req.State.Message.Text, "PAYLOAD") {
				p = 0.99
			}
			answers[k] = map[string]float64{"noul": p}
		}
		tokens := f.tokens
		if tokens < 0 {
			tokens = int64(len(raw)) / -tokens
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int64{"input_tokens": tokens}, "model": "jev-1.13.0"})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newPayloadEnv(t *testing.T, policy string, tokens int64) (*Engine, *payloadJev, *env) {
	t.Helper()
	v := &env{db: openDB(t), clock: &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}}
	f := newPayloadJev(t, tokens)
	o := Options{DB: v.db, JevKeyFile: writeKey(t), Now: v.clock.now, Alert: func(Alert) {}}
	if policy != "" {
		o.PolicyFile = writePolicy(t, t.TempDir(), policy)
	}
	e, err := New(withFakeJev(o, f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	v.e = e
	return e, f, v
}

// M: past jevChunksMax chunks, jevChunks screens the head chunks and the
// tail only. With a policy max_text_bytes below about 2,400 (the policy
// allows 256), the middle of a 16 KiB screen.text is never sent to Jev, and
// the call answers "pass" with a signed receipt for text it did not screen.
func TestReviewScreenCoversTheWholeText(t *testing.T) {
	e, f, _ := newPayloadEnv(t, `{"schema":1,"version":1,"jev":{"model":"jev-1.13.0","daily_spend_cap_microusd":2000000,"screen_daily_spend_cap_microusd":500000,"price_per_mtok_microusd":42000,"max_text_bytes":1024,"timeout_ms":5000}}`, 1000)
	text := strings.Repeat("a", 8000) + "PAYLOAD" + strings.Repeat("b", 16384-8000-7)
	r, err := e.ScreenText(context.Background(), text, "web", "")
	if err == nil && r.Scores["injection"] < 0.5 {
		t.Fatalf("a 16 KiB text with its payload at byte 8000 was screened in %d Jev calls that never saw it: injection %.2f, no error (the service would sign a pass)", f.requests.Load(), r.Scores["injection"])
	}
}

// L: a multi-chunk screen whose later chunk hits the screen sub-cap (or any
// later-chunk error) fails after Jev answered and billed the first chunk;
// services refunds the caller in full, so the caller never pays for it.
func TestReviewScreenNoJevCallBeforeTheWholeReservation(t *testing.T) {
	// The first 16 KiB chunk's estimate (~732) fits the sub-cap, the second
	// (~434 more) does not.
	e, f, v := newPayloadEnv(t, `{"schema":1,"version":1,"jev":{"model":"jev-1.13.0","daily_spend_cap_microusd":2000000,"screen_daily_spend_cap_microusd":1000,"price_per_mtok_microusd":42000,"max_text_bytes":12000,"timeout_ms":5000}}`, -1)
	_, err := e.ScreenText(context.Background(), strings.Repeat("x", 16384), "web", "")
	var spent int64
	_ = v.db.QueryRow("SELECT coalesce((SELECT spent_microusd FROM moderation_screen_spend),0)").Scan(&spent)
	if errors.Is(err, errSpendCap) && f.requests.Load() > 0 {
		t.Fatalf("refused at the cap after %d Jev call(s) already spent %d microUSD; the caller is refunded all of it", f.requests.Load(), spent)
	}
}

// L: when Jev reports no usage (or more tokens than the body's bytes),
// askJev bills the byte estimate. For a short text that is ~229 microUSD
// while the screen.text ceiling is 85, so the service pays ~2.7x what it
// may charge.
func TestReviewScreenCostWithinItsCeiling(t *testing.T) {
	e, _, _ := newPayloadEnv(t, "", 0)
	r, err := e.ScreenText(context.Background(), "x", "web", strings.Repeat("i", 256))
	if err != nil {
		t.Fatal(err)
	}
	if ceiling := int64(5 + 80); 5+r.CostMicroUSD > ceiling {
		t.Fatalf("a 1-byte screen cost %d microUSD, above its %d-credit ceiling", r.CostMicroUSD, ceiling)
	}
}
