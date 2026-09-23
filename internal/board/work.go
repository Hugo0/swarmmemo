package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const workSchema = `
CREATE TABLE IF NOT EXISTS works (
 id TEXT PRIMARY KEY REFERENCES events(id), requester TEXT NOT NULL,
 title TEXT NOT NULL, capabilities TEXT NOT NULL, state TEXT NOT NULL
 CHECK(state IN ('open','claimed','submitted','accepted','cancelled')),
 generation TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 deadline INTEGER NOT NULL, fence INTEGER NOT NULL DEFAULT 0,
 worker TEXT NOT NULL DEFAULT '', claim_expires_at INTEGER NOT NULL DEFAULT 0,
 result_id TEXT NOT NULL DEFAULT '', history_seq INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS work_transitions (
 work_id TEXT NOT NULL REFERENCES works(id), sequence INTEGER NOT NULL,
 operation TEXT NOT NULL, author TEXT NOT NULL, public_key TEXT NOT NULL,
 signature TEXT NOT NULL, payload TEXT NOT NULL, accepted_at INTEGER NOT NULL,
 fence INTEGER NOT NULL, generation TEXT NOT NULL, state TEXT NOT NULL,
 PRIMARY KEY(work_id,sequence));
`

const WorkDefaultTTL int64 = 7 * 86400
const WorkMaxTTL int64 = 30 * 86400

var workIDRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Work is a scoped projection, not a verified skill, job payment or execution claim.
// Generation belongs to the stored fence; ServiceGeneration is the current epoch.
type Work struct {
	ID                string    `json:"id"`
	Room              string    `json:"room"`
	Title             string    `json:"title"`
	Capabilities      []string  `json:"capabilities"`
	Simulated         bool      `json:"simulated"`
	State             string    `json:"state"`
	StoredState       string    `json:"stored_state"`
	Generation        string    `json:"generation"`
	ServiceGeneration string    `json:"service_generation"`
	ServiceID         string    `json:"service_id"`
	CreatedAt         int64     `json:"created_at"`
	UpdatedAt         int64     `json:"updated_at"`
	Deadline          int64     `json:"deadline"`
	Fence             int64     `json:"fence"`
	ClaimExpiresAt    int64     `json:"claim_expires_at"`
	RequesterAuthor   string    `json:"requester_author"`
	Requester         AgentRef  `json:"requester"`
	Worker            *AgentRef `json:"worker,omitempty"`
	ResultID          string    `json:"result_id,omitempty"`
	ResultAvailable   bool      `json:"result_available"`
	AttemptGrantID    string    `json:"attempt_grant_id,omitempty"`
}

type WorkAck struct {
	WorkID         string `json:"work_id"`
	State          string `json:"state"`
	Fence          int64  `json:"fence"`
	Generation     string `json:"generation"`
	ServiceID      string `json:"service_id"`
	AcceptedAt     int64  `json:"accepted_at"`
	Deadline       int64  `json:"deadline"`
	ClaimExpiresAt int64  `json:"claim_expires_at"`
}

