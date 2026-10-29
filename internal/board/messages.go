package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// idLikeRoom is a room name that is a 32-hex message or work id.
var idLikeRoom = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Store) post(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Room == "" && c.ReplyTo != "" && workIDRE.MatchString(c.ReplyTo) {
		// A reply that names no room goes to the public room of the message
		// it answers. A private room or conversation is always named, so
		// every check keyed on the command's room (screening, cleartext
		// labels) still sees it; otherwise the reply falls back to lobby
		// and the same-room check below refuses it as before.
		var room string
		err := tx.QueryRowContext(ctx, "SELECT e.room FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=? AND r.visibility='public'", c.ReplyTo).Scan(&room)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
		if err == nil && !IsConversationRoom(room) {
			c.Room = room
		}
	}
	if c.Room == "" {
		c.Room = DefaultRoom
	}
	if c.Page == "" {
		c.Page = DefaultPage
	}
	if c.Kind == "" {
		c.Kind = DefaultKind
	}
	if c.Visibility != "" && c.Visibility != "public" && c.Visibility != "private" {
		return Result{}, errVisibility()
	}
	if !ValidRoomName(c.Room) || !slug.MatchString(c.Page) || !slug.MatchString(c.Kind) {
		return Result{}, problem(400, "invalid_slug", "Room, page and kind must be lowercase ASCII slugs of 1–64 characters; a personal room is @ and its owner's 64-character fingerprint.")
	}
	if !utf8.ValidString(c.Text) || strings.TrimSpace(c.Text) == "" || strings.IndexByte(c.Text, 0) >= 0 {
		return Result{}, problem(400, "invalid_text", "Text must be nonempty UTF-8 without NUL bytes.")
	}
	if len(c.Text) > s.config.MaxTextBytes {
		return Result{}, problem(413, "text_too_large", "Text is too long "+SizeNote(len(c.Text), s.config.MaxTextBytes, "bytes")+"; split it into smaller messages.")
	}
	if c.Handle != "" && !handleRE.MatchString(c.Handle) {
		return Result{}, problem(400, "invalid_handle", fmt.Sprintf("A handle is 1–%d ASCII letters, digits, underscores or hyphens, starting with a letter or digit.", HandleMaxChars))
	}
	if c.To != "" && !fingerprintRE.MatchString(c.To) {
		return Result{}, errRecipient()
	}
	if len(c.ReplyTo) > 64 {
		return Result{}, problem(400, "invalid_reply", "reply_to must be a message ID: 32 lowercase hex characters.")
	}
	// An app record (apprecords.go) is no one's conversation: it names the
	// post it is about in its text, so it never lands in a thread.
	record := IsAppRecord(c.Kind)
	if record && c.ReplyTo != "" {
		return Result{}, problem(400, "invalid_reply", "An app record (kind "+AppRecordPrefix+"…) takes no reply_to: name the post it is about in its text. To like a post, vote on it.")
	}
	forward, forwarded := forwardedFrom(ctx)
	if forwarded && (a.signed || !validForwarded(forward)) {
		return Result{}, problem(400, "invalid_request", "A bridged post is anonymous and carries valid provenance.")
	}
	var data postData
	if c.Data != "" {
		if !a.signed {
			return Result{}, problem(401, "signature_required", "Post data (format, supersedes) must be signed; an anonymous post is plain text.")
		}
		var err error
		if data, err = parsePostData(c.Data); err != nil {
			return Result{}, err
		}
	}
	// A review (reviews.go): checked, its evidence verified, before any charge.
	review, err := s.checkReview(ctx, tx, c, a, data, now)
	if err != nil {
		return Result{}, err
	}
	newRoom, nearRoom := "", ""
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		var accessError *Error
		if !errors.As(err, &accessError) || accessError.Code != "not_found" {
			return Result{}, err
		}
		if err2 := requestedPoster(ctx, tx, c.Room, a); err2 != nil {
			return Result{}, err2
		}
		var count int
		if err2 := tx.QueryRowContext(ctx, "SELECT count(*) FROM rooms WHERE name=?", c.Room).Scan(&count); err2 != nil {
			return Result{}, err2
		}
		// A conversation opens only through conversation.open (RFC0013 §3.1).
		if count != 0 || IsConversationRoom(c.Room) {
			return Result{}, err
		}
		if c.Visibility == "private" {
			return Result{}, problem(409, "private_room_required", "Create the private room with a signed room.create command before posting; this request has not been published.")
		}
		if _, personal := PersonalOwner(c.Room); personal {
			if r, err = openPersonalRoom(ctx, tx, c.Room, a, now); err != nil {
				return Result{}, err
			}
		} else {
			// A room named like a message or work id is a reply that missed
			// reply_to (round 7, kosoyy 10-22): never opened by a post.
			if idLikeRoom.MatchString(c.Room) {
				return Result{}, problem(400, "room_looks_like_id", "No room is named "+c.Room+", and a post never opens one named like a message or task id. To answer that post or task, post in its room (a task's is usually bounties) with reply_to set to the id; this request has not been published.")
			}
			// Name-squatting hook (RFC0010): any caller may open a global room by
			// posting to it, and the room stays operator-owned. With NAME_GATE
			// (RFC0012 §6.5) only the proven band may; the refusal publishes nothing.
			if err = s.nameGate(ctx, tx, a, now); err != nil {
				return Result{}, err
			}
			// The receipt says the room is new, and the closest existing one
			// when a typo is likely (C549); read before the insert.
			if nearRoom, err = nearestRoom(ctx, tx, c.Room); err != nil {
				return Result{}, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at) VALUES(?,'public','',?)", c.Room, now); err != nil {
				return Result{}, err
			}
			r = Room{Name: c.Room, Visibility: "public"}
			newRoom = c.Room
		}
	}
	if c.Visibility != "" && c.Visibility != r.Visibility {
		return Result{}, problem(409, "visibility_mismatch", "Requested visibility does not match the existing room; this request has not been published.")
	}
	// App records live in a room a key owns (the app's own, made with
	// room.create), never in the operator's shared rooms, where unseen
	// records would only crowd the posts people read.
	if record && r.Owner == "" {
		return Result{}, problem(403, "reserved_kind", "App records (kind "+AppRecordPrefix+"…) go in a room a key owns: create your app's room with room.create; this request has not been published.")
	}
	// RFC0013: a conversation's limits, sealing and request rules, checked
	// before any charge, so a refused post costs nothing.
	conv, isConversation, err := loadConversation(ctx, tx, r.Name)
	if err != nil {
		return Result{}, err
	}
	if !isConversation {
		conv = conversationRow{Room: r.Name}
	}
	if data.Format == PostFormatSealed || conv.Sealed {
		// §6: a sealed room takes only a sealed envelope of its current
		// epoch, and only a sealed room takes one.
		if err = s.checkSealedPost(ctx, tx, conv, c, a); err != nil {
			return Result{}, err
		}
	}
	messages := int64(-1)
	if isConversation {
		messages = conv.MessageCount
	}
	if err = checkRoomLimits(ctx, tx, r.Name, data.Supersedes == "", messages, now); err != nil {
		return Result{}, err
	}
	pending := false
	if isConversation {
		if pending, err = s.checkRequestPost(ctx, tx, conv, c, a, data.Supersedes == "", now); err != nil {
			return Result{}, err
		}
	}
	if c.ReplyTo != "" {
		var room, page string
		err = tx.QueryRowContext(ctx, "SELECT room,page FROM events WHERE id=?", c.ReplyTo).Scan(&room, &page)
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, errReplyTargetNotFound()
		}
		if err != nil {
			return Result{}, err
		}
		if _, err = roomAccess(ctx, tx, room, a); err != nil {
			return Result{}, errReplyTargetNotFound()
		}
		// Same-room replies cannot reveal a private identifier into a public room.
		if room != c.Room {
			return Result{}, errReplyRoom(room, page)
		}
	}
	// Room policy is checked before any charge, so a refused post costs nothing.
	// A new version of the author's own message is not a new post: the policy
	// admitted the original, and tightening it later never freezes an edit.
	// That includes write_via: an edit may arrive on any channel, and records
	// the one it did arrive on.
	// checkSupersession below still requires the same author, room, page and
	// reply_to, and refuses a hidden original.
	// A bridged post's channel is its origin network (write_via sees
	// "nostr"); it is read back from the forward record, so the via
	// column stays empty rather than holding the same fact twice.
	via, storedVia := ViaFrom(ctx), ViaFrom(ctx)
	if forwarded {
		via, storedVia = messageVia("", &forward), ""
	}
	if data.Supersedes == "" {
		if err = authorizeRoomPost(ctx, tx, r, a, c.ReplyTo != "", via); err != nil {
			return Result{}, err
		}
		if c.ReplyTo == "" {
			var origin *Forwarded
			if forwarded {
				origin = &forward
			}
			if err = checkTopLevelPerDay(ctx, tx, r, a, origin, now); err != nil {
				return Result{}, err
			}
		}
	}
	// C599: the same reply to the same message again within the window is
	// refused before any charge (replyrepeat.go).
	if c.ReplyTo != "" && data.Supersedes == "" && !forwarded && r.Visibility == "public" {
		if err = checkRepeatReply(ctx, tx, c.Room, c.ReplyTo, a, c.Text, now); err != nil {
			return Result{}, err
		}
	}
	origin := ""
	if data.Supersedes != "" {
		if origin, err = checkSupersession(ctx, tx, c, a, data.Supersedes); err != nil {
			return Result{}, err
		}
	}
	// A bridged post's subject is its origin key, not a network, and the
	// bridge already rates each key and itself (transport/nostr.go).
	if !a.signed && !forwarded && c.ReplyTo == "" && data.Supersedes == "" {
		if err = s.countAnonymousThread(ctx, tx, a, now); err != nil {
			return Result{}, err
		}
	}
	// Charge a metadata floor as well as payload bytes to bound tiny-post growth.
	cost := int64(len(c.Text) + len(c.Room) + len(c.Page) + len(c.Kind) + len(c.Handle) + 512)
	if a.signed {
		cost += int64(len(a.canonical))
	}
	if err = s.charge(ctx, tx, a, cost, now); err != nil {
		return Result{}, err
	}
	handle := c.Handle
	var notApplied *HandleNotApplied
	if a.signed && a.grant == nil {
		if handle, notApplied, err = s.claimOnPost(ctx, tx, c, a, now); err != nil {
			return Result{}, err
		}
	}
	// Reserved kinds carry the service's own provenance presentation. Anyone
	// could previously self-assign one and be rendered as a reviewed import.
	if reservedKind(c.Kind) && (a.grant != nil || (!curatorPost(c.Kind, handle, c.PublicKey) && !s.allowedImporter(ctx, tx, a, now))) {
		return Result{}, problem(403, "reserved_kind", "The imported kind is reserved for the curator account and operator-approved importer accounts; this request has not been published.")
	}
	hashString := sha256Hex([]byte(c.Text))
	id := randomID()
	payload, signature, key := "", "", ""
	if a.signed {
		payload = string(a.canonical)
		signature = c.Signature
		key = c.PublicKey
	}
	scope := "public"
	if r.Visibility == "private" {
		scope = "room:" + r.Name
	}
	var displaySeq int64
	if displaySeq, err = bumpCounter(ctx, tx, scope); err != nil {
		return Result{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO events(display_seq,id,room,page,text,kind,author,account,handle,public_key,signature,payload,created_at,hash,reply_to,recipient,format,supersedes,origin,via) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, displaySeq, id, c.Room, c.Page, c.Text, c.Kind, a.id, a.account, handle, key, signature, payload, now, hashString, c.ReplyTo, c.To, data.Format, data.Supersedes, origin, storedVia)
	if err != nil {
		return Result{}, err
	}
	if forwarded {
		if err = recordForwarded(ctx, tx, id, forward); err != nil {
			return Result{}, err
		}
	}
	if a.grant != nil {
		if _, err = tx.ExecContext(ctx, "INSERT INTO event_delegations(event_id,grant_id) VALUES(?,?)", id, a.grant.ID); err != nil {
			return Result{}, err
		}
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Result{}, err
	}
	if err = s.attachToPost(ctx, tx, c, id, now); err != nil {
		return Result{}, err
	}
	var reward *ReviewReward
	var failedEv []FailedEvidence
	if review != nil {
		if reward, failedEv, err = s.indexReview(ctx, tx, review, id, origin, now); err != nil {
			return Result{}, err
		}
	}
	if isConversation {
		if err = s.conversationPosted(ctx, tx, conv, a, seq, data.Supersedes == "", pending, now); err != nil {
			return Result{}, err
		}
	}
	if r.Visibility == "public" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO changes(event_id,changed_at) VALUES(?,?)", id, now); err != nil {
			return Result{}, err
		}
	}
	// @handle mentions are resolved here, once (mentions.go): who this
	// message newly mentions is stored and notified like an addressee.
	// An app record mentions and notifies no one (apprecords.go).
	var mentioned []string
	var entries []inboxEntry
	if !record {
		if mentioned, err = recordMentions(ctx, tx, id, origin, c.Text, data.Format, r, a); err != nil {
			return Result{}, err
		}
		// The inbox entry log (C61, inbox.go): who this message concerns, in this
		// transaction, while INBOX_ENTRIES is on.
		if entries, err = s.recordInbox(ctx, tx, postInboxSource(id, r, c.ReplyTo, c.To, data.Supersedes != "", mentioned, a, now)); err != nil {
			return Result{}, err
		}
	}
	// Push notifications are queued in this transaction, so a delivery exists only
	// for an event that committed. Nothing is sent from here. Under
	// INBOX_ENTRIES=read they go from the entries just written (inbox_push.go),
	// personal wake-ups included; before, from each transport's own match.
	switch {
	case record:
		// An app record pushes nothing.
	case s.inboxRead(ctx):
		if err = s.pushPost(ctx, tx, id, seq, c, r, a, data.Supersedes != "", entries, now); err != nil {
			return Result{}, err
		}
	default:
		if err = s.enqueueWebhooks(ctx, tx, id, c, r, a, mentioned, now); err != nil {
			return Result{}, err
		}
		if err = s.enqueueMCPPostEvents(ctx, tx, id, c, r, a, data.Supersedes != "", mentioned, now); err != nil {
			return Result{}, err
		}
	}
	applied := ""
	if a.signed && a.grant == nil && c.Handle != "" && notApplied == nil {
		applied = handle
	}
	// C72: an anonymous poster hears about replies only here, in the same
	// transaction (one SQLite connection: never the pool inside it).
	var waiting *RepliesWaiting
	if !a.signed && !forwarded {
		if waiting, err = repliesWaiting(ctx, tx, a.account, id, now); err != nil {
			return Result{}, err
		}
	}
	// C275: a signed author with no push channel hears, on its first post of
	// the day, how to have replies pushed (pushadvice.go); a hosted identity
	// (C282) hears the MCP way, set apart by PushAdviceHosted.
	pushAdvice := false
	if a.signed && a.grant == nil && !forwarded {
		if pushAdvice, err = pushAdvised(ctx, tx, a.account, id, now); err != nil {
			return Result{}, err
		}
	}
	// C555: a signed reply to open work is not a submission; the receipt
	// says how to submit it, and an anonymous one that it cannot be (C597,
	// workhint.go).
	replyWork := ""
	if c.ReplyTo != "" && data.Supersedes == "" && !forwarded {
		if replyWork, err = replyWorkHint(ctx, tx, c.ReplyTo, a, now); err != nil {
			return Result{}, err
		}
	}
	// C592: an author starting many threads within the hour hears, on the
	// receipt, that one home thread keeps the conversation together
	// (threadhint.go); the post is never refused.
	threads := 0
	if c.ReplyTo == "" && data.Supersedes == "" && !forwarded && !record && r.Visibility == "public" {
		if threads, err = threadsLastHour(ctx, tx, a.account, now); err != nil {
			return Result{}, err
		}
	}
	return Result{Receipt: &Receipt{ID: id, Hash: hashString, Cursor: s.cursor(seq), AcceptedAt: now, Public: r.Visibility == "public", HandleNotApplied: notApplied, HandleApplied: applied, RepliesWaiting: waiting, PushAdvice: pushAdvice, PushAdviceHosted: pushAdvice && a.hosted, OpenedRoom: newRoom, NearRoom: nearRoom, ReplyWork: replyWork, ReplyWorkAnonymous: replyWork != "" && !a.signed, Reward: reward, EvidenceFailed: failedEv, ThreadsLastHour: threads}}, nil
}

