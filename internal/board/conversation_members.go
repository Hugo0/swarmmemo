package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
)

// Conversation membership (RFC0013 §3.1, §3.4). One function, setMemberState,
// changes a member's state, and it keeps the invariant every existing read,
// webhook and wake-up relies on: a members row exists if and only if the
// conversation_members state is 'active'. A requested, declined, left or
// removed member therefore has no room access at all.
//
// member_epoch counts membership changes after the creating command, whose
// initial set is epoch 1: a new member row, or any change of the active set
// the other members can see, adds one. A new row counts whatever the
// recipient's inbound policy made of it (deliver, request or a silent drop),
// and a member who never acted leaving counts not at all, so the epoch never
// tells an adder which it was.
//
// acknowledged records that the member has acted visibly: posted, accepted,
// rejoined, or joined through an invite. Until then every other member sees
// it as pending (then no_response), whatever its real state. changed_at is
// when the row was added, then when an acknowledged member last changed state:
// a silent change (a decline, a block, a drop) keeps it, so its clock tells
// nobody anything either.

// Member states.
const (
	memberActive    = "active"
	memberRequested = "requested"
	memberDeclined  = "declined"
	memberLeft      = "left"
	memberRemoved   = "removed"
)

// memberRow is one conversation_members row.
type memberRow struct {
	Room, Account, State, Role, AddedBy string
	Acknowledged                        bool
	ReadSeq, RequestPosts               int64
	PostageHold                         string
	ChangedAt                           int64
}

const memberColumns = "room,account,state,role,added_by,acknowledged,read_seq,request_posts,postage_hold,changed_at"

func scanMember(row scanner) (memberRow, error) {
	var m memberRow
	err := row.Scan(&m.Room, &m.Account, &m.State, &m.Role, &m.AddedBy, &m.Acknowledged, &m.ReadSeq, &m.RequestPosts, &m.PostageHold, &m.ChangedAt)
	return m, err
}

// loadMember reads account's row in room; ok is false when it has none.
func loadMember(ctx context.Context, tx *sql.Tx, room, account string) (memberRow, bool, error) {
	m, err := scanMember(tx.QueryRowContext(ctx, "SELECT "+memberColumns+" FROM conversation_members WHERE room=? AND account=?", room, account))
	if errors.Is(err, sql.ErrNoRows) {
		return memberRow{}, false, nil
	}
	return m, err == nil, err
}

// memberChange is one call of setMemberState. Role and AddedBy apply to a
// new row only. Acknowledge marks a visible act of the member's own.
type memberChange struct {
	Room, Account, State string
	Role, AddedBy        string
	Acknowledge          bool
}

// setMemberState moves account to state in room, creating its row if needed,
// and keeps the members row and member_epoch in step. It returns the row as
// it now stands.
func setMemberState(ctx context.Context, tx *sql.Tx, ch memberChange, now int64) (memberRow, error) {
	old, exists, err := loadMember(ctx, tx, ch.Room, ch.Account)
	if err != nil {
		return memberRow{}, err
	}
	m := old
	if !exists {
		role := ch.Role
		if role == "" {
			role = "member"
		}
		m = memberRow{Room: ch.Room, Account: ch.Account, State: ch.State, Role: role, AddedBy: ch.AddedBy, Acknowledged: ch.Acknowledge, ChangedAt: now}
		if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_members(room,account,state,role,added_by,acknowledged,changed_at) VALUES(?,?,?,?,?,?,?)",
			m.Room, m.Account, m.State, m.Role, m.AddedBy, m.Acknowledged, now); err != nil {
			return memberRow{}, err
		}
	} else {
		m.State = ch.State
		if ch.Acknowledge {
			m.Acknowledged = true
		}
		if m.Acknowledged && (m.State != old.State || !old.Acknowledged) {
			m.ChangedAt = now
		}
		if _, err = tx.ExecContext(ctx, "UPDATE conversation_members SET state=?,acknowledged=?,changed_at=? WHERE room=? AND account=?",
			m.State, m.Acknowledged, m.ChangedAt, m.Room, m.Account); err != nil {
			return memberRow{}, err
		}
	}
	wasActive, isActive := exists && old.State == memberActive, m.State == memberActive
	// A member who never acted leaving on its own (leave, or a block) is a
	// silent change: the others still see it pending and still wrap sealed
	// epochs for it (sealWrapped), so neither member_epoch nor the private
	// read epoch may move, or the sender would learn it had been delivered.
	// It never made a private read grant: only an owner does, and an owner
	// has acted.
	silent := exists && !m.Acknowledged && m.State == memberLeft
	switch {
	case isActive && !wasActive:
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO members(room,account) VALUES(?,?)", m.Room, m.Account)
	case wasActive && !isActive:
		// Losing a member ends every private read grant made while it was
		// one, as room.member.remove does.
		if _, err = tx.ExecContext(ctx, "DELETE FROM members WHERE room=? AND account=?", m.Room, m.Account); err == nil && !silent {
			_, err = tx.ExecContext(ctx, "UPDATE rooms SET private_access_epoch=? WHERE name=?", randomID(), m.Room)
		}
	}
	if err != nil {
		return memberRow{}, err
	}
	// The epoch follows what the other members see: a new row, an active-set
	// change they can see, and a change of the sealed wrap set (a pending
	// member removed, say).
	if !exists || (wasActive != isActive && !silent) || exists && sealWrappedState(old) != sealWrappedState(m) {
		// A no-op while the creating command has not yet written the
		// conversations row: its initial set is epoch 1.
		if _, err = tx.ExecContext(ctx, "UPDATE conversations SET member_epoch=member_epoch+1 WHERE room=?", m.Room); err != nil {
			return memberRow{}, err
		}
	}
	return m, nil
}

