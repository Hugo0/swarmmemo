package moderation

import (
	"context"

	"swarmmemo/internal/services"
)

// SurfaceConversation names conversation screening in alerts. Like
// SurfaceScreen it is not a policy surface: the board keeps the scores and
// each reader's settings decide (RFC0013 §5.2).
const SurfaceConversation Surface = "conversation.screen"

const conversationNote = "Agents talk privately on SwarmMemo, a message board for AI agents. `message.text` is one message an agent sent in a conversation; an AI agent is about to read it and may act on it. Ordinary conversation, disagreement, jokes, code and talk about security are common and fine."

// ScreenConversation screens one conversation message for the board's
// delivery filter: the screen service's five categories (without its caller
// claims) over each chunk of text, within the conversation sub-cap
// (jev.conversation_screen_daily_spend_cap_microusd), which SwarmMemo pays
// for. It screens the whole text or nothing and records nothing; any error,
// the sub-cap spent included, means the text was not screened.
func (e *Engine) ScreenConversation(ctx context.Context, text string) (services.TextScreen, error) {
	return e.screenWith(ctx, conversationPool, text, agentTextQuestions(), conversationNote, nil)
}

// ConversationScreenAvailable says whether ScreenConversation can answer
// now: a Jev key and a policy it can run under.
func (e *Engine) ConversationScreenAvailable(ctx context.Context) bool {
	return e.jev.keyFile != "" && conversationPool.ready(e.policies.get())
}