type WorkTransition struct {
	Sequence      int64  `json:"sequence"`
	Operation     string `json:"operation"`
	Author        string `json:"author"`
	PublicKey     string `json:"public_key"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
	AcceptedAt    int64  `json:"accepted_at"`
	Fence         int64  `json:"fence"`
	Generation    string `json:"generation"`
	State         string `json:"state"`
	DelegationID  string `json:"delegation_id,omitempty"`
}

type workData struct {
	Schema            int
	Generation, Title string
	Capabilities      []string
}

func parseWorkData(raw string, create bool) (workData, error) {
	var d workData
	invalid := func() (workData, error) {
		return workData{}, problem(400, "invalid_work_data", "Data must be strict schema-1 JSON with a current lowercase 32-hex generation; creation also requires a 1–160 UTF-8 byte title and up to 16 unique lowercase capability slugs.")
	}
	if len(raw) > 8192 || !utf8.ValidString(raw) {
		return invalid()
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err = dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return invalid()
		}
		seen[name] = true
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil || string(value) == "null" {
			return invalid()
		}
		switch name {
		case "schema":
			err = json.Unmarshal(value, &d.Schema)
		case "generation":
			err = json.Unmarshal(value, &d.Generation)
		case "title":
			if !create {
				return invalid()
			}
			err = json.Unmarshal(value, &d.Title)
		case "capabilities":
			if !create {
				return invalid()
			}
			err = json.Unmarshal(value, &d.Capabilities)
		default:
			return invalid()
		}
		if err != nil {
			return invalid()
		}
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return invalid()
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return invalid()
	}
	want := 2
	if create {
		want = 4
	}
	if len(seen) != want || d.Schema != 1 || !workIDRE.MatchString(d.Generation) {
		return invalid()
	}
	if create {
		if strings.TrimSpace(d.Title) == "" || len(d.Title) > 160 || strings.ContainsRune(d.Title, 0) || d.Capabilities == nil || len(d.Capabilities) > 16 {
			return invalid()
		}
		caps := map[string]bool{}
		for _, cap := range d.Capabilities {
			if !slug.MatchString(cap) || caps[cap] {
				return invalid()
			}
			caps[cap] = true
		}
	}
	return d, nil
}

type workRow struct {
	ID, Requester, Title, Caps, State, Generation string
	Created, Updated, Deadline, Fence             int64
	Worker                                        string
	ClaimExpires                                  int64
	Result                                        string
	Sequence                                      int64
	AttemptGrantID                                string
}

const workColumns = `w.id,w.requester,w.title,w.capabilities,w.state,w.generation,w.created_at,w.updated_at,w.deadline,w.fence,w.worker,w.claim_expires_at,w.result_id,w.history_seq,w.attempt_grant_id`

func scanWork(scan interface{ Scan(...any) error }) (workRow, error) {
	var w workRow
	err := scan.Scan(&w.ID, &w.Requester, &w.Title, &w.Caps, &w.State, &w.Generation, &w.Created, &w.Updated, &w.Deadline, &w.Fence, &w.Worker, &w.ClaimExpires, &w.Result, &w.Sequence, &w.AttemptGrantID)
	return w, err
}

// The same expression is evaluated by transitions, projections and directory filters.
const workEffectiveSQL = `CASE WHEN w.state IN ('accepted','cancelled') THEN w.state WHEN w.deadline<=? THEN 'expired' WHEN w.generation<>? THEN 'recovery_required' WHEN w.state='claimed' AND w.claim_expires_at<=? THEN 'open' ELSE w.state END`

func effectiveWork(ctx context.Context, tx *sql.Tx, id, generation string, now int64) (string, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT `+workEffectiveSQL+` FROM works w WHERE id=?`, now, generation, now, id).Scan(&state)
	return state, err
}

type workRoot struct{ Room, Account, Author, Kind, Parent, PublicKey, Signature string }

func visibleWorkRoot(ctx context.Context, tx *sql.Tx, id string, a actor) (workRoot, error) {
	var root workRoot
	if !workIDRE.MatchString(id) {
		return root, problem(400, "invalid_message_id", "A message ID is 32 lowercase hexadecimal characters.")
	}
	err := tx.QueryRowContext(ctx, `SELECT room,account,author,kind,reply_to,public_key,signature FROM events WHERE id=? AND hidden=0`, id).Scan(&root.Room, &root.Account, &root.Author, &root.Kind, &root.Parent, &root.PublicKey, &root.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return root, workNotFound()
	}
	if err != nil {
		return root, err
	}
	if _, err = roomAccess(ctx, tx, root.Room, a); err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status == 404 {
			return workRoot{}, workNotFound()
		}
		return workRoot{}, err
	}
	return root, nil
}

func workNotFound() error { return problem(404, "not_found", "Work not found.") }

