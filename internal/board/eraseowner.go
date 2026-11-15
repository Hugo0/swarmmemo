package board

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"swarmmemo/internal/services"
)

// account.erase (C1012, build 2): the owner erases their own account in two
// signed steps. "preview" runs the erasure in the command's transaction and
// rolls it back (the operator's dry run): its counts per store, what stays
// and why, what still blocks it, and a hold token bound to the account and
// those counts. "confirm" with the token and the typed text "erase " + the
// first 8 characters of the fingerprint erases (board.EraseAccount), records
// erasure_log and an account.erase log leaf carrying the receipt's hash, and
// returns the receipt signed by the log key. A hosted identity also gives its
// recovery code, as hosted.claim does; a device, a worker key or an app's
// token is refused. When the account's key changed in the last 7 days the
// token opens only after 24 hours (a stolen key cannot erase at once).

// Account erase hold windows, in seconds.
const (
	// EraseHoldSeconds is how long a preview's hold token confirms.
	EraseHoldSeconds = 15 * 60
	// EraseRecentKeySeconds is how recent a key change delays an erasure.
	EraseRecentKeySeconds = 7 * 24 * 3600
	// EraseDelaySeconds is that delay; the token then confirms for as long.
	EraseDelaySeconds = 24 * 3600
	// EraseConfirmChars is how much of the fingerprint the confirm text types.
	EraseConfirmChars = 8
)

// eraseData is account.erase's data.
type eraseData struct {
	Schema       int    `json:"schema"`
	Step         string `json:"step"`
	Hold         string `json:"hold,omitempty"`
	Confirm      string `json:"confirm,omitempty"`
	RecoveryCode string `json:"recovery_code,omitempty"`
}

const eraseDataShape = `{"schema":1,"step":"preview"} or {"schema":1,"step":"confirm","hold":TOKEN,"confirm":"erase FINGERPRINT8","recovery_code"?}`

func invalidEraseData(detail string) error {
	return problem(400, "invalid_erase_data", "account.erase data is "+eraseDataShape+detail)
}

func parseEraseData(raw string) (eraseData, error) {
	var d eraseData
	if services.StrictObject([]byte(raw), &d) != nil || d.Schema != 1 {
		return d, invalidEraseData(".")
	}
	if d.Step != "preview" && d.Step != "confirm" {
		return d, problem(400, "invalid_erase_data", `account.erase step is "preview" or "confirm".`)
	}
	if d.Step == "preview" && (d.Hold != "" || d.Confirm != "" || d.RecoveryCode != "") {
		return d, invalidEraseData("; a preview takes only the step.")
	}
	if d.Step == "confirm" && (d.Hold == "" || d.Confirm == "") {
		return d, invalidEraseData("; a confirm takes the preview's hold and the typed confirm text.")
	}
	return d, nil
}

// EraseConfirmText is the text an owner types to confirm an erasure.
func EraseConfirmText(account string) string { return "erase " + account[:EraseConfirmChars] }

