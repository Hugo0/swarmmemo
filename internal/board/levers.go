package board

// Levers (RFC0012 §2.6), owned by builder E. A lever is inert until the
// steward pulls it with the local CLI (swarmmemo lever pull NAME [ARGS]
// --reason TEXT [--until UNIX]); every pull, release and expiry is a public
// lever_log row, shown at /api/levers. Levers never change parameters or
// delete data: the ledger reads them as an overlay through LeverSource, and
// admit refuses writes for signed-only and block-prefix.
//
// The lever table is read once per transaction: a transaction reads only
// max(lever_log.seq), and reloads the (at most 16) lever rows and the
// active blocked prefixes when it changed, since the CLI writes from another
// process.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"swarmmemo/internal/allowance"
)

// Lever names, in the order of §2.6.
const (
	LeverTier4Shrink     = "tier4-shrink"
	LeverSignedOnly      = "signed-only"
	LeverPauseNewKeys    = "pause-new-keys"
	LeverCutBudget       = "cut-budget"
	LeverBlockPrefix     = "block-prefix"
	LeverProvenOnly      = "proven-only"
	LeverFreezeTransfers = "freeze-transfers"
	// LeverSignedServices turns anonymous service calls off at once:
	// unsigned service.call is refused and the anonymous tier gets no credit.
	LeverSignedServices = "signed-services"
)

// LeverNames are the levers the CLI accepts.
var LeverNames = []string{LeverTier4Shrink, LeverSignedOnly, LeverPauseNewKeys, LeverCutBudget, LeverBlockPrefix, LeverProvenOnly, LeverFreezeTransfers, LeverSignedServices}

// Lever bounds. A blocked prefix is at least an IPv4 /8 or an IPv6 /16 (wider
// is signed-only's job); an --until more than a year away is refused as a
// likely millisecond timestamp.
const (
	BlockedPrefixesMax   = 256
	blockPrefixMinBitsV4 = 8
	blockPrefixMinBitsV6 = 16
	leverUntilMaxSeconds = 366 * 86400
	leverReasonBytes     = 500
	leverActorBytes      = 64
	leverLogPage         = 100
	leverReleasedShown   = 50
)

// leverState is the lever cache the Store carries (Store.levers).
type leverState struct {
	mu       sync.Mutex
	loaded   bool
	version  int64 // max(lever_log.seq) the snapshot reflects
	rows     []leverRow
	prefixes []blockedPrefix // released_at = 0 only
}

type leverRow struct {
	name, state, args           string
	pulledAt, until, releasedAt int64
	actor, reason               string
}

type blockedPrefix struct {
	id         int64
	prefix     netip.Prefix
	until      int64
	releasedAt int64
}

type leverSnapshot struct {
	version  int64
	rows     []leverRow
	prefixes []blockedPrefix
}

func (s *Store) openLevers() error { return nil }

func leverActive(state string, until, now int64) bool {
	return state == "pulled" && (until == 0 || until > now)
}