func eligibleWorkResult(ctx context.Context, tx *sql.Tx, id string, w workRow, root workRoot) (bool, error) {
	if !workIDRE.MatchString(id) {
		return false, nil
	}
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE id=? AND room=? AND reply_to=? AND account=? AND hidden=0 AND public_key<>'' AND signature<>''`, id, root.Room, w.ID, w.Worker).Scan(&exists)
	return exists == 1, err
}

func (s *Store) changeWork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	d, err := parseWorkData(c.Data, c.Operation == "work.create")
	if err != nil {
		return Result{}, err
	}
	root, err := visibleWorkRoot(ctx, tx, c.MessageID, a)
	if err != nil {
		return Result{}, err
	}
	var generation string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return Result{}, err
	}
	if d.Generation != generation {
		return Result{}, problem(409, "work_generation_mismatch", "The signed generation is stale; inspect current work before explicitly preparing a new transition.")
	}
	if c.Operation == "work.reject" || c.Operation == "work.cancel" {
		if strings.TrimSpace(c.Reason) == "" || len(c.Reason) > 2048 || !utf8.ValidString(c.Reason) || strings.ContainsRune(c.Reason, 0) {
			return Result{}, problem(400, "invalid_reason", "A nonempty reason of at most 2048 UTF-8 bytes without NUL is required.")
		}
	}
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, c.MessageID))
	if c.Operation == "work.create" {
		if err == nil {
			return Result{}, problem(409, "work_exists", "This message already has a work lifecycle.")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Result{}, err
		}
		if root.Account != a.account {
			return Result{}, problem(403, "work_forbidden", "Only the requester's continuous account may create this work.")
		}
		if (root.Kind != "request" && root.Kind != "simulation") || root.Parent != "" || root.PublicKey == "" || root.Signature == "" {
			return Result{}, problem(400, "invalid_work_root", "Work requires your own signed root request, or a message labeled kind=simulation.")
		}
		ttl := c.TTL
		if ttl == 0 {
			ttl = WorkDefaultTTL
		}
		if ttl < 60 || ttl > WorkMaxTTL {
			return Result{}, problem(400, "invalid_ttl", "Work TTL must be 60–2592000 seconds; zero defaults to 604800.")
		}
		caps, _ := json.Marshal(d.Capabilities)
		w = workRow{ID: c.MessageID, Requester: a.account, Title: d.Title, Caps: string(caps), State: "open", Generation: generation, Created: now, Updated: now, Deadline: now + ttl}
	} else {
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, workNotFound()
		}
		if err != nil {
			return Result{}, err
		}
		state, e := effectiveWork(ctx, tx, w.ID, generation, now)
		if e != nil {
			return Result{}, e
		}
		allowed := false
		switch c.Operation {
		case "work.claim":
			allowed = state == "open"
		case "work.renew", "work.submit":
			allowed = state == "claimed"
		case "work.accept":
			allowed = state == "submitted"
		case "work.reject":
			allowed = state == "claimed" || state == "submitted" || state == "recovery_required"
		case "work.cancel":
			allowed = state == "open" || state == "claimed" || state == "submitted" || state == "recovery_required"
		}
		if !allowed {
			return Result{}, problem(409, "work_state_conflict", "This transition is not allowed in the current effective state.")
		}
		switch c.Operation {
		case "work.claim":
			if a.account == w.Requester {
				return Result{}, problem(403, "work_forbidden", "The requester cannot claim their own work.")
			}
		case "work.renew", "work.submit":
			if a.account != w.Worker {
				return Result{}, problem(403, "work_forbidden", "Only the current worker's continuous account may perform this transition.")
			}
			if a.grant != nil && w.AttemptGrantID != a.grant.ID {
				return Result{}, delegationError("delegation_forbidden")
			}
		default:
			if a.account != w.Requester {
				return Result{}, problem(403, "work_forbidden", "Only the requester's continuous account may perform this transition.")
			}
		}
		if c.Operation != "work.claim" && c.Operation != "work.cancel" && (c.Amount != w.Fence || c.Amount < 0 || (c.Amount == 0 && state != "recovery_required")) {
			return Result{}, problem(409, "work_fence_mismatch", "The attempt fencing token does not match.")
		}
		if c.Operation == "work.claim" || c.Operation == "work.renew" {
			if c.TTL < 60 || c.TTL > 3600 || c.TTL > w.Deadline-now {
				return Result{}, problem(400, "invalid_ttl", "Claim TTL must be 60–3600 seconds and fit entirely before the work deadline.")
			}
			if c.Operation == "work.renew" && now+c.TTL <= w.ClaimExpires {
				return Result{}, problem(409, "work_renew_not_extended", "Renewal must strictly extend the current claim expiry.")
			}
		}
		switch c.Operation {
		case "work.claim":
			if w.Fence == math.MaxInt64 {
				return Result{}, problem(409, "work_fence_exhausted", "This work cannot allocate another fencing token.")
			}
			w.State = "claimed"
			w.Fence++
			w.Worker = a.account
			w.AttemptGrantID = ""
			if a.grant != nil {
				w.AttemptGrantID = a.grant.ID
			}
			w.ClaimExpires = now + c.TTL
			w.Result = ""
		case "work.renew":
			w.ClaimExpires = now + c.TTL
		case "work.submit", "work.accept":
			result := w.Result
			if c.Operation == "work.submit" {
				result = c.Target
				if a.grant != nil {
					var matches int
					if e := tx.QueryRowContext(ctx, "SELECT count(*) FROM event_delegations d JOIN events e ON e.id=d.event_id WHERE d.event_id=? AND d.grant_id=? AND e.author=?", result, a.grant.ID, a.id).Scan(&matches); e != nil {
						return Result{}, e
					}
					if matches != 1 {
						return Result{}, problem(400, "invalid_work_result", "A delegated result must be signed under the same grant.")
					}
				}
			}
			eligible, e := eligibleWorkResult(ctx, tx, result, w, root)
			if e != nil {
				return Result{}, e
			}
			if !eligible {
				return Result{}, problem(400, "invalid_work_result", "Result must be a visible signed direct reply in the same room by the current worker account.")
			}
			w.State = "accepted"
			if c.Operation == "work.submit" {
				w.State = "submitted"
				w.Result = result
			}
		case "work.reject":
			w.State = "open"
			w.AttemptGrantID = ""
			w.Worker = ""
			w.Result = ""
			w.ClaimExpires = 0
		case "work.cancel":
			w.State = "cancelled"
		}
		w.Generation = generation
		w.Updated = now
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+512, now); err != nil {
		return Result{}, err
	}
	w.Sequence++
	_, err = tx.ExecContext(ctx, `INSERT INTO works(id,requester,title,capabilities,state,generation,created_at,updated_at,deadline,fence,worker,claim_expires_at,result_id,history_seq) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,generation=excluded.generation,updated_at=excluded.updated_at,fence=excluded.fence,worker=excluded.worker,claim_expires_at=excluded.claim_expires_at,result_id=excluded.result_id,history_seq=excluded.history_seq`, w.ID, w.Requester, w.Title, w.Caps, w.State, w.Generation, w.Created, w.Updated, w.Deadline, w.Fence, w.Worker, w.ClaimExpires, w.Result, w.Sequence)
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE works SET attempt_grant_id=? WHERE id=?", w.AttemptGrantID, w.ID); err != nil {
		return Result{}, err
	}
	grantID := ""
	if a.grant != nil {
		grantID = a.grant.ID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_transitions(work_id,sequence,operation,author,public_key,signature,payload,accepted_at,fence,generation,state,delegation_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, w.ID, w.Sequence, c.Operation, a.id, a.publicKey, c.Signature, string(a.canonical), now, w.Fence, generation, w.State, grantID)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"ack": WorkAck{WorkID: w.ID, State: w.State, Fence: w.Fence, Generation: w.Generation, ServiceID: s.config.ServiceID, AcceptedAt: now, Deadline: w.Deadline, ClaimExpiresAt: w.ClaimExpires}}}, nil
}

func currentWorkIdentity(ctx context.Context, tx *sql.Tx, account string) (AgentRef, error) {
	var identity AgentRef
	err := tx.QueryRowContext(ctx, `SELECT id,public_key,handle FROM identities WHERE account=? AND successor=''`, account).Scan(&identity.ID, &identity.PublicKey, &identity.Handle)
	return identity, err
}

func (s *Store) projectWork(ctx context.Context, tx *sql.Tx, w workRow, root workRoot, generation string, now int64) (Work, error) {
	p := Work{ID: w.ID, Room: root.Room, Title: w.Title, Simulated: root.Kind == "simulation", StoredState: w.State, Generation: w.Generation, ServiceGeneration: generation, ServiceID: s.config.ServiceID, CreatedAt: w.Created, UpdatedAt: w.Updated, Deadline: w.Deadline, Fence: w.Fence, ClaimExpiresAt: w.ClaimExpires, RequesterAuthor: root.Author}
	p.AttemptGrantID = w.AttemptGrantID
	var err error
	if err = json.Unmarshal([]byte(w.Caps), &p.Capabilities); err != nil {
		return Work{}, err
	}
	if p.State, err = effectiveWork(ctx, tx, w.ID, generation, now); err != nil {
		return Work{}, err
	}
	if p.Requester, err = currentWorkIdentity(ctx, tx, w.Requester); err != nil {
		return Work{}, err
	}
	if w.Worker != "" {
		identity, e := currentWorkIdentity(ctx, tx, w.Worker)
		if e != nil {
			return Work{}, e
		}
		p.Worker = &identity
	}
	if w.Result != "" {
		if p.ResultAvailable, err = eligibleWorkResult(ctx, tx, w.Result, w, root); err != nil {
			return Work{}, err
		}
		if p.ResultAvailable {
			p.ResultID = w.Result
		}
	}
	return p, nil
}

func (s *Store) readWork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if c.Limit < 0 || c.Limit > DirectoryPageMax {
		return Result{}, problem(400, "invalid_limit", fmt.Sprintf("Work read limit must be 1–%d, or zero for default 25.", DirectoryPageMax))
	}
	limit := c.Limit
	if limit == 0 {
		limit = 25
	}
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='generation'`).Scan(&generation); err != nil {
		return Result{}, workReadError(err)
	}
	if c.Operation != "works.list" {
		root, err := visibleWorkRoot(ctx, tx, c.MessageID, a)
		if err != nil {
			return Result{}, workReadError(err)
		}
		w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM works w WHERE id=?`, c.MessageID))
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, workNotFound()
		}
		if err != nil {
			return Result{}, workReadError(err)
		}
		if c.Operation == "work.history" {
			return s.workHistory(ctx, tx, c, w, root, generation, limit)
		}
		p, err := s.projectWork(ctx, tx, w, root, generation, now)
		return Result{Data: map[string]any{"work": p}}, workReadError(err)
	}
	if !utf8.ValidString(c.Query) || strings.ContainsRune(c.Query, 0) {
		return Result{}, problem(400, "invalid_query", "Query must be valid UTF-8 without NUL.")
	}
	if c.Kind != "" && c.Kind != "open" && c.Kind != "claimed" && c.Kind != "submitted" && c.Kind != "accepted" && c.Kind != "cancelled" && c.Kind != "expired" && c.Kind != "recovery_required" {
		return Result{}, problem(400, "invalid_work_state", "Unknown work state filter.")
	}
	// An agent's own page asks the same listing for the work it is part of, so the
	// scope has to include the agent: a cursor from one scope must not decode in
	// another.
	agentAccount := ""
	if c.Target != "" {
		var err error
		if agentAccount, err = lookupAccount(ctx, tx, c.Target); err != nil {
			return Result{}, err
		}
	}
	scopeBytes, _ := json.Marshal([]string{c.Room, c.Kind, c.Query, c.Target})
	scope := string(scopeBytes)
	cursor, err := s.decodeConversationCursor(c.Cursor, "works.list", scope)
	if err != nil {
		return Result{}, err
	}
	if cursor.Page != "" && !workIDRE.MatchString(cursor.Page) {
		return Result{}, problem(400, "invalid_cursor", "Invalid work directory cursor.")
	}
	where := `e.hidden=0 AND w.id>?`
	args := []any{cursor.Page}
	if c.Room != "" {
		if _, err = roomAccess(ctx, tx, c.Room, a); err != nil {
			return Result{}, workReadError(err)
		}
		where += ` AND e.room=?`
		args = append(args, c.Room)
	} else {
		where += ` AND r.visibility='public' AND e.kind<>'simulation'`
	}
	if c.Kind != "" {
		where += ` AND (` + workEffectiveSQL + `)=?`
		args = append(args, now, generation, now, c.Kind)
	}
	if c.Query != "" {
		where += ` AND (instr(lower(w.title),lower(?))>0 OR EXISTS(SELECT 1 FROM json_each(w.capabilities) cap WHERE cap.value=lower(?)))`
		args = append(args, c.Query, c.Query)
	}
	if agentAccount != "" {
		// Requested or worked: both are this agent's involvement in the work.
		where += ` AND (w.requester=? OR w.worker=?)`
		args = append(args, agentAccount, agentAccount)
	}
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT `+workColumns+` FROM works w JOIN events e ON e.id=w.id JOIN rooms r ON r.name=e.room WHERE `+where+` ORDER BY w.id LIMIT ?`, args...)
	if err != nil {
		return Result{}, workReadError(err)
	}
	stored := []workRow{}
	for rows.Next() {
		w, e := scanWork(rows)
		if e != nil {
			rows.Close()
			return Result{}, workReadError(e)
		}
		stored = append(stored, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Result{}, workReadError(err)
	}
	hasMore := len(stored) > limit
	if hasMore {
		stored = stored[:limit]
	}
	works := []Work{}
	for _, w := range stored {
		root, e := visibleWorkRoot(ctx, tx, w.ID, a)
		if e != nil {
			return Result{}, workReadError(e)
		}
		p, e := s.projectWork(ctx, tx, w, root, generation, now)
		if e != nil {
			return Result{}, workReadError(e)
		}
		works = append(works, p)
	}
	result := Result{Data: map[string]any{"works": works, "has_more": hasMore}}
	if hasMore {
		result.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "works.list", Scope: scope, Page: stored[len(stored)-1].ID})
	}
	return result, nil
}

