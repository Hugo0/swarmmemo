package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Owner erasure (C1012, first build): the operator erases an account on its
// owner's request (`swarmmemo erase account FP`). Content and personal data
// are blanked in place, so threads, ledgers and the transparency log still
// resolve: posts read as tombstones hidden by their author with the reason
// ErasedReason (the Hugging Face publisher already accepts hidden_by
// "author"), and keys, profile, links, memory, docs, pastes, files,
// receivers, webhooks, tool results, run source code (unless an account not
// erased ran the same code), stored write answers and write records go, and
// the values it supplied leave its audit rows. Ledger and payout
// rows, log leaves and hashes, moderation decisions and sanctions, votes and
// vouches, signed work transitions and other members' words stay.
//
// The erasure itself is recorded in erasure_log (counts only, never content),
// so a restored older copy can be erased again.

// ErasedReason is the public reason an erased post reads.
const ErasedReason = "erased by owner"

// erasureLogSchema is schema 54: one row per erasure run. It holds counts,
// the operator's request reference, the digests of the identity links the
// account had (the "linked once" marker) and the receipt's hash.
const erasureLogSchema = `
CREATE TABLE IF NOT EXISTS erasure_log (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT NOT NULL, erased_at INTEGER NOT NULL,
 reference TEXT NOT NULL DEFAULT '', counts TEXT NOT NULL DEFAULT '[]', link_digests TEXT NOT NULL DEFAULT '[]',
 raw_held INTEGER NOT NULL DEFAULT 0, receipt_sha256 TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS erasure_log_account ON erasure_log(account,erased_at);
CREATE TRIGGER IF NOT EXISTS erasure_log_no_delete BEFORE DELETE ON erasure_log BEGIN SELECT RAISE(ABORT,'the erasure log is kept'); END;
`

// EraseCount is what one step of an erasure did to one store.
type EraseCount struct {
	Store  string `json:"store"`
	Action string `json:"action"`
	Rows   int64  `json:"rows"`
}

// ErasureKept names what an erasure keeps, and why, for the receipt.
var ErasureKept = []string{
	"transparency log leaves and hashes (they never held text)",
	"credit ledger, holds, payouts and their transaction hashes (accounting, public by policy)",
	"moderation decisions, reports and sanctions (an erasure does not end a sanction)",
	"votes and vouches as weights (other accounts' standing rests on them)",
	"signed work transitions (the other party's record of the work)",
	"offering calls, their inputs, answers and pay_to (the other party's record of a paid call)",
	"payments, top-ups and bonds with their wallets and transaction hashes (accounting)",
	"standing proofs' assessments, their values and roots only as keyed hashes (one outside account never makes a second identity)",
	"activity counts and funnel rows (numbers only, never content)",
	"the operator's audit trail: operations, keys and times, with the values the account supplied redacted",
	"other members' messages and replies",
	"the fingerprint itself, so no one else can claim the erased history",
}

// ErasureReceipt is what an erasure run reports; its JSON is hashed into
// erasure_log.receipt_sha256.
type ErasureReceipt struct {
	Schema      int          `json:"schema"`
	Account     string       `json:"account"`
	ErasedAt    int64        `json:"erased_at"`
	Reference   string       `json:"reference,omitempty"`
	Counts      []EraseCount `json:"counts"`
	RawErased   int64        `json:"raw_signals_erased"`
	RawHeld     int64        `json:"raw_signals_held_as_evidence"`
	Kept        []string     `json:"kept"`
	Backups     string       `json:"backups"`
	DryRun      bool         `json:"-"`
	ReceiptHash string       `json:"-"`
	// Signed is the log key's signed statement of the receipt's hash
	// (signErasure), once recorded.
	Signed string `json:"-"`
}

// ErasureBackupsNote is the receipt's line on copies outside the live database.
const ErasureBackupsNote = "Backups keep the erased data until they age out (about 30 days); a restored copy is erased again at start from the side copy of erasure_log. The web server's access log has no account field and rolls over."

// eraseRefRE is an operator's request reference: no personal data, just an id.
var eraseRefRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// eraseOwn is the account's identity ids and the account itself, as a
// subquery over ?1.
const eraseOwn = "(SELECT id FROM identities WHERE account=?1 UNION SELECT ?1)"

