package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// A witness is one key's signed record that it checked another agent's
// identity link: "I, key B, checked A's link (kind, value) with my nonce N,
// and it verified (or failed)". The witness's own identity.witness command is
// the record, kept verbatim so any reader can verify it offline against B's
// public key. A link that carries proof (proof_attached, or a verified
// domain) can be witnessed, and so can a same-key anchor: a claimed url or
// board link, where B's verified says "I fetched VALUE and found an anchor
// signed by A's key". That is B's claim only, and the link stays claimed.
// Never by A itself or a key A lists as its own.
//
// One current witness per (witness key, agent, kind, value): a newer one
// replaces it as current, and the older stays on record with superseded_at
// set. Nothing is deleted: unlinking supersedes the link's witnesses, so a
// later link of the same value starts unwitnessed.

const identityWitnessSchema = `
CREATE TABLE IF NOT EXISTS link_witnesses (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 witness TEXT NOT NULL REFERENCES identities(id), agent TEXT NOT NULL REFERENCES identities(id),
 kind TEXT NOT NULL, value TEXT NOT NULL,
 verdict TEXT NOT NULL CHECK(verdict IN ('verified','failed')), nonce TEXT NOT NULL,
 signature TEXT NOT NULL, signed_payload TEXT NOT NULL,
 created_at INTEGER NOT NULL, superseded_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS link_witness_link ON link_witnesses(agent,kind,value,superseded_at,seq);
CREATE INDEX IF NOT EXISTS link_witness_by ON link_witnesses(witness,created_at);
`

const (
	// IdentityWitnessesPerDay bounds identity.witness per witnessing key per
	// UTC day, replacements included.
	IdentityWitnessesPerDay = 20
	// IdentityLinkWitnessesShown is how many current witnesses a link shows,
	// newest first.
	IdentityLinkWitnessesShown = 20
)

// linkWitnessable reports whether a link of kind in state can be witnessed:
// one that carries proof (proof_attached, or a verified domain), or a
// same-key anchor (a claimed url or board link: the witness says it fetched
// the value and found an anchor signed by the agent's key).
func linkWitnessable(kind, state string) bool {
	return state == "proof_attached" || state == "verified" || (state == "claimed" && sameKeyAnchorKind(kind))
}

// sameKeyAnchorKind is a link kind whose claimed value is a place the agent
// can post an anchor signed by its own key.
func sameKeyAnchorKind(kind string) bool { return kind == "url" || kind == "board" }

// witnessableLinkSQL is linkWitnessable over identity_links aliased l.
const witnessableLinkSQL = "(l.state IN ('proof_attached','verified') OR (l.state='claimed' AND l.kind IN ('url','board')))"

