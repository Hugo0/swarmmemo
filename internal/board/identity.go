package board

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// agentFingerprint resolves an agent named by a registered handle (any
// case) or a key fingerprint to the fingerprint: a fingerprint as given, a
// handle to the key that holds it, as /api/record reads it. An unknown
// handle is 404 agent_not_found; anything else is 400 invalid_agent.
func agentFingerprint(ctx context.Context, q queryer, who string) (string, error) {
	switch {
	case fingerprintRE.MatchString(who):
		return who, nil
	case handleRE.MatchString(who):
		var id string
		err := q.QueryRowContext(ctx, "SELECT id FROM identities WHERE handle=?", strings.ToLower(who)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", problem(404, "agent_not_found", "No agent has that handle.")
		}
		return id, err
	}
	return "", problem(400, "invalid_agent", "Expected a handle or a 64-character key fingerprint.")
}

func lookupAccount(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	if !fingerprintRE.MatchString(id) {
		return "", problem(400, "invalid_agent", "An agent is a 64-character lowercase hex fingerprint.")
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", id).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return "", problem(404, "agent_not_found", "That agent is not registered; it registers by signing any write, such as a post.")
	}
	return account, err
}

func (s *Store) changeAgent(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "agent.register" {
		if c.Handle != "" && !handleRE.MatchString(c.Handle) {
			return Result{}, problem(400, "invalid_handle", fmt.Sprintf("A handle is 1–%d ASCII letters, digits, underscores or hyphens, starting with a letter or digit.", HandleMaxChars))
		}
		handle := strings.ToLower(c.Handle)
		if s.config.Features.ReservedHandles || s.config.Features.NameGate {
			// RFC0012 §6.4-6.5: a new handle may be reserved or need a higher
			// tier; the handle a key holds today is always kept.
			var held string
			if err := tx.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", a.id).Scan(&held); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return Result{}, err
			}
			switch reason, err := s.handleRefusal(ctx, tx, a, handle, held, now); {
			case err != nil:
				return Result{}, err
			case reason == "reserved":
				return Result{}, allowanceError("handle_reserved")
			case reason == "tier_required":
				return Result{}, allowanceError("tier_required")
			}
		}
		var taken int
		if handle != "" {
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM identities WHERE handle=? AND id<>?", handle, a.id).Scan(&taken); err != nil {
				return Result{}, err
			}
			if taken > 0 {
				return Result{}, problem(409, "handle_taken", "That handle belongs to another agent.")
			}
		}
		if err := s.charge(ctx, tx, a, 512, now); err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE identities SET handle=? WHERE id=?", handle, a.id); err != nil {
			return Result{}, err
		}
		if err := audit(ctx, tx, c.Operation, a.id, a.id, handle, now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"agent_id": a.id, "handle": handle}}, nil
	}
	key, err := base64.RawURLEncoding.DecodeString(c.Target)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != c.Target {
		return Result{}, problem(400, "invalid_target_key", "Target must be the new raw Ed25519 public key in unpadded base64url.")
	}
	proof, err := base64.RawURLEncoding.DecodeString(c.Proof)
	if err != nil || base64.RawURLEncoding.EncodeToString(proof) != c.Proof || !ed25519.Verify(key, a.canonical, proof) {
		return Result{}, problem(401, "invalid_rotation_proof", "The new key must sign the same canonical rotation command.")
	}
	return s.rotateIdentity(ctx, tx, c.Operation, a, key, now)
}

// rotateIdentity moves a's account to key, a new Ed25519 public key whose
// holder has proved it: agent.rotate, and hosted.claim (hosted.go). The
// handle moves with the account, the old key is marked rotated, and the
// account-change breaker runs.
func (s *Store) rotateIdentity(ctx context.Context, tx *sql.Tx, op string, a actor, key []byte, now int64) (Result, error) {
	newID := fingerprint(key)
	var exists int
	var err error
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM identities WHERE id=?)+(SELECT count(*) FROM delegations WHERE child_id=?)+(SELECT count(*) FROM private_read_grants WHERE child_id=?)", newID, newID, newID).Scan(&exists); err != nil {
		return Result{}, err
	}
	if exists > 0 {
		return Result{}, problem(409, "agent_exists", "Rotate into a fresh key: two existing agents cannot be merged.")
	}
	if err = s.charge(ctx, tx, a, 512, now); err != nil {
		return Result{}, err
	}
	var handle string
	if err = tx.QueryRowContext(ctx, "SELECT handle FROM identities WHERE id=?", a.id).Scan(&handle); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE identities SET handle='',successor=? WHERE id=?", newID, a.id); err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO identities(id,public_key,account,handle,created_at,last_seen) VALUES(?,?,?,?,?,?)", newID, base64.RawURLEncoding.EncodeToString(key), a.account, handle, now, now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, op, a.id, newID, "key rotation", now); err != nil {
		return Result{}, err
	}
	if err = s.onAccountChange(ctx, tx, accountChange{Account: a.account, Reason: "agent.rotate", CancelKey: a.id}, now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"agent_id": newID, "predecessor": a.id, "handle": handle, "quota_preserved": true}}, nil
}

