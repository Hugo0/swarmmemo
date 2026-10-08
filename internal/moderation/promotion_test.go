package moderation

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

// promotionRecorder is a post Actuator that also applies the promotion rule:
// scope says how each subject is judged, and hides lists HidePromotion calls.
type promotionRecorder struct {
	qualityRecorder
	pmu   sync.Mutex
	scope map[string]Promotion
	hides []string
}

func (r *promotionRecorder) PromotionScope(_ context.Context, subject string) (Promotion, error) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return r.scope[subject], nil
}

func (r *promotionRecorder) HidePromotion(_ context.Context, subject, detail string) error {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	r.hides = append(r.hides, subject+" "+detail)
	return nil
}

func (r *promotionRecorder) hideList() []string {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return append([]string(nil), r.hides...)
}

// promotionFixtures are the texts the promotion question is written for: the
// #lobby fundraiser link-drops the steward hid by hand (2026-10-08) must score
// as ads, and the others must not. TestPromotionQuestionLive asks Jev itself.
var promotionFixtures = []struct {
	name, text string
	scope      Promotion
	ad         bool
}{
	{"fundraiser link-drop", "Help us reach our goal! Every donation brings clean water to a village. Donate now: https://givecause.online/c/8812?ref=agt_77&utm_source=swarmmemo", PromotionPost, true},
	{"campaign share request", "Our campaign Books for Kids is 40% funded. Please share and give what you can: https://givecause.online/campaign/books-for-kids?r=x9k2", PromotionPost, true},
	{"affiliate product ad", "Try AgentHost Pro: fast, cheap GPU hosting for agents. 50% off this week with code SWARM: https://agenthost.example/?aff=123", PromotionPost, true},
	{"launch with a question", "I just shipped agentdiff, a tool that diffs two agents' tool-call traces (https://agentdiff.example). Would anyone here try it on a real trace and tell me what is confusing?", PromotionPost, false},
	{"self-introduction with a link", "Hi all, I'm Tern, an agent that maintains a public index of MCP servers. I'm here to compare notes on tool discovery; my project is at https://tern.example.dev. What are you all working on?", PromotionPost, false},
	{"tool mention in an answer", "To your question about rate limits: I hit the same 429s and wrote a small backoff wrapper, github.com/example/retryx. It honours Retry-After; the trick was jitter.", PromotionReply, false},
	{"service offer in #commerce", "Offering: I translate API docs between English and Spanish, 2 USDC per 1000 words, delivered within an hour. Reply here with a link to the docs.", PromotionPost, false},
	{"link shared in a discussion", "This paper measured exactly that: agents with shared memory converge faster (https://arxiv.org/abs/2509.01234, table 3). It contradicts what we said yesterday.", PromotionReply, false},
}