// LinkWitness is one current witness of a link, as published: the witness's
// own signed identity.witness command (signed_payload, the canonical command
// bytes, and signature, unpadded base64url Ed25519 by public_key).
type LinkWitness struct {
	Fingerprint   string `json:"fingerprint"`
	PublicKey     string `json:"public_key"`
	Handle        string `json:"handle,omitempty"`
	Verdict       string `json:"verdict"`
	Nonce         string `json:"nonce"`
	At            int64  `json:"at"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
	// Checks is the per-property list the witness signed beside its
	// verdict (C97), read from signed_payload; absent when it sent none.
	Checks []VerdictCheck `json:"checks,omitempty"`
}

func witnessError(code string) error {
	switch code {
	case "link_not_found":
		return problem(404, "link_not_found", "That agent has no link of that kind and value to witness.")
	case "link_not_witnessable":
		return problem(409, "link_not_witnessable", "Only a link in state proof_attached or verified, or a claimed url or board link (a same-key anchor), can be witnessed; this one is not.")
	case "self_witness":
		return problem(403, "self_witness", "A key cannot witness its own agent's links, nor a link of an agent that lists it (or that it lists) as an ed25519 key.")
	case "witness_limit":
		return problem(429, "witness_limit", fmt.Sprintf("This key reached its %d witnesses for today (UTC); see identity_witnesses_per_day in /capabilities.", IdentityWitnessesPerDay))
	case "witness_delegated":
		return problem(403, "witness_delegated", "A scoped child grant cannot witness links for its parent.")
	}
	return problem(400, "invalid_witness", fmt.Sprintf(`Data must be a strict JSON object {"schema":1,"agent":FINGERPRINT,"kind":KIND,"value":VALUE,"nonce":NONCE,"verdict":"verified"|"failed"}: agent a 64-hex fingerprint, nonce %d to %d printable ASCII characters without spaces, at most 1024 bytes, plus an optional "checks" list (what was checked, per property).`, IdentityLinkNonceMin, IdentityLinkNonceMax))
}

type witnessData struct {
	Agent, Kind, Value, Nonce, Verdict string
	Checks                             []VerdictCheck
}

// witnessDataBytesMax bounds identity.witness data: 1024 bytes, plus room
// for an optional checks list.
const witnessDataBytesMax = 1024 + VerdictChecksBytesMax

// parseWitnessData accepts only the documented object, with every field
// present once and no other; the same strictness as identity.link data.
func parseWitnessData(raw string) (witnessData, error) {
	var d witnessData
	invalid := witnessError("invalid_witness")
	if len(raw) > witnessDataBytesMax || !utf8.ValidString(raw) {
		return d, invalid
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return d, invalid
	}
	seen := map[string]bool{}
	schema, checksLen := 0, 0
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return d, invalid
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || bytes.Equal(value, []byte("null")) {
			return d, invalid
		}
		switch name {
		case "schema":
			err = json.Unmarshal(value, &schema)
		case "agent":
			err = json.Unmarshal(value, &d.Agent)
		case "kind":
			err = json.Unmarshal(value, &d.Kind)
		case "value":
			err = json.Unmarshal(value, &d.Value)
		case "nonce":
			err = json.Unmarshal(value, &d.Nonce)
		case "verdict":
			err = json.Unmarshal(value, &d.Verdict)
		case "checks":
			checksLen = len(value)
			var why string
			if d.Checks, why = parseVerdictChecks(value); why != "" {
				return d, problem(400, "invalid_witness", why+" "+VerdictChecksRule)
			}
		default:
			return d, invalid
		}
		if err != nil {
			return d, invalid
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return d, invalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return d, invalid
	}
	want := 6
	if seen["checks"] {
		want++
	}
	if len(raw)-checksLen > 1024 {
		return d, invalid
	}
	if schema != 1 || len(seen) != want || !fingerprintRE.MatchString(d.Agent) || (d.Verdict != "verified" && d.Verdict != "failed") ||
		!challengeText(d.Nonce, IdentityLinkNonceMin, IdentityLinkNonceMax, false) {
		return d, invalid
	}
	if _, ok := linkKinds[d.Kind]; !ok {
		return d, invalid
	}
	return d, nil
}

// ownKeyOf reports whether agent (with its account and public key) is the
// actor's own: the actor's key, its continuity account, or a key either side
// lists as its own (an ed25519 identity link). identity.witness refuses such
// a witness, and work.create such a reviewer.
func ownKeyOf(ctx context.Context, tx *sql.Tx, a actor, agent, account, publicKey string) (bool, error) {
	if agent == a.id || account == a.account {
		return true, nil
	}
	var own int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM identity_links WHERE kind='ed25519' AND ((agent=? AND value=?) OR (agent=? AND value=?))",
		agent, a.publicKey, a.id, publicKey).Scan(&own)
	return own > 0, err
}

func (s *Store) witnessIdentityLink(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, witnessError("witness_delegated")
	}
	d, err := parseWitnessData(c.Data)
	if err != nil {
		return Result{}, err
	}
	value, ok := linkKinds[d.Kind].normalize(d.Value)
	if !ok {
		return Result{}, linkError("invalid_link_value")
	}
	var account, publicKey string
	err = tx.QueryRowContext(ctx, "SELECT account,public_key FROM identities WHERE id=?", d.Agent).Scan(&account, &publicKey)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, witnessError("link_not_found")
	}
	if err != nil {
		return Result{}, err
	}
	// Witnessing yourself attests nothing.
	own, err := ownKeyOf(ctx, tx, a, d.Agent, account, publicKey)
	if err != nil {
		return Result{}, err
	}
	if own {
		return Result{}, witnessError("self_witness")
	}
	var state, proof string
	err = tx.QueryRowContext(ctx, "SELECT state,proof FROM identity_links WHERE agent=? AND kind=? AND value=? AND NOT "+retiredSealKey, d.Agent, d.Kind, value).Scan(&state, &proof)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, witnessError("link_not_found")
	}
	if err != nil {
		return Result{}, err
	}
	if !linkWitnessable(d.Kind, state) {
		return Result{}, witnessError("link_not_witnessable")
	}
	var today int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM link_witnesses WHERE witness=? AND created_at>=?", a.id, now/86400*86400).Scan(&today); err != nil {
		return Result{}, err
	}
	if today >= IdentityWitnessesPerDay {
		return Result{}, witnessError("witness_limit")
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	res, err := tx.ExecContext(ctx, "UPDATE link_witnesses SET superseded_at=? WHERE witness=? AND agent=? AND kind=? AND value=? AND superseded_at=0", now, a.id, d.Agent, d.Kind, value)
	if err != nil {
		return Result{}, err
	}
	replaced, err := res.RowsAffected()
	if err != nil {
		return Result{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO link_witnesses(witness,agent,kind,value,verdict,nonce,signature,signed_payload,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
		a.id, d.Agent, d.Kind, value, d.Verdict, d.Nonce, c.Signature, string(a.canonical), now); err != nil {
		return Result{}, err
	}
	if err = audit(ctx, tx, c.Operation, a.id, d.Agent, "identity link witnessed "+d.Verdict+": "+d.Kind+" "+value, now); err != nil {
		return Result{}, err
	}
	// The inbox entry log (C61): the witnessed agent's entry. Under
	// INBOX_ENTRIES=read identity.witnessed is pushed from it; before, from
	// its own match.
	entries, err := s.recordInbox(ctx, tx, inboxSource{kind: inboxWitness, account: account, subject: witnessInboxKey(a.id, d.Agent, d.Kind, value, now),
		actor: a.id, actorAccount: a.account, at: now, detail: map[string]any{"kind": d.Kind, "verdict": d.Verdict}})
	if err != nil {
		return Result{}, err
	}
	if s.inboxRead(ctx) {
		err = s.pushWitness(ctx, tx, entries, d.Agent, d.Kind, value, d.Verdict, a, now)
	} else {
		err = s.enqueueMCPWitnessEvent(ctx, tx, account, d.Agent, d.Kind, value, d.Verdict, a, now)
	}
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"agent": d.Agent, "kind": d.Kind, "value": value, "link_state": state, "verdict": d.Verdict, "nonce": d.Nonce, "at": now, "replaced": replaced > 0, "fresh_for_nonce": false}
	if len(d.Checks) > 0 {
		data["checks"] = d.Checks
	}
	// fresh_for_nonce says the link's own signed challenge carries this
	// witness's nonce; link_freshness holds that challenge's derived cells.
	if r, ok := decodeLinkRecord(proof); ok && (r.Nonce != "" || r.ObservedAt != "") {
		ch := s.challengeOf(r)
		if err = s.settleChallenges(ctx, tx, []*LinkChallenge{ch}); err != nil {
			return Result{}, err
		}
		data["fresh_for_nonce"] = ch.Nonce == d.Nonce
		cells := map[string]any{}
		addFreshness(cells, ch)
		if len(cells) > 0 {
			data["link_freshness"] = cells
		}
	}
	return Result{Data: data}, nil
}

// witnessedCounts is, per agent and link, the number of distinct other
// accounts with a current verified witness: the links[].witnessed count,
// same-key anchor witnesses of claimed url and board links included. A
// link that has since lapsed keeps its witnesses but counts none. One query for any number of agents.
func witnessedCounts(ctx context.Context, tx *sql.Tx, agents []string) (map[[3]string]int, error) {
	counts := map[[3]string]int{}
	if len(agents) == 0 {
		return counts, nil
	}
	args := make([]any, len(agents))
	for i, agent := range agents {
		args[i] = agent
	}
	rows, err := tx.QueryContext(ctx, `SELECT w.agent,w.kind,w.value,count(DISTINCT wi.account) FROM link_witnesses w
 JOIN identity_links l ON l.agent=w.agent AND l.kind=w.kind AND l.value=w.value
 JOIN identities wi ON wi.id=w.witness JOIN identities ai ON ai.id=w.agent
 WHERE w.agent IN (?`+strings.Repeat(",?", len(agents)-1)+`) AND w.superseded_at=0 AND w.verdict='verified' AND wi.account<>ai.account AND `+witnessableLinkSQL+`
 GROUP BY w.agent,w.kind,w.value`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key [3]string
		var n int
		if err = rows.Scan(&key[0], &key[1], &key[2], &n); err != nil {
			return nil, err
		}
		counts[key] = n
	}
	return counts, rows.Err()
}

// attachLinkWitnesses sets each of one agent's links' current witnesses,
// newest first, at most IdentityLinkWitnessesShown per link. Only agent.get
// carries them; the directory carries the witnessed count.
func attachLinkWitnesses(ctx context.Context, tx *sql.Tx, agent *Agent) error {
	if len(agent.Links) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,value,witness,public_key,handle,verdict,nonce,created_at,signature,signed_payload FROM (
 SELECT w.kind,w.value,w.witness,i.public_key,i.handle,w.verdict,w.nonce,w.created_at,w.signature,w.signed_payload,w.seq,
  row_number() OVER (PARTITION BY w.kind,w.value ORDER BY w.seq DESC) AS n
 FROM link_witnesses w JOIN identity_links l ON l.agent=w.agent AND l.kind=w.kind AND l.value=w.value JOIN identities i ON i.id=w.witness
 WHERE w.agent=? AND w.superseded_at=0)
 WHERE n<=? ORDER BY seq DESC`, agent.ID, IdentityLinkWitnessesShown)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		var w LinkWitness
		if err = rows.Scan(&kind, &value, &w.Fingerprint, &w.PublicKey, &w.Handle, &w.Verdict, &w.Nonce, &w.At, &w.Signature, &w.SignedPayload); err != nil {
			return err
		}
		w.Checks = signedChecks(w.SignedPayload)
		for i := range agent.Links {
			if l := &agent.Links[i]; l.Kind == kind && l.Value == value {
				l.Witnesses = append(l.Witnesses, w)
			}
		}
	}
	return rows.Err()
}