// snapshot returns the lever rows and blocked prefixes, reloading them
// through q only when lever_log has changed.
func (s *Store) leverSnapshot(ctx context.Context, q allowance.Querier) (leverSnapshot, error) {
	var version int64
	if err := q.QueryRowContext(ctx, "SELECT coalesce(max(seq),0) FROM lever_log").Scan(&version); err != nil {
		return leverSnapshot{}, err
	}
	st := &s.levers
	st.mu.Lock()
	if st.loaded && st.version == version {
		snap := leverSnapshot{version: st.version, rows: st.rows, prefixes: st.prefixes}
		st.mu.Unlock()
		return snap, nil
	}
	st.mu.Unlock()
	snap := leverSnapshot{version: version}
	if version > 0 {
		rows, err := q.QueryContext(ctx, "SELECT name,state,args,pulled_at,until,released_at,actor,reason FROM levers ORDER BY name LIMIT 16")
		if err != nil {
			return leverSnapshot{}, err
		}
		for rows.Next() {
			var r leverRow
			if err = rows.Scan(&r.name, &r.state, &r.args, &r.pulledAt, &r.until, &r.releasedAt, &r.actor, &r.reason); err != nil {
				rows.Close()
				return leverSnapshot{}, err
			}
			snap.rows = append(snap.rows, r)
		}
		if err = closeRows(rows); err != nil {
			return leverSnapshot{}, err
		}
		rows, err = q.QueryContext(ctx, "SELECT id,cidr,until,released_at FROM blocked_prefixes WHERE released_at=0 ORDER BY id LIMIT ?", BlockedPrefixesMax)
		if err != nil {
			return leverSnapshot{}, err
		}
		for rows.Next() {
			var p blockedPrefix
			var cidr string
			if err = rows.Scan(&p.id, &cidr, &p.until, &p.releasedAt); err != nil {
				rows.Close()
				return leverSnapshot{}, err
			}
			if p.prefix, err = netip.ParsePrefix(cidr); err != nil {
				rows.Close()
				return leverSnapshot{}, fmt.Errorf("blocked prefix %d: %w", p.id, err)
			}
			snap.prefixes = append(snap.prefixes, p)
		}
		if err = closeRows(rows); err != nil {
			return leverSnapshot{}, err
		}
	}
	st.mu.Lock()
	st.loaded, st.version, st.rows, st.prefixes = true, snap.version, snap.rows, snap.prefixes
	st.mu.Unlock()
	return snap, nil
}

// levers is the allowance.Levers the snapshot means at now.
func (snap leverSnapshot) levers(now int64) allowance.Levers {
	l := allowance.Levers{Tier4SharePPM: -1, Version: snap.version}
	for _, r := range snap.rows {
		if !leverActive(r.state, r.until, now) {
			continue
		}
		var args struct {
			PPM      int64  `json:"ppm"`
			Resource string `json:"resource"`
		}
		_ = json.Unmarshal([]byte(r.args), &args)
		name, _, _ := strings.Cut(r.name, ":")
		switch name {
		case LeverSignedOnly:
			l.SignedOnly = true
		case LeverProvenOnly:
			l.ProvenOnly = true
		case LeverFreezeTransfers:
			l.FreezeTransfers = true
		case LeverSignedServices:
			l.SignedServices = true
		case LeverPauseNewKeys:
			l.PauseNewKeys, l.PauseNewKeysSince = true, r.pulledAt
		case LeverTier4Shrink:
			l.Tier4SharePPM = args.PPM
		case LeverCutBudget:
			if l.BudgetCutPPM == nil {
				l.BudgetCutPPM = map[allowance.Resource]int64{}
			}
			l.BudgetCutPPM[allowance.Resource(args.Resource)] = args.PPM
		}
	}
	return l
}