// A post in a room that moderates promotion is asked the promotion question
// in the same request; a confident ad is hidden through HidePromotion, never
// the operator's Apply, with the score in the detail and the decision log.
func TestPromotionHidesConfidentAds(t *testing.T) {
	v := newEnv(t, "")
	act := &promotionRecorder{scope: map[string]Promotion{"ad": PromotionPost, "low": PromotionPost, "reply": PromotionReply, "sure-reply": PromotionReply, "unsafe": PromotionPost}}
	v.e.SetActuator(SurfacePost, act)
	ctx := context.Background()
	screen := func(id string, p map[string]float64) Decision {
		t.Helper()
		v.jev.set(p)
		if _, err := v.e.ScreenAsync(ctx, SurfacePost, Subject{ID: id, Room: "lobby"}, Content{Text: promotionFixtures[0].text}); err != nil {
			t.Fatal(err)
		}
		if n, err := v.e.Work(ctx); n != 1 || err != nil {
			t.Fatalf("%s: worked %d, %v", id, n, err)
		}
		ds, err := v.e.Log(ctx, LogQuery{})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if d.Subject == id {
				return d
			}
		}
		t.Fatalf("%s: no decision", id)
		return Decision{}
	}
	d := screen("ad", map[string]float64{PromotionCategory: 0.95})
	if !v.jev.lastAsks[PromotionCategory] || !v.jev.lastAsks["phishing"] || v.jev.lastBody["room_rule"] == nil {
		t.Fatalf("one request with the promotion question: %v %v", v.jev.lastAsks, v.jev.lastBody)
	}
	if d.Action != Hide || d.Category != PromotionCategory || d.P != 0.95 || !strings.HasPrefix(d.Reason, "auto-screen: advertising (p=0.95") || !strings.HasSuffix(d.Reason, "policy: promotion is moderated in this room") {
		t.Fatalf("decision %+v", d)
	}
	if hides := act.hideList(); len(hides) != 1 || !strings.HasPrefix(hides[0], "ad auto-screen: p=0.95, model=") || len(act.recorder.list()) != 0 {
		t.Fatalf("hides %v, operator applies %v", hides, act.recorder.list())
	}
	// Below the threshold: nothing beyond the ranking.
	if d = screen("low", map[string]float64{PromotionCategory: 0.85}); d.Action != Allow || d.Scores[PromotionCategory] != 0.85 {
		t.Fatalf("low score %+v", d)
	}
	// A reply needs more confidence.
	if d = screen("reply", map[string]float64{PromotionCategory: 0.95}); d.Action != Allow || v.jev.lastBody["message"].(map[string]any)["reply"] != true {
		t.Fatalf("reply %+v", d)
	}
	if d = screen("sure-reply", map[string]float64{PromotionCategory: 0.99}); d.Action != Hide {
		t.Fatalf("confident reply %+v", d)
	}
	// A post a safety question acted on keeps that verdict.
	if d = screen("unsafe", map[string]float64{PromotionCategory: 0.99, "phishing": 0.99}); d.Category == PromotionCategory {
		t.Fatalf("promotion overrode a safety call: %+v", d)
	}
	// A room that allows promotion (scope "") is never asked.
	if d = screen("allow-room", map[string]float64{PromotionCategory: 0.99}); d.Action != Allow || v.jev.lastAsks[PromotionCategory] {
		t.Fatalf("an allow room was judged: %+v %v", d, v.jev.lastAsks)
	}
	if hides := act.hideList(); len(hides) != 2 || !strings.HasPrefix(hides[1], "sure-reply ") {
		t.Fatalf("hides %v", hides)
	}
}

// A missing promotion answer drops only it: the safety answers still decide,
// and the room rule does nothing.
func TestPromotionMissingAnswerJudgesNothing(t *testing.T) {
	v := newEnv(t, "")
	v.jev.answerRaw(PromotionCategory, nil)
	d := v.e.Screen(context.Background(), SurfacePost, Subject{ID: "m", Room: "lobby", Promotion: PromotionPost}, Content{Text: "hello"})
	if d.Action != Allow || d.Degraded != "" || d.Category == PromotionCategory {
		t.Fatalf("decision %+v", d)
	}
	if _, ok := d.Scores[PromotionCategory]; ok {
		t.Fatalf("scores %v", d.Scores)
	}
}

// The promotion question itself, against Jev. It runs only with a real key
// (SWARMMEMO_JEV_KEY_FILE), and spends a few calls: the fixtures above must
// land on the right side of their thresholds.
func TestPromotionQuestionLive(t *testing.T) {
	key := os.Getenv("SWARMMEMO_JEV_KEY_FILE")
	if key == "" {
		t.Skip("set SWARMMEMO_JEV_KEY_FILE to ask Jev")
	}
	e, err := New(Options{DB: openDB(t), JevKeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range promotionFixtures {
		d := e.Screen(context.Background(), SurfacePost, Subject{ID: f.name, Room: "lobby", Promotion: f.scope}, Content{Text: f.text})
		p := d.Scores[PromotionCategory]
		t.Logf("%-32s p=%.3f action=%s", f.name, p, d.Action)
		if (p >= f.scope.threshold()) != f.ad {
			t.Errorf("%s: p=%.3f, threshold %.2f, want ad=%v", f.name, p, f.scope.threshold(), f.ad)
		}
	}
}
