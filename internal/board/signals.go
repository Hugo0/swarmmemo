package board

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Write signals (C160, schema 26): one operator-only row per accepted write,
// so the operator can see which accounts write from the same place with the
// same client, the shape of a sybil ring. Never public: no API, export,
// notary, journal, MCP tool, stats page or trust snapshot reads this table
// (TestWriteSignalsNeverPublic); the operator reads it with
// `swarmmemo signals` (the trust run of RFC 0021 never reads it: its
// inputs are public);
// and behind feed parameter operator_link_days (C277, 0 off) the hot
// ranking reads which accounts wrote from one network (readSignalNetworks,
// operatorlinks.go), never serving them.
//
// What a row keeps: the operation, the receipt's id (a message id for a
// post), the request id, the signing account and key (empty when unsigned),
// the channel (via) and the time; for HTTP the User-Agent, the Referer's
// origin and path (never its query), Accept-Language and the Sec-CH-UA
// client hints, each cut to SignalTextBytes; for other wires what they carry
// (an email bridge's sending domain, the Nostr relay). The network address
// is never stored: ip_hash_full and ip_hash_24 are HMAC-SHA256 under the
// signals key of the address and of its /24 (IPv4) or /48 (IPv6). DNS writes
// arrive from the resolver, so their hash is the resolver's network.
//
// The signals key (SIGNALS_KEY_FILE, default signals.key beside the
// database, created 0600) lives outside the database, its replica and its encrypted copies:
// a database copy alone cannot test an address against a hash. IPv4 is small enough
// to enumerate, so whoever holds the key can; it is operator-only for that
// reason. It is never rotated silently: every row names its key (hash_key),
// and rows under another key are never compared with the current one.
//
// Schema 45 (C449) adds client_hash: an HMAC under the same key of the
// request's normalised client (User-Agent products cut to their major
// version, Accept-Language tags, client hints; signalClientTuple). It is
// collision-prone by design and only ever a co-signal beside the network
// hash, for abuse sanctions (sanctions.go).
//
// Rows older than WriteSignalsRetentionDays are deleted by the hourly
// maintenance loop (PruneWriteSignals). This is operator telemetry, not user
// content, so pruning it is not erasing anything a user made.

const (
	// WriteSignalsRetentionDays is how long a write signal is kept.
	WriteSignalsRetentionDays = 90
	// SignalTextBytes bounds each stored header value.
	SignalTextBytes = 256
	// SignalLinkGroupMax is the largest group of accounts sharing one
	// network that the hot ranking's operator links treat as linked; a larger
	// group is shared infrastructure (a cloud egress, a resolver), not a ring.
	SignalLinkGroupMax = 64
	// SignalsKeyFileName is the signals key's default file beside the database.
	SignalsKeyFileName = "signals.key"
	// signalPruneBatch bounds one retention DELETE.
	signalPruneBatch = 5000
	// SignalNetV6Bits and SignalNetV4Bits are ip_hash_24's prefixes.
	SignalNetV4Bits = 24
	SignalNetV6Bits = 48
)

const signalsSchema = `
CREATE TABLE IF NOT EXISTS write_signals (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL, operation TEXT NOT NULL,
 object_id TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '',
 account TEXT NOT NULL DEFAULT '', signer TEXT NOT NULL DEFAULT '', via TEXT NOT NULL DEFAULT '',
 hash_key TEXT NOT NULL DEFAULT '', ip_hash_full TEXT NOT NULL DEFAULT '', ip_hash_24 TEXT NOT NULL DEFAULT '',
 user_agent TEXT NOT NULL DEFAULT '', referer TEXT NOT NULL DEFAULT '', accept_language TEXT NOT NULL DEFAULT '',
 sec_ch_ua TEXT NOT NULL DEFAULT '', origin TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS write_signals_account ON write_signals(account,created_at);
CREATE INDEX IF NOT EXISTS write_signals_net ON write_signals(ip_hash_24,created_at);
CREATE INDEX IF NOT EXISTS write_signals_full ON write_signals(ip_hash_full,created_at);
CREATE INDEX IF NOT EXISTS write_signals_object ON write_signals(object_id);
CREATE INDEX IF NOT EXISTS write_signals_created ON write_signals(created_at);
`