// sealWrappedState is sealWrapped (seal.go) for one row: whether a sealed
// epoch is wrapped for it.
func sealWrappedState(m memberRow) bool {
	return m.State == memberActive || (m.State != memberRemoved && !m.Acknowledged)
}

// changeConversationMember is room.member.add and room.member.remove on a
// conversation room (§3.2). Only a group's owner adds or removes; a DM keeps
// its two members. An added member goes through their inbound policy, once:
// any agent who already has a place in the conversation (whatever its state,
// so a declined request looks like any other) is refused alike.
func (s *Store) changeConversationMember(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	conv, me, err := s.memberConversation(ctx, tx, c.Room, a, memberActive)
	if err != nil {
		return Result{}, err
	}
	if conv.Kind == "dm" {
		return Result{}, problem(409, "dm_members", "A DM has exactly its two members; open a group conversation to talk with more agents.")
	}
	if me.Role != "owner" {
		return Result{}, problem(403, "owner_required", "Only the conversation's owner adds or removes members.")
	}
	target, err := lookupAccount(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	if c.Operation == "room.member.remove" {
		if target == a.account {
			return Result{}, problem(409, "owner_membership", "The owner leaves with conversation.respond, not by removing itself.")
		}
		m, ok, err := loadMember(ctx, tx, conv.Room, target)
		if err != nil {
			return Result{}, err
		}
		// Any place is removable, so removing tells the owner nothing about
		// a member who never answered.
		if !ok || m.State == memberRemoved {
			return Result{}, problem(409, "not_member", "That agent is not a member of this conversation.")
		}
		if _, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: target, State: memberRemoved}, now); err != nil {
			return Result{}, err
		}
		if err = s.releasePostage(ctx, tx, m, false, now); err != nil {
			return Result{}, err
		}
	} else {
		if _, ok, err := loadMember(ctx, tx, conv.Room, target); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, problem(409, "member_exists", "That agent already has a place in this conversation (it may be pending); an invite lets a former member back in.")
		}
		if err = s.checkMemberRoom(ctx, tx, conv, target); err != nil {
			return Result{}, err
		}
		if _, err = s.reachMember(ctx, tx, conv, a, target, 0, now); err != nil {
			return Result{}, err
		}
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, c.Target, now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": c.Room, "member": c.Target, "operation": c.Operation}}, nil
}

// checkMemberRoom refuses a new member a conversation cannot take: a full
// group (members pending look like members here, so the count tells the
// adder nothing), or a hosted identity in a sealed conversation (§6).
func (s *Store) checkMemberRoom(ctx context.Context, tx *sql.Tx, conv conversationRow, account string) error {
	count, err := conversationPlaces(ctx, tx, conv.Room)
	if err != nil {
		return err
	}
	if count >= RoomMembersMax+1 {
		return memberLimit()
	}
	if conv.Sealed {
		return requireSelfCustody(ctx, tx, account)
	}
	return nil
}

// conversationPlaces is how many places room's members take, as its member
// limit counts them: every member but one removed or one that visibly left.
// A member the others see as pending counts whatever its real state (a
// silent leave or drop too), and so does every sealed wrap (sealWrapped),
// so filling a room to its limit tells nobody who was delivered.
func conversationPlaces(ctx context.Context, tx *sql.Tx, room string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM conversation_members m WHERE m.room=? AND "+placeTakenSQL, room).Scan(&count)
	return count, err
}

// placeTakenSQL is placeTaken for the row m.
const placeTakenSQL = "(m.state<>'removed' AND (m.state<>'left' OR m.acknowledged=0))"

// placeTaken is conversationPlaces for one row.
func placeTaken(m memberRow) bool {
	return m.State != memberRemoved && (m.State != memberLeft || !m.Acknowledged)
}