// eraseRequestOwn is every requests namespace the account's writes are
// stored under, as a subquery over ?1 (C1124): its account and key ids
// (actor.requestNamespace, store.go), their private-read controls
// ("private-control:", private_read.go) and its worker keys' grants
// ("delegate:", delegation.go).
const eraseRequestOwn = "(SELECT id FROM " + eraseOwn + " UNION SELECT 'private-control:'||id FROM " + eraseOwn +
	" UNION SELECT 'delegate:'||child_id FROM delegations WHERE parent_account IN " + eraseOwn + ")"

// eraseErased is every account erased before and its identity ids, as a
// subquery: content it shared with the account being erased no longer keeps
// that content (C1134).
const eraseErased = "(SELECT account FROM erasure_log UNION SELECT id FROM identities WHERE account IN (SELECT account FROM erasure_log))"

// ErasedDetail is what an erasure leaves of an audit row's detail that held
// a value the account supplied.
const ErasedDetail = "[erased]"

// eraseAuditOps are the audit operations whose detail is a value the
// account supplied (C1124): identity links and their witnessing, handles,
// a file's or a room's closing reason, an event subscription's name. An
// erasure redacts that detail in the account's rows, as actor or target;
// the row (operation, keys, time) stays as the operator's record. The other
// details are counts, ids, hashes, amounts and server text.
const eraseAuditOps = "'identity.link','identity.unlink','identity.witness','agent.register','handle.claim','blob.delete','room.close','mcp.events.subscribe'"

// EraseObligations lists what keeps an account from being erased: open work
// it claimed or posted, held credits, and rewards, review fees or USDC
// payouts still in flight. Erasure must not dodge a fee or a clawback.
func EraseObligations(ctx context.Context, tx *sql.Tx, account string) ([]string, error) {
	checks := []struct{ what, query string }{
		{"work it claimed and has not finished", "SELECT count(*) FROM works WHERE worker IN " + eraseOwn + " AND state IN ('claimed','submitted')"},
		{"work it posted that is still open", "SELECT count(*) FROM works WHERE requester IN " + eraseOwn + " AND state IN ('open','claimed','submitted')"},
		{"held credits", "SELECT count(*) FROM ledger_holds WHERE account IN " + eraseOwn + " AND state='held'"},
		{"work rewards in flight", "SELECT count(*) FROM work_rewards WHERE (requester IN " + eraseOwn + " OR worker IN " + eraseOwn + ") AND state IN ('held','pending')"},
		{"review fees in flight", "SELECT count(*) FROM work_review_fees WHERE (requester IN " + eraseOwn + " OR worker IN " + eraseOwn + ") AND state IN ('held','pending')"},
		{"USDC payouts in flight", "SELECT count(*) FROM work_usdc WHERE (requester IN " + eraseOwn + " OR worker IN " + eraseOwn + ") AND state IN ('promised','payable')"},
		{"offering calls paid or in payment and not yet answered", "SELECT count(*) FROM offering_calls WHERE account IN " + eraseOwn + " AND state IN ('verifying','authorized','settling','paid','overdue')"},
	}
	var out []string
	for _, c := range checks {
		var n int64
		if err := tx.QueryRowContext(ctx, c.query, account).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, fmt.Sprintf("%d %s", n, c.what))
		}
	}
	return out, nil
}