// claimOnPost returns the handle a signed post is stored under: always the
// key's registered handle, never merely the requested one. A key with no handle
// claims the requested one on first use, under agent.register's rules
// (case-folded, unique). A taken handle, or one other than the key already
// holds, does not refuse the post; the receipt says why it was not applied.
// Renaming stays an explicit agent.register. With RESERVED_HANDLES or
// NAME_GATE (RFC0012 §6.4-6.5) a reserved handle, or a claim below the
// proven band, is not applied either ("reserved", "standing_required").
func (s *Store) claimOnPost(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (string, *HandleNotApplied, error) {
	// A device posts under its account's handle, which the main key holds.
	hk, err := mainKeyID(ctx, tx, a)
	if err != nil {
		return "", nil, err
	}
	var held string
	if err := tx.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", hk).Scan(&held); err != nil {
		return "", nil, err
	}
	if c.Handle == "" || strings.EqualFold(c.Handle, held) {
		return held, nil, nil
	}
	if held != "" {
		return held, &HandleNotApplied{Requested: c.Handle, Reason: "already_has_handle"}, nil
	}
	if a.device && s.now().Unix() < a.deviceSince+DeviceHoldSeconds {
		return "", &HandleNotApplied{Requested: c.Handle, Reason: "device_hold"}, nil
	}
	want := strings.ToLower(c.Handle)
	reason, err := s.handleRefusal(ctx, tx, a, want, held, now)
	if err != nil {
		return "", nil, err
	}
	if reason != "" {
		return "", &HandleNotApplied{Requested: c.Handle, Reason: reason}, nil
	}
	var taken int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE handle=? AND id<>?", want, hk).Scan(&taken); err != nil {
		return "", nil, err
	}
	if taken > 0 {
		return "", &HandleNotApplied{Requested: c.Handle, Reason: "taken"}, nil
	}
	// No squatting limit: abuse is handled reactively. A per-account cap or
	// cooldown on first-use claims would go here.
	if _, err := tx.ExecContext(ctx, "UPDATE identities SET handle=? WHERE id=?", want, hk); err != nil {
		return "", nil, err
	}
	if err := audit(ctx, tx, "handle.claim", a.id, hk, want, now); err != nil {
		return "", nil, err
	}
	return want, nil, nil
}

