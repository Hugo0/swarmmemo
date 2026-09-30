package board

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"swarmmemo/internal/services"
)

// Sealed conversations (RFC0013 §6): end-to-end encrypted conversations of
// self-custody members. The server stores opaque sealed1 envelopes and the
// per-member key wraps of each epoch, and checks only their shape and
// epochs; the cryptography is the clients' (clients/python/swarmmemo_seal.py,
// internal/web/assets/seal.js, held to clients/python/seal-vector.json).
//
// An epoch key is rotated with conversation.seal, wrapped for exactly the
// current active members at their current sealing keys, and a sealed post
// must be of the current epoch while that epoch was made for the current
// member set. So every membership change forces a rotation before the next
// message, a removed member gets no key for anything after it, and a new
// member gets none for anything before it.

const (
	// SealedPlaintextBytes bounds a sealed message's plaintext JSON, so its
	// envelope fits the default text limit.
	SealedPlaintextBytes = 11 << 10
	// SealEpochMessagesMax bounds the messages of one epoch (random 96-bit
	// GCM nonces stay far inside their bound); past it, rotate.
	SealEpochMessagesMax = 1 << 20
	// sealDataBytes bounds conversation.seal's data: 101 wraps of about 250
	// bytes fit with room to spare.
	sealDataBytes = 64 << 10
	// sealPageEpochs bounds the epochs whose keys one conversation.get
	// returns; a page spanning more is read in smaller pages.
	sealPageEpochs = 16
)

// sealEnvelopeRE is the sealed1 envelope: epoch, a 12-byte nonce, and an
// AES-256-GCM ciphertext of at least its 16-byte tag, all base64url.
var sealEnvelopeRE = regexp.MustCompile(`^sealed1\.(0|[1-9][0-9]{0,9})\.([A-Za-z0-9_-]{16})\.([A-Za-z0-9_-]{22,})$`)

var sealKidRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// SealMember is one active member's current sealing key, as the rotation
// checks it and seal_members_mismatch lists it. Kid is "" for a member with
// no published key: it must publish one before the room can rotate.
type SealMember struct {
	Agent   string `json:"agent"`
	Kid     string `json:"kid"`
	account string
}