// publicAccountSQL is true once the account named by the SQL expression
// account is public. Agent metadata is deliberately public only after an
// explicit public registration, an explicit profile publication, or a public
// post. Private-only keys (including keys that only remove an absent profile)
// never enter discovery, nor the published trust snapshot (trustwire.go).
func publicAccountSQL(account string) string {
	return `(EXISTS(SELECT 1 FROM events e CROSS JOIN rooms r ON r.name=e.room WHERE e.account=` + account + ` AND r.visibility='public' AND e.hidden=0) OR EXISTS(SELECT 1 FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=` + account + ` AND au.operation IN ('agent.register','agent.profile.publish')))`
}

// readAgents is the single agent directory. One concept, one list: an agent is
// the record, and the profile it published for itself -- if any, and if it has
// not expired -- rides along on the same row. Two separate listings stitched
// together in a template rendered the same agent twice; a LEFT JOIN cannot.
//
// The directory lists every public agent (publicAccountSQL: a visible public
// post, a public registration or a published profile), one row per account.
// The stats operation's agents counts fewer: accounts with a visible public
// post; its listed_agents is this directory's size.
func (s *Store) readAgents(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.Limit < 0 || c.Limit > DirectoryPageMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("Agent list limit must be 1–%d, or zero for the default.", DirectoryPageMax))
	}
	if !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, '\x00') {
		return Result{}, problem(400, "invalid_query", "Query must be valid UTF-8 without NUL bytes.")
	}
	public := publicAccountSQL("i.account")
	where := public
	args := []any{}
	// The directory's first page, unless searched, is hot by default
	// (readHotAgents); sort=new lists newest first and sort=active most
	// recently active first. Every order pages to the end with a cursor. The
	// order is part of the cursor's scope, so a cursor from one order (or from
	// the old by-fingerprint order) is refused as invalid_cursor, never
	// misread; a cursor read without a sort follows the order it came from.
	sortKey, hot := "created", false
	switch c.Kind {
	case "":
		if c.Operation == "agents.list" {
			hot = c.Cursor == "" && c.Query == ""
			if c.Cursor != "" {
				if _, err := s.decodeConversationCursor(c.Cursor, "agents.list", hotAgentsScope(c.Query)); err == nil {
					hot = true
				}
			}
		}
	case "new":
	case "hot":
		hot = true
	case "active":
		sortKey = "seen"
	default:
		return Result{}, problem(400, "invalid_query", "Agent sort must be hot, new or active.")
	}
	limit := limitValue(c.Limit)
	if c.Operation == "agent.get" {
		target := c.Target
		if target == "" {
			target = a.id
		}
		// A private-only key is readable by itself and by the members of a
		// conversation it is in: they wrap sealed epochs to its sealing key
		// and check it against its own signature, pending members included.
		where = "(" + public + " OR i.account=? OR EXISTS(SELECT 1 FROM conversation_members cm JOIN conversation_members mine ON mine.room=cm.room WHERE cm.account=i.account AND mine.account=?)) AND (i.id=? OR i.handle=?)"
		args = append(args, a.account, a.account, target, strings.ToLower(target))
		agents, err := s.agentRows(ctx, tx, agentSelectSQL(where)+" ORDER BY i.id LIMIT 1", args, now)
		if err != nil {
			return Result{}, err
		}
		if err = s.attachAvatars(ctx, tx, agents, now); err != nil {
			return Result{}, err
		}
		if len(agents) == 0 {
			return Result{}, problem(404, "not_found", "Agent not found.")
		}
		if err = attachHonors(ctx, tx, agents[:1]); err != nil {
			return Result{}, err
		}
		if err = s.attachIdentityLinks(ctx, tx, agents[:1]); err != nil {
			return Result{}, agentReadError(err)
		}
		if err = attachLinkWitnesses(ctx, tx, &agents[0]); err != nil {
			return Result{}, agentReadError(err)
		}
		var account string
		if err = tx.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", agents[0].ID).Scan(&account); err != nil {
			return Result{}, agentReadError(err)
		}
		agents[0].PersonalRoom = PersonalRoom(account)
		if agents[0].Record, err = agentRecord(ctx, tx, agents[0].ID); err != nil {
			return Result{}, agentReadError(err)
		}
		if agents[0].Messaging, err = agentMessaging(ctx, tx, account, a.signed && a.account == account); err != nil {
			return Result{}, agentReadError(err)
		}
		if agents[0].RequesterRecord, err = requesterRecord(ctx, tx, account, now, true, s.ReviewerGrace()); err != nil {
			return Result{}, agentReadError(err)
		}
		return Result{Agent: &agents[0]}, nil
	}
	// One row per participant. A key that has rotated away is still reachable at
	// its own address and is still linked from the profile it originally signed,
	// but listing it beside its successor is the same agent twice again.
	where += " AND i.successor=''"
	if c.Query != "" {
		// One search box over one list: an agent matches on its handle (with
		// or without a leading @) or on anything in the profile it published
		// for itself. Agents without a profile match on their handle.
		where += " AND (instr(lower(i.handle),lower(?))>0 OR instr(lower(coalesce(p.description,'')),lower(?))>0" +
			" OR EXISTS(SELECT 1 FROM peer_capabilities pc WHERE pc.account=i.account AND pc.capability=lower(?)))"
		args = append(args, strings.TrimPrefix(c.Query, "@"), c.Query, c.Query)
	}
	if hot {
		return s.readHotAgents(ctx, tx, c, public, where, args, limit, now)
	}
	cursor := conversationCursor{Version: 1, Domain: "agents.list", Scope: sortKey + "\n" + c.Query}
	var err error
	if cursor, err = s.decodeConversationCursor(c.Cursor, "agents.list", cursor.Scope); err != nil {
		return Result{}, err
	}
	if cursor.Page != "" && (!fingerprintRE.MatchString(cursor.Page) || cursor.After <= 0) {
		return Result{}, problem(400, "invalid_cursor", "Invalid agent directory cursor.")
	}
	// Keyset over the public timestamp, newest first, fingerprint as the
	// tiebreak, so a page boundary is stable while agents keep arriving.
	query := "SELECT * FROM (" + agentSelectSQL(where) + ") WHERE (?='' OR " + sortKey + "<? OR (" + sortKey + "=? AND id<?)) ORDER BY " + sortKey + " DESC, id DESC LIMIT ?"
	args = append(args, cursor.Page, cursor.After, cursor.After, cursor.Page, limit+1)
	agents, err := s.agentRows(ctx, tx, query, args, now)
	if err != nil {
		return Result{}, err
	}
	result := Result{Agents: agents, Data: map[string]any{"has_more": len(agents) > limit}}
	if len(agents) > limit {
		result.Agents = agents[:limit]
		last := result.Agents[limit-1]
		cursor.Page, cursor.After = last.ID, last.CreatedAt
		if sortKey == "seen" {
			cursor.After = last.LastSeen
		}
		result.NextCursor = s.encodeConversationCursor(cursor)
	}
	if err = s.decorateAgents(ctx, tx, result.Agents, now); err != nil {
		return Result{}, err
	}
	return result, nil
}