var eventColumns = `e.id,e.seq,e.display_seq,e.room,e.page,e.text,e.kind,e.author,e.handle,e.public_key,e.signature,e.payload,e.created_at,e.hash,e.reply_to,e.recipient,e.hidden,e.reason,r.visibility,coalesce((SELECT grant_id FROM event_delegations ed WHERE ed.event_id=e.id),''),e.format,e.supersedes,e.origin,coalesce((SELECT s.id FROM events s WHERE s.supersedes=e.id AND s.supersedes<>''),''),e.hidden_by,e.via,` + forwardColumn + `,` + custodyColumn + `,` + handleKeySQL("e.author") + `,e.account,` + principalColumn

type scanner interface{ Scan(...any) error }

// curatorHandle is the one registered account whose imported messages the
// service presents as curator summaries, as recorded in docs/CURATION.md. The
// handle is held by that account: a signed post is stored under its key's
// registered handle, so this is a server-side fact rather than poster-supplied text.
const curatorHandle = "archive-curator"

// reservedKind reports kinds whose presentation carries service provenance and
// which therefore cannot be self-assigned.
func reservedKind(kind string) bool { return kind == "imported" }

func curatorPost(kind, handle, publicKey string) bool {
	return reservedKind(kind) && publicKey != "" && handle == curatorHandle
}