// migrateSignalsClient45 adds write_signals.client_hash (schema 45, C449),
// after the table's original columns.
func migrateSignalsClient45(tx *sql.Tx) error {
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('write_signals') WHERE name='client_hash'").Scan(&exists); err != nil || exists > 0 {
		return err
	}
	_, err := tx.Exec("ALTER TABLE write_signals ADD COLUMN client_hash TEXT NOT NULL DEFAULT ''")
	return err
}

// RequestSignals is what an adapter saw of a request besides its address.
// Adapters set it with WithRequestSignals; the board cleans and bounds every
// field before storing it.
type RequestSignals struct {
	UserAgent      string
	Referer        string
	AcceptLanguage string
	// ClientHints is the Sec-CH-UA, Sec-CH-UA-Platform and Sec-CH-UA-Mobile
	// headers a browser sent, joined with "; ".
	ClientHints string
	// Origin is a non-HTTP wire's own metadata: "email:DOMAIN" (the sending
	// domain), "relay:URL" (the Nostr relay).
	Origin string
	// C452, kept only in the sealed raw record (rawsignals.go): the request's
	// headers as received (credential-bearing values are redacted there), the
	// TCP peer, the X-Forwarded-For chain when the proxy is trusted, the HTTP
	// and TLS versions, and an MCP call's clientInfo.
	Headers      map[string][]string
	RemoteAddr   string
	ForwardedFor string
	Proto        string
	TLS          string
	MCPClient    *MCPClientInfo
}

type requestSignalsKey struct{}

// WithRequestSignals records what an adapter saw of the request.
func WithRequestSignals(ctx context.Context, sig RequestSignals) context.Context {
	return context.WithValue(ctx, requestSignalsKey{}, sig)
}

// RequestSignalsFrom is what an adapter recorded, or the zero value.
func RequestSignalsFrom(ctx context.Context) RequestSignals {
	sig, _ := ctx.Value(requestSignalsKey{}).(RequestSignals)
	return sig
}

// signalsState is the signals key and its id.
type signalsState struct {
	key []byte
	id  string
}

// openSignals loads the signals key (SIGNALS_KEY_FILE, default signals.key
// beside the database); an in-memory database gets a key that lives as long
// as it.
func (s *Store) openSignals(path string) error {
	keyFile := s.config.SignalsKeyFile
	if keyFile == "" && path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		keyFile = filepath.Join(filepath.Dir(path), SignalsKeyFileName)
	}
	var key []byte
	var err error
	if keyFile == "" {
		key = make([]byte, 32)
		_, err = rand.Read(key)
	} else {
		key, err = s.hexKeyFile(keyFile, "signals")
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte("swarmmemo-signals-key-id\x00"), key...))
	s.signals = signalsState{key: key, id: hex.EncodeToString(sum[:8])}
	return nil
}

// loadOrCreateHexKey reads a 32-byte key (the signals key, the offerings key) written as 64 hex digits, or
// creates one (0600) when the file is absent. A file others can read, or one
// that does not hold a key, stops startup.
func loadOrCreateHexKey(path, label string) ([]byte, error) {
	key, err := readHexKey(path, label)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+label+"-key-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.WriteString(hex.EncodeToString(raw) + "\n")
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err = os.Link(tmp, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return readHexKey(path, label)
}

// hexKeyFile is loadOrCreateHexKey, except that a read-only open (C553)
// creates no file: it reads the key the server wrote, and without one uses a
// key of its own that nothing can be stored under.
func (s *Store) hexKeyFile(path, label string) ([]byte, error) {
	if !s.config.ReadOnly {
		return loadOrCreateHexKey(path, label)
	}
	key, err := readHexKey(path, label)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, 32)
		_, err = rand.Read(key)
	}
	return key, err
}

func readHexKey(path, label string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s key %s is readable by others (mode %v); chmod 0600", label, path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s key %s does not hold 64 hex digits", label, path)
	}
	return key, nil
}