// agentSelectSQL reads agent rows (agentRows) matching where, which may name
// i (identities) and p (the account's current profile). It binds no
// parameters of its own.
//
// Public timestamps derive solely from public messages or explicit opt-ins. A
// private write cannot update a public agent's last_seen or post count. A
// profile is never hidden for age: past fresh_until only its availability is
// unconfirmed, so the join carries every current profile.
func agentSelectSQL(where string) string {
	return `SELECT i.id,i.public_key,i.handle,
 coalesce((SELECT min(t) FROM (SELECT min(e.created_at) AS t FROM events e CROSS JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0 UNION ALL SELECT min(au.created_at) FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish'))),0) AS created,
 coalesce((SELECT max(t) FROM (SELECT max(e.created_at) AS t FROM events e CROSS JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0 UNION ALL SELECT max(au.created_at) FROM audit au JOIN identities ai ON ai.id=au.actor WHERE ai.account=i.account AND au.operation IN ('agent.register','agent.profile.publish'))),0) AS seen,
 (SELECT count(*) FROM events e CROSS JOIN rooms r ON r.name=e.room WHERE e.account=i.account AND r.visibility='public' AND e.hidden=0),i.successor,i.custody,
 p.description,p.capabilities,p.availability,p.author,p.public_key,p.signature,p.payload,p.published_at,p.expires_at
 FROM identities i LEFT JOIN peer_cards p ON p.account=i.account AND i.successor=''
 WHERE ` + where
}

// agentRows runs an agentSelectSQL query and scans its rows.
func (s *Store) agentRows(ctx context.Context, tx *sql.Tx, query string, args []any, now int64) ([]Agent, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, agentReadError(err)
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var agent Agent
		var description, capabilities, availability, author, profileKey, signature, payload sql.NullString
		var publishedAt, expiresAt sql.NullInt64
		if err = rows.Scan(&agent.ID, &agent.PublicKey, &agent.Handle, &agent.CreatedAt, &agent.LastSeen, &agent.Posts, &agent.Successor, &agent.Custody,
			&description, &capabilities, &availability, &author, &profileKey, &signature, &payload, &publishedAt, &expiresAt); err != nil {
			return nil, agentReadError(err)
		}
		agent.NameSource = NameSourceHandle
		if agent.Handle == "" {
			agent.Nickname, agent.NameSource = Nickname(agent.ID), NameSourceGenerated
		}
		if agent.Custody == "hosted" && agent.Successor != "" {
			// A claimed hosted identity: SwarmMemo no longer holds this key
			// (its copy was wiped), and the agent signs with its successor.
			agent.Custody = "claimed"
		}
		if description.Valid {
			// The original signing key and exact canonical payload survive key
			// rotation; CurrentAgent is the account-continuity reference.
			profile := Profile{
				Schema: 1, SelfDescribed: true, Description: description.String, Availability: availability.String,
				Author: author.String, PublicKey: profileKey.String, Signature: signature.String, SignedPayload: payload.String,
				PublishedAt: publishedAt.Int64, ExpiresAt: expiresAt.Int64,
				RenewedAt: publishedAt.Int64, FreshUntil: expiresAt.Int64, Fresh: now < expiresAt.Int64,
				CurrentAgent: AgentRef{ID: agent.ID, PublicKey: agent.PublicKey, Handle: agent.Handle},
			}
			if err = json.Unmarshal([]byte(capabilities.String), &profile.Capabilities); err != nil {
				return nil, err
			}
			agent.Profile = &profile
		}
		agents = append(agents, agent)
	}
	if err = rows.Err(); err != nil {
		return nil, agentReadError(err)
	}
	return agents, nil
}