// eraseHold is the stateless hold token: "NOTBEFORE.EXPIRES.MAC", an HMAC
// over the account, the SHA-256 of the preview's counts and the window.
func (s *Store) eraseHold(account, counts string, notBefore, expires int64) string {
	mac := hmac.New(sha256.New, s.eraseHoldKey)
	mac.Write([]byte("swarmmemo-erase/1\x00" + account + "\x00" + counts + "\x00" + strconv.FormatInt(notBefore, 10) + "\x00" + strconv.FormatInt(expires, 10)))
	return strconv.FormatInt(notBefore, 10) + "." + strconv.FormatInt(expires, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// eraseHoldWindow parses a token's window; ok is false when it is malformed.
func eraseHoldWindow(hold string) (notBefore, expires int64, ok bool) {
	parts := strings.Split(hold, ".")
	if len(parts) != 3 {
		return 0, 0, false
	}
	nb, err1 := strconv.ParseInt(parts[0], 10, 64)
	ex, err2 := strconv.ParseInt(parts[1], 10, 64)
	return nb, ex, err1 == nil && err2 == nil && ex > nb && ex-nb <= EraseDelaySeconds
}

// eraseCountsDigest is what a hold binds: the counts per store, without the
// write records, which the erase commands themselves never add to but other
// writes in between may.
func eraseCountsDigest(counts []EraseCount) string {
	var kept []EraseCount
	for _, c := range counts {
		if !eraseSignalCount(c) {
			kept = append(kept, c)
		}
	}
	b, _ := json.Marshal(kept)
	return sha256Hex(b)
}

func eraseHoldInvalid() error {
	return problem(409, "erase_hold_invalid", "This hold token is not valid for this account now: it expired, belongs to another account, or the account changed since the preview. Run account.erase with step preview again.")
}

// eraseKeyChanged is when the account's key last changed: a rotation, a
// claim or a linked device; 0 when never.
func eraseKeyChanged(ctx context.Context, tx *sql.Tx, account string) (int64, error) {
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(au.created_at),0) FROM audit au WHERE au.operation IN ('agent.rotate','hosted.claim','device.add')
 AND (au.actor IN `+eraseOwn+` OR au.target IN `+eraseOwn+`)`, account).Scan(&at)
	return at, err
}

// eraseOwnAccount is account.erase.
func (s *Store) eraseOwnAccount(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if !a.signed {
		return Result{}, problem(401, "signature_required", "account.erase is your own account: sign it with your agent's key, or call it as your hosted identity.")
	}
	if a.grant != nil || a.bearer != nil {
		return Result{}, delegationError("delegation_scope_mismatch")
	}
	d, err := parseEraseData(c.Data)
	if err != nil {
		return Result{}, err
	}
	account := a.account
	if app, err := houseApp(ctx, tx, account); err != nil || app != "" {
		if err != nil {
			return Result{}, err
		}
		return Result{}, problem(403, "owner_required", "A house agent is its app's: the app's owner retires it, and it is not erased by its own key.")
	}
	var recovery string
	if err = tx.QueryRowContext(ctx, "SELECT recovery_sha256 FROM hosted_keys WHERE account=? AND state='active'", account).Scan(&recovery); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	hosted := err == nil
	blocking, err := EraseObligations(ctx, tx, account)
	if err != nil {
		return Result{}, err
	}
	if d.Step == "confirm" {
		if subtle.ConstantTimeCompare([]byte(d.Confirm), []byte(EraseConfirmText(account))) != 1 {
			return Result{}, problem(400, "erase_confirm_mismatch", fmt.Sprintf("Type the confirm text exactly: %q.", EraseConfirmText(account)))
		}
		if hosted && (!hostedSecretRE.MatchString(d.RecoveryCode) || !strings.HasPrefix(d.RecoveryCode, HostedRecoveryPrefix) ||
			subtle.ConstantTimeCompare([]byte(hostedHash(d.RecoveryCode)), []byte(recovery)) != 1) {
			return Result{}, recoveryInvalid()
		}
		notBefore, expires, ok := eraseHoldWindow(d.Hold)
		if !ok || now >= expires {
			return Result{}, eraseHoldInvalid()
		}
		if now < notBefore {
			return Result{}, problem(409, "erase_waiting", fmt.Sprintf("This account's key changed in the last 7 days, so its erasure opens 24 hours after the preview: confirm from %s.", ErasureTime(notBefore)))
		}
	}
	if len(blocking) > 0 {
		return Result{}, problem(409, "erase_pending_obligations", "This account cannot be erased yet: it has "+strings.Join(blocking, ", ")+". Finish, settle or cancel them, then preview again; settled payouts and fees stay as numbers.")
	}
	r := ErasureReceipt{Schema: 1, Account: account, Kept: ErasureKept, Backups: ErasureBackupsNote}
	// The erasure runs in a savepoint: a preview, or a confirm whose counts
	// changed, rolls it back and the command's transaction goes on.
	if _, err = tx.ExecContext(ctx, "SAVEPOINT account_erase"); err != nil {
		return Result{}, err
	}
	release := func(undo bool) error {
		if undo {
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO account_erase"); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "RELEASE account_erase")
		return err
	}
	run, err := s.eraseRun(ctx, tx, r, now)
	if err != nil {
		_ = release(true)
		return Result{}, err
	}
	digest := eraseCountsDigest(run.receipt.Counts)
	if d.Step == "preview" {
		if err = release(true); err != nil {
			return Result{}, err
		}
		changed, err := eraseKeyChanged(ctx, tx, account)
		if err != nil {
			return Result{}, err
		}
		notBefore, expires := now, now+EraseHoldSeconds
		if changed > now-EraseRecentKeySeconds {
			notBefore, expires = now+EraseDelaySeconds, now+2*EraseDelaySeconds
		}
		data := map[string]any{
			"step": "preview", "account": account, "counts": run.receipt.Counts,
			"raw_signals_erased": run.receipt.RawErased, "raw_signals_held_as_evidence": run.receipt.RawHeld,
			"kept": ErasureKept, "backups": ErasureBackupsNote,
			"hold": s.eraseHold(account, digest, notBefore, expires), "confirm_after": notBefore, "hold_expires": expires,
			"confirm_text": EraseConfirmText(account), "recovery_code_required": hosted,
			"notice": "Nothing is erased yet. To erase, send account.erase with step confirm, this hold and the confirm text. Erasure cannot be undone; a new key starts a new account.",
		}
		return Result{transient: true, Data: data}, nil
	}
	notBefore, expires, _ := eraseHoldWindow(d.Hold)
	if subtle.ConstantTimeCompare([]byte(d.Hold), []byte(s.eraseHold(account, digest, notBefore, expires))) != 1 {
		_ = release(true)
		return Result{}, eraseHoldInvalid()
	}
	if err = s.recordErasure(ctx, tx, &run, a.id, "by its owner"); err != nil {
		_ = release(true)
		return Result{}, err
	}
	if err = release(false); err != nil {
		return Result{}, err
	}
	receipt := run.receipt
	data := map[string]any{
		"step": "confirm", "account": account, "erased_at": receipt.ErasedAt, "receipt": receipt, "receipt_sha256": receipt.ReceiptHash,
		"signed_receipt": receipt.Signed, "verifier_key": s.LogVerifierKey(),
		"notice": "Erased. Keep this receipt: its SHA-256 is an account.erase leaf in the transparency log (/api/log), signed by the log key. This key signs nothing more.",
	}
	result := Result{transient: true, Data: data}
	result.afterCommit = func() (Result, error) {
		if err := s.afterErasure(context.WithoutCancel(ctx), run); err != nil {
			result.Data["warning"] = err.Error()
		}
		return Result{Data: result.Data}, nil
	}
	return result, nil
}
