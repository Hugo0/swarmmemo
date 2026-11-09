package board

// Verified by SwarmMemo (C353): a verification is one key's signed record
// that it ran a command against a subject (a repository at a full commit,
// or a file by its SHA-256) and what each check showed. The verifier's own
// verify.publish command is the record, kept verbatim like a link witness
// (identitywitness.go): signed_payload is the canonical command bytes and
// signature the verifier's Ed25519 signature over them, so any reader checks
// it offline against the verifier's public key. Its checks are the verdict
// checks of C97 (checks.go). The record's SHA-256 (of signed_payload) is
// stamped by the notary (services.StampHash) where the notary runs, which
// puts it on the Bitcoin-anchored transparency log. Trust in a record is
// trust in its verifier: the record names the key, and its standing is the
// one trust run's.
//
// Records are public, immutable and never deleted; the moderation screen
// (moderationwire.go) may hide one, which keeps the row and says why. A
// verifier corrects a record by publishing a new one whose signed data
// names it in supersedes (C371): the old record stays, marked superseded_by
// the new one, and a subject's badge follows the newest record not
// superseded.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/b64"
	"swarmmemo/internal/moderation"
	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

const verificationSchema = `
CREATE TABLE IF NOT EXISTS verifications (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 verifier TEXT NOT NULL REFERENCES identities(id), account TEXT NOT NULL,
 repo TEXT NOT NULL DEFAULT '', commit_sha TEXT NOT NULL DEFAULT '', file_sha256 TEXT NOT NULL DEFAULT '',
 passed INTEGER NOT NULL, failed INTEGER NOT NULL,
 record_sha256 TEXT NOT NULL, signature TEXT NOT NULL, signed_payload TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 hidden_at INTEGER NOT NULL DEFAULT 0, hidden_reason TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS verifications_repo ON verifications(repo,commit_sha,seq);
CREATE INDEX IF NOT EXISTS verifications_file ON verifications(file_sha256,seq);
CREATE INDEX IF NOT EXISTS verifications_by ON verifications(account,created_at);
`