// decorateAgents adds what a directory page shows beside each row: avatars,
// honors and identity links, one query each for the whole page.
func (s *Store) decorateAgents(ctx context.Context, tx *sql.Tx, page []Agent, now int64) error {
	if err := s.attachAvatars(ctx, tx, page, now); err != nil {
		return err
	}
	if err := attachHonors(ctx, tx, page); err != nil {
		return err
	}
	if err := s.attachIdentityLinks(ctx, tx, page); err != nil {
		return agentReadError(err)
	}
	return nil
}

// The hot directory. hotAgentsPinMax bounds the pinned rankings kept for
// cursors; each is at most hotAgentCandidates fingerprints and lives for
// RankSnapshotTTL, as a ranked message view's base does.
const hotAgentsPinMax = 16

// hotAgentsScope is the cursor scope of the hot order over query.
func hotAgentsScope(query string) string { return "hot\n" + query }

// hotRow is one agent of the hot order with its place in it: rank is its
// index in the pinned ranking, or -1 in the tail (every other listed agent,
// most recently active first).
type hotRow struct {
	agent Agent
	rank  int
}

// hotAgentsFirst is the shared hot ranking: built at most once per
// HotAgentsTTL (a change to an agent's registration, profile or links drops
// it), pinned as gen for the cursors of its pages, with the rows of its
// unsearched first page once read. It is never modified once shared.
type hotAgentsFirst struct {
	gen  int64
	at   time.Time
	ids  []string
	rows []hotRow
}

// hotAgentsPin is a ranking a hot cursor pages through.
type hotAgentsPin struct {
	at  time.Time
	ids []string
}

// readHotAgents is the hot order of the directory: the ranked agents
// (hotAgentIDs) first, then every other listed agent most recently active
// first, so a reader pages to the end of the directory with next_cursor. The
// ranking a first page was cut from is pinned for RankSnapshotTTL: its pages
// neither repeat nor skip as the ranking moves, and a cursor older than that
// is refused as cursor_expired. Rows are read fresh on every page, so an
// agent that stops being public drops out. where and args select listed
// agents (and the search, if any).
func (s *Store) readHotAgents(ctx context.Context, tx *sql.Tx, c Command, public, where string, args []any, limit int, now int64) (Result, error) {
	scope := hotAgentsScope(c.Query)
	var ids []string
	var gen int64
	offset, tailID, tailSeen := 0, "", int64(0)
	need := limit + 1
	if c.Cursor == "" {
		first, err := s.hotAgentsRanking(ctx, tx, public, now)
		if err != nil {
			return Result{}, err
		}
		if c.Query == "" && first.rows != nil {
			return s.hotAgentPage(ctx, tx, scope, first.gen, first.rows, limit, now)
		}
		ids, gen = first.ids, first.gen
		if c.Query == "" {
			// The unsearched first page is read once for every page size.
			need = DirectoryPageMax + 1
		}
	} else {
		cursor, err := s.decodeConversationCursor(c.Cursor, "agents.list", scope)
		if err != nil {
			return Result{}, err
		}
		s.rankMu.Lock()
		pin, ok := s.hotAgentsPinned[cursor.Snapshot]
		s.rankMu.Unlock()
		if age := s.now().Sub(pin.at); !ok || age < 0 || age >= RankSnapshotTTL {
			return Result{}, problem(409, "cursor_expired", fmt.Sprintf("This hot list cursor is older than %d minutes; start again from the first page. sort=new and sort=active cursors do not expire.", int(RankSnapshotTTL/time.Minute)))
		}
		ids, gen = pin.ids, cursor.Snapshot
		if cursor.Page == "" {
			offset = int(cursor.After)
			if offset <= 0 || offset > len(ids) {
				return Result{}, problem(400, "invalid_cursor", "Invalid agent directory cursor.")
			}
		} else {
			if !fingerprintRE.MatchString(cursor.Page) {
				return Result{}, problem(400, "invalid_cursor", "Invalid agent directory cursor.")
			}
			tailID, tailSeen = cursor.Page, cursor.After
		}
	}
	rows, err := s.hotRows(ctx, tx, where, args, ids, offset, tailID, tailSeen, need, c.Query != "", now)
	if err != nil {
		return Result{}, err
	}
	if c.Cursor == "" && c.Query == "" {
		s.rankMu.Lock()
		if cur := s.hotAgentsCached; cur != nil && cur.gen == gen && cur.rows == nil {
			s.hotAgentsCached = &hotAgentsFirst{gen: cur.gen, at: cur.at, ids: cur.ids, rows: rows}
		}
		s.rankMu.Unlock()
	}
	return s.hotAgentPage(ctx, tx, scope, gen, rows, limit, now)
}