// blocked reports whether source, a peer address, is inside an active
// blocked prefix. A source that is not an address (a bridge) never is.
func (snap leverSnapshot) blocked(source string, now int64) bool {
	if len(snap.prefixes) == 0 {
		return false
	}
	addr, ok := sourceAddr(source)
	if !ok {
		return false
	}
	for _, p := range snap.prefixes {
		if p.releasedAt == 0 && (p.until == 0 || p.until > now) && p.prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// sourceAddr parses a peer address ("192.0.2.1", "2001:db8::1", optionally
// with a port or zone); an IPv4-mapped IPv6 address is its IPv4 address.
func sourceAddr(source string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(source)
	if err != nil {
		ap, err := netip.ParseAddrPort(source)
		if err != nil {
			return netip.Addr{}, false
		}
		addr = ap.Addr()
	}
	return addr.WithZone("").Unmap(), true
}

// admit runs after authentication, before any other work in the command's
// transaction: signed-only and block-prefix refuse writes here (403
// signed_only, 403 prefix_blocked). Reads are never refused. nil admits.
func (s *Store) admit(ctx context.Context, tx *sql.Tx, c Command, a actor, source string, now int64) error {
	if !mutation(c.Operation) {
		return nil
	}
	snap, err := s.leverSnapshot(ctx, tx)
	if err != nil || snap.version == 0 {
		return err
	}
	if !a.signed && snap.levers(now).SignedOnly {
		return allowanceError("signed_only")
	}
	if snap.blocked(source, now) {
		return allowanceError("prefix_blocked")
	}
	// Log levers whose --until has passed; they stopped acting at until either way.
	_, err = expireLevers(ctx, tx, snap, now)
	return err
}

// leverSource is the allowance.LeverSource the ledger reads.
func (s *Store) leverSource() allowance.LeverSource { return leverSource{s} }

type leverSource struct{ s *Store }

func (l leverSource) Levers(ctx context.Context, q allowance.Querier, now int64) (allowance.Levers, error) {
	snap, err := l.s.leverSnapshot(ctx, q)
	if err != nil {
		return allowance.Levers{Tier4SharePPM: -1}, err
	}
	return snap.levers(now), nil
}

func (l leverSource) PrefixBlocked(ctx context.Context, q allowance.Querier, source string, now int64) (bool, error) {
	snap, err := l.s.leverSnapshot(ctx, q)
	if err != nil {
		return false, err
	}
	return snap.blocked(source, now), nil
}

// expireLevers marks levers and blocked prefixes whose until has passed as
// released at until and logs an expire entry for each.
func expireLevers(ctx context.Context, q allowance.Querier, snap leverSnapshot, now int64) (int, error) {
	n := 0
	for _, r := range snap.rows {
		if r.state != "pulled" || r.until == 0 || r.until > now {
			continue
		}
		res, err := q.ExecContext(ctx, "UPDATE levers SET state='released',released_at=until WHERE name=? AND state='pulled' AND until>0 AND until<=?", r.name, now)
		if err != nil {
			return n, err
		}
		if changed, _ := res.RowsAffected(); changed == 0 {
			continue
		}
		name, _, _ := strings.Cut(r.name, ":")
		if err = logLever(ctx, q, name, "expire", r.args, "lever", "until passed", r.until); err != nil {
			return n, err
		}
		n++
	}
	for _, p := range snap.prefixes {
		if p.until == 0 || p.until > now {
			continue
		}
		res, err := q.ExecContext(ctx, "UPDATE blocked_prefixes SET released_at=until WHERE id=? AND released_at=0 AND until>0 AND until<=?", p.id, now)
		if err != nil {
			return n, err
		}
		if changed, _ := res.RowsAffected(); changed == 0 {
			continue
		}
		args, err := prefixLogArgs(ctx, q, p.id)
		if err != nil {
			return n, err
		}
		if err = logLever(ctx, q, LeverBlockPrefix, "expire", args, "lever", "until passed", p.until); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func logLever(ctx context.Context, q allowance.Querier, name, action, args, actor, reason string, at int64) error {
	_, err := q.ExecContext(ctx, "INSERT INTO lever_log(name,action,args,actor,reason,created_at) VALUES(?,?,?,?,?,?)", name, action, args, actor, reason, at)
	return err
}

// prefixLogArgs is what the public log says about a blocked prefix: its id,
// length and keyed hash, never the prefix.
func prefixLogArgs(ctx context.Context, q allowance.Querier, id int64) (string, error) {
	var bits int
	var hash string
	if err := q.QueryRowContext(ctx, "SELECT bits,keyed_hash FROM blocked_prefixes WHERE id=?", id).Scan(&bits, &hash); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(map[string]any{"id": id, "bits": bits, "keyed_hash": hash})
	return string(raw), nil
}

// LeverPull is one `swarmmemo lever pull`.
type LeverPull struct {
	Name   string
	Args   []string
	Reason string
	Actor  string // who pulled it: "operator" when empty
	Until  int64  // 0: until released
}

// LeverChange is the result of a pull, release or expiry run.
type LeverChange struct {
	Action string         `json:"action"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args,omitempty"`
	Until  int64          `json:"until,omitempty"`
	Seq    int64          `json:"seq"`
}

func leverText(value, field string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max {
		return "", fmt.Errorf("%s must be 1–%d bytes", field, max)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must not contain control characters", field)
	}
	return value, nil
}

func parsePPM(value, what string, min int64) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < min || n > 1_000_000 {
		return 0, fmt.Errorf("%s must be an integer from %d to 1000000 (parts per million)", what, min)
	}
	return n, nil
}

// parseBlockPrefix reads a CIDR for block-prefix: masked, IPv4-mapped
// prefixes as IPv4, at least /8 (IPv4) or /16 (IPv6).
func parseBlockPrefix(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(value))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("block-prefix takes a CIDR such as 203.0.113.0/24 or 2001:db8::/48")
	}
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	p = p.Masked()
	if p.Addr().Is4() && p.Bits() < blockPrefixMinBitsV4 || !p.Addr().Is4() && p.Bits() < blockPrefixMinBitsV6 {
		return netip.Prefix{}, fmt.Errorf("block-prefix is at least /%d for IPv4 and /%d for IPv6; use signed-only for wider", blockPrefixMinBitsV4, blockPrefixMinBitsV6)
	}
	return p, nil
}

// prefixKey is the key of blocked prefixes' public keyed hashes, created on
// the first block.
func prefixKey(ctx context.Context, tx *sql.Tx) ([]byte, error) {
	var value string
	err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='lever_prefix_key'").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		value = hex.EncodeToString(key)
		_, err = tx.ExecContext(ctx, "INSERT INTO meta(key,value) VALUES('lever_prefix_key',?)", value)
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(value)
}

// PullLever pulls a lever, or updates the arguments, reason and until of one
// already pulled (keeping when it was first pulled), and logs it.
func (s *Store) PullLever(ctx context.Context, p LeverPull) (LeverChange, error) {
	now := s.now().Unix()
	reason, err := leverText(p.Reason, "--reason", leverReasonBytes)
	if err != nil {
		return LeverChange{}, err
	}
	actor := p.Actor
	if actor == "" {
		actor = "operator"
	}
	if actor, err = leverText(actor, "--actor", leverActorBytes); err != nil {
		return LeverChange{}, err
	}
	if p.Until != 0 && (p.Until <= now || p.Until > now+leverUntilMaxSeconds) {
		return LeverChange{}, fmt.Errorf("--until must be a UNIX time in seconds after now (%d) and within a year", now)
	}
	want := map[string]int{LeverTier4Shrink: 1, LeverCutBudget: 2, LeverBlockPrefix: 1}[p.Name]
	if !slices.Contains(LeverNames, p.Name) {
		return LeverChange{}, fmt.Errorf("unknown lever %q; levers: %s", p.Name, strings.Join(LeverNames, ", "))
	}
	if len(p.Args) != want {
		return LeverChange{}, fmt.Errorf("%s takes %d argument(s)", p.Name, want)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LeverChange{}, err
	}
	defer tx.Rollback()
	change := LeverChange{Action: "pull", Name: p.Name, Until: p.Until}
	if p.Name == LeverBlockPrefix {
		if change.Args, err = s.blockPrefix(ctx, tx, p.Args[0], reason, p.Until, now); err != nil {
			return LeverChange{}, err
		}
	} else {
		row := p.Name
		change.Args = map[string]any{}
		switch p.Name {
		case LeverTier4Shrink:
			ppm, err := parsePPM(p.Args[0], "tier4-shrink PPM", 0)
			if err != nil {
				return LeverChange{}, err
			}
			change.Args["ppm"] = ppm
		case LeverCutBudget:
			resource := allowance.Resource(p.Args[0])
			if resource != allowance.PostBytes && resource != allowance.MemoryBytes && resource != allowance.Credit {
				return LeverChange{}, fmt.Errorf("cut-budget RESOURCE is post_bytes, memory_bytes or credit")
			}
			ppm, err := parsePPM(p.Args[1], "cut-budget PPM", 1)
			if err != nil {
				return LeverChange{}, err
			}
			change.Args["resource"], change.Args["ppm"] = string(resource), ppm
			row = LeverCutBudget + ":" + string(resource)
		}
		args, _ := json.Marshal(change.Args)
		if _, err = tx.ExecContext(ctx, `INSERT INTO levers(name,state,args,pulled_at,until,released_at,actor,reason) VALUES(?,'pulled',?,?,?,0,?,?)
 ON CONFLICT(name) DO UPDATE SET pulled_at=CASE WHEN state='pulled' AND (until=0 OR until>?) THEN pulled_at ELSE excluded.pulled_at END,
 state='pulled',args=excluded.args,until=excluded.until,released_at=0,actor=excluded.actor,reason=excluded.reason`, row, string(args), now, p.Until, actor, reason, now); err != nil {
			return LeverChange{}, err
		}
	}
	args, _ := json.Marshal(change.Args)
	if change.Seq, err = logLeverSeq(ctx, tx, p.Name, "pull", string(args), actor, reason, now); err != nil {
		return LeverChange{}, err
	}
	return change, tx.Commit()
}

func logLeverSeq(ctx context.Context, tx *sql.Tx, name, action, args, actor, reason string, now int64) (int64, error) {
	res, err := tx.ExecContext(ctx, "INSERT INTO lever_log(name,action,args,actor,reason,created_at) VALUES(?,?,?,?,?,?)", name, action, args, actor, reason, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// blockPrefix stores a blocked prefix (or renews the same active one) and
// returns its public arguments.
func (s *Store) blockPrefix(ctx context.Context, tx *sql.Tx, cidr, reason string, until, now int64) (map[string]any, error) {
	p, err := parseBlockPrefix(cidr)
	if err != nil {
		return nil, err
	}
	if strings.Contains(reason, p.Addr().String()) || strings.Contains(reason, strings.TrimSpace(cidr)) {
		return nil, errors.New("--reason is public: do not include the prefix in it")
	}
	key, err := prefixKey(ctx, tx)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p.String()))
	hash := hex.EncodeToString(mac.Sum(nil))[:32]
	var id int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM blocked_prefixes WHERE cidr=? AND released_at=0 AND (until=0 OR until>?)", p.String(), now).Scan(&id)
	switch {
	case err == nil:
		if _, err = tx.ExecContext(ctx, "UPDATE blocked_prefixes SET reason=?,until=? WHERE id=?", reason, until, id); err != nil {
			return nil, err
		}
	case errors.Is(err, sql.ErrNoRows):
		var active int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM blocked_prefixes WHERE released_at=0 AND (until=0 OR until>?)", now).Scan(&active); err != nil {
			return nil, err
		}
		if active >= BlockedPrefixesMax {
			return nil, fmt.Errorf("at most %d prefixes may be blocked at once; release one first", BlockedPrefixesMax)
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO blocked_prefixes(cidr,bits,keyed_hash,reason,created_at,until) VALUES(?,?,?,?,?,?)", p.String(), p.Bits(), hash, reason, now, until)
		if err != nil {
			return nil, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return map[string]any{"id": id, "bits": p.Bits(), "keyed_hash": hash}, nil
}

// ReleaseLever releases a pulled lever: cut-budget names its RESOURCE,
// block-prefix the blocked prefix's public id or its CIDR.
func (s *Store) ReleaseLever(ctx context.Context, name string, args []string, actor, reason string) (LeverChange, error) {
	now := s.now().Unix()
	if !slices.Contains(LeverNames, name) {
		return LeverChange{}, fmt.Errorf("unknown lever %q; levers: %s", name, strings.Join(LeverNames, ", "))
	}
	want := 0
	if name == LeverCutBudget || name == LeverBlockPrefix {
		want = 1
	}
	if len(args) != want {
		return LeverChange{}, fmt.Errorf("lever release %s takes %d argument(s)", name, want)
	}
	if reason == "" {
		reason = "released"
	}
	var err error
	if reason, err = leverText(reason, "--reason", leverReasonBytes); err != nil {
		return LeverChange{}, err
	}
	if actor == "" {
		actor = "operator"
	}
	if actor, err = leverText(actor, "--actor", leverActorBytes); err != nil {
		return LeverChange{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LeverChange{}, err
	}
	defer tx.Rollback()
	change := LeverChange{Action: "release", Name: name}
	var logArgs string
	if name == LeverBlockPrefix {
		var id int64
		if n, err := strconv.ParseInt(args[0], 10, 64); err == nil {
			id = n
		} else if p, err := parseBlockPrefix(args[0]); err == nil {
			if err = tx.QueryRowContext(ctx, "SELECT id FROM blocked_prefixes WHERE cidr=? AND released_at=0 ORDER BY id DESC LIMIT 1", p.String()).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return LeverChange{}, err
			}
		} else {
			return LeverChange{}, errors.New("lever release block-prefix takes the prefix's id (from lever list) or its CIDR")
		}
		res, err := tx.ExecContext(ctx, "UPDATE blocked_prefixes SET released_at=? WHERE id=? AND released_at=0 AND (until=0 OR until>?)", now, id, now)
		if err != nil {
			return LeverChange{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return LeverChange{}, errors.New("that prefix is not blocked")
		}
		if logArgs, err = prefixLogArgs(ctx, tx, id); err != nil {
			return LeverChange{}, err
		}
	} else {
		row := name
		if name == LeverCutBudget {
			row = name + ":" + args[0]
		}
		err := tx.QueryRowContext(ctx, "UPDATE levers SET state='released',released_at=? WHERE name=? AND state='pulled' AND (until=0 OR until>?) RETURNING args", now, row, now).Scan(&logArgs)
		if errors.Is(err, sql.ErrNoRows) {
			return LeverChange{}, fmt.Errorf("%s is not pulled", strings.Join(append([]string{name}, args...), " "))
		}
		if err != nil {
			return LeverChange{}, err
		}
	}
	_ = json.Unmarshal([]byte(logArgs), &change.Args)
	if change.Seq, err = logLeverSeq(ctx, tx, name, "release", logArgs, actor, reason, now); err != nil {
		return LeverChange{}, err
	}
	return change, tx.Commit()
}

// ExpireLevers logs every lever and blocked prefix whose until has passed.
// They stop acting at until whether or not this has run; admit also runs it
// on writes.
func (s *Store) ExpireLevers(ctx context.Context) (int, error) {
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	snap, err := s.leverSnapshot(ctx, tx)
	if err != nil {
		return 0, err
	}
	n, err := expireLevers(ctx, tx, snap, now)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// LeverView is one lever in the public report.
type LeverView struct {
	Name       string         `json:"name"`
	Args       map[string]any `json:"args"`
	State      string         `json:"state"` // "pulled", "released", "expired"
	Active     bool           `json:"active"`
	PulledAt   int64          `json:"pulled_at"`
	Until      int64          `json:"until,omitempty"`
	ReleasedAt int64          `json:"released_at,omitempty"`
	Actor      string         `json:"actor"`
	Reason     string         `json:"reason"`
}

// BlockedPrefixView is a blocked prefix as the public sees it: its length
// and keyed hash, never the prefix.
type BlockedPrefixView struct {
	ID         int64  `json:"id"`
	Family     string `json:"family"`
	Bits       int    `json:"bits"`
	KeyedHash  string `json:"keyed_hash"`
	Reason     string `json:"reason"`
	CreatedAt  int64  `json:"created_at"`
	Until      int64  `json:"until,omitempty"`
	ReleasedAt int64  `json:"released_at,omitempty"`
	Active     bool   `json:"active"`
}

// LeverLogEntry is one lever_log row.
type LeverLogEntry struct {
	Seq       int64          `json:"seq"`
	Name      string         `json:"name"`
	Action    string         `json:"action"`
	Args      map[string]any `json:"args"`
	Actor     string         `json:"actor"`
	Reason    string         `json:"reason"`
	CreatedAt int64          `json:"created_at"`
}

// LeverReport is /api/levers and `swarmmemo lever list`: every lever row,
// the active blocked prefixes and the last released ones, the newest log
// entries, and the effective allowance.Levers.
type LeverReport struct {
	Schema          int                 `json:"schema"`
	Version         int64               `json:"version"`
	AsOf            int64               `json:"as_of"`
	Pulled          []string            `json:"pulled"`
	Levers          []LeverView         `json:"levers"`
	BlockedPrefixes []BlockedPrefixView `json:"blocked_prefixes"`
	Log             []LeverLogEntry     `json:"log"`
	Names           []string            `json:"names"`
}

// LeverReport reads the public lever state in one read transaction.
func (s *Store) LeverReport(ctx context.Context) (LeverReport, error) {
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LeverReport{}, err
	}
	defer tx.Rollback()
	snap, err := s.leverSnapshot(ctx, tx)
	if err != nil {
		return LeverReport{}, err
	}
	out := LeverReport{Schema: 1, Version: snap.version, AsOf: now, Pulled: []string{}, Levers: []LeverView{}, BlockedPrefixes: []BlockedPrefixView{}, Log: []LeverLogEntry{}, Names: LeverNames}
	for _, r := range snap.rows {
		name, _, _ := strings.Cut(r.name, ":")
		v := LeverView{Name: name, State: r.state, Active: leverActive(r.state, r.until, now), PulledAt: r.pulledAt, Until: r.until, ReleasedAt: r.releasedAt, Actor: r.actor, Reason: r.reason}
		_ = json.Unmarshal([]byte(r.args), &v.Args)
		if r.state == "pulled" && !v.Active || r.state == "released" && r.until > 0 && r.releasedAt == r.until {
			v.State = "expired"
			v.ReleasedAt = r.until
		}
		if v.Active && !slices.Contains(out.Pulled, name) {
			out.Pulled = append(out.Pulled, name)
		}
		out.Levers = append(out.Levers, v)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,cidr,bits,keyed_hash,reason,created_at,until,released_at FROM
 (SELECT * FROM blocked_prefixes WHERE released_at=0 UNION ALL SELECT * FROM (SELECT * FROM blocked_prefixes WHERE released_at>0 ORDER BY id DESC LIMIT ?))
 ORDER BY id DESC`, leverReleasedShown)
	if err != nil {
		return LeverReport{}, err
	}
	for rows.Next() {
		var v BlockedPrefixView
		var cidr string
		if err = rows.Scan(&v.ID, &cidr, &v.Bits, &v.KeyedHash, &v.Reason, &v.CreatedAt, &v.Until, &v.ReleasedAt); err != nil {
			rows.Close()
			return LeverReport{}, err
		}
		v.Family = "ipv6"
		if p, err := netip.ParsePrefix(cidr); err == nil && p.Addr().Is4() {
			v.Family = "ipv4"
		}
		v.Active = v.ReleasedAt == 0 && (v.Until == 0 || v.Until > now)
		if !v.Active && v.ReleasedAt == 0 {
			v.ReleasedAt = v.Until
		}
		if v.Active && !slices.Contains(out.Pulled, LeverBlockPrefix) {
			out.Pulled = append(out.Pulled, LeverBlockPrefix)
		}
		out.BlockedPrefixes = append(out.BlockedPrefixes, v)
	}
	if err = closeRows(rows); err != nil {
		return LeverReport{}, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT seq,name,action,args,actor,reason,created_at FROM lever_log ORDER BY seq DESC LIMIT ?", leverLogPage)
	if err != nil {
		return LeverReport{}, err
	}
	for rows.Next() {
		var e LeverLogEntry
		var args string
		if err = rows.Scan(&e.Seq, &e.Name, &e.Action, &args, &e.Actor, &e.Reason, &e.CreatedAt); err != nil {
			rows.Close()
			return LeverReport{}, err
		}
		_ = json.Unmarshal([]byte(args), &e.Args)
		out.Log = append(out.Log, e)
	}
	if err = closeRows(rows); err != nil {
		return LeverReport{}, err
	}
	slices.Sort(out.Pulled)
	return out, nil
}

// ActiveLevers is the sorted names of the levers acting now and the lever
// version, from the cached snapshot (one indexed read when nothing changed),
// for /capabilities.
func (s *Store) ActiveLevers(ctx context.Context) ([]string, int64, error) {
	now := s.now().Unix()
	snap, err := s.leverSnapshot(ctx, s.db)
	if err != nil {
		return nil, 0, err
	}
	pulled := []string{}
	for _, r := range snap.rows {
		name, _, _ := strings.Cut(r.name, ":")
		if leverActive(r.state, r.until, now) && !slices.Contains(pulled, name) {
			pulled = append(pulled, name)
		}
	}
	for _, p := range snap.prefixes {
		if p.releasedAt == 0 && (p.until == 0 || p.until > now) {
			pulled = append(pulled, LeverBlockPrefix)
			break
		}
	}
	slices.Sort(pulled)
	return pulled, snap.version, nil
}