// EraseAccount blanks one account's content and personal data in the main
// database, in the caller's transaction (it touches nothing else: no pool,
// no second connection). It is idempotent: a second run counts zero rows, so
// a restore can replay erasure_log through it. Raw write records live in
// their own database and are erased by OperatorEraseAccount after commit.
func (s *Store) EraseAccount(ctx context.Context, tx *sql.Tx, account string, now int64, keep EraseEvidence) ([]EraseCount, error) {
	if !fingerprintRE.MatchString(account) {
		return nil, errAgentFingerprint()
	}
	var counts []EraseCount
	exec := func(store, action, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, append([]any{account}, args...)...)
		if err != nil {
			return fmt.Errorf("erase %s: %w", store, err)
		}
		n, _ := res.RowsAffected()
		counts = append(counts, EraseCount{store, action, n})
		return nil
	}
	// Posts and private messages, every version. A visible one is hidden by
	// its author through setHidden (the public correction is queued and
	// pending deliveries dropped); a post a moderator already hid keeps that
	// decision, its category and reason. Then the bytes go.
	rows, err := tx.QueryContext(ctx, "SELECT e.id,r.visibility FROM events e JOIN rooms r ON r.name=e.room WHERE e.account IN "+eraseOwn+" AND e.hidden=0 ORDER BY e.seq", account)
	if err != nil {
		return nil, err
	}
	type visible struct{ id, visibility string }
	var hide []visible
	for rows.Next() {
		var v visible
		if err = rows.Scan(&v.id, &v.visibility); err != nil {
			rows.Close()
			return nil, err
		}
		hide = append(hide, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, v := range hide {
		if err = setHidden(ctx, tx, v.id, v.visibility, true, ErasedReason, hiddenByAuthor, now); err != nil {
			return nil, err
		}
	}
	counts = append(counts, EraseCount{"events", "hidden as erased by owner", int64(len(hide))})
	steps := []struct{ store, action, query string }{
		{"events", "text, signature, payload and handle blanked", "UPDATE events SET text='',signature='',payload='',format='',handle='' WHERE account IN " + eraseOwn + " AND (text<>'' OR signature<>'' OR payload<>'' OR format<>'' OR handle<>'')"},
		{"moderation_queue", "held copy of the text blanked (decision kept)", "UPDATE moderation_queue SET content='' WHERE content<>'' AND subject IN (SELECT id FROM events WHERE account IN " + eraseOwn + ")"},
		{"peer_cards", "profile removed", "DELETE FROM peer_cards WHERE account IN " + eraseOwn},
		{"identities", "handle and labels blanked, keys revoked (fingerprint kept)", "UPDATE identities SET handle='',label='',revoked_at=CASE WHEN revoked_at=0 THEN ?2 ELSE revoked_at END WHERE account=?1 AND (handle<>'' OR label<>'' OR revoked_at=0)"},
		{"link_witnesses", "witnessed link values blanked", "UPDATE link_witnesses SET value='',signature='',signed_payload='',superseded_at=CASE WHEN superseded_at=0 THEN ?2 ELSE superseded_at END WHERE agent IN " + eraseOwn + " AND (value<>'' OR signature<>'' OR signed_payload<>'')"},
		{"identity_links", "removed (digests kept in erasure_log)", "DELETE FROM identity_links WHERE agent IN " + eraseOwn},
		{"hosted_keys", "private key and recovery hash erased", "UPDATE hosted_keys SET sealed_private_key='',recovery_sha256='',state='suspended' WHERE account IN " + eraseOwn + " AND (sealed_private_key<>'' OR recovery_sha256<>'' OR state<>'suspended')"},
		{"hosted_tokens", "revoked, labels blanked (hashes kept)", "UPDATE hosted_tokens SET label='',revoked_at=CASE WHEN revoked_at=0 THEN ?2 ELSE revoked_at END WHERE account IN " + eraseOwn + " AND (label<>'' OR revoked_at=0)"},
		{"oauth_families", "app sign-ins revoked", "UPDATE oauth_families SET revoked_at=?2,revoked_reason='erased' WHERE account IN " + eraseOwn + " AND revoked_at=0"},
		{"oauth_returning", "remembered browsers removed", "DELETE FROM oauth_returning WHERE account IN " + eraseOwn},
		{"key_backups", "removed", "DELETE FROM key_backups WHERE account IN " + eraseOwn},
		{"device_requests", "labels and approvals blanked", "UPDATE device_requests SET label='',approved_command='' WHERE account IN " + eraseOwn + " AND (label<>'' OR approved_command<>'')"},
		{"webhook_subscriptions", "removed with their deliveries and push keys", "DELETE FROM webhook_subscriptions WHERE account IN " + eraseOwn},
		{"memory_items", "removed", "DELETE FROM memory_items WHERE account IN " + eraseOwn},
		{"memory_usage", "removed", "DELETE FROM memory_usage WHERE account IN " + eraseOwn},
		{"docs", "own docs deleted, titles blanked", "UPDATE docs SET title='',state='deleted',deleted_at=CASE WHEN deleted_at=0 THEN ?2 ELSE deleted_at END WHERE account IN " + eraseOwn + " AND room='' AND (title<>'' OR state<>'deleted')"},
		{"doc_versions", "text blanked (own docs, and own versions in group docs)", "UPDATE doc_versions SET text='',verdict='',title=CASE WHEN doc IN (SELECT id FROM docs WHERE account IN " + eraseOwn + " AND room='') THEN '' ELSE title END WHERE (doc IN (SELECT id FROM docs WHERE account IN " + eraseOwn + " AND room='') OR author IN " + eraseOwn + ") AND (text<>'' OR verdict<>'')"},
		{"blobs", "file bytes and names erased", "UPDATE blobs SET deleted=1,data=NULL,filename='' WHERE account IN " + eraseOwn + " AND (deleted=0 OR data IS NOT NULL OR filename<>'')"},
		{"receivers", "deleted, labels and secrets blanked", "UPDATE receivers SET state=CASE WHEN state='active' THEN 'deleted' ELSE state END,finished_at=CASE WHEN state='active' THEN ?2 ELSE finished_at END,label='',allow_from='',hmac_secret='' WHERE account IN " + eraseOwn + " AND (state='active' OR label<>'' OR allow_from<>'' OR hmac_secret<>'')"},
		{"receiver_items", "removed", "DELETE FROM receiver_items WHERE account IN " + eraseOwn},
		{"wakeups", "cancelled", "UPDATE wakeups SET state='cancelled',finished_at=?2 WHERE account IN " + eraseOwn + " AND state='active'"},
		{"inbox_entries", "removed", "DELETE FROM inbox_entries WHERE account IN " + eraseOwn},
		{"messaging_settings", "removed", "DELETE FROM messaging_settings WHERE account IN " + eraseOwn},
		{"contact_blocks", "removed", "DELETE FROM contact_blocks WHERE account IN " + eraseOwn},
		{"seal_wraps", "own sealed-conversation keys removed", "DELETE FROM seal_wraps WHERE account IN " + eraseOwn},
		{"conversation_members", "left (other members' messages stay)", "UPDATE conversation_members SET state='left',changed_at=?2 WHERE account IN " + eraseOwn + " AND state IN ('active','requested')"},
		{"works", "titles of work it posted blanked (states and amounts kept)", "UPDATE works SET title='' WHERE requester IN " + eraseOwn + " AND title<>''"},
		// Offerings (C1051): retired and unlisted; the listing and pay_to go,
		// the signed revisions lose their bytes. Calls keep their own pay_to
		// (the payer's record of a paid call).
		{"offerings", "retired, listing and pay_to blanked (calls keep their own)", "UPDATE offerings SET listing='{}',pay_to='',updated_at=CASE WHEN state='active' THEN ?2 ELSE updated_at END,state='retired' WHERE account IN " + eraseOwn + " AND (state='active' OR listing<>'{}' OR pay_to<>'')"},
		{"offering_revisions", "listing, signature and signed payload blanked", "UPDATE offering_revisions SET listing='{}',signature='',signed_payload='' WHERE account IN " + eraseOwn + " AND (listing<>'{}' OR signature<>'' OR signed_payload<>'')"},
		{"verifications", "hidden as erased by owner, signed record blanked", "UPDATE verifications SET hidden_reason=CASE WHEN hidden_at=0 THEN '" + ErasedReason + "' ELSE hidden_reason END,hidden_at=CASE WHEN hidden_at=0 THEN ?2 ELSE hidden_at END,signature='',signed_payload='' WHERE account IN " + eraseOwn + " AND (hidden_at=0 OR signature<>'' OR signed_payload<>'')"},
		{"reviews", "review text blanked (verdicts kept as weights)", "UPDATE reviews SET review='' WHERE account IN " + eraseOwn + " AND review<>''"},
		{"peer_capabilities", "removed with the profile", "DELETE FROM peer_capabilities WHERE account IN " + eraseOwn},
		{"mcp_event_subscriptions", "removed with their deliveries", "DELETE FROM mcp_event_subscriptions WHERE account IN " + eraseOwn},
		{"runs_log", "inputs, outputs and errors blanked (hashes kept)", "UPDATE runs_log SET input='',output='',error='',egress='' WHERE account IN " + eraseOwn + " AND (input<>'' OR output<>'' OR error<>'' OR egress<>'')"},
		// C1134: runs_code holds a run's source by its hash, with no account
		// column; the account's runs reach it. The source goes unless an
		// account not erased ran the same code; hash and language stay, as
		// the runs' records and receipts name them.
		{"runs_code", "source code of its runs blanked (code another account also ran stays)", "UPDATE runs_code SET code='' WHERE code<>'' AND sha256 IN (SELECT code_sha256 FROM runs_log WHERE account IN " + eraseOwn + ") AND sha256 NOT IN (SELECT code_sha256 FROM runs_log WHERE account NOT IN " + eraseOwn + " AND account NOT IN " + eraseErased + ")"},
		{"service_jobs", "finished jobs' data blanked", "UPDATE service_jobs SET data='' WHERE account IN " + eraseOwn + " AND state IN ('done','failed','cancelled') AND data<>''"},
		{"moderation_jobs", "screened copies blanked (decisions kept)", "UPDATE moderation_jobs SET content='' WHERE agent IN " + eraseOwn + " AND state IN ('done','failed') AND content<>''"},
		// C1124: the stored answers retries return, and the values the
		// account supplied in its audit rows.
		{"requests", "stored write answers removed", "DELETE FROM requests WHERE actor IN " + eraseRequestOwn},
		{"audit", "supplied values redacted (operations, keys and times kept)", "UPDATE audit SET detail='" + ErasedDetail + "' WHERE operation IN (" + eraseAuditOps + ") AND (actor IN " + eraseOwn + " OR target IN " + eraseOwn + ") AND detail<>'" + ErasedDetail + "'"},
	}
	for _, st := range steps {
		var args []any
		if strings.Contains(st.query, "?2") {
			args = append(args, now)
		}
		if err = exec(st.store, st.action, st.query, args...); err != nil {
			return nil, err
		}
	}
	// The old pastes table mirrors pastes, where a database still has it.
	var pastes int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='pastes'").Scan(&pastes); err != nil {
		return nil, err
	}
	if pastes > 0 {
		if err = exec("pastes", "text erased", "UPDATE pastes SET text='',title='',verdict='',state='deleted',deleted_at=CASE WHEN deleted_at=0 THEN ?2 ELSE deleted_at END WHERE account IN "+eraseOwn+" AND (text<>'' OR title<>'' OR state<>'deleted')", now); err != nil {
			return nil, err
		}
	}
	// Tool history (C1003): stored results and public records cleared,
	// notary receipts unnamed, cites ended.
	var calls int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM service_calls WHERE account=?1 AND (body<>'' OR public<>'{}')", account).Scan(&calls); err != nil {
		return nil, err
	}
	if err = EraseToolHistory(ctx, tx, account); err != nil {
		return nil, err
	}
	counts = append(counts, EraseCount{"service_calls", "tool results and public records cleared", calls})
	n, err := eraseFeedbackText(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	counts = append(counts, EraseCount{"feedback", "text blanked", n})
	// Standing proofs (C1059) stay, so one outside account never makes a
	// second identity (login.go), but only as keyed hashes: value and root
	// become erasedMAC, the detail goes.
	if err = s.eraseAssessmentValues(ctx, tx, account); err != nil {
		return nil, err
	}
	// Write signals (90-day store, signals.go): erased, except those that
	// are evidence for an open report or an active sanction.
	ws, err := eraseWriteSignals(ctx, tx, account, keep)
	if err != nil {
		return nil, err
	}
	counts = append(counts, ws)
	return counts, nil
}