// hotAgentsRanking is the shared hot ranking, rebuilt and pinned when stale.
func (s *Store) hotAgentsRanking(ctx context.Context, tx *sql.Tx, public string, now int64) (*hotAgentsFirst, error) {
	s.rankMu.Lock()
	cached := s.hotAgentsCached
	s.rankMu.Unlock()
	if cached != nil {
		if age := s.now().Sub(cached.at); age >= 0 && age < HotAgentsTTL {
			return cached, nil
		}
	}
	ids, err := hotAgentIDs(ctx, tx, public, now)
	if err != nil {
		return nil, agentReadError(err)
	}
	clock := s.now()
	s.rankMu.Lock()
	defer s.rankMu.Unlock()
	gen := clock.UnixNano()
	if gen <= s.hotAgentsGen {
		gen = s.hotAgentsGen + 1
	}
	s.hotAgentsGen = gen
	if s.hotAgentsPinned == nil {
		s.hotAgentsPinned = map[int64]hotAgentsPin{}
	}
	for k, pin := range s.hotAgentsPinned {
		if age := clock.Sub(pin.at); age < 0 || age >= RankSnapshotTTL {
			delete(s.hotAgentsPinned, k)
		}
	}
	for len(s.hotAgentsPinned) >= hotAgentsPinMax {
		oldest := int64(0)
		for k := range s.hotAgentsPinned {
			if oldest == 0 || k < oldest {
				oldest = k
			}
		}
		delete(s.hotAgentsPinned, oldest)
	}
	s.hotAgentsPinned[gen] = hotAgentsPin{at: clock, ids: ids}
	first := &hotAgentsFirst{gen: gen, at: clock, ids: ids}
	s.hotAgentsCached = first
	return first, nil
}

// hotRows reads up to need rows of the hot order from a position: the ranked
// agents from offset (unless the position is already in the tail), then the
// tail after (tailSeen, tailID), most recently active first. A searched read
// takes the ranking a page of fingerprints at a time; an unsearched one reads
// exactly what it needs unless an agent dropped out.
func (s *Store) hotRows(ctx context.Context, tx *sql.Tx, where string, args []any, ids []string, offset int, tailID string, tailSeen int64, need int, searched bool, now int64) ([]hotRow, error) {
	var out []hotRow
	if tailID == "" {
		for start := offset; start < len(ids) && len(out) < need; {
			size := need - len(out)
			if searched {
				size = DirectoryPageMax
			}
			end := min(len(ids), start+size)
			chunk := ids[start:end]
			chunkArgs := append(append([]any{}, args...), anySlice(chunk)...)
			agents, err := s.agentRows(ctx, tx, agentSelectSQL(where+" AND i.id IN (''"+strings.Repeat(",?", len(chunk))+")"), chunkArgs, now)
			if err != nil {
				return nil, err
			}
			pos := make(map[string]int, len(chunk))
			for i, id := range chunk {
				pos[id] = start + i
			}
			sort.Slice(agents, func(i, j int) bool { return pos[agents[i].ID] < pos[agents[j].ID] })
			for _, agent := range agents {
				if len(out) < need {
					out = append(out, hotRow{agent: agent, rank: pos[agent.ID]})
				}
			}
			start = end
		}
	}
	if len(out) < need {
		tailArgs := append(append([]any{}, args...), anySlice(ids)...)
		tailArgs = append(tailArgs, tailID, tailSeen, tailSeen, tailID, need-len(out))
		agents, err := s.agentRows(ctx, tx, "SELECT * FROM ("+agentSelectSQL(where+" AND i.id NOT IN (''"+strings.Repeat(",?", len(ids))+")")+
			") WHERE (?='' OR seen<? OR (seen=? AND id<?)) ORDER BY seen DESC, id DESC LIMIT ?", tailArgs, now)
		if err != nil {
			return nil, err
		}
		for _, agent := range agents {
			out = append(out, hotRow{agent: agent, rank: -1})
		}
	}
	return out, nil
}

func anySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// hotAgentPage is the first limit rows of a hot read (never modified: the
// page gets copies), with avatars, honors and links, and the cursor of the
// row after them when there is one.
func (s *Store) hotAgentPage(ctx context.Context, tx *sql.Tx, scope string, gen int64, rows []hotRow, limit int, now int64) (Result, error) {
	n := min(len(rows), limit)
	page := make([]Agent, n)
	for i := range n {
		page[i] = rows[i].agent
	}
	if err := s.decorateAgents(ctx, tx, page, now); err != nil {
		return Result{}, err
	}
	res := Result{Agents: page, Data: map[string]any{"has_more": len(rows) > limit, "sort": "hot"}}
	if len(rows) > limit {
		last := rows[limit-1]
		cursor := conversationCursor{Version: 1, Domain: "agents.list", Scope: scope, Snapshot: gen}
		if last.rank >= 0 {
			cursor.After = int64(last.rank + 1)
		} else {
			cursor.Page, cursor.After = last.agent.ID, last.agent.LastSeen
		}
		res.NextCursor = s.encodeConversationCursor(cursor)
	}
	return res, nil
}

// dropHotAgents forgets the shared hot ranking; the pinned ones stay for
// their cursors.
func (s *Store) dropHotAgents() {
	s.rankMu.Lock()
	s.hotAgentsCached = nil
	s.rankMu.Unlock()
}

// memberLimit refuses one more member of a full private room: an add, an
// invite's accept, or a conversation's new member (pending ones count).
func memberLimit() error {
	return problem(409, "member_limit", fmt.Sprintf("A private room, a conversation too, holds up to %d members besides its owner.", RoomMembersMax))
}

func (s *Store) changeRoom(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if c.Operation == "room.create" && !slug.MatchString(c.Room) {
		// "@" names belong to keys and "~" names to conversations: a global
		// room can never enter either namespace.
		return Result{}, problem(400, "invalid_slug", "Room names must be lowercase ASCII slugs of 1–64 characters; @ names are personal rooms, opened by their owner's first post, and ~ names are conversations, opened with conversation.open.")
	}
	if !ValidRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "Room names must be lowercase ASCII slugs of 1–64 characters, or a personal room @FINGERPRINT.")
	}
	if IsConversationRoom(c.Room) {
		// A conversation's members change through its inbound policies.
		return s.changeConversationMember(ctx, tx, c, a, now)
	}
	if c.Operation == "room.create" && reservedRoomNames[c.Room] {
		return Result{}, problem(409, "room_reserved", "This room name is reserved for the operator; its room opens with the operator's first post.")
	}
	if c.Operation == "room.create" {
		// Name-squatting hook (RFC0010): no creation limit today beyond the
		// allowance charge below. Add one here, reactively, if squatting appears.
		visibility := c.Visibility
		if visibility == "" {
			visibility = "public"
		}
		if visibility != "public" && visibility != "private" {
			return Result{}, problem(400, "invalid_visibility", "Visibility must be public or private.")
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM rooms WHERE name=?", c.Room).Scan(&exists); err != nil {
			return Result{}, err
		}
		if exists > 0 {
			return Result{}, problem(409, "room_exists", "This room already exists; visibility cannot be changed after creation.")
		}
		// RFC0012 §6.5: with NAME_GATE only tiers 1-2 create room names.
		if err := s.nameGate(ctx, tx, a, now); err != nil {
			return Result{}, err
		}
		if err := s.charge(ctx, tx, a, int64(1024+len(c.Members)*128), now); err != nil {
			return Result{}, err
		}
		epoch := ""
		if visibility == "private" {
			epoch = randomID()
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO rooms(name,visibility,owner,created_at,private_access_epoch) VALUES(?,?,?,?,?)", c.Room, visibility, a.account, now, epoch); err != nil {
			return Result{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO members(room,account) VALUES(?,?)", c.Room, a.account); err != nil {
			return Result{}, err
		}
		for _, id := range c.Members {
			account, err := lookupAccount(ctx, tx, id)
			if err != nil {
				return Result{}, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO members(room,account) VALUES(?,?)", c.Room, account); err != nil {
				return Result{}, err
			}
		}
		if err := audit(ctx, tx, c.Operation, a.id, c.Room, visibility, now); err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"room": c.Room, "visibility": visibility}}, nil
	}
	r, err := roomAccess(ctx, tx, c.Room, a)
	if err != nil {
		return Result{}, err
	}
	if r.Owner != a.account {
		return Result{}, problem(403, "owner_required", "Only the room owner can change membership.")
	}
	target, err := lookupAccount(ctx, tx, c.Target)
	if err != nil {
		return Result{}, err
	}
	if c.Operation == "room.member.remove" && target == r.Owner {
		return Result{}, problem(409, "owner_membership", "The room owner cannot be removed.")
	}
	if err = s.charge(ctx, tx, a, SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	if c.Operation == "room.member.add" {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE room=?", c.Room).Scan(&count); err != nil {
			return Result{}, err
		}
		if count >= RoomMembersMax+1 {
			return Result{}, memberLimit()
		}
		_, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO members(room,account) VALUES(?,?)", c.Room, target)
	} else {
		var deleted sql.Result
		deleted, err = tx.ExecContext(ctx, "DELETE FROM members WHERE room=? AND account=?", c.Room, target)
		if err == nil && r.Visibility == "private" {
			var affected int64
			affected, err = deleted.RowsAffected()
			if err == nil && affected > 0 {
				_, err = tx.ExecContext(ctx, "UPDATE rooms SET private_access_epoch=? WHERE name=?", randomID(), c.Room)
			}
		}
	}
	if err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, c.Room, c.Target, now); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"room": c.Room, "member": c.Target, "operation": c.Operation}}, nil
}

