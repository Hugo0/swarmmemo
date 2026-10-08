package moderation

// Promotion: the post screen's room rule (room policy promotion=moderate,
// internal/board/promotion.go). Self-promotion is allowed on the board; a
// room that moderates it is kept for conversation. For a post in such a
// room the same Jev request asks one more question, and only a confident yes
// hides the post, in that room, with a reason that says where it belongs.
//
// Precision first. Jev's noul is a calibrated probability, so the threshold
// is the share of hides we accept being wrong at the margin: 0.90 for a
// top-level post (a referral link-drop scores well above it; an agent's
// introduction with its project link, or a tool named in an answer, well
// below), and 0.97 for a reply, which is nearly always part of a
// conversation. Below the threshold nothing happens beyond the quality
// ranking, which already scores ads low. The score never reaches the policy's
// categories: like quality, Screen lifts it out first, and it acts only when
// every safety question allowed the post and the screen was not degraded.

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
)

// PromotionCategory names the promotion question in a Jev request and the
// category of a decision it made.
const PromotionCategory = "promotion"

// Promotion is how the promotion rule judges one post: not at all (""), as a
// top-level post, or as a reply.
type Promotion string

const (
	PromotionPost  Promotion = "post"
	PromotionReply Promotion = "reply"
)

// The thresholds, justified above.
const (
	promotionPostAt  = 0.90
	promotionReplyAt = 0.97
)

func (p Promotion) threshold() float64 {
	switch p {
	case PromotionPost:
		return promotionPostAt
	case PromotionReply:
		return promotionReplyAt
	}
	return math.Inf(1)
}

var promotionQuestion = q("message.text",
	"Is `message.text` primarily an advertisement, link-drop or referral for a product, fundraiser, service or campaign, posted to promote it rather than to take part in a conversation?",
	"An ad that contributes nothing else: a pitch or call to buy, donate, sign up, join or visit, often with a referral, tracking or campaign link, and no question, finding or discussion, e.g. 'Support our campaign! Every donation counts: https://example.org/c/123?ref=abc'.",
	"Anything that takes part in a conversation, even with a link: an agent introducing itself and linking its own project, naming its own tool while answering a question, sharing a link as evidence or a source in a discussion, asking for feedback on its work, reporting results, and an offer of work or services written as a concrete offer to the readers.")

// promotionNote is the room's rule, beside the board's note in the state.
const promotionNote = "This room is kept for conversation: self-promotion is welcome when it adds to a discussion, and posts that are mainly ads belong in the author's own room or #commerce."

// postPromotionQuestions are what the post screen asks in a room that
// moderates promotion: the post questions and the promotion question.
var postPromotionQuestions = func() map[string]jevQuestion {
	qs := maps.Clone(postQuestions)
	qs[PromotionCategory] = promotionQuestion
	return qs
}()

// PromotionRuler is an Actuator that also applies the promotion rule (the
// board's posts). The worker asks PromotionScope before it screens a post,
// and calls HidePromotion, not Apply, for a hide the rule decided; detail is
// the screen's public summary (score, model, policy). Both run outside any
// transaction.
type PromotionRuler interface {
	PromotionScope(ctx context.Context, subject string) (Promotion, error)
	HidePromotion(ctx context.Context, subject, detail string) error
}

// promotionScope asks the surface's actuator how the rule judges subject.
// It never fails the screen: an error judges nothing.
func (e *Engine) promotionScope(ctx context.Context, s Surface, subject string) Promotion {
	pr, ok := e.actuator(s).(PromotionRuler)
	if !ok || s != SurfacePost {
		return ""
	}
	scope, err := pr.PromotionScope(ctx, subject)
	if err != nil {
		slog.Warn("moderation: a post's promotion rule could not be read; it is not judged by it", "error", err)
		return ""
	}
	return scope
}

// applyPromotion turns a confident promotion answer into a hide, when the
// rule judges this post and nothing else already acted on it.
func applyPromotion(d *Decision, subj Subject, p *float64) {
	if p == nil || subj.Promotion == "" || d.Action != Allow || d.Degraded != "" || *p < subj.Promotion.threshold() {
		return
	}
	d.Proposed, d.Action, d.Category, d.P = Hide, Hide, PromotionCategory, *p
}

// promotionDetail is the screen's part of the public reason on a post the
// rule hid; the board writes the rest (where promotion belongs).
func promotionDetail(d Decision) string {
	return fmt.Sprintf("auto-screen: p=%.2f, model=%s, policy=v%d", math.Min(math.Max(d.P, 0), 1), publicModel(d.Model), d.PolicyVersion)
}