// erasedHashPrefix starts a standing_assessments value or root an erasure
// replaced by its keyed hash.
const erasedHashPrefix = "hmac:"

// erasedMAC is v as an erasure keeps it: its keyed hash under the signals
// key (signalMAC), named by the key's id, so a row kept under another key is
// never compared with the current one.
func (s *Store) erasedMAC(v string) string {
	return erasedHashPrefix + s.signals.id + ":" + s.signalMAC("erased:"+v)
}

// eraseAssessmentValues replaces each raw link value and root in the
// account's standing_assessments rows with its keyed hash and blanks the
// detail. A root lookup (loginOwner) hashes what it looks for the same way,
// so an erased outside account still matches; the plain value is gone.
func (s *Store) eraseAssessmentValues(ctx context.Context, tx *sql.Tx, account string) error {
	rows, err := tx.QueryContext(ctx, "SELECT agent,kind,value,root FROM standing_assessments WHERE agent IN "+eraseOwn+" AND (value NOT LIKE '"+erasedHashPrefix+"%' OR detail<>'')", account)
	if err != nil {
		return err
	}
	type assessment struct{ agent, kind, value, root string }
	var all []assessment
	for rows.Next() {
		var a assessment
		if err = rows.Scan(&a.agent, &a.kind, &a.value, &a.root); err != nil {
			rows.Close()
			return err
		}
		all = append(all, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range all {
		value, root := a.value, a.root
		if !strings.HasPrefix(value, erasedHashPrefix) {
			value = s.erasedMAC(a.kind + "\x00" + a.value)
		}
		if root != "" && !strings.HasPrefix(root, erasedHashPrefix) {
			root = s.erasedMAC(root)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE standing_assessments SET value=?,root=?,detail='' WHERE agent=? AND kind=? AND value=?", value, root, a.agent, a.kind, a.value); err != nil {
			return fmt.Errorf("erase standing_assessments: %w", err)
		}
	}
	return nil
}

// numberedParams is n numbered SQL parameters starting at ?from.
func numberedParams(n, from int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf("?%d", from+i)
	}
	return strings.Join(p, ",")
}

// EraseEvidence is what an erasure keeps as evidence: write records of the
// objects under an open report or an active sanction, or all of them while
// the account itself is sanctioned.
type EraseEvidence struct {
	Objects    []string
	Sanctioned bool
}

func eraseEvidence(ctx context.Context, tx *sql.Tx, account string, now int64) (EraseEvidence, error) {
	var ev EraseEvidence
	var err error
	if ev.Sanctioned, err = eraseSanctioned(ctx, tx, account, now); err != nil {
		return ev, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id FROM reports WHERE resolved=0 AND event_id IN (SELECT id FROM events WHERE account IN `+eraseOwn+`)
 UNION SELECT message_id FROM sanctions WHERE message_id<>'' AND lifted_at=0 AND ends_at>?2 AND message_id IN (SELECT id FROM events WHERE account IN `+eraseOwn+`)`, account, now)
	if err != nil {
		return ev, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return ev, err
		}
		ev.Objects = append(ev.Objects, id)
	}
	sort.Strings(ev.Objects)
	return ev, rows.Err()
}

// OperatorEraseAccount is `swarmmemo erase account FP`: with confirm false a
// dry run (the same transaction, rolled back, so its counts are exact), with
// confirm true the erasure, then the raw write records. It refuses a key that
// is not an account's own fingerprint and an account with open obligations.
func (s *Store) OperatorEraseAccount(ctx context.Context, account, reference string, confirm bool) (ErasureReceipt, error) {
	r := ErasureReceipt{Schema: 1, Account: account, Reference: reference, Kept: ErasureKept, Backups: ErasureBackupsNote, DryRun: !confirm}
	if !fingerprintRE.MatchString(account) {
		return r, errAgentFingerprint()
	}
	if reference != "" && !eraseRefRE.MatchString(reference) {
		return r, errors.New("--reason is a request reference (letters, digits, '.', '_' or '-', up to 64), never personal data")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	root, err := lookupAccount(ctx, tx, account)
	if err != nil {
		return r, err
	}
	if root != account {
		return r, fmt.Errorf("%s is a key of account %s: erase the account by its own fingerprint", account, root)
	}
	blocking, err := EraseObligations(ctx, tx, account)
	if err != nil {
		return r, err
	}
	if len(blocking) > 0 {
		return r, fmt.Errorf("erase refused: the account has %s; settle or cancel them first", strings.Join(blocking, ", "))
	}
	run, err := s.eraseRun(ctx, tx, r, s.now().Unix())
	if err != nil {
		return run.receipt, err
	}
	if !confirm {
		return run.receipt, nil // the deferred rollback undoes the dry run
	}
	if err = s.recordErasure(ctx, tx, &run, "operator", "on the owner's request"); err != nil {
		return run.receipt, err
	}
	if err = tx.Commit(); err != nil {
		return run.receipt, err
	}
	return run.receipt, s.afterErasure(ctx, run)
}

// erasureRun is one erasure in its transaction: the receipt, and what the
// steps after the commit need.
type erasureRun struct {
	receipt ErasureReceipt
	ids     []string
	keep    EraseEvidence
	digests []string
}

// eraseRun erases r.Account in tx at now (EraseAccount) and fills r's
// counts, raw-record counts and hash. The caller has checked the account and
// its obligations; it commits, or rolls back for a dry run or a preview.
func (s *Store) eraseRun(ctx context.Context, tx *sql.Tx, r ErasureReceipt, now int64) (erasureRun, error) {
	run := erasureRun{receipt: r}
	run.receipt.ErasedAt = now
	var err error
	if run.keep, err = eraseEvidence(ctx, tx, r.Account, now); err != nil {
		return run, err
	}
	if run.digests, err = eraseLinkDigests(ctx, tx, r.Account); err != nil {
		return run, err
	}
	if run.receipt.Counts, err = s.EraseAccount(ctx, tx, r.Account, now, run.keep); err != nil {
		return run, err
	}
	// The raw records are in their own database (another file and
	// connection): counted here, erased after the commit.
	if run.ids, err = eraseIDs(ctx, tx, r.Account); err != nil {
		return run, err
	}
	if run.receipt.RawErased, run.receipt.RawHeld, err = s.rawEraseCounts(ctx, run.ids, run.keep); err != nil {
		return run, err
	}
	receipt, _ := json.Marshal(run.receipt)
	run.receipt.ReceiptHash = sha256Hex(receipt)
	return run, nil
}

// recordErasure writes run's erasure_log row and its audit row, which the
// transparency log takes as an account.erase leaf carrying the receipt's
// hash and nothing else (transparency.go), and signs the receipt with the
// log key. by is the audit actor, how says who asked.
func (s *Store) recordErasure(ctx context.Context, tx *sql.Tx, run *erasureRun, by, how string) error {
	r := &run.receipt
	linkJSON, _ := json.Marshal(run.digests)
	countJSON, _ := json.Marshal(r.Counts)
	if _, err := tx.ExecContext(ctx, "INSERT INTO erasure_log(account,erased_at,reference,counts,link_digests,raw_held,receipt_sha256) VALUES(?,?,?,?,?,?,?)",
		r.Account, r.ErasedAt, r.Reference, string(countJSON), string(linkJSON), r.RawHeld, r.ReceiptHash); err != nil {
		return err
	}
	if err := audit(ctx, tx, "account.erase", by, r.Account, "erased "+how+"; "+erasureReceiptDetail+r.ReceiptHash, r.ErasedAt); err != nil {
		return err
	}
	signed, err := s.signErasure(*r)
	r.Signed = signed
	return err
}

// erasureReceiptDetail precedes the receipt hash in an account.erase audit
// row; the log leaf reads the hash after it.
const erasureReceiptDetail = "receipt "

// ErasureReceiptNote is the first line of a signed erasure receipt, after
// the log's origin.
const ErasureReceiptNote = "erasure receipt"

// signErasure is the receipt's signed statement: a C2SP note by the log key
// (/api/log/keys) naming the account, the time and the receipt's SHA-256.
func (s *Store) signErasure(r ErasureReceipt) (string, error) {
	return s.transparency.signer.Sign(fmt.Sprintf("%s %s\naccount %s\nerased_at %d\nreceipt_sha256 %s\n", s.transparency.origin, ErasureReceiptNote, r.Account, r.ErasedAt, r.ReceiptHash))
}

// afterErasure is what follows an erasure's commit: the raw write records
// (their own database) and the side copy of erasure_log a restore replays.
func (s *Store) afterErasure(ctx context.Context, run erasureRun) error {
	side := s.appendErasureSide(erasureSideFromRun(run))
	if err := s.rawErase(ctx, run.ids, run.keep); err != nil {
		return fmt.Errorf("the account is erased, but its raw write records are not (run the erase again): %w", err)
	}
	if side != nil {
		return fmt.Errorf("the account is erased, but its side copy of erasure_log is not written (the next start writes it): %w", side)
	}
	return nil
}

// ErasureLogEntry is one erasure_log row.
type ErasureLogEntry struct {
	Account       string `json:"account"`
	ErasedAt      int64  `json:"erased_at"`
	Reference     string `json:"reference,omitempty"`
	RawHeld       int64  `json:"raw_signals_held_as_evidence"`
	ReceiptSHA256 string `json:"receipt_sha256"`
}

// ErasureLog lists the erasures, oldest first: the list a restore replays.
func (s *Store) ErasureLog(ctx context.Context) ([]ErasureLogEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT account,erased_at,reference,raw_held,receipt_sha256 FROM erasure_log ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ErasureLogEntry
	for rows.Next() {
		var e ErasureLogEntry
		if err = rows.Scan(&e.Account, &e.ErasedAt, &e.Reference, &e.RawHeld, &e.ReceiptSHA256); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// eraseIDs is the account's identity ids and the account.
func eraseIDs(ctx context.Context, tx *sql.Tx, account string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM identities WHERE account=?1 UNION SELECT ?1 ORDER BY 1", account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// eraseLinkDigests is the SHA-256 of each identity link's kind and value:
// a "linked once" marker that holds no value.
func eraseLinkDigests(ctx context.Context, tx *sql.Tx, account string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT kind,value FROM identity_links WHERE agent IN "+eraseOwn+" ORDER BY kind,value", account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var kind, value string
		if err = rows.Scan(&kind, &value); err != nil {
			return nil, err
		}
		out = append(out, sha256Hex([]byte(kind+"\x00"+value)))
	}
	return out, rows.Err()
}

// rawEraseWhere selects an account's raw records outside the evidence hold.
func rawEraseWhere(ids []string, keep EraseEvidence) (string, []any) {
	args := make([]any, 0, len(ids)+len(keep.Objects))
	for _, id := range ids {
		args = append(args, id)
	}
	where := "account IN (" + numberedParams(len(ids), 1) + ")"
	if len(keep.Objects) > 0 {
		where += " AND object_id NOT IN (" + numberedParams(len(keep.Objects), len(ids)+1) + ")"
		for _, o := range keep.Objects {
			args = append(args, o)
		}
	}
	return where, args
}

// rawEraseCounts counts the raw records an erasure removes and holds.
func (s *Store) rawEraseCounts(ctx context.Context, ids []string, keep EraseEvidence) (erase, held int64, err error) {
	if s.rawSignals.db == nil {
		return 0, 0, nil
	}
	all, args := rawEraseWhere(ids, EraseEvidence{})
	var total int64
	if err = s.rawSignals.db.QueryRowContext(ctx, "SELECT count(*) FROM raw_signals WHERE "+all, args...).Scan(&total); err != nil {
		return 0, 0, err
	}
	if keep.Sanctioned {
		return 0, total, nil
	}
	where, args := rawEraseWhere(ids, keep)
	if err = s.rawSignals.db.QueryRowContext(ctx, "SELECT count(*) FROM raw_signals WHERE "+where, args...).Scan(&erase); err != nil {
		return 0, 0, err
	}
	return erase, total - erase, nil
}

// rawErase deletes the account's raw records outside the evidence hold.
func (s *Store) rawErase(ctx context.Context, ids []string, keep EraseEvidence) error {
	if s.rawSignals.db == nil || keep.Sanctioned {
		return nil
	}
	where, args := rawEraseWhere(ids, keep)
	_, err := s.rawSignals.db.ExecContext(ctx, "DELETE FROM raw_signals WHERE "+where, args...)
	return err
}

// ErasureTime formats an erasure time for the operator.
func ErasureTime(at int64) string { return time.Unix(at, 0).UTC().Format(time.RFC3339) }
