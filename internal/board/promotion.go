package board

// The promotion rule (room policy promotion, Hugo 2026-10-08): self-promotion
// is welcome on the board, but a room its owner (or, for #lobby, the operator)
// sets to "moderate" is kept for conversation. With moderation on, the post
// screen asks Jev one more question over a public post in such a room: is it
// mainly an advertisement, link-drop or referral that adds nothing to a
// conversation? Only a high-confidence yes hides it, with a public reason that
// says where promotion belongs (moderation/promotion.go holds the question and
// the thresholds). Room officials are never judged by it, and a room set to
// "allow" (the default) never asks.

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"swarmmemo/internal/moderation"
)

const (
	// PromotionAllow is the default: promotion is judged only by the
	// board-wide rules, like any post.
	PromotionAllow = "allow"
	// PromotionModerate hides posts that are mainly advertising.
	PromotionModerate = "moderate"
)

// promotionReason is the public reason on a post the rule hid, for room
// ROOM. It tells the author where the post belongs.
func promotionReason(room, detail string) string {
	reason := "Advertising: " + roomMention(room) + " is kept for conversation. Self-promotion is welcome in your own room (room.create) or #commerce; mentions that add to a discussion are fine."
	if detail != "" {
		reason += " (" + detail + ")"
	}
	return reason
}

// roomMention is #room for a global room, the address for a personal one.
func roomMention(room string) string {
	if strings.HasPrefix(room, "@") {
		return room
	}
	return "#" + room
}

// promotionScope is whether the promotion rule judges post eventID: "" when
// it does not (the room allows promotion, is private, the post is hidden or
// gone, or its author is the room's owner or a moderator), else
// moderation.PromotionPost or moderation.PromotionReply.
func promotionScope(ctx context.Context, tx *sql.Tx, eventID string) (scope moderation.Promotion, room string, err error) {
	var account, publicKey, replyTo, visibility, owner string
	var hidden bool
	err = tx.QueryRowContext(ctx, "SELECT e.room,e.account,e.public_key,e.reply_to,e.hidden,r.visibility,r.owner FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=?", eventID).
		Scan(&room, &account, &publicKey, &replyTo, &hidden, &visibility, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil || hidden || visibility != "public" {
		return "", room, err
	}
	p, err := loadPolicy(ctx, tx, room)
	if err != nil || p.Promotion != PromotionModerate {
		return "", room, err
	}
	// Room officials post what they like in their own room. Only a signed
	// post can be theirs: an anonymous author has no account to match.
	if publicKey != "" {
		if owner != "" && account == owner {
			return "", room, nil
		}
		var moderator int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM room_moderators WHERE room=? AND account=?", room, account).Scan(&moderator); err != nil || moderator > 0 {
			return "", room, err
		}
	}
	if replyTo != "" {
		return moderation.PromotionReply, room, nil
	}
	return moderation.PromotionPost, room, nil
}

// PromotionScope is moderation.PromotionRuler: the worker asks it before it
// screens a post. It runs outside any transaction (the worker holds none).
func (p postActuator) PromotionScope(ctx context.Context, subject string) (moderation.Promotion, error) {
	tx, err := p.s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	scope, _, err := promotionScope(ctx, tx, subject)
	return scope, err
}

// HidePromotion hides a post the promotion rule caught. The rule is the
// room's, so the hide is the room's too (hidden_by room): its owner or a
// moderator may restore it, and the operator always can. The scope is
// checked again in the same transaction, so a room switched back to allow,
// or an author made a moderator, since the screen keeps the post up.
func (p postActuator) HidePromotion(ctx context.Context, subject, detail string) error {
	s := p.s
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	scope, room, err := promotionScope(ctx, tx, subject)
	if err != nil || scope == "" {
		return err
	}
	now := s.now().Unix()
	reason := promotionReason(room, detail)
	if err = setHidden(ctx, tx, subject, "public", true, reason, hiddenByRoom, now); err != nil {
		return err
	}
	if err = audit(ctx, tx, "moderate", operatorActor, subject, reason, now); err != nil {
		return err
	}
	if err = writeLog(ctx, tx, logEntry{room: room, action: "hide", actor: operatorActor, target: subject, reason: reason}, now); err != nil {
		return err
	}
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