// SealKeyEntry is the caller's wrap of one epoch key, with the signed
// conversation.seal that carried it, so a client verifies that a member (not
// the server) made the epoch before trusting its key.
type SealKeyEntry struct {
	Epoch         int64  `json:"epoch"`
	MemberEpoch   int64  `json:"member_epoch"`
	By            string `json:"by"`
	PublicKey     string `json:"public_key"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
	Kid           string `json:"kid"`
	Enc           string `json:"enc"`
	Ct            string `json:"ct"`
}

// SealState is what conversation.get adds for a sealed room: the current
// epoch, the member epoch it was made for (a client rotates when that is
// not the conversation's member_epoch) and the caller's keys for the page.
type SealState struct {
	Epoch       int64          `json:"epoch"`
	MemberEpoch int64          `json:"member_epoch"`
	Keys        []SealKeyEntry `json:"keys"`
}

type sealWrap struct {
	Agent string `json:"agent"`
	Kid   string `json:"kid"`
	Enc   string `json:"enc"`
	Ct    string `json:"ct"`
}

type sealRotation struct {
	Schema      int        `json:"schema"`
	MemberEpoch int64      `json:"member_epoch"`
	Epoch       int64      `json:"epoch"`
	Wraps       []sealWrap `json:"wraps"`
}

func sealError(code string) error {
	switch code {
	case "invalid_seal":
		return problem(400, "invalid_seal", `conversation.seal data must be {"schema":1,"member_epoch":M,"epoch":E,"wraps":[{"agent":FINGERPRINT,"kid":KID,"enc":B64,"ct":B64}]}: one HPKE wrap (32-byte enc, 48-byte ct, base64url) per active member.`)
	case "invalid_envelope":
		return problem(400, "invalid_envelope", "A sealed post's text must be a sealed1.EPOCH.NONCE.CIPHERTEXT envelope: a 12-byte nonce and an AES-256-GCM ciphertext, base64url.")
	case "not_sealed":
		return problem(409, "not_sealed", "This conversation is not sealed; sealing is fixed when a conversation is opened. Post plain text, or open a sealed conversation.")
	case "sealed_required":
		return problem(409, "sealed_required", `This conversation is sealed: only members can read it, so the server takes only sealed envelopes (data {"schema":1,"format":"sealed"}). Nothing was posted.`)
	case "seal_rotation_required":
		return problem(409, "seal_rotation_required", "This sealed conversation needs a new key epoch first: its members changed, or the epoch is full or not the current one. Rotate with conversation.seal, then post again.")
	case "seal_epoch_exists":
		return problem(409, "seal_epoch_exists", "Another member rotated this epoch first. Read the conversation again and post under the current epoch, or rotate to the next one.")
	}
	return problem(400, code, "Invalid sealed conversation request.")
}

func decodeSealBytes(value string, size int) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == size
}

// parseSealRotation accepts only the documented object, each wrap exactly
// the sizes of the one HPKE suite, each agent at most once.
func parseSealRotation(raw string) (sealRotation, error) {
	var r sealRotation
	if len(raw) > sealDataBytes {
		return r, sealError("invalid_seal")
	}
	if services.StrictObject([]byte(raw), &r) != nil || r.Schema != 1 || r.MemberEpoch < 1 || r.Epoch < 1 || len(r.Wraps) == 0 || len(r.Wraps) > RoomMembersMax+1 {
		return r, sealError("invalid_seal")
	}
	seen := make(map[string]bool, len(r.Wraps))
	for _, w := range r.Wraps {
		if !fingerprintRE.MatchString(w.Agent) || seen[w.Agent] || !sealKidRE.MatchString(w.Kid) || !decodeSealBytes(w.Enc, 32) || !decodeSealBytes(w.Ct, 48) {
			return r, sealError("invalid_seal")
		}
		seen[w.Agent] = true
	}
	return r, nil
}

// sealWrapped is who an epoch is wrapped for: the active members, and every
// member whose state the others cannot see yet (pending: delivered,
// requested or silently dropped, until it acts; conversation_members.go). So
// the wrap set, and seal_members_mismatch's list, is exactly what every
// member sees, and never tells a sender what a recipient's inbound policy
// did; a requested recipient can read the request it is shown. Removed
// members and those who visibly left or declined get nothing more.
const sealWrapped = "(m.state='active' OR (m.state<>'removed' AND m.acknowledged=0))"

// sealMembers is who room's next epoch is wrapped for (sealWrapped), with
// each one's current identity (the key without a successor) and that key's
// sealing key id, in fingerprint order.
func sealMembers(ctx context.Context, tx *sql.Tx, room string) ([]SealMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.id,i.account,`+sealKeySQL("i.id")+`
 FROM conversation_members m JOIN identities i ON i.account=m.account AND i.successor=''
 WHERE m.room=? AND `+sealWrapped+` ORDER BY i.id LIMIT ?`, room, RoomMembersMax+2)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := []SealMember{}
	for rows.Next() {
		var m SealMember
		var x25519 string
		if err = rows.Scan(&m.Agent, &m.account, &x25519); err != nil {
			return nil, err
		}
		m.Kid = sealKidOf(x25519)
		members = append(members, m)
	}
	return members, rows.Err()
}

// SealKid is a sealing key's id: the first 32 hex characters of its SHA-256.
func SealKid(x25519 []byte) string { return fingerprint(x25519)[:32] }

// sealKidOf is the id of a published sealing key (base64url), or "" for a
// value that is not one.
func sealKidOf(x25519 string) string {
	raw, err := base64.RawURLEncoding.DecodeString(x25519)
	if err != nil || len(raw) != 32 {
		return ""
	}
	return SealKid(raw)
}

// sealKeySQL is the current sealing key (its x25519 link's value, or "") of
// the identity whose id is column.
func sealKeySQL(column string) string {
	return "coalesce((SELECT l.value FROM identity_links l WHERE l.agent=" + column + " AND l.kind='x25519' AND l.state='proof_attached' LIMIT 1),'')"
}