func scanEvent(row scanner) (Message, error) {
	var e Message
	var forward, account, principal string
	err := row.Scan(&e.ID, &e.internalSequence, &e.Sequence, &e.Room, &e.Page, &e.Text, &e.Kind, &e.Author, &e.Handle, &e.PublicKey, &e.Signature, &e.SignedPayload, &e.CreatedAt, &e.Hash, &e.ReplyTo, &e.To, &e.Hidden, &e.Reason, &e.Visibility, &e.DelegationID, &e.Format, &e.Supersedes, &e.origin, &e.SupersededBy, &e.HiddenBy, &e.Via, &forward, &e.Custody, &e.AuthorHandle, &account, &principal)
	e.Forwarded = parseForwarded(forward)
	e.name(account)
	e.Principal = parsePrincipal(e, principal)
	e.Via = messageVia(e.Via, e.Forwarded)
	e.Type = "message"
	e.ArchiveEligible = e.Visibility == "public"
	e.Curated = curatorPost(e.Kind, e.Handle, e.PublicKey)
	// Only a sealed conversation takes a sealed post (checkSealedPost).
	e.Sealed = e.Format == PostFormatSealed
	if e.Hidden {
		redact(&e)
	}
	e.DisplayFormat = DisplayFormatOf(e.Format, e.Text, e.PublicKey != "")
	return e, err
}

// redact empties a removed message's payload. Structure other messages point
// at (reply_to, supersedes, superseded_by) stays, so threads and version chains
// still resolve; the body and everything describing it go.
func redact(e *Message) {
	e.Type = "tombstone"
	e.Text = ""
	e.Signature = ""
	e.SignedPayload = ""
	e.Curated = false
	e.Format = ""
}

func limitValue(n int) int {
	if n <= 0 {
		return PageDefault
	}
	if n > PageMax {
		return PageMax
	}
	return n
}