func (s *Store) workHistory(ctx context.Context, tx *sql.Tx, c Command, w workRow, root workRoot, generation string, limit int) (Result, error) {
	cursor, err := s.decodeConversationCursor(c.Cursor, "work.history", w.ID)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,operation,author,public_key,signature,payload,accepted_at,fence,generation,state,delegation_id FROM work_transitions WHERE work_id=? AND sequence>? ORDER BY sequence LIMIT ?`, w.ID, cursor.After, limit+1)
	if err != nil {
		return Result{}, workReadError(err)
	}
	defer rows.Close()
	transitions := []WorkTransition{}
	for rows.Next() {
		var tr WorkTransition
		if err = rows.Scan(&tr.Sequence, &tr.Operation, &tr.Author, &tr.PublicKey, &tr.Signature, &tr.SignedPayload, &tr.AcceptedAt, &tr.Fence, &tr.Generation, &tr.State, &tr.DelegationID); err != nil {
			return Result{}, workReadError(err)
		}
		transitions = append(transitions, tr)
	}
	if err = rows.Err(); err != nil {
		return Result{}, workReadError(err)
	}
	hasMore := len(transitions) > limit
	if hasMore {
		transitions = transitions[:limit]
	}
	r := Result{Data: map[string]any{"work_id": w.ID, "simulated": root.Kind == "simulation", "service_generation": generation, "transitions": transitions, "has_more": hasMore}}
	if hasMore {
		r.NextCursor = s.encodeConversationCursor(conversationCursor{Domain: "work.history", Scope: w.ID, After: transitions[len(transitions)-1].Sequence})
	}
	return r, nil
}

func workReadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: 503, Code: "work_read_timeout", Message: "Work read exceeded its two-second query budget; narrow the query or retry.", RetryAfter: 2}
	}
	return err
}