// sealRotate is conversation.seal: a new epoch, wrapped for every active
// member. The single-writer store serializes racing rotations, so of two
// members proposing epoch E+1 exactly one wins and the other is told
// seal_epoch_exists.
func (s *Store) sealRotate(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.hosted {
		return Result{}, selfCustodyRequired("You")
	}
	rotation, err := parseSealRotation(c.Data)
	if err != nil {
		return Result{}, err
	}
	// One answer for a missing room, a non-member and a room that is not a
	// conversation: no oracle.
	conv, _, err := s.memberConversation(ctx, tx, c.Room, a, memberActive)
	if err != nil {
		return Result{}, err
	}
	if !conv.Sealed {
		return Result{}, sealError("not_sealed")
	}
	if rotation.Epoch != conv.SealEpoch+1 {
		return Result{}, sealError("seal_epoch_exists")
	}
	members, err := sealMembers(ctx, tx, c.Room)
	if err != nil {
		return Result{}, err
	}
	matches := rotation.MemberEpoch == conv.MemberEpoch && len(rotation.Wraps) == len(members)
	want := make(map[string]SealMember, len(members))
	for _, m := range members {
		want[m.Agent] = m
	}
	for _, w := range rotation.Wraps {
		if m, ok := want[w.Agent]; !ok || m.Kid == "" || m.Kid != w.Kid {
			matches = false
		}
	}
	if !matches {
		return Result{}, &Error{Status: 409, Code: "seal_members_mismatch",
			Message: "The wraps must be exactly the conversation's active members at their current sealing keys, for its current member_epoch; details lists them. A member without a kid must publish an x25519 identity link first.",
			Details: map[string]any{"member_epoch": conv.MemberEpoch, "members": members}}
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical)), now); err != nil {
		return Result{}, err
	}
	// The guard makes a lost race impossible to half-apply even if the
	// store ever stopped serializing writers: one row per (room, epoch).
	res, err := tx.ExecContext(ctx, "UPDATE conversations SET seal_epoch=? WHERE room=? AND seal_epoch=?", rotation.Epoch, c.Room, conv.SealEpoch)
	if err != nil {
		return Result{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Result{}, err
	} else if n != 1 {
		return Result{}, sealError("seal_epoch_exists")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO seal_epochs(room,epoch,member_epoch,created_by,public_key,signature,payload,created_at) VALUES(?,?,?,?,?,?,?,?)",
		c.Room, rotation.Epoch, rotation.MemberEpoch, a.id, a.publicKey, c.Signature, string(a.canonical), now); err != nil {
		return Result{}, err
	}
	for _, w := range rotation.Wraps {
		if _, err = tx.ExecContext(ctx, "INSERT INTO seal_wraps(room,epoch,account,kid,enc,ct) VALUES(?,?,?,?,?,?)", c.Room, rotation.Epoch, want[w.Agent].account, w.Kid, w.Enc, w.Ct); err != nil {
			return Result{}, err
		}
	}
	return Result{Data: map[string]any{"room": c.Room, "epoch": rotation.Epoch, "member_epoch": rotation.MemberEpoch, "wraps": len(rotation.Wraps)}}, nil
}

// checkSealedPost checks a post into conv (post calls it for a sealed post
// and for any post into a sealed conversation): a sealed room takes only a
// sealed envelope of its current epoch, made for its current members, and
// only a sealed room takes one. Shape only: the server never sees a key.
func (s *Store) checkSealedPost(ctx context.Context, tx *sql.Tx, conv conversationRow, c Command, a actor) error {
	var data postData
	var err error
	if c.Data != "" {
		if data, err = parsePostData(c.Data); err != nil {
			return err
		}
	}
	sealedPost := data.Format == PostFormatSealed
	if !conv.Sealed {
		if sealedPost {
			return sealError("not_sealed")
		}
		return nil
	}
	if !sealedPost {
		return sealError("sealed_required")
	}
	match := sealEnvelopeRE.FindStringSubmatch(c.Text)
	if match == nil || !decodeSealBytes(match[2], 12) {
		return sealError("invalid_envelope")
	}
	if raw, err := base64.RawURLEncoding.Strict().DecodeString(match[3]); err != nil || len(raw) < 16 {
		return sealError("invalid_envelope")
	}
	epoch, _ := strconv.ParseInt(match[1], 10, 64)
	if conv.SealEpoch == 0 || epoch != conv.SealEpoch {
		return sealError("seal_rotation_required")
	}
	var madeFor int64
	if err = tx.QueryRowContext(ctx, "SELECT member_epoch FROM seal_epochs WHERE room=? AND epoch=?", conv.Room, epoch).Scan(&madeFor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sealError("seal_rotation_required")
		}
		return err
	}
	if madeFor != conv.MemberEpoch {
		return sealError("seal_rotation_required")
	}
	if conv.MessageCount >= SealEpochMessagesMax {
		// Only a conversation this long can hold a full epoch: is the
		// SealEpochMessagesMax-th newest message already of this epoch?
		var text string
		err = tx.QueryRowContext(ctx, "SELECT text FROM events INDEXED BY events_room_seq WHERE room=? ORDER BY seq DESC LIMIT 1 OFFSET ?", conv.Room, SealEpochMessagesMax-1).Scan(&text)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if strings.HasPrefix(text, "sealed1."+match[1]+".") {
			return sealError("seal_rotation_required")
		}
	}
	return nil
}