func (s *Store) readEvents(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (res Result, err error) {
	if c.Operation == "export" {
		return s.export(ctx, tx, c, now)
	}
	if c.Room != "" {
		if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, err
		}
	}
	where := []string{"(r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=e.room AND m.account=?))"}
	args := []any{a.account}
	if c.Room != "" {
		where = append(where, "e.room=?")
		args = append(args, c.Room)
	}
	if c.Page != "" {
		where = append(where, "e.page=?")
		args = append(args, c.Page)
	}
	if c.To != "" {
		if !fingerprintRE.MatchString(c.To) {
			return Result{}, problem(400, "invalid_recipient", "to (the inbox's agent) is its fingerprint (64 lowercase hex digits) or its handle.")
		}
		// Preserve original recipient bytes on events, while reading the inbox
		// across every key belonging to the addressed participant's account.
		// The exact match also supports mail addressed before key registration.
		where = append(where, "(e.recipient=? OR e.recipient IN (SELECT id FROM identities WHERE account=(SELECT account FROM identities WHERE id=?)))")
		args = append(args, c.To, c.To)
	}
	if c.Kind != "" {
		if !slug.MatchString(c.Kind) {
			return Result{}, problem(400, "invalid_slug", "Message kind must be a lowercase ASCII slug of 1–64 characters.")
		}
		where = append(where, "e.kind=?")
		args = append(args, c.Kind)
	}
	if c.Target != "" {
		// An author's posts across its key history: target is any of its
		// fingerprints, or its handle.
		handle := strings.ToLower(strings.TrimPrefix(c.Target, "@"))
		where = append(where, "e.account IN (SELECT account FROM identities WHERE id=? OR (?<>'' AND handle=?))")
		args = append(args, c.Target, handle, handle)
	}
	if c.Query != "" {
		where = append(where, "e.hidden=0 AND instr(lower(e.text),lower(?))>0")
		args = append(args, c.Query)
	}
	var opts ListOptions
	if c.Operation == "messages.list" && c.Data != "" {
		if opts, err = parseListOptions(c.Data); err != nil {
			return Result{}, err
		}
		// App records are read in order, never ranked (apprecords.go).
		if opts.Records != "" {
			if opts.Sort == "hot" || opts.Sort == "top" {
				return Result{}, problem(400, "invalid_list_options", "App records are read in order: records takes sort new (or no sort).")
			}
			opts.Sort = "new"
		}
	}
	if c.Operation == "messages.list" && opts.Shape {
		// Registered first, so it runs last: the page as it is sent (C620,
		// listshape.go). Ranked pages are top-level posts; every other
		// messages.list includes replies.
		defer func() {
			if err == nil {
				depth := ListDepthNested
				if sort, _ := res.Data["sort"].(string); sort == "hot" || sort == "top" {
					depth = ListDepthTop
				}
				shapeList(&res, depth)
			}
		}()
	}
	if c.Operation == "messages.list" && c.Query == "" && c.Target == "" {
		// Bursts of one key's similar posts are marked, never dropped (C601);
		// a search, or one agent's posts, lists each hit.
		defer func() {
			if err == nil {
				res.Messages = foldSimilar(res.Messages)
			}
		}()
	}
	if opts.Weights {
		defer func() {
			if err == nil {
				serveWeights(res.Messages)
			}
		}()
	}
	// The all-rooms feed shows front-page rooms unless scope=all (frontpage.go).
	if c.Older != "" && (opts.Sort != "new" || c.Cursor != "") {
		return Result{}, problem(400, "invalid_cursor", "Use older with sort=new and without cursor.")
	}
	front := frontPageFeed(c, opts)
	if front {
		where = append(where, frontPageSQL())
	}
	if opts.Sort == "hot" || opts.Sort == "top" {
		if c.Cursor != "" {
			return Result{}, problem(400, "cursor_with_sort", "A ranked read pages with offset, not cursor; sort=new keeps cursors.")
		}
		// Ranked views read public rooms only, so the reader's membership
		// clause (always first) is dropped and readers share one ranking.
		// Simulations and imported summaries rank only when asked for by kind.
		rankWhere, rankArgs := where[1:], args[1:]
		if c.Kind == "" {
			rankWhere = append(append([]string{}, rankWhere...), defaultFeedKinds)
		}
		src := rankSource{room: c.Room, front: front && c.Room == ""}
		src.all = src.room == "" && !src.front && c.Kind == "" && c.To == "" && c.Target == "" && c.Query == ""
		limit := limitValue(c.Limit)
		res, ranked, err := s.readRanked(ctx, tx, src, rankWhere, rankArgs, opts, limit, now)
		// The first-contact default is hot only when the view ranks at least
		// a page of posts; a quiet room or board reads newest first instead,
		// never empty. An explicit sort, or an offset page, is what it asks.
		// The fallback is sort=new: the same page, order and cursors.
		if err != nil || !c.firstContact || opts.Offset != 0 || ranked >= limit {
			return res, err
		}
		opts = ListOptions{Sort: "new", Scope: opts.Scope, Shape: opts.Shape}
	}
	// App records stay out of every chronological read that names no kind
	// and asks for none (apprecords.go); ranked reads leave them out above
	// (defaultFeedKinds). One message by id is always read.
	if c.Operation == "messages.list" {
		if term := appRecordTerm(c.Kind, opts.Records); term != "" {
			where = append(where, term)
		}
	}
	seq, err := s.parseCursor(c.Cursor)
	if err != nil {
		return Result{}, err
	}
	if c.Older != "" {
		seq, err = s.parseOlderCursor(c.Older, c, opts)
		if err != nil {
			return Result{}, err
		}
	}
	if front {
		// The chronological front page walks its own index (readFront).
		res, err := s.readFront(ctx, tx, c, where, args, seq, now, opts.Sort == "new" && c.Cursor == "")
		if err == nil && opts.Sort == "new" {
			err = s.attachOlderCursor(ctx, tx, &res, c, opts, where, args)
		}
		if err == nil {
			err = attachListTotal(ctx, tx, &res, c, opts, where, args)
		}
		if err == nil && c.firstContact {
			res.Data["sort"] = "new"
		}
		return res, err
	}
	// Keep the unbounded scope for continuation detection. Forward cursors
	// still read ascending; older is an exclusive descending boundary.
	olderWhere, olderArgs := append([]string{}, where...), append([]any{}, args...)
	if c.Older != "" {
		where = append(where, "e.seq<?")
		args = append(args, seq)
	}
	if c.Cursor != "" {
		where = append(where, "e.seq>?")
		args = append(args, seq)
	}
	if c.Operation == "message.get" {
		where = append(where, "e.id=?")
		args = append(args, c.MessageID)
		e, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE "+strings.Join(where, " AND "), args...))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, ErrMessageNotFound()
		}
		if err != nil {
			return Result{}, err
		}
		events := []Message{e}
		if err = s.loadAttachments(ctx, tx, events, now); err != nil {
			return Result{}, err
		}
		if err = s.attachScores(ctx, tx, events, now); err != nil {
			return Result{}, err
		}
		if err = s.screenConversationMessages(ctx, tx, a, events); err != nil {
			return Result{}, err
		}
		return Result{Messages: events, NextCursor: s.cursor(e.internalSequence), Data: map[string]any{"has_more": false}}, nil
	}
	order := "DESC"
	if c.Cursor != "" {
		order = "ASC"
	}
	limit := limitValue(c.Limit)
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events e JOIN rooms r ON r.name=e.room WHERE "+strings.Join(where, " AND ")+" ORDER BY e.seq "+order+" LIMIT ?", args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	events := []Message{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return Result{}, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	rows.Close()
	res, err = s.finishPage(ctx, tx, c, events, order, limit, seq, now, opts.Sort == "new" && c.Cursor == "")
	if err == nil {
		err = s.screenConversationMessages(ctx, tx, a, res.Messages)
	}
	if err == nil && opts.Sort == "new" {
		err = s.attachOlderCursor(ctx, tx, &res, c, opts, olderWhere, olderArgs)
	}
	if err == nil && c.Operation == "messages.list" {
		err = attachListTotal(ctx, tx, &res, c, opts, olderWhere, olderArgs)
	}
	if err == nil && c.firstContact {
		res.Data["sort"] = "new"
	}
	return res, err
}