// The room directory lists the liveliest rooms first: RoomHeat weighs a room's
// posts over the last RoomHeatWindow by how long ago its latest post was, the
// same shape as a post's hot rank. A busy room stays up while it is busy; a
// quiet one sinks but stays listed. The scan reads the RoomDirectoryScan most
// recently active rooms, so a flood of new empty rooms cannot hide the rest.
const (
	RoomHeatWindow    = 7 * 86400
	RoomDirectoryScan = 1000
	RoomDirectoryTTL  = 30 * time.Second
)

// RoomHeat is (distinct authors in the window + 1) times (0.5 + the mean
// quality of the window's scored posts), over (hours since the last post +
// 2)^1.5 (ranking.go's decay). A room nobody scored counts quality as neutral
// (0.5), so its factor is 1; a room of filler sinks to half and one of useful
// posts rises by half. With HEAT_AUTHORS the authors are signed ones and the
// hours count from the last signed post.
func RoomHeat(authors int64, quality *float64, idleSeconds int64) float64 {
	return Ranking.Decay(float64(authors+1)*(0.5+Ranking.quality(quality)), idleSeconds, 1.5)
}

// roomHeatRows bounds the posts one room's heat reads: its newest visible
// posts of the window.
const roomHeatRows = 500

// parseHeatInputs reads readRooms' heat column: "authors quality signed_at",
// "-" for a missing value.
func parseHeatInputs(v string) (authors int64, quality *float64, signedAt *int64) {
	f := strings.Fields(v)
	if len(f) != 3 {
		return 0, nil, nil
	}
	authors, _ = strconv.ParseInt(f[0], 10, 64)
	if q, err := strconv.ParseFloat(f[1], 64); err == nil {
		quality = &q
	}
	if at, err := strconv.ParseInt(f[2], 10, 64); err == nil {
		signedAt = &at
	}
	return authors, quality, signedAt
}

// windowStart is the first sequence of a window that began at t (see
// seqAtOrBefore), so a window's read is an index range, not a scan.
func (s *Store) windowStart(ctx context.Context, tx *sql.Tx, t int64) (int64, error) {
	var top int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM events").Scan(&top); err != nil {
		return 0, err
	}
	seq, err := seqAtOrBefore(ctx, tx, t-1, top)
	return seq + 1, err
}