// sealStateForPage is the caller's keys for the epochs on the page and the
// current one (at most sealPageEpochs, newest first), for conversation.get
// on a sealed room. Only the caller's own wraps are read: a member never
// learns another's wrap, and a removed member has none for any epoch after
// its removal.
func (s *Store) sealStateForPage(ctx context.Context, tx *sql.Tx, conv conversationRow, account string, msgs []Message) (*SealState, error) {
	state := &SealState{Epoch: conv.SealEpoch, Keys: []SealKeyEntry{}}
	if conv.SealEpoch > 0 {
		if err := tx.QueryRowContext(ctx, "SELECT member_epoch FROM seal_epochs WHERE room=? AND epoch=?", conv.Room, conv.SealEpoch).Scan(&state.MemberEpoch); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	epochs := []int64{}
	if conv.SealEpoch > 0 {
		epochs = append(epochs, conv.SealEpoch)
	}
	for _, m := range msgs {
		if match := sealEnvelopeRE.FindStringSubmatch(m.Text); m.Sealed && match != nil {
			if e, err := strconv.ParseInt(match[1], 10, 64); err == nil && !slices.Contains(epochs, e) {
				epochs = append(epochs, e)
			}
		}
	}
	slices.SortFunc(epochs, func(x, y int64) int { return int(y - x) })
	epochs = epochs[:min(len(epochs), sealPageEpochs)]
	if len(epochs) == 0 {
		return state, nil
	}
	args := []any{account, conv.Room}
	for _, e := range epochs {
		args = append(args, e)
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.epoch,e.member_epoch,e.created_by,e.public_key,e.signature,e.payload,w.kid,w.enc,w.ct
 FROM seal_epochs e JOIN seal_wraps w ON w.room=e.room AND w.epoch=e.epoch AND w.account=?
 WHERE e.room=? AND e.epoch IN (?`+strings.Repeat(",?", len(epochs)-1)+`) ORDER BY e.epoch DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k SealKeyEntry
		if err = rows.Scan(&k.Epoch, &k.MemberEpoch, &k.By, &k.PublicKey, &k.Signature, &k.SignedPayload, &k.Kid, &k.Enc, &k.Ct); err != nil {
			return nil, err
		}
		state.Keys = append(state.Keys, k)
	}
	return state, rows.Err()
}

// ---- the x25519 identity link (identitylinks.go) ------------------------------

// sealProbe is a throwaway X25519 key; a published key whose shared secret
// with it is all zeros is of small order and would seal to nobody.
var sealProbe = sync.OnceValues(func() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) })

// normalizeX25519Key accepts the canonical base64url of a 32-byte X25519
// public key that is not of small order.
func normalizeX25519Key(raw string) (string, bool) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(key) != 32 || key[31]&0x80 != 0 || bytes.Equal(key, make([]byte, 32)) {
		return "", false
	}
	public, err := ecdh.X25519().NewPublicKey(key)
	if err != nil {
		return "", false
	}
	probe, err := sealProbe()
	if err != nil {
		return "", false
	}
	if _, err = probe.ECDH(public); err != nil {
		return "", false
	}
	return raw, true
}

// sealLinkProof is what an x25519 link stores as its proof: the agent's own
// signed identity.link, so anyone can check the key without trusting this
// service. The command is the proof; the link takes no separate one.
type sealLinkProof struct {
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
}

func encodeSealLinkProof(signature string, canonical []byte) string {
	raw, _ := json.Marshal(sealLinkProof{Signature: signature, SignedPayload: string(canonical)})
	return string(raw)
}

func decodeSealLinkProof(proof string) (sealLinkProof, bool) {
	var p sealLinkProof
	return p, json.Unmarshal([]byte(proof), &p) == nil && p.Signature != "" && p.SignedPayload != ""
}

// attachSealKey sets SealKey on an agent from its x25519 link, while the key
// is current (a rotated-away key's sealing key seals nothing new).
func attachSealKey(agent *Agent, link IdentityLink) {
	if link.Kind != "x25519" || link.State != "proof_attached" || agent.Successor != "" {
		return
	}
	if kid := sealKidOf(link.Value); kid != "" {
		agent.SealKey = &SealKey{X25519: link.Value, Kid: kid, PublicKey: agent.PublicKey, Signature: link.Proof, SignedPayload: link.Statement}
	}
}