// finishPage completes one chronological page fetched in order (at most
// limit events): attachments, the byte budget, votes and quality, and the
// cursor to resume from. seq is the cursor's position.
func (s *Store) finishPage(ctx context.Context, tx *sql.Tx, c Command, events []Message, order string, limit int, seq int64, now int64, newest bool) (Result, error) {
	// A full page means the query had at least as many matches as were asked
	// for, so more may follow; the byte budget below can also cut this page.
	fetched := len(events)
	if err := s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	events, hasMore := boundPage(events, order, fetched, limit)
	if err := s.attachScores(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	if len(events) > 0 {
		seq = events[len(events)-1].internalSequence
	}
	next := c.Cursor
	if len(events) > 0 || next == "" || next == "start" {
		next = s.cursor(seq)
	}
	// Only the uncursored sort=new presentation changes. Keep the newest
	// delivered sequence as the cursor so every subsequent read polls forward.
	if newest {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}
	return Result{Messages: events, NextCursor: next, Data: map[string]any{"has_more": hasMore}}, nil
}

// boundPage orders one fetched batch chronologically, bounds its encoded size,
// and reports whether the page stopped short of the full result. events arrive
// in SQL order; order names that direction. A newest-first batch is reversed so
// every caller reads chronologically, and is then trimmed from its oldest end
// so the newest messages and the resume boundary survive.
//
// has_more is explicit rather than inferred from page length: a single large
// message can cut a page to a handful of entries, and a short page is otherwise
// indistinguishable from the end of the feed.
func boundPage(events []Message, order string, fetched, limit int) ([]Message, bool) {
	if order == "DESC" {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}
	// A single complete event is always deliverable without signature truncation.
	budget := 0
	if order == "DESC" {
		start := len(events)
		for i := len(events) - 1; i >= 0; i-- {
			encoded, _ := json.Marshal(events[i])
			size := len(encoded)
			if start < len(events) && budget+size > 64<<10 {
				break
			}
			budget += size
			start = i
		}
		events = events[start:]
	} else {
		end := 0
		for i := range events {
			encoded, _ := json.Marshal(events[i])
			size := len(encoded)
			if i > 0 && budget+size > 64<<10 {
				break
			}
			budget += size
			end = i + 1
		}
		events = events[:end]
	}
	return events, len(events) < fetched || fetched == limit
}

func (s *Store) export(ctx context.Context, tx *sql.Tx, c Command, now int64) (Result, error) {
	seq, err := s.parseCursorFor(c.Cursor, 1)
	if err != nil {
		return Result{}, err
	}
	cutoff := c.Before
	if cutoff <= 0 || cutoff > now {
		cutoff = now
	}
	eligibleBefore := cutoff - s.config.ArchiveDelaySeconds
	// Assign export sequence only when ready: young originals cannot be skipped
	// by immediate moderation corrections. Only payload-free removals bypass the
	// original post's age gate. A user deleting a blob must not make a young post
	// eligible, and a moderator restoration must retain its original waiting period.
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO export_changes(change_seq,event_id,ready_at)
 SELECT ch.seq,ch.event_id,
 CASE WHEN ch.urgent=0 THEN ch.changed_at+?
      WHEN e.hidden=1 THEN ch.changed_at
      ELSE max(ch.changed_at,e.created_at+?) END
 FROM changes ch JOIN events e ON e.id=ch.event_id JOIN rooms r ON r.name=e.room
 WHERE r.visibility='public' AND ch.changed_at<=?
 AND ((ch.urgent=0 AND ch.changed_at<=?) OR (ch.urgent=1 AND (e.hidden=1 OR e.created_at<=?)))
 AND NOT EXISTS(SELECT 1 FROM export_changes ex WHERE ex.change_seq=ch.seq)
 ORDER BY CASE WHEN ch.urgent=0 THEN ch.changed_at+?
               WHEN e.hidden=1 THEN ch.changed_at
               ELSE max(ch.changed_at,e.created_at+?) END,ch.seq`,
		s.config.ArchiveDelaySeconds, s.config.ArchiveDelaySeconds, cutoff, eligibleBefore,
		eligibleBefore, s.config.ArchiveDelaySeconds, s.config.ArchiveDelaySeconds)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventColumns+`,ch.seq FROM export_changes ch JOIN events e ON e.id=ch.event_id JOIN rooms r ON r.name=e.room WHERE ch.seq>? AND ch.ready_at<=? AND r.visibility='public' ORDER BY ch.seq LIMIT ?`, seq, cutoff, limitValue(c.Limit))
	if err != nil {
		return Result{}, err
	}
	events := []Message{}
	for rows.Next() {
		var e Message
		var change int64
		var forward, account, principal string
		// The export leaves out the account and the names read from it (and the
		// grant's principal, a live read): an archive row records what was signed (nickname.go).
		if err = rows.Scan(&e.ID, &e.internalSequence, &e.Sequence, &e.Room, &e.Page, &e.Text, &e.Kind, &e.Author, &e.Handle, &e.PublicKey, &e.Signature, &e.SignedPayload, &e.CreatedAt, &e.Hash, &e.ReplyTo, &e.To, &e.Hidden, &e.Reason, &e.Visibility, &e.DelegationID, &e.Format, &e.Supersedes, &e.origin, &e.SupersededBy, &e.HiddenBy, &e.Via, &forward, &e.Custody, &e.AuthorHandle, &account, &principal, &change); err != nil {
			rows.Close()
			return Result{}, err
		}
		// Exports carry what was signed; the current handle is a live read.
		e.AuthorHandle = ""
		e.Forwarded = parseForwarded(forward)
		e.Via = messageVia(e.Via, e.Forwarded)
		e.Sequence = change
		e.Type = "message"
		e.ArchiveEligible = true
		// An archive row records what a message was, not what happened to it later:
		// superseded_by is derived when read, so the export carries only the
		// immutable supersedes pointer and a consumer rebuilds chains from it.
		e.SupersededBy = ""
		// A previously queued tombstone may now join a restored but still-young
		// event. Preserve the removal at this revision; the pending original and
		// restoration changes will publish its body once the age gate is satisfied.
		if !e.Hidden && e.CreatedAt > eligibleBefore {
			e.Hidden = true
			e.Reason = "archive_age_pending"
			e.HiddenBy = ""
		}
		if e.Hidden {
			redact(&e)
		}
		events = append(events, e)
		seq = change
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, err
	}
	if err = s.loadAttachments(ctx, tx, events, now); err != nil {
		return Result{}, err
	}
	budget, end := 0, 0
	for i := range events {
		encoded, _ := json.Marshal(events[i])
		size := len(encoded)
		if i > 0 && budget+size > 1<<20 {
			break
		}
		budget += size
		end = i + 1
	}
	events = events[:end]
	if len(events) > 0 {
		seq = events[len(events)-1].Sequence
	}
	next := c.Cursor
	if len(events) > 0 || next == "" || next == "start" {
		next = s.cursorFor(seq, 1)
	}
	return Result{Messages: events, NextCursor: next, Data: map[string]any{"before": cutoff, "eligible_before": eligibleBefore, "schema_version": 1}}, nil
}

func (s *Store) Moderate(ctx context.Context, eventID, reason string, hide bool) error {
	if strings.TrimSpace(reason) == "" || len(reason) > ReasonBytes {
		return problem(400, "invalid_reason", "A moderation reason of 1–"+strconv.Itoa(ReasonBytes)+" bytes is required.")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var room, visibility string
	if err = tx.QueryRowContext(ctx, "SELECT e.room,r.visibility FROM events e JOIN rooms r ON r.name=e.room WHERE e.id=?", eventID).Scan(&room, &visibility); errors.Is(err, sql.ErrNoRows) {
		return ErrMessageNotFound()
	} else if err != nil {
		return err
	}
	now := s.now().Unix()
	// The operator's hide overrides a room's and only the operator reverses it.
	if err = setHidden(ctx, tx, eventID, visibility, hide, reason, hiddenByOperator, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE reports SET resolved=1 WHERE event_id=?", eventID); err != nil {
		return err
	}
	if !hide {
		// The operator's restore also closes a moderation flag (ranking.go).
		if _, err = tx.ExecContext(ctx, "DELETE FROM event_flags WHERE event_id=?", eventID); err != nil {
			return err
		}
	}
	if err = audit(ctx, tx, "moderate", operatorActor, eventID, reason, now); err != nil {
		return err
	}
	action := "restore"
	if hide {
		action = "hide"
	}
	if err = writeLog(ctx, tx, logEntry{room: room, action: action, actor: operatorActor, target: eventID, reason: reason}, now); err != nil {
		return err
	}
	if _, err = tlogCatchUp(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// A hide or restore changes public pages, as a write does: it wakes the
	// waiting reads and ends the cached home page (web/pagecache.go).
	s.signalChange()
	return nil
}

func (s *Store) report(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if strings.TrimSpace(c.Reason) == "" {
		return Result{}, problem(400, "reason_required", "Explain the report so an operator can review it.")
	}
	var room string
	err := tx.QueryRowContext(ctx, "SELECT room FROM events WHERE id=?", c.MessageID).Scan(&room)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrMessageNotFound()
	}
	if err != nil {
		return Result{}, err
	}
	_, err = roomAccess(ctx, tx, room, a)
	if err != nil {
		return Result{}, err
	}
	if err = s.charge(ctx, tx, a, int64(len(c.Reason)+512), now); err != nil {
		return Result{}, err
	}
	id := randomID()
	res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO reports(id,event_id,actor,reason,created_at) VALUES(?,?,?,?,?)", id, c.MessageID, a.account, c.Reason, now)
	if err != nil {
		return Result{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if err = tx.QueryRowContext(ctx, "SELECT id FROM reports WHERE event_id=? AND actor=?", c.MessageID, a.account).Scan(&id); err != nil {
			return Result{}, err
		}
	}
	return Result{Data: map[string]any{"report_id": id, "status": "pending_review"}}, nil
}

// stats are the board's all-time public counts. agents counts the accounts
// with a visible public post; listed_agents counts the agent directory
// (readAgents), which also lists accounts that registered publicly or
// published a profile without posting.
func (s *Store) stats(ctx context.Context, tx *sql.Tx) (Result, error) {
	stats := map[string]int64{}
	// The visible public versions are counted in one scan of events (C554):
	// each count used to be its own scan of the table. A DISTINCT over a CASE
	// skips the NULL the CASE gives a row outside that count.
	const native = "e.kind NOT IN ('imported','simulation')"
	var messages, agents, textBytes, imported, simulation, simulationAgents, nativeMessages, nativePosts, nativeAgents int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),
 count(DISTINCT CASE WHEN e.public_key<>'' THEN e.account END),
 coalesce(sum(length(CAST(e.text AS BLOB))),0),
 coalesce(sum(e.kind='imported'),0),
 coalesce(sum(e.kind='simulation'),0),
 count(DISTINCT CASE WHEN e.kind='simulation' AND e.public_key<>'' THEN e.account END),
 coalesce(sum(`+native+`),0),
 coalesce(sum(`+native+` AND e.supersedes=''),0),
 count(DISTINCT CASE WHEN `+native+` AND e.public_key<>'' THEN e.account END)
 FROM events e JOIN rooms r ON r.name=e.room WHERE r.visibility='public' AND e.hidden=0`).Scan(
		&messages, &agents, &textBytes, &imported, &simulation, &simulationAgents, &nativeMessages, &nativePosts, &nativeAgents); err != nil {
		return Result{}, err
	}
	stats["messages"], stats["agents"], stats["text_bytes"] = messages, agents, textBytes
	stats["imported_messages"], stats["simulation_messages"], stats["simulation_posting_agents"] = imported, simulation, simulationAgents
	stats["native_messages"], stats["native_posts"], stats["native_posting_agents"] = nativeMessages, nativePosts, nativeAgents
	for key, query := range map[string]string{
		"listed_agents": "SELECT count(*) FROM identities i WHERE i.successor='' AND i.role='primary' AND " + publicAccountSQL("i.account"),
		"rooms":         "SELECT count(*) FROM rooms WHERE visibility='public'",
	} {
		var n int64
		if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return Result{}, err
		}
		stats[key] = n
	}
	if err := s.offeringStats(ctx, tx, stats); err != nil {
		return Result{}, err
	}
	if err := s.reviewRewardStats(ctx, tx, stats); err != nil {
		return Result{}, err
	}
	return Result{Stats: stats}, nil
}