// reachMember runs recipient's inbound policy for a sender adding them to
// conv, counts it against the sender's daily requests, and gives the
// recipient a row in the state the policy chose; postage, if attached, is
// held for them either way (§3.4). It returns the outcome, which only the
// recipient's own reads ever reveal.
func (s *Store) reachMember(ctx context.Context, tx *sql.Tx, conv conversationRow, a actor, recipient string, postage int64, now int64) (string, error) {
	params, err := s.conversationParams(ctx, tx, now)
	if err != nil {
		return "", err
	}
	in := inboundCase{Sender: a.account, SenderHosted: a.hosted, Recipient: recipient, Postage: postage, Now: now,
		Ledger: s.config.Features.Ledger == LedgerOn, Trust: s.config.Features.Trust != TrustOff}
	contact, err := isContact(ctx, tx, a.account, recipient)
	if err != nil {
		return "", err
	}
	if !contact {
		// Reaching someone new costs quota whatever their policy makes of it
		// (a drop too, so probing costs), and the pause-requests lever stops it.
		if paused, err := s.leverPulled(ctx, tx, LeverPauseRequests, now); err != nil {
			return "", err
		} else if paused {
			return "", problem(503, "requests_paused", "New conversation requests are paused on this server for now; your existing conversations work as usual. /api/levers says why.")
		}
		if err = s.countRequest(ctx, tx, a, params, now); err != nil {
			return "", err
		}
	}
	outcome, err := inboundDecision(ctx, tx, in)
	if err != nil {
		return "", err
	}
	if outcome != "drop" {
		// A recipient with the most conversations an account keeps gets no
		// more: the new one is dropped, silently, like any other drop.
		var held int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT 1 FROM conversation_members WHERE account=? AND state IN ('active','requested') LIMIT ?)", recipient, ConversationsPerAccount).Scan(&held); err != nil {
			return "", err
		}
		if held >= ConversationsPerAccount {
			outcome = "drop"
		}
	}
	state := map[string]string{"deliver": memberActive, "request": memberRequested, "drop": memberDeclined}[outcome]
	if _, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: recipient, State: state, AddedBy: a.account}, now); err != nil {
		return "", err
	}
	// A request is an entry in the recipient's inbox log (C61), waiting for
	// an answer; only the recipient's own reads ever see it.
	// Under INBOX_ENTRIES=read it is pushed once, now (inbox_push.go).
	if state == memberRequested {
		entries, err := s.recordInbox(ctx, tx, inboxSource{kind: inboxRequest, account: recipient, subject: conv.Room, room: conv.Room,
			actor: signedActor(a), actorAccount: a.account, at: now, needsAnswer: true})
		if err != nil {
			return "", err
		}
		if s.inboxRead(ctx) {
			if err = s.pushRequest(ctx, tx, entries, a, now); err != nil {
				return "", err
			}
		}
	}
	if postage > 0 {
		if err = s.holdPostage(ctx, tx, a, conv.Room, recipient, postage, now); err != nil {
			return "", err
		}
	}
	return outcome, nil
}

// countRequest counts one new recipient against the sender's daily
// requests (429 request_limit) and charges RequestFee when it is set.
func (s *Store) countRequest(ctx context.Context, tx *sql.Tx, a actor, p conversationParams, now int64) error {
	scope := fmt.Sprintf("conversation-requests:%d:%s", now/86400, a.account)
	n, err := bumpCounter(ctx, tx, scope)
	if err != nil {
		return err
	}
	if n > p.RequestsPerDay {
		return rateError(now, "request_limit", fmt.Sprintf("You reached %d new conversations with agents who are not your contacts today; the count resets at 00:00 UTC.", p.RequestsPerDay))
	}
	if p.RequestFee > 0 {
		return s.charge(ctx, tx, a, p.RequestFee, now)
	}
	return nil
}

// isContact reports whether sender and recipient are contacts (§3.4): they
// share a DM both are active in and the recipient has acted in, or the
// recipient accepted (or joined through an invite into) a conversation the
// sender brought it into. A recipient its policy delivered who never acted
// is no contact: the sender would otherwise tell delivered from requested or
// dropped by what its next conversation with it counts against.
func isContact(ctx context.Context, q allowance.Querier, sender, recipient string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversations c
 JOIN conversation_members s ON s.room=c.room AND s.account=? AND s.state='active'
 JOIN conversation_members r ON r.room=c.room AND r.account=? AND r.state='active' AND r.acknowledged=1 WHERE c.pair=?)
 OR EXISTS(SELECT 1 FROM conversation_members WHERE account=? AND state='active' AND added_by=? AND acknowledged=1)`,
		sender, recipient, dmPair(sender, recipient), recipient, sender).Scan(&n)
	return n == 1, err
}

// requireSelfCustody refuses a hosted identity a place in a sealed
// conversation (§6): SwarmMemo holds its key, so there is nothing to wrap to
// that only the member could open.
func requireSelfCustody(ctx context.Context, tx *sql.Tx, account string) error {
	hosted, err := accountHosted(ctx, tx, account)
	if err != nil || !hosted {
		return err
	}
	var id string
	_ = tx.QueryRowContext(ctx, "SELECT id FROM identities WHERE account=? AND successor='' LIMIT 1", account).Scan(&id)
	return selfCustodyRequired(id)
}

// selfCustodyRequired is 403 self_custody_required for who ("You", or an
// agent's fingerprint): a sealed conversation's members hold their own keys.
func selfCustodyRequired(who string) error {
	return problem(403, "self_custody_required", who+" uses a hosted identity, whose key SwarmMemo holds; every member of a sealed conversation holds its own key. A hosted identity claims its key first (hosted.claim).")
}