// migrateVerificationSupersedes43 adds verifications.supersedes (schema 43,
// C371): the id of the verifier's own earlier record this one replaces,
// copied from its signed data, empty for none; and its index, so a record's
// superseded_by is one lookup.
func migrateVerificationSupersedes43(tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('verifications') WHERE name='supersedes'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := tx.Exec("ALTER TABLE verifications ADD COLUMN supersedes TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	_, err := tx.Exec("CREATE INDEX IF NOT EXISTS verifications_supersedes ON verifications(supersedes) WHERE supersedes<>''")
	return err
}

// migrateVerificationFlaky44 adds verifications.flaky (schema 44, C385):
// the count of flaky checks, beside passed and failed.
func migrateVerificationFlaky44(tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('verifications') WHERE name='flaky'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if _, err := tx.Exec("ALTER TABLE verifications ADD COLUMN flaky INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	return nil
}

const (
	// VerificationsPerDay bounds verify.publish per account per UTC day.
	VerificationsPerDay = 50
	// VerificationDataBytes bounds verify.publish data: 16 KiB of fields
	// and room for output, VerificationOutputBytes in base64url.
	VerificationDataBytes = 48 << 10
	// VerificationOutputBytes bounds output (C591), the exact bytes
	// output_sha256 hashes; a larger output is published hash-only.
	VerificationOutputBytes = 24 << 10
	// VerificationStdioEncodingMax bounds stdio_encoding, one line, in
	// bytes.
	VerificationStdioEncodingMax = 64
	// VerificationCommandMax, VerificationEnvironmentMax and
	// VerificationLimitsMax bound those fields, in characters; each may
	// span lines.
	VerificationCommandMax     = 1000
	VerificationEnvironmentMax = 1000
	VerificationLimitsMax      = 2000
	// VerificationOutputNormaliserMax bounds output_normaliser, one line,
	// in bytes.
	VerificationOutputNormaliserMax = 512
	// VerificationRepoBytes bounds the repository URL, VerificationFileMax
	// the file path, in characters.
	VerificationRepoBytes = 300
	VerificationFileMax   = 200
	// VerificationsPageMax bounds one verify.list page, and
	// VerificationsPageDefault is its size when limit is left out.
	VerificationsPageMax     = 50
	VerificationsPageDefault = 20
	// VerificationPath is where a record's page is; its badge is
	// VerificationPath+ID+"/badge.svg".
	VerificationPath = "/v/"
	// VerificationOutputSuffix follows VerificationPath+ID where a record's
	// exact output bytes are served raw (C591).
	VerificationOutputSuffix = "/output"
)

// cursorVerification is verify.list's cursor kind (cursorFor).
const cursorVerification byte = 'V'

var (
	verificationIDRE = regexp.MustCompile(`^[0-9a-f]{16}$`)
	commitRE         = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// ValidVerificationID reports whether id is shaped like a verification's:
// the first 16 hex characters of its record's SHA-256.
func ValidVerificationID(id string) bool { return verificationIDRE.MatchString(id) }

// VerificationOutputHash is the one instruction for output_sha256 (C437):
// the rule's, the MCP tool's and the /v page's re-run block's.
const VerificationOutputHash = `Hash the output after removing timings and machine-specific paths, and put that normaliser in command so others apply the same one, e.g. python3 -m unittest 2>&1 | sed -E 's/ in [0-9.]+s$//'`

// VerificationRule is the whole data rule, the tail of every refusal.
var VerificationRule = fmt.Sprintf(`verify.publish data is a strict JSON object {"schema":1,"repo","commit","file","file_sha256","command","environment","stdio_encoding","output_sha256","output_normaliser","output","checks","limits","supersedes"}, at most %d bytes: the subject is repo (an https URL, at most %d bytes) with commit (the full 40 or 64 lowercase hex), or file_sha256 (64 lowercase hex), or both; file (a path or name, at most %d characters) is optional; command (at most %d characters), environment (what it ran on, at most %d) and limits (what was not tested, at most %d) are text and may span lines; stdio_encoding (optional, one line, at most %d bytes) is the output's encoding, e.g. utf-8; output_sha256 is the SHA-256 of the output you captured (%s); output_normaliser (optional, one line, at most %d bytes) names that normaliser; output (optional) is those exact bytes, canonical unpadded base64url, at most %d bytes decoded, whose SHA-256 must be output_sha256 (a larger output is published hash-only); checks is 1 to %d checks of which at least one is pass, fail or flaky; supersedes (optional) is the id of your own earlier record this one replaces. `,
	VerificationDataBytes, VerificationRepoBytes, VerificationFileMax, VerificationCommandMax, VerificationEnvironmentMax, VerificationLimitsMax,
	VerificationStdioEncodingMax, VerificationOutputHash, VerificationOutputNormaliserMax, VerificationOutputBytes, VerdictChecksMax)

func verificationError(code string) error {
	switch code {
	case "verification_not_found":
		return problem(404, "verification_not_found", "No verification has that id.")
	case "verification_limit":
		return problem(429, "verification_limit", fmt.Sprintf("This agent published its %d verifications for today (UTC); see verifications_per_day in /capabilities.", VerificationsPerDay))
	case "verification_delegated":
		return problem(403, "verification_delegated", "A scoped worker key cannot publish a verification; sign with the agent's own key.")
	case "invalid_verification_id":
		return problem(400, "invalid_verification_id", "A verification id is 16 lowercase hex characters, as in "+VerificationPath+"ID (the record's URL works too).")
	}
	return problem(400, "invalid_verification", VerificationRule+VerdictChecksRule+VerificationChecksRule)
}

// verifyData is one parsed verify.publish data.
type verifyData struct {
	Repo, Commit, File, FileSHA256     string
	Command, Environment, OutputSHA256 string
	OutputNormaliser, StdioEncoding    string
	Limits, Supersedes                 string
	Checks                             []VerdictCheck
	// Output is the exact bytes OutputSHA256 hashes, when the verifier
	// sent them (C591); outputB64 is their base64url as signed.
	Output    []byte
	outputB64 string
}

// passed, failed and flaky count the checks that ran.
func (d verifyData) counts() (passed, failed, flaky int) {
	for _, c := range d.Checks {
		switch c.State {
		case "pass":
			passed++
		case "fail":
			failed++
		case CheckFlaky:
			flaky++
		}
	}
	return passed, failed, flaky
}

// verifyText is a text field: valid UTF-8, 1 to max characters, no
// leading or trailing space, and no control or format characters but a
// line break (and a tab) where lines are allowed.
func verifyText(s string, max int, lines bool) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if lines && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// NormalizeRepoURL is a repository's canonical https URL: lower-case host
// (and path, on a forge that ignores its case), no user, query, fragment,
// trailing slash or .git; ok is false for anything else. The one rule for verify.publish, verify.list and the
// subject badge.
func NormalizeRepoURL(raw string) (string, bool) {
	if raw == "" || len(raw) > VerificationRepoBytes || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	p := strings.TrimSuffix(strings.TrimRight(u.EscapedPath(), "/"), ".git")
	p = strings.TrimRight(p, "/")
	if p == "" || strings.Contains(p, "//") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") || strings.Contains(p, "/./") {
		return "", false
	}
	for _, r := range p + u.Host {
		if r <= ' ' || r > '~' {
			return "", false
		}
	}
	// One authority, one spelling: no trailing root dot and no default
	// port (github.com. and github.com:443 are github.com), as the other
	// host-normalising sites do (conclave-pickle 9b95ba7d).
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if strings.Contains(host, ":") {
		return "", false // an IPv6 literal names no forge
	}
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	if host == "" || strings.HasPrefix(host, ":") {
		return "", false
	}
	if caseFoldingForges[host] {
		// These forges name a repository case-insensitively, in ASCII: one
		// record subject, however a README spells it.
		if strings.Contains(p, "%") {
			return "", false
		}
		p = strings.ToLower(p)
	}
	return "https://" + host + p, true
}

// caseFoldingForges are the hosts whose owner/repo paths ignore case.
var caseFoldingForges = map[string]bool{"github.com": true, "gitlab.com": true, "codeberg.org": true, "bitbucket.org": true}

// CommitURL is where a forge shows commit of repo, or "" for a host whose
// commit pages this board does not know.
func CommitURL(repo, commit string) string {
	if repo == "" || commit == "" {
		return ""
	}
	host := strings.TrimPrefix(repo, "https://")
	host, _, _ = strings.Cut(host, "/")
	switch host {
	case "github.com", "codeberg.org", "bitbucket.org", "gitea.com":
		return repo + "/commit/" + commit
	case "gitlab.com":
		return repo + "/-/commit/" + commit
	}
	return ""
}

// parseVerifyData accepts only the documented object: each field once,
// none other, every bound checked.
func parseVerifyData(raw string) (verifyData, error) {
	var d verifyData
	invalid := verificationError("invalid_verification")
	if len(raw) > VerificationDataBytes {
		return d, problem(400, "invalid_verification", fmt.Sprintf("data is %d bytes, at most %d: output (the exact bytes, base64url) takes at most %d bytes decoded; leave it out to publish a larger output hash-only. ", len(raw), VerificationDataBytes, VerificationOutputBytes)+VerificationRule)
	}
	if !utf8.ValidString(raw) {
		return d, invalid
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return d, invalid
	}
	seen := map[string]bool{}
	schema := 0
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return d, invalid
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil || bytes.Equal(value, []byte("null")) || !dataTakes("verify.publish", name) {
			return d, invalid
		}
		var dest *string
		switch name {
		case "schema":
			err = json.Unmarshal(value, &schema)
		case "repo":
			dest = &d.Repo
		case "commit":
			dest = &d.Commit
		case "file":
			dest = &d.File
		case "file_sha256":
			dest = &d.FileSHA256
		case "command":
			dest = &d.Command
		case "environment":
			dest = &d.Environment
		case "output_sha256":
			dest = &d.OutputSHA256
		case "output_normaliser":
			dest = &d.OutputNormaliser
		case "stdio_encoding":
			dest = &d.StdioEncoding
		case "output":
			dest = &d.outputB64
		case "limits":
			dest = &d.Limits
		case "supersedes":
			dest = &d.Supersedes
		case "checks":
			var why string
			if d.Checks, why = parseVerdictChecks(value, verificationCheckItems); why != "" {
				return d, problem(400, "invalid_verification", why+" "+VerificationRule+VerdictChecksRule+VerificationChecksRule)
			}
		default:
			return d, invalid
		}
		if dest != nil {
			err = json.Unmarshal(value, dest)
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
	if schema != 1 || !seen["command"] || !seen["environment"] || !seen["output_sha256"] || !seen["checks"] || !seen["limits"] {
		return d, invalid
	}
	if seen["repo"] != seen["commit"] || !seen["repo"] && !seen["file_sha256"] {
		return d, invalid
	}
	if seen["repo"] {
		repo, ok := NormalizeRepoURL(d.Repo)
		if !ok || !commitRE.MatchString(d.Commit) {
			return d, invalid
		}
		d.Repo = repo
	}
	// Name the field that failed before the whole rule, so an agent knows what to shorten
	// (Skitter, 10-21: a 1.3 KB command was refused with only the generic text).
	field := func(name, what string) (verifyData, error) {
		return d, problem(400, "invalid_verification", name+": "+what+". "+VerificationRule)
	}
	if seen["file_sha256"] && !fingerprintRE.MatchString(d.FileSHA256) {
		return field("file_sha256", "64 lowercase hex")
	}
	if seen["file"] && !verifyText(d.File, VerificationFileMax, false) {
		return field("file", fmt.Sprintf("one line, at most %d characters", VerificationFileMax))
	}
	if !fingerprintRE.MatchString(d.OutputSHA256) {
		return field("output_sha256", "64 lowercase hex")
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"command", d.Command, VerificationCommandMax}, {"environment", d.Environment, VerificationEnvironmentMax}, {"limits", d.Limits, VerificationLimitsMax}} {
		if !verifyText(f.v, f.max, true) {
			return field(f.name, fmt.Sprintf("text, at most %d characters (this one has %d)", f.max, utf8.RuneCountInString(f.v)))
		}
	}
	if seen["output_normaliser"] && (len(d.OutputNormaliser) > VerificationOutputNormaliserMax || !verifyText(d.OutputNormaliser, VerificationOutputNormaliserMax, false)) {
		return d, invalid
	}
	if seen["supersedes"] && !ValidVerificationID(d.Supersedes) {
		return d, invalid
	}
	if seen["stdio_encoding"] && (len(d.StdioEncoding) > VerificationStdioEncodingMax || !verifyText(d.StdioEncoding, VerificationStdioEncodingMax, false)) {
		return field("stdio_encoding", fmt.Sprintf("one line, at most %d bytes, e.g. utf-8", VerificationStdioEncodingMax))
	}
	if seen["output"] {
		out, err := b64.URL(d.outputB64)
		switch {
		case err != nil:
			return field("output", "the exact output bytes, "+err.Error())
		case len(out) == 0 || len(out) > VerificationOutputBytes:
			return field("output", fmt.Sprintf("1 to %d bytes decoded (this one has %d); leave it out to publish a larger output hash-only", VerificationOutputBytes, len(out)))
		}
		if sum := sha256.Sum256(out); hex.EncodeToString(sum[:]) != d.OutputSHA256 {
			return field("output", fmt.Sprintf("its SHA-256 is %s, not output_sha256; send the exact bytes you hashed", hex.EncodeToString(sum[:])))
		}
		d.Output = out
	}
	if passed, failed, flaky := d.counts(); passed+failed+flaky == 0 {
		return d, problem(400, "invalid_verification", "At least one check must have run: pass, fail or flaky. "+VerificationRule)
	}
	return d, nil
}

// verificationFromPayload reads a stored record's data back out of its
// signed command bytes; it was validated when the command was accepted.
func verificationFromPayload(payload string) (verifyData, bool) {
	var envelope struct {
		Command struct {
			Data string `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return verifyData{}, false
	}
	d, err := parseVerifyData(envelope.Command.Data)
	return d, err == nil
}

// Verification is one record as published.
type Verification struct {
	ID string `json:"id"`
	// URL is the record's page, Badge its badge (BadgeMarkdown links one
	// to the other).
	URL   string `json:"url"`
	Badge string `json:"badge"`
	// Verifier names the key that signed it, with its standing in the
	// latest trust run where trust runs.
	Verifier VerificationVerifier `json:"verifier"`
	// The subject: Repo at Commit (CommitURL where the forge is known),
	// and/or a file by FileSHA256.
	Repo         string `json:"repo,omitempty"`
	Commit       string `json:"commit,omitempty"`
	CommitURL    string `json:"commit_url,omitempty"`
	File         string `json:"file,omitempty"`
	FileSHA256   string `json:"file_sha256,omitempty"`
	Command      string `json:"command"`
	Environment  string `json:"environment"`
	OutputSHA256 string `json:"output_sha256"`
	// StdioEncoding is the output's encoding as the verifier declared it
	// (C591), e.g. utf-8.
	StdioEncoding string `json:"stdio_encoding,omitempty"`
	// OutputNormaliser is the filter the output went through before it
	// was hashed, where the verifier named one.
	OutputNormaliser string `json:"output_normaliser,omitempty"`
	// OutputURL is where the exact output bytes OutputSHA256 hashes are
	// served raw, and OutputBytes their size, when the verifier sent them
	// (C591); Output is those bytes, for the page's download.
	OutputURL   string         `json:"output_url,omitempty"`
	OutputBytes int            `json:"output_bytes,omitempty"`
	Output      []byte         `json:"-"`
	Checks      []VerdictCheck `json:"checks"`
	Limits      string         `json:"limits"`
	// Supersedes is the id of the verifier's earlier record this one
	// replaces, from its signed data; SupersededBy the newest visible record
	// that replaces this one. Each is absent when there is none.
	Supersedes   string `json:"supersedes,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// Passed, Failed and Flaky count the checks that ran (pass, fail,
	// flaky); Ran is all three. Result is failed when one failed, else
	// flaky when one was flaky (C385), else passed.
	Passed int    `json:"passed"`
	Failed int    `json:"failed"`
	Flaky  int    `json:"flaky"`
	Ran    int    `json:"ran"`
	Result string `json:"result"`
	At     int64  `json:"at"`
	// RecordSHA256 is the SHA-256 of SignedPayload, the canonical command
	// bytes Signature (by Verifier.PublicKey) covers.
	RecordSHA256  string `json:"record_sha256"`
	Signature     string `json:"signature"`
	SignedPayload string `json:"signed_payload"`
	// Notary is the notary's receipt for RecordSHA256, and Proof where its
	// transparency-log inclusion proof is; absent where the notary is off.
	Notary *services.NotaryReceipt `json:"notary,omitempty"`
	Proof  string                  `json:"proof,omitempty"`
	// TextIsUntrusted: every text field is the verifier's own words.
	TextIsUntrusted bool `json:"text_is_untrusted"`
}

// VerificationVerifier is who signed a record.
type VerificationVerifier struct {
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	Handle      string `json:"handle,omitempty"`
	// StandingCents and Band are the latest trust run's; nil where trust
	// is off.
	StandingCents *int64 `json:"standing_cents,omitempty"`
	Band          string `json:"band,omitempty"`
	Agent         string `json:"agent"`
}

// SubjectLabel is the record's subject in a few words: owner/repo@short,
// or the file.
func (v Verification) SubjectLabel() string {
	if v.Repo != "" {
		host := strings.TrimPrefix(v.Repo, "https://")
		if _, rest, ok := strings.Cut(host, "/"); ok && (strings.HasPrefix(host, "github.com/") || strings.HasPrefix(host, "gitlab.com/") || strings.HasPrefix(host, "codeberg.org/")) {
			host = rest
		}
		return host + "@" + v.Commit[:7]
	}
	if v.File != "" {
		return v.File + " (sha256 " + v.FileSHA256[:12] + ")"
	}
	return "sha256 " + v.FileSHA256[:12]
}

// ShortHash is the subject's short hash for a badge: the commit's first
// seven characters, else the file's.
func (v Verification) ShortHash() string {
	if v.Commit != "" {
		return v.Commit[:7]
	}
	return v.FileSHA256[:7]
}

// verificationRow is one row with its verifier.
type verificationRow struct {
	seq                                                  int64
	id, verifier, account, publicKey, handle             string
	recordSHA256, signature, signedPayload, hiddenReason string
	supersededBy                                         string
	createdAt, hiddenAt                                  int64
}

// VerificationResult is a record's result from its counts: failed when a
// check failed, else flaky when one was flaky, else passed.
func VerificationResult(failed, flaky int) string {
	switch {
	case failed > 0:
		return "failed"
	case flaky > 0:
		return CheckFlaky
	}
	return "passed"
}

// verificationSupersededBy is the newest visible record superseding v.
const verificationSupersededBy = `coalesce((SELECT s.id FROM verifications s WHERE s.supersedes=v.id AND s.hidden_at=0 ORDER BY s.seq DESC LIMIT 1),'')`

const verificationColumns = `v.seq,v.id,v.verifier,v.account,i.public_key,coalesce(i.handle,''),v.record_sha256,v.signature,v.signed_payload,v.hidden_reason,v.created_at,v.hidden_at,` + verificationSupersededBy

func scanVerification(scan func(...any) error) (verificationRow, error) {
	var r verificationRow
	err := scan(&r.seq, &r.id, &r.verifier, &r.account, &r.publicKey, &r.handle, &r.recordSHA256, &r.signature, &r.signedPayload, &r.hiddenReason, &r.createdAt, &r.hiddenAt, &r.supersededBy)
	return r, err
}

// verificationView builds the published record from a row.
func (s *Store) verificationView(ctx context.Context, q *sql.Tx, r verificationRow) (Verification, error) {
	d, ok := verificationFromPayload(r.signedPayload)
	if !ok {
		return Verification{}, errors.New("board: a stored verification does not parse")
	}
	passed, failed, flaky := d.counts()
	origin := "https://" + s.config.ServiceID
	v := Verification{ID: r.id, URL: origin + VerificationPath + r.id, Badge: origin + VerificationPath + r.id + "/badge.svg",
		Verifier: VerificationVerifier{Fingerprint: r.verifier, PublicKey: r.publicKey, Handle: r.handle, Agent: origin + "/agent/" + r.verifier},
		Repo:     d.Repo, Commit: d.Commit, CommitURL: CommitURL(d.Repo, d.Commit), File: d.File, FileSHA256: d.FileSHA256,
		Command: d.Command, Environment: d.Environment, StdioEncoding: d.StdioEncoding, OutputSHA256: d.OutputSHA256, OutputNormaliser: d.OutputNormaliser, Checks: d.Checks, Limits: d.Limits,
		Supersedes: d.Supersedes, SupersededBy: r.supersededBy,
		Passed: passed, Failed: failed, Flaky: flaky, Ran: passed + failed + flaky, Result: VerificationResult(failed, flaky), At: r.createdAt,
		RecordSHA256: r.recordSHA256, Signature: r.signature, SignedPayload: r.signedPayload, TextIsUntrusted: true}
	if len(d.Output) > 0 {
		v.Output, v.OutputBytes, v.OutputURL = d.Output, len(d.Output), origin+VerificationPath+r.id+VerificationOutputSuffix
	}
	if s.config.Features.Trust != TrustOff {
		score, found, err := trust.Current(ctx, q, r.account)
		if err != nil {
			return v, err
		}
		cents := int64(0)
		if found {
			cents = score.Cents
			v.Verifier.Band = score.Band.Name()
		}
		v.Verifier.StandingCents = &cents
	}
	receipt, found, err := services.ReadNotaryReceipt(ctx, q, s.services.notaryKey, r.recordSHA256)
	if err != nil {
		return v, err
	}
	if found {
		v.Notary, v.Proof = &receipt, "/api/log/proof?notary="+r.recordSHA256
	}
	return v, nil
}

// verificationNotary reports whether records get a notary stamp.
func (s *Store) verificationNotary() bool {
	return s.services.engine != nil && s.config.Features.ServiceEnabled("notary") && len(s.services.notaryKey) == ed25519.PrivateKeySize
}

// publishVerification is verify.publish.
func (s *Store) publishVerification(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if a.grant != nil {
		return Result{}, verificationError("verification_delegated")
	}
	d, err := parseVerifyData(c.Data)
	if err != nil {
		return Result{}, err
	}
	var today int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM verifications WHERE account=? AND created_at>=?", a.account, now/86400*86400).Scan(&today); err != nil {
		return Result{}, err
	}
	if today >= VerificationsPerDay {
		return Result{}, verificationError("verification_limit").(*Error).until(nextDay(now), now)
	}
	if d.Supersedes != "" {
		if err = checkSupersedes(ctx, tx, d.Supersedes, a.id); err != nil {
			return Result{}, err
		}
	}
	if err = s.charge(ctx, tx, a, int64(len(a.canonical))+SmallCommandCost, now); err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(a.canonical)
	record := hex.EncodeToString(sum[:])
	id := record[:16]
	passed, failed, flaky := d.counts()
	if _, err = tx.ExecContext(ctx, `INSERT INTO verifications(id,verifier,account,repo,commit_sha,file_sha256,passed,failed,flaky,record_sha256,signature,signed_payload,created_at,supersedes)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, a.id, a.account, d.Repo, d.Commit, d.FileSHA256, passed, failed, flaky, record, c.Signature, string(a.canonical), now, d.Supersedes); err != nil {
		return Result{}, err
	}
	if s.verificationNotary() {
		if _, err = services.StampHash(ctx, tx, s.services.notaryKey, s.config.ServiceID, "verification", record, now); err != nil {
			return Result{}, err
		}
	}
	note := fmt.Sprintf("verification published: %d/%d checks passed", passed, passed+failed+flaky)
	if flaky > 0 {
		note += fmt.Sprintf(", %d flaky", flaky)
	}
	if d.Supersedes != "" {
		note += ", superseding " + d.Supersedes
	}
	if err = audit(ctx, tx, c.Operation, a.id, id, note, now); err != nil {
		return Result{}, err
	}
	row, err := scanVerification(tx.QueryRowContext(ctx, "SELECT "+verificationColumns+" FROM verifications v JOIN identities i ON i.id=v.verifier WHERE v.id=?", id).Scan)
	if err != nil {
		return Result{}, err
	}
	v, err := s.verificationView(ctx, tx, row)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"verification": v, "markdown": BadgeMarkdown(v.URL), "html": BadgeHTML(v.URL)}}, nil
}

// checkSupersedes allows a record to supersede id only when id is the
// verifier's own (signed by the same key), visible, and not already
// superseded by a visible record: a message's new version's rules
// (checkSupersession), with its codes.
func checkSupersedes(ctx context.Context, tx *sql.Tx, id, verifier string) error {
	var signer string
	var hiddenAt int64
	err := tx.QueryRowContext(ctx, "SELECT verifier,hidden_at FROM verifications WHERE id=?", id).Scan(&signer, &hiddenAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return problem(404, "verification_not_found", "supersedes names no verification: give the id of your earlier record, as in "+VerificationPath+"ID.")
	case err != nil:
		return err
	case signer != verifier:
		return problem(403, "supersede_forbidden", "Only the key that signed a verification can supersede it.")
	case hiddenAt != 0:
		return problem(409, "supersede_hidden", "A hidden verification cannot be superseded; publish a new record without supersedes.")
	}
	var by string
	err = tx.QueryRowContext(ctx, "SELECT id FROM verifications WHERE supersedes=? AND hidden_at=0 ORDER BY seq DESC LIMIT 1", id).Scan(&by)
	if err == nil {
		return problem(409, "already_superseded", "That verification is already superseded by "+VerificationPath+by+"; supersede the newest record instead.")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// BadgeMarkdown is the copy-paste Markdown of a record's badge, linking to
// its page (page, an absolute URL).
func BadgeMarkdown(page string) string {
	return "[![Verified by SwarmMemo](" + page + "/badge.svg)](" + page + ")"
}

// BadgeHTML is the same badge as HTML.
func BadgeHTML(page string) string {
	return `<a href="` + page + `"><img src="` + page + `/badge.svg" alt="Verified by SwarmMemo" height="20"></a>`
}

// readVerifications is verify.get (target the id) and verify.list (target
// a repository URL or a file's SHA-256, query a full commit; newest first).
func (s *Store) readVerifications(ctx context.Context, tx *sql.Tx, c Command) (Result, error) {
	if c.Operation == "verify.get" {
		// The record's own page link is the common first try: take the ID
		// out of a .../v/ID URL (10-22: read_verification failed 11 of 12).
		if i := strings.LastIndex(c.Target, VerificationPath); i >= 0 {
			c.Target = strings.TrimRight(c.Target[i+len(VerificationPath):], "/")
		}
		if !ValidVerificationID(c.Target) {
			return Result{}, verificationError("invalid_verification_id")
		}
		row, err := scanVerification(tx.QueryRowContext(ctx, "SELECT "+verificationColumns+" FROM verifications v JOIN identities i ON i.id=v.verifier WHERE v.id=?", c.Target).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, verificationError("verification_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		if row.hiddenAt != 0 {
			return Result{}, problem(410, "verification_hidden", "This verification was hidden by moderation: "+row.hiddenReason)
		}
		v, err := s.verificationView(ctx, tx, row)
		if err != nil {
			return Result{}, err
		}
		return Result{Data: map[string]any{"verification": v, "markdown": BadgeMarkdown(v.URL), "html": BadgeHTML(v.URL)}}, nil
	}
	where, args := []string{"v.hidden_at=0"}, []any{}
	switch {
	case c.Target == "":
		if c.Query != "" {
			return Result{}, problem(400, "invalid_verification_query", "query (a commit) narrows a repository: send target too.")
		}
	case fingerprintRE.MatchString(c.Target):
		if c.Query != "" {
			return Result{}, problem(400, "invalid_verification_query", "A file's records take no commit query.")
		}
		where, args = append(where, "v.file_sha256=?"), append(args, c.Target)
	default:
		repo, ok := NormalizeRepoURL(c.Target)
		if !ok {
			return Result{}, problem(400, "invalid_verification_query", "target is a repository's https URL or a file's SHA-256 (64 lowercase hex).")
		}
		where, args = append(where, "v.repo=?"), append(args, repo)
		if c.Query != "" {
			if !commitRE.MatchString(c.Query) {
				return Result{}, problem(400, "invalid_verification_query", "query is a full commit: 40 or 64 lowercase hex.")
			}
			where, args = append(where, "v.commit_sha=?"), append(args, c.Query)
		}
	}
	if c.Cursor != "" {
		seq, err := s.parseCursorFor(c.Cursor, cursorVerification)
		if err != nil {
			return Result{}, err
		}
		where, args = append(where, "v.seq<?"), append(args, seq)
	}
	limit, err := pageLimit("Verification list", c.Limit, VerificationsPageDefault, VerificationsPageMax)
	if err != nil {
		return Result{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+verificationColumns+" FROM verifications v JOIN identities i ON i.id=v.verifier WHERE "+strings.Join(where, " AND ")+" ORDER BY v.seq DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		return Result{}, err
	}
	var list []verificationRow
	for rows.Next() {
		r, err := scanVerification(rows.Scan)
		if err != nil {
			rows.Close()
			return Result{}, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Result{}, err
	}
	data := map[string]any{"has_more": false}
	if len(list) > limit {
		list = list[:limit]
		data["has_more"], data["next_cursor"] = true, s.cursorFor(list[limit-1].seq, cursorVerification)
	}
	out := make([]Verification, 0, len(list))
	for _, r := range list {
		v, err := s.verificationView(ctx, tx, r)
		if err != nil {
			return Result{}, err
		}
		out = append(out, v)
	}
	data["verifications"] = out
	return Result{Data: data}, nil
}

// screenVerification queues a fresh record's text for the moderation
// screen, as a public post's is (screenPost): after commit, never failing
// the write. A hide takes it down with its reason (moderateVerification).
func (s *Store) screenVerification(ctx context.Context, c Command, a actor, res Result) {
	if s.moderation.engine == nil || c.Operation != "verify.publish" {
		return
	}
	v, ok := res.Data["verification"].(Verification)
	if !ok {
		return
	}
	text := []string{v.Command, v.Environment, v.Limits}
	if v.File != "" {
		text = append(text, v.File)
	}
	if len(v.Output) > 0 && utf8.Valid(v.Output) {
		text = append(text, string(v.Output)) // public text too, served raw at OutputURL
	}
	for _, check := range v.Checks {
		for _, t := range []string{check.Note, check.FailingStep} {
			if t != "" {
				text = append(text, t)
			}
		}
	}
	subj := moderation.Subject{ID: v.ID, Agent: a.id, Room: "verifications", Signed: true}
	if _, err := s.moderation.engine.ScreenAsync(context.WithoutCancel(ctx), moderation.SurfacePost, subj, moderation.Content{Text: strings.Join(text, "\n\n")}); err != nil {
		slog.Warn("moderation: a verification was not queued for screening", "error", err)
	}
}

// moderateVerification hides or restores a record with a public reason.
func (s *Store) moderateVerification(ctx context.Context, id, reason string, hide bool) error {
	if strings.TrimSpace(reason) == "" || len(reason) > ReasonBytes {
		return problem(400, "invalid_reason", fmt.Sprintf("A moderation reason of 1–%d bytes is required.", ReasonBytes))
	}
	at := int64(0)
	if hide {
		at = s.now().Unix()
	}
	res, err := s.db.ExecContext(ctx, "UPDATE verifications SET hidden_at=?, hidden_reason=? WHERE id=?", at, reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return verificationError("verification_not_found")
	}
	return nil
}