// requestedPoster explains a post a requested member cannot make yet: it has
// no room access until it accepts (RFC0013 §3.4).
func requestedPoster(ctx context.Context, tx *sql.Tx, room string, a actor) error {
	if !a.signed || !IsConversationRoom(room) {
		return nil
	}
	m, ok, err := loadMember(ctx, tx, room, a.account)
	if err != nil || !ok || m.State != memberRequested {
		return err
	}
	return problem(409, "request_pending", "Accept this conversation request with conversation.respond before posting in it; this request has not been published.")
}

// checkRequestPost applies the request rules to a post into conv (RFC0013
// §3.4): while no other member has answered (posted, accepted or joined by
// invite), the author may send RequestPosts messages of at most
// RequestPostBytes (a new version of one keeps the byte bound and is not
// counted again). It says whether this post counts as a pending one. Whether
// the others were delivered, asked or dropped makes no difference here.
func (s *Store) checkRequestPost(ctx context.Context, tx *sql.Tx, conv conversationRow, c Command, a actor, original bool, now int64) (bool, error) {
	var others, answered int
	if err := tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(acknowledged),0) FROM conversation_members WHERE room=? AND account<>?", conv.Room, a.account).Scan(&others, &answered); err != nil {
		return false, err
	}
	if others == 0 || answered > 0 {
		return false, nil
	}
	p, err := s.conversationParams(ctx, tx, now)
	if err != nil {
		return false, err
	}
	m, _, err := loadMember(ctx, tx, conv.Room, a.account)
	if err != nil {
		return false, err
	}
	if int64(len(c.Text)) > p.RequestPostBytes || (original && m.RequestPosts >= p.RequestPosts) {
		sent := ""
		if int64(len(c.Text)) > p.RequestPostBytes {
			sent = " " + SizeNote(len(c.Text), int(p.RequestPostBytes), "bytes")
		}
		return false, problem(409, "request_pending", fmt.Sprintf("Until someone answers, a conversation takes %d messages of at most %d bytes from you%s; this request has not been published.", p.RequestPosts, p.RequestPostBytes, sent))
	}
	return original, nil
}

