package moderation

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// qualityRecorder is a post Actuator that also keeps quality scores and
// review flags.
type qualityRecorder struct {
	recorder
	qmu    sync.Mutex
	scores []string
	flags  []string
}

func (r *qualityRecorder) RecordFlag(_ context.Context, subject string, open bool) error {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	r.flags = append(r.flags, fmt.Sprintf("%s %v", subject, open))
	return nil
}

func (r *qualityRecorder) flagList() []string {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	return append([]string(nil), r.flags...)
}

func (r *qualityRecorder) RecordQuality(_ context.Context, subject string, quality float64, model string) error {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	r.scores = append(r.scores, fmt.Sprintf("%s %.2f %s", subject, quality, model))
	return nil
}

func (r *qualityRecorder) list() []string {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	return append([]string(nil), r.scores...)
}

// The post screen asks the quality question in the same request as the
// moderation questions; the score leaves the decision as Quality and never
// reaches the policy, even one that flags every category it does not list.
func TestPostScreenAsksQualityInTheSameRequest(t *testing.T) {
	v := newEnv(t, `{"schema":1,"version":4,"surfaces":{"post":{"classifiers":["jev"],"on_unavailable":"flag","default":{"thresholds":[{"at":0.6,"action":"flag"}]}}}}`)
	v.jev.set(map[string]float64{QualityCategory: 0.97})
	d := v.screen(t, SurfacePost, "q1", "Measured: sqlite WAL checkpoints stall writers for 40 ms at 1 GB.")
	if v.jev.requests.Load() != 1 || !v.jev.lastAsks[QualityCategory] || !v.jev.lastAsks["phishing"] {
		t.Fatalf("one request with both kinds of question: %d %v", v.jev.requests.Load(), v.jev.lastAsks)
	}
	if d.Quality == nil || *d.Quality != 0.97 || d.qualityModel != "jev-1.13.0" {
		t.Fatalf("quality %v %q", d.Quality, d.qualityModel)
	}
	if _, ok := d.Scores[QualityCategory]; ok || d.Action != Allow || d.Category == QualityCategory || d.Reason != "" {
		t.Fatalf("the quality score reached the policy: %+v", d)
	}
	// The other surfaces never ask it.
	v.screen(t, SurfaceInferenceOutput, "o1", "some model output")
	if v.jev.lastAsks[QualityCategory] {
		t.Fatal("inference output was asked the quality question")
	}
	if d := v.screen(t, SurfaceRunCode, "c1", "print(1)"); d.Quality != nil || v.jev.lastAsks[QualityCategory] {
		t.Fatalf("code was asked the quality question: %+v", d)
	}
}

// The async post worker hands the score to an actuator that keeps quality;
// without Jev there is no score and nothing is recorded.
func TestAsyncPostScreenRecordsQuality(t *testing.T) {
	v := newEnv(t, "")
	act := &qualityRecorder{}
	v.e.SetActuator(SurfacePost, act)
	ctx := context.Background()
	v.jev.set(map[string]float64{QualityCategory: 0.2})
	if _, err := v.e.ScreenAsync(ctx, SurfacePost, Subject{ID: "m1", Room: "lobby"}, Content{Text: "test"}); err != nil {
		t.Fatal(err)
	}
	if n, err := v.e.Work(ctx); n != 1 || err != nil {
		t.Fatalf("worked %d: %v", n, err)
	}
	if got := act.list(); len(got) != 1 || got[0] != "m1 0.20 jev-1.13.0" {
		t.Fatalf("recorded %v", got)
	}
	v.jev.fail(503)
	if _, err := v.e.ScreenAsync(ctx, SurfacePost, Subject{ID: "m2", Room: "lobby"}, Content{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if got := act.list(); len(got) != 1 {
		t.Fatalf("a score was recorded without Jev: %v", got)
	}
}

// ScoreQuality, the backfill's call, asks the quality question alone and
// spends from the day's cap.
func TestScoreQualityAsksOneQuestion(t *testing.T) {
	v := newEnv(t, "")
	v.jev.set(map[string]float64{QualityCategory: 0.66})
	ctx := context.Background()
	q, model, err := v.e.ScoreQuality(ctx, Subject{ID: "old", Room: "lobby", Signed: true}, "an older post")
	if err != nil || q != 0.66 || model != "jev-1.13.0" {
		t.Fatalf("score %v %q %v", q, model, err)
	}
	if len(v.jev.lastAsks) != 1 || !v.jev.lastAsks[QualityCategory] {
		t.Fatalf("asked %v", v.jev.lastAsks)
	}
	if s, err := v.e.SpendToday(ctx); err != nil || s.SpentMicroUSD <= 0 || s.Calls != 1 {
		t.Fatalf("spend %+v %v", s, err)
	}
	v.jev.fail(503)
	if _, _, err := v.e.ScoreQuality(ctx, Subject{ID: "old2"}, "x"); err == nil {
		t.Fatal("a failed call returned a score")
	}
}

// The quality question is never a safety one: a missing or invalid answer to
// it drops only the score, and the screen still acts on the safety answers
// (phishing is still hidden, not degraded to a flag).
func TestQualityAnswerCannotDegradeTheScreen(t *testing.T) {
	for name, answer := range map[string]any{
		"missing":      nil,
		"out of range": map[string]float64{"noul": 1.7},
		"no value":     map[string]any{"other": 1},
	} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t, "")
			act := &qualityRecorder{}
			v.e.SetActuator(SurfacePost, act)
			v.jev.set(map[string]float64{"phishing": 0.99})
			v.jev.answerRaw(QualityCategory, answer)
			d := v.screen(t, SurfacePost, "p1", "Your wallet is locked: enter your seed phrase at walletfix.example")
			if d.Action != Hide || d.Degraded != "" || d.Category != "phishing" || d.Quality != nil {
				t.Fatalf("decision %+v", d)
			}
			// ScoreQuality (the backfill) gets an error, never a score.
			if _, _, err := v.e.ScoreQuality(context.Background(), Subject{ID: "old"}, "x"); err == nil {
				t.Fatal("a missing quality answer returned a score")
			}
		})
	}
	// Any other malformed answer still fails the call (the surface's fail mode).
	v := newEnv(t, "")
	v.jev.answerRaw("phishing", nil)
	if d := v.screen(t, SurfacePost, "p2", "hello"); d.Degraded == "" {
		t.Fatalf("a missing safety answer was accepted: %+v", d)
	}
}

// A flag that comes only from a classifier being unavailable is not a
// category flag: the post keeps no flag and no forced quality.
func TestDegradedFlagDoesNotFlagForRanking(t *testing.T) {
	v := newEnv(t, "")
	act := &qualityRecorder{}
	v.e.SetActuator(SurfacePost, act)
	ctx := context.Background()
	v.jev.fail(503)
	if _, err := v.e.ScreenAsync(ctx, SurfacePost, Subject{ID: "down", Room: "lobby"}, Content{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if len(act.list()) != 0 || len(act.flagList()) != 0 {
		t.Fatalf("recorded %v %v", act.list(), act.flagList())
	}
}