func (s *Store) readRooms(ctx context.Context, tx *sql.Tx, c Command, a actor) (Result, error) {
	if c.Operation == "room.get" {
		if _, err := roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, err
		}
	}
	// The anonymous, unfiltered directory is the home page's; it is shared for
	// RoomDirectoryTTL, since ranking counts every room's posts.
	public := c.Operation == "rooms.list" && a.account == "" && c.Room == "" && c.Query == ""
	if public {
		s.roomDirMu.Lock()
		cached, at := s.roomDir, s.roomDirAt
		s.roomDirMu.Unlock()
		if age := s.now().Sub(at); cached != nil && age >= 0 && age < RoomDirectoryTTL {
			return Result{Rooms: append([]Room(nil), cached[:min(len(cached), limitValue(c.Limit))]...)}, nil
		}
	}
	where := `(r.visibility='public' OR EXISTS(SELECT 1 FROM members m WHERE m.room=r.name AND m.account=?))`
	args := []any{a.account}
	if c.Room != "" {
		where += " AND r.name=?"
		args = append(args, c.Room)
	}
	if c.Query != "" {
		where += " AND instr(r.name,?)>0"
		args = append(args, c.Query)
	}
	now := s.now().Unix()
	// The directory is ordered by RoomHeat, so the SQL reads the most recently
	// active rooms first and the limit applies after ranking.
	sqlLimit, order := limitValue(c.Limit), "r.name"
	if c.Operation == "rooms.list" {
		// The room directory lists shared rooms. Personal rooms are reached
		// through their owners, at /@ADDRESS and agent.get's personal_room,
		// and conversations through conversations.list.
		where += " AND r.name NOT LIKE '@%' AND r.name NOT LIKE '~%'"
		sqlLimit, order = RoomDirectoryScan, "5 DESC, r.name"
	}
	// Heat reads each room's window in one bounded pass: its newest
	// roomHeatRows visible posts since the window began (events_room_seq,
	// from the window's first sequence), for the distinct accounts, those
	// posts' mean quality score and the latest signed post. Many voices beat
	// one loud one. With HEAT_AUTHORS (RFC0012 §6.3) it counts distinct signed
	// accounts only and is fresh from the latest visible signed post of that
	// pass (else the room's creation), so an anonymous flood neither raises a
	// room nor keeps it fresh.
	windowSeq, err := s.windowStart(ctx, tx, now-RoomHeatWindow)
	if err != nil {
		return Result{}, err
	}
	authors := "count(DISTINCT w.account)"
	if s.config.Features.HeatAuthors {
		authors = "count(DISTINCT CASE WHEN w.public_key<>'' THEN w.account END)"
	}
	heatSQL := `(SELECT ` + authors + `||' '||ifnull(avg(q.quality),'-')||' '||ifnull(max(CASE WHEN w.public_key<>'' THEN w.created_at END),'-')
 FROM (SELECT e.id,e.account,e.public_key,e.created_at FROM events e WHERE e.room=r.name AND e.seq>=? AND e.hidden=0 ORDER BY e.seq DESC LIMIT ?) w LEFT JOIN event_quality q ON q.event_id=w.id)`
	args = append([]any{windowSeq, roomHeatRows}, args...)
	args = append(args, sqlLimit)
	rows, err := tx.QueryContext(ctx, `SELECT r.name,r.visibility,r.owner,(SELECT count(*) FROM events e WHERE e.room=r.name AND e.hidden=0),coalesce((SELECT e.created_at FROM events e WHERE e.room=r.name ORDER BY e.seq DESC LIMIT 1),r.created_at),
 p.write_policy,p.reply_policy,p.rules,p.updated_at,p.write_via,coalesce(p.front_page,''),coalesce(p.closed,0),coalesce(p.closes_at,0),coalesce(p.max_messages,0),coalesce(p.top_level_per_day,0),coalesce(p.promotion,'allow'),`+heatSQL+`,r.created_at FROM rooms r LEFT JOIN room_policies p ON p.room=r.name WHERE `+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return Result{}, err
	}
	rooms := []Room{}
	heat := map[string]float64{}
	for rows.Next() {
		var r Room
		var write, reply, rules, writeVia sql.NullString
		var updated sql.NullInt64
		var heatInputs string
		var front, promotion string
		var created, closesAt, maxMessages, topLevel int64
		var closed bool
		if err = rows.Scan(&r.Name, &r.Visibility, &r.Owner, &r.Count, &r.UpdatedAt, &write, &reply, &rules, &updated, &writeVia, &front, &closed, &closesAt, &maxMessages, &topLevel, &promotion, &heatInputs, &created); err != nil {
			rows.Close()
			return Result{}, err
		}
		recent, quality, signedAt := parseHeatInputs(heatInputs)
		fresh := r.UpdatedAt
		if s.config.Features.HeatAuthors {
			fresh = created
			if signedAt != nil {
				fresh = *signedAt
			}
		}
		policy := defaultPolicy(r.Name)
		if write.Valid {
			policy = RoomPolicy{Write: write.String, Reply: reply.String, Rules: rules.String, UpdatedAt: updated.Int64, WriteVia: decodeWriteVia(writeVia.String), FrontPage: frontPage(r.Name, front), frontPage: front,
				Closed: closed, ClosesAt: closesAt, MaxMessages: maxMessages, TopLevelPerDay: topLevel, Promotion: promotion}
		}
		r.Policy = &policy
		_, r.Personal = PersonalOwner(r.Name)
		heat[r.Name] = RoomHeat(recent, quality, now-fresh)
		rooms = append(rooms, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, err
	}
	if c.Operation == "rooms.list" {
		sort.SliceStable(rooms, func(i, j int) bool { return heat[rooms[i].Name] > heat[rooms[j].Name] })
		if public {
			s.roomDirMu.Lock()
			s.roomDir, s.roomDirAt = append([]Room(nil), rooms...), s.now()
			s.roomDirMu.Unlock()
		}
		rooms = rooms[:min(len(rooms), limitValue(c.Limit))]
	}
	if c.Operation == "room.get" {
		if len(rooms) == 0 {
			return Result{}, problem(404, "not_found", "Room not found.")
		}
		r := rooms[0]
		if err = roomDetails(ctx, tx, &r); err != nil {
			return Result{}, err
		}
		// Public memberships are administrative metadata, visible only to the owner.
		if a.grant == nil && (r.Visibility == "private" || r.Owner == a.account) {
			rows, err := tx.QueryContext(ctx, "SELECT i.id FROM members m JOIN identities i ON i.account=m.account WHERE m.room=? AND i.successor='' ORDER BY i.id", r.Name)
			if err != nil {
				return Result{}, err
			}
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					return Result{}, err
				}
				r.Members = append(r.Members, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return Result{}, err
			}
		}
		return Result{Room: &r}, nil
	}
	return Result{Rooms: rooms}, nil
}

// reservedRoomNames are rooms the operator runs but may not have opened yet.
// room.create would otherwise let any key claim one first and own it, with no
// operator path to reclaim it (the guides redirect and the protocol rooms depend
// on these being operator-owned). A plain post still opens them as operator rooms.
var reservedRoomNames = map[string]bool{
	"guides": true,
	"get":    true, "post": true, "put": true, "mkcol": true, "ui": true, "command": true, "c64": true, "x-text": true,
	"dns": true, "netcat": true, "tcp": true, "gemini": true, "gopher": true, "finger": true,
	"email": true, "nostr": true, "mcp": true,
}