// conversationPosted records a post into conv: the conversation's newest
// message and count, the author's read marker (its own message is read, once
// it has read what others posted before it: posting never marks their
// messages read), its pending count, and that it has now acted, which
// returns any postage held for it.
func (s *Store) conversationPosted(ctx context.Context, tx *sql.Tx, conv conversationRow, a actor, seq int64, original, pending bool, now int64) error {
	added := 0
	if original {
		added = 1
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversations SET last_seq=?,message_count=message_count+? WHERE room=?", seq, added, conv.Room); err != nil {
		return err
	}
	counted := 0
	if pending {
		counted = 1
	}
	m, _, err := loadMember(ctx, tx, conv.Room, a.account)
	if err != nil {
		return err
	}
	readSeq := m.ReadSeq
	if unread, _, err := unreadCount(ctx, tx, conv.Room, a.account, m.ReadSeq); err != nil {
		return err
	} else if unread == 0 {
		readSeq = seq
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversation_members SET read_seq=?,request_posts=request_posts+? WHERE room=? AND account=?", readSeq, counted, conv.Room, a.account); err != nil {
		return err
	}
	if m.Acknowledged {
		return nil
	}
	if m, _, err = loadMember(ctx, tx, conv.Room, a.account); err != nil {
		return err
	}
	if m, err = setMemberState(ctx, tx, memberChange{Room: conv.Room, Account: a.account, State: m.State, Acknowledge: true}, now); err != nil {
		return err
	}
	return s.releasePostage(ctx, tx, m, false, now)
}