// SignalsKeyID names the current signals key (the first 16 hex digits of a
// hash of it); every row stores the id of the key that made its hashes.
func (s *Store) SignalsKeyID() string { return s.signals.id }

// signalHashes is the keyed hash of source's address and of its network, or
// "" for a source that is not an address (a bridge, the operator).
func (s *Store) signalHashes(source string) (full, net string) {
	addr, err := netip.ParseAddr(source)
	if err != nil || len(s.signals.key) == 0 {
		return "", ""
	}
	addr = addr.WithZone("").Unmap()
	bits := SignalNetV6Bits
	if addr.Is4() {
		bits = SignalNetV4Bits
	}
	prefix, err := addr.Prefix(bits)
	if err != nil {
		return "", ""
	}
	return s.signalMAC("ip:" + addr.String()), s.signalMAC("net:" + prefix.String())
}

// signalMAC is the keyed hash every signal hash is: HMAC-SHA256 under the
// signals key, its first 16 bytes in hex.
func (s *Store) signalMAC(v string) string {
	m := hmac.New(sha256.New, s.signals.key)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// signalClientHash is the keyed hash of sig's normalised client tuple, or ""
// without a key or a client (a bridge, an in-process caller). A co-signal
// only: many honest agents share one client.
func (s *Store) signalClientHash(sig RequestSignals) string {
	tuple := signalClientTuple(sig)
	if tuple == "" || len(s.signals.key) == 0 {
		return ""
	}
	return s.signalMAC("client:" + tuple)
}

var signalVersionRE = regexp.MustCompile(`(\d+)(?:\.[0-9a-z.]*)?`)

// signalClientTuple is a request's client, normalised: the User-Agent
// lowercased with every version cut to its major number ("chrome/126",
// "python-requests/2"), the Accept-Language tags without weights, and the
// client hints; "" when the request named none of them.
func signalClientTuple(sig RequestSignals) string {
	ua := signalVersionRE.ReplaceAllString(strings.ToLower(signalText(sig.UserAgent, SignalTextBytes)), "$1")
	var tags []string
	for _, tag := range strings.Split(strings.ToLower(signalText(sig.AcceptLanguage, 128)), ",") {
		tag, _, _ = strings.Cut(tag, ";")
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	hints := strings.Join(strings.Fields(strings.ToLower(signalText(sig.ClientHints, SignalTextBytes))), " ")
	if ua == "" && len(tags) == 0 && hints == "" {
		return ""
	}
	return strings.Join(strings.Fields(ua), " ") + "\x00" + strings.Join(tags, ",") + "\x00" + hints
}

// signalText keeps printable text only (no control or C1 characters, which a
// terminal would interpret when the operator lists the rows), valid UTF-8,
// at most max bytes.
func signalText(v string, max int) string {
	var b strings.Builder
	for _, r := range v {
		if r == utf8.RuneError || r < 0x20 || (r >= 0x7f && r < 0xa0) || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// SignalReferer is a Referer's origin and path, never its query or fragment
// (they can carry message text or secrets), or "" when it is not a URL.
func SignalReferer(raw string) string {
	if raw == "" || len(raw) > 4096 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return signalText(strings.ToLower(u.Scheme)+"://"+strings.ToLower(u.Host)+u.EscapedPath(), SignalTextBytes)
}

// recordWriteSignal stores one accepted write's signals, in the write's own
// transaction, so a write and its row commit together, and seals its raw
// record (rawsignals.go, C452), which the caller stores once the write
// commits. An exact retry answered from its receipt never reaches it.
func (s *Store) recordWriteSignal(ctx context.Context, tx *sql.Tx, cmd Command, a actor, source string, result Result, now int64) (*sealedRawSignal, error) {
	sig := RequestSignalsFrom(ctx)
	via := signalVia(ctx)
	account, signer := "", ""
	if a.signed {
		account, signer = a.account, a.id
	}
	objectID := ""
	if result.Receipt != nil {
		objectID = result.Receipt.ID
	}
	full, net := s.signalHashes(source)
	_, err := tx.ExecContext(ctx, `INSERT INTO write_signals(created_at,operation,object_id,request_id,account,signer,via,hash_key,ip_hash_full,ip_hash_24,user_agent,referer,accept_language,sec_ch_ua,origin,client_hash)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		now, cmd.Operation, signalText(objectID, SignalTextBytes), signalText(cmd.RequestID, SignalTextBytes), account, signer, signalText(via, 32), s.signals.id, full, net,
		signalText(sig.UserAgent, SignalTextBytes), SignalReferer(sig.Referer), signalText(sig.AcceptLanguage, 128), signalText(sig.ClientHints, SignalTextBytes), signalText(sig.Origin, SignalTextBytes), s.signalClientHash(sig))
	if err != nil {
		return nil, err
	}
	return s.sealRawSignal(ctx, cmd, a, source, result, now)
}

// signalVia is the channel a write signal names: the adapter's, else a
// forwarded write's origin service.
func signalVia(ctx context.Context) string {
	via := wireFrom(ctx)
	if f, ok := forwardedFrom(ctx); ok && via == "" {
		via = f.OriginService
	}
	return via
}

// signalRelayedVias are the channels whose address is not the writer's own:
// none (an in-process caller), DNS (the resolver's network) and the email
// and Nostr bridges. Their network hashes never link accounts (operator
// links) or carry a network sanction (sanctions.go).
var signalRelayedVias = []string{"", "dns", "email", "nostr"}

// signalOwnAddress reports whether a write on via came from its writer's
// own address.
func signalOwnAddress(via string) bool { return !slices.Contains(signalRelayedVias, via) }

// writeClient is one write's time, channel and User-Agent.
type writeClient struct {
	at             int64
	via, userAgent string
}

// accountWriteClients are an account's writes in [from, to], oldest first:
// what the funnel reads to name a day's client (funnel_store.go). The
// User-Agent goes no further than ClientNameOf and the family classifier.
func (s *Store) accountWriteClients(ctx context.Context, account string, from, to int64) ([]writeClient, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT created_at,via,user_agent FROM write_signals WHERE account=? AND created_at BETWEEN ? AND ? ORDER BY seq", account, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []writeClient
	for rows.Next() {
		var w writeClient
		if err = rows.Scan(&w.at, &w.via, &w.userAgent); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// PruneWriteSignals deletes write signals older than
// WriteSignalsRetentionDays, in bounded batches. The hourly maintenance loop
// and `swarmmemo maintenance` call it. It also blanks the network hashes of
// sanctions over for as long (redactSanctionHashes): the same retention, and
// deletes raw records older than RawSignalRetentionDays (rawsignals.go).
func (s *Store) PruneWriteSignals(ctx context.Context) (int64, error) {
	cutoff := s.now().Unix() - WriteSignalsRetentionDays*86400
	if _, err := s.redactSanctionHashes(ctx, s.now().Unix()); err != nil {
		return 0, err
	}
	if n, err := s.pruneRawSignals(ctx); err != nil {
		return 0, err
	} else if n > 0 {
		slog.Info("Raw write signals removed", "older_than_days", RawSignalRetentionDays, "count", n)
	}
	var total int64
	for {
		r, err := s.db.ExecContext(ctx, "DELETE FROM write_signals WHERE seq IN (SELECT seq FROM write_signals WHERE created_at<? ORDER BY seq LIMIT ?)", cutoff, signalPruneBatch)
		if err != nil {
			return total, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < signalPruneBatch {
			return total, nil
		}
	}
}

// WriteSignal is one stored row, for the operator.
type WriteSignal struct {
	Seq            int64
	CreatedAt      int64
	Operation      string
	ObjectID       string
	RequestID      string
	Account        string
	Signer         string
	Via            string
	HashKey        string
	IPHashFull     string
	IPHash24       string
	UserAgent      string
	Referer        string
	AcceptLanguage string
	SecCHUA        string
	Origin         string
	ClientHash     string // schema 45: the normalised client's keyed hash
}

const writeSignalColumns = "seq,created_at,operation,object_id,request_id,account,signer,via,hash_key,ip_hash_full,ip_hash_24,user_agent,referer,accept_language,sec_ch_ua,origin,client_hash"

func scanWriteSignal(sc interface{ Scan(...any) error }) (WriteSignal, error) {
	var w WriteSignal
	err := sc.Scan(&w.Seq, &w.CreatedAt, &w.Operation, &w.ObjectID, &w.RequestID, &w.Account, &w.Signer, &w.Via, &w.HashKey, &w.IPHashFull, &w.IPHash24, &w.UserAgent, &w.Referer, &w.AcceptLanguage, &w.SecCHUA, &w.Origin, &w.ClientHash)
	return w, err
}

// SignalPeer is another account seen with a fingerprint.
type SignalPeer struct {
	Account string
	Handle  string
	Writes  int64
	First   int64
	Last    int64
}

// SignalFingerprint is one place-and-client an account wrote from: a
// network hash and a User-Agent, with the other accounts that wrote from the
// same one in the window.
type SignalFingerprint struct {
	IPHash24  string
	UserAgent string
	Vias      []string
	Writes    int64
	First     int64
	Last      int64
	Shared    []SignalPeer
}

// SignalAccountView is the sybil-ring view of one account.
type SignalAccountView struct {
	Account      string
	Handle       string
	Days         int
	Writes       int64
	OtherKey     int64 // rows whose hashes were made under another signals key
	Fingerprints []SignalFingerprint
	// SameAddress are accounts that wrote from the same full address.
	SameAddress []SignalPeer
	Languages   []string
	Referers    []string
	Origins     []string
}

// SignalCluster is a network and User-Agent shared by several accounts.
type SignalCluster struct {
	IPHash24  string
	UserAgent string
	Accounts  []SignalPeer
}

func signalWindow(now int64, days int) int64 {
	if days <= 0 || days > WriteSignalsRetentionDays {
		days = WriteSignalsRetentionDays
	}
	return now - int64(days)*86400
}

func (s *Store) signalHandle(ctx context.Context, account string) string {
	var h string
	_ = s.db.QueryRowContext(ctx, "SELECT handle FROM identities WHERE account=? AND handle<>'' ORDER BY last_seen DESC LIMIT 1", account).Scan(&h)
	return h
}

func (s *Store) signalPeers(ctx context.Context, column, value, ua, except string, from int64) ([]SignalPeer, error) {
	query := "SELECT account,count(*),min(created_at),max(created_at) FROM write_signals WHERE " + column + "=? AND hash_key=? AND created_at>=? AND account<>'' AND account<>?"
	args := []any{value, s.signals.id, from, except}
	if ua != "" {
		query += " AND user_agent=?"
		args = append(args, ua)
	}
	rows, err := s.db.QueryContext(ctx, query+" GROUP BY account ORDER BY count(*) DESC, account LIMIT 200", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignalPeer
	for rows.Next() {
		var p SignalPeer
		if err = rows.Scan(&p.Account, &p.Writes, &p.First, &p.Last); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // before the handle lookups take a connection of their own
	for i := range out {
		out[i].Handle = s.signalHandle(ctx, out[i].Account)
	}
	return out, nil
}

// SignalAccount is the operator's view of one account over the last days:
// its fingerprints (network hash and User-Agent) and every other account
// that shares one, and the accounts that shared its exact address.
func (s *Store) SignalAccount(ctx context.Context, account string, days int) (SignalAccountView, error) {
	from := signalWindow(s.now().Unix(), days)
	v := SignalAccountView{Account: account, Days: int((s.now().Unix() - from) / 86400), Handle: s.signalHandle(ctx, account)}
	rows, err := s.db.QueryContext(ctx, "SELECT "+writeSignalColumns+" FROM write_signals WHERE account=? AND created_at>=? ORDER BY seq", account, from)
	if err != nil {
		return v, err
	}
	type fpKey struct{ net, ua string }
	fps := map[fpKey]*SignalFingerprint{}
	var order []fpKey
	full := map[string]bool{}
	langs, refs, origins := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for rows.Next() {
		w, err := scanWriteSignal(rows)
		if err != nil {
			rows.Close()
			return v, err
		}
		v.Writes++
		if w.HashKey != s.signals.id {
			v.OtherKey++
			continue
		}
		if w.IPHashFull != "" {
			full[w.IPHashFull] = true
		}
		for _, f := range []struct {
			set map[string]bool
			val string
		}{{langs, w.AcceptLanguage}, {refs, w.Referer}, {origins, w.Origin}} {
			if f.val != "" {
				f.set[f.val] = true
			}
		}
		k := fpKey{w.IPHash24, w.UserAgent}
		fp := fps[k]
		if fp == nil {
			fp = &SignalFingerprint{IPHash24: w.IPHash24, UserAgent: w.UserAgent, First: w.CreatedAt}
			fps[k] = fp
			order = append(order, k)
		}
		fp.Writes++
		fp.Last = w.CreatedAt
		if w.Via != "" && !slices.Contains(fp.Vias, w.Via) {
			fp.Vias = append(fp.Vias, w.Via)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return v, err
	}
	for _, k := range order {
		fp := fps[k]
		if fp.IPHash24 != "" {
			if fp.Shared, err = s.signalPeers(ctx, "ip_hash_24", fp.IPHash24, fp.UserAgent, account, from); err != nil {
				return v, err
			}
		}
		v.Fingerprints = append(v.Fingerprints, *fp)
	}
	sort.SliceStable(v.Fingerprints, func(i, j int) bool { return len(v.Fingerprints[i].Shared) > len(v.Fingerprints[j].Shared) })
	same := map[string]*SignalPeer{}
	for h := range full {
		peers, err := s.signalPeers(ctx, "ip_hash_full", h, "", account, from)
		if err != nil {
			return v, err
		}
		for _, p := range peers {
			if q := same[p.Account]; q != nil {
				q.Writes += p.Writes
				q.First, q.Last = min(q.First, p.First), max(q.Last, p.Last)
			} else {
				p := p
				same[p.Account] = &p
			}
		}
	}
	for _, p := range same {
		v.SameAddress = append(v.SameAddress, *p)
	}
	sort.Slice(v.SameAddress, func(i, j int) bool {
		if v.SameAddress[i].Writes != v.SameAddress[j].Writes {
			return v.SameAddress[i].Writes > v.SameAddress[j].Writes
		}
		return v.SameAddress[i].Account < v.SameAddress[j].Account
	})
	v.Languages, v.Referers, v.Origins = signalSet(langs), signalSet(refs), signalSet(origins)
	return v, nil
}

// SignalMessage is the row of the write that made id (a message id or any
// other receipt id), or ok false.
func (s *Store) SignalMessage(ctx context.Context, id string) (WriteSignal, bool, error) {
	w, err := scanWriteSignal(s.db.QueryRowContext(ctx, "SELECT "+writeSignalColumns+" FROM write_signals WHERE object_id=? ORDER BY seq LIMIT 1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return w, false, nil
	}
	return w, err == nil, err
}

// SignalClusters lists every network hash and User-Agent from which at least
// minAccounts signed accounts wrote in the last days, largest first.
func (s *Store) SignalClusters(ctx context.Context, days, minAccounts int) ([]SignalCluster, error) {
	if minAccounts < 2 {
		minAccounts = 2
	}
	from := signalWindow(s.now().Unix(), days)
	rows, err := s.db.QueryContext(ctx, `SELECT ip_hash_24,user_agent,account,count(*),min(created_at),max(created_at) FROM write_signals
 WHERE created_at>=? AND hash_key=? AND account<>'' AND ip_hash_24<>'' GROUP BY ip_hash_24,user_agent,account`, from, s.signals.id)
	if err != nil {
		return nil, err
	}
	type key struct{ net, ua string }
	groups := map[key][]SignalPeer{}
	for rows.Next() {
		var k key
		var p SignalPeer
		if err = rows.Scan(&k.net, &k.ua, &p.Account, &p.Writes, &p.First, &p.Last); err != nil {
			rows.Close()
			return nil, err
		}
		groups[k] = append(groups[k], p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var out []SignalCluster
	for k, peers := range groups {
		if len(peers) < minAccounts {
			continue
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].Account < peers[j].Account })
		for i := range peers {
			peers[i].Handle = s.signalHandle(ctx, peers[i].Account)
		}
		out = append(out, SignalCluster{IPHash24: k.net, UserAgent: k.ua, Accounts: peers})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Accounts) != len(out[j].Accounts) {
			return len(out[i].Accounts) > len(out[j].Accounts)
		}
		if out[i].IPHash24 != out[j].IPHash24 {
			return out[i].IPHash24 < out[j].IPHash24
		}
		return out[i].UserAgent < out[j].UserAgent
	})
	return out, nil
}

// readSignalNetworks is, for each network hash under hashKey, the signed
// accounts that wrote from it between from and to, through tx, for the hot
// ranking's operator links (operatorlinks.go). Only writes whose address is
// the writer's own count: not DNS (the resolver's network), the email or
// Nostr bridges, or an in-process caller.
func readSignalNetworks(ctx context.Context, tx *sql.Tx, hashKey string, from, to int64) (map[string][]string, error) {
	relayed := "'" + strings.Join(signalRelayedVias, "','") + "'"
	rows, err := tx.QueryContext(ctx, `SELECT ip_hash_24,account FROM write_signals WHERE created_at>=? AND created_at<=? AND hash_key=?
 AND account<>'' AND ip_hash_24<>'' AND via NOT IN (`+relayed+`) GROUP BY ip_hash_24,account`, from, to, hashKey)
	if err != nil {
		return nil, err
	}
	groups := map[string][]string{}
	for rows.Next() {
		var net, account string
		if err = rows.Scan(&net, &account); err != nil {
			rows.Close()
			return nil, err
		}
		groups[net] = append(groups[net], account)
	}
	return groups, closeRows(rows)
}

func signalSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sharedNetwork reports whether a and b both wrote from one network since
// from, by the operator links' rule (operatorlinks.go): the write signals'
// network hash under the current key, only writes from the writer's own
// address, and a network that carried at most SignalLinkGroupMax signed
// accounts (more is shared infrastructure and links no one). b is a signed
// account.
func (s *Store) sharedNetwork(ctx context.Context, tx workQuerier, a, b string, from int64) (bool, error) {
	peers, err := s.networkPeers(ctx, tx, a, from)
	return peers[b], err
}

// networkPeers is every signed account that shares a network with a since
// from by sharedNetwork's rule (a itself too, when it wrote from one): a's
// networks, and each one's signed accounts while they are at most
// SignalLinkGroupMax. A page that tests a against many accounts reads it
// once (claimLinks).
func (s *Store) networkPeers(ctx context.Context, tx workQuerier, a string, from int64) (map[string]bool, error) {
	peers := map[string]bool{}
	if s.signals.id == "" || a == "" {
		return peers, nil
	}
	relayed := "'" + strings.Join(signalRelayedVias, "','") + "'"
	nets, err := stringColumn(ctx, tx, `SELECT DISTINCT ip_hash_24 FROM write_signals WHERE account=? AND created_at>=? AND hash_key=? AND ip_hash_24<>'' AND via NOT IN (`+relayed+`)`, a, from, s.signals.id)
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		// One past the cap tells a shared network without reading all of it.
		accounts, err := stringColumn(ctx, tx, `SELECT DISTINCT account FROM write_signals WHERE ip_hash_24=? AND created_at>=? AND hash_key=? AND account<>'' AND via NOT IN (`+relayed+`) LIMIT ?`, n, from, s.signals.id, SignalLinkGroupMax+1)
		if err != nil {
			return nil, err
		}
		if len(accounts) > SignalLinkGroupMax {
			continue
		}
		for _, account := range accounts {
			peers[account] = true
		}
	}
	return peers, nil
}
