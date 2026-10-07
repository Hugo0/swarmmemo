package board

// RFC0012 Design 0 (§6, §10): tiered byte caps, anonymous keying per salted
// network prefix, reserved handles, the name gate, heat by signed authors and
// the Design 0 classifier. Every case also runs, or is checked, with the flags
// off, where today's behaviour must hold exactly.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
)

var anonPseudonymRE = regexp.MustCompile(`^anon:[0-9a-f]{32}$`)

func d0At(s *Store, unix int64) { s.now = func() time.Time { return time.Unix(unix, 0) } }

func d0Exec(s *Store, source string, c Command) (Result, error) {
	return s.Execute(testContext, c, source)
}

func d0Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// d0Identity registers key without spending allowance, as if it had written
// before the flags were turned on.
func d0Identity(t *testing.T, s *Store, key ed25519.PrivateKey) string {
	t.Helper()
	id := keyID(key)
	pub := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	if _, err := s.db.Exec("INSERT OR IGNORE INTO identities(id,public_key,account,created_at,last_seen) VALUES(?,?,?,?,?)",
		id, pub, id, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	return id
}

func d0Charge(s *Store, a actor, cost int64) error {
	tx, err := s.db.BeginTx(testContext, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.charge(testContext, tx, a, cost, s.now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func d0Signed(key ed25519.PrivateKey) actor {
	id := keyID(key)
	return actor{id: id, account: id, signed: true}
}

func d0Anon(name string) actor { return actor{id: "anonymous", account: "anon:" + name} }

func d0Used(t *testing.T, s *Store, actorName string) int64 {
	t.Helper()
	return sqlCount(t, s, "SELECT coalesce(sum(used),0) FROM quota WHERE actor=? AND day=?", actorName, s.now().Unix()/86400)
}

func TestDesign0FlagsOffKeepsTodaysKeying(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1})
	if Design0Capabilities(Features{}) != nil {
		t.Fatal("design0 capability shown with every flag off")
	}
	src := "203.0.113.5"
	res, err := d0Exec(s, src, Command{Operation: "post", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var account string
	if err = s.db.QueryRow("SELECT account FROM events WHERE id=?", res.Receipt.ID).Scan(&account); err != nil {
		t.Fatal(err)
	}
	if account != "anon:"+fingerprint([]byte(src)) {
		t.Fatalf("flags off changed anonymous keying: %s", account)
	}
	if s.anonymousClient(testContext, src, s.now().Unix()) != "" || s.previousAnonymousAccount(src, testTime, AnonPrefixV6Bits) != "" {
		t.Fatal("client descriptor or previous pseudonym with ANON_PREFIX off")
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM meta WHERE key LIKE 'anon_salt%'"); n != 0 {
		t.Fatalf("salt stored with ANON_PREFIX off: %d rows", n)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM quota WHERE actor LIKE 'global:t%'"); n != 0 {
		t.Fatalf("tier rows with ALLOWANCE_TIERS off: %d", n)
	}
	// A tier grant has no effect while ALLOWANCE_TIERS is off.
	key := keyFor(90)
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: "anon-bob"}))
	if err = s.GrantTier(testContext, keyID(key), 1, "test"); err != nil {
		t.Fatal(err)
	}
	if q := run(t, s, signed(key, Command{Operation: "quota.get"})); q.Data["daily_bytes"] != int64(4<<20) {
		t.Fatalf("quota.get with tiers off: %v", q.Data)
	}
	// Room creation and new rooms by post stay open to every signed tier.
	other := keyFor(91)
	run(t, s, signed(other, Command{Operation: "room.create", Room: "den"}))
	run(t, s, signed(other, Command{Operation: "post", Room: "new-by-post", Text: "hi", Handle: "k-other"}))
	if h := handleOf(t, s, keyID(other)); h != "k-other" {
		t.Fatalf("handle claim changed with flags off: %q", h)
	}
}

func TestAnonPrefixKey(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"203.0.113.5", "ip:203.0.113.0/24"},
		{"203.0.113.255", "ip:203.0.113.0/24"},
		{"::ffff:203.0.113.9", "ip:203.0.113.0/24"},
		{"2001:db8:1:2::1", "ip:2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:fffe", "ip:2001:db8:1:2::/64"},
		{"fe80::1%eth0", "ip:fe80::/64"},
		{"nostr-bridge", "src:nostr-bridge"},
		{"", "src:"},
		{"[::1]", "src:[::1]"},
		{"ip:1.2.3.0/24", "src:ip:1.2.3.0/24"},
	} {
		if got := anonPrefixKey(tc.source); got != tc.want {
			t.Fatalf("%q: %q, want %q", tc.source, got, tc.want)
		}
	}
}

func FuzzAnonPrefix(f *testing.F) {
	for _, seed := range []string{"203.0.113.5", "::ffff:1.2.3.4", "2001:db8::1", "fe80::1%eth0", "nostr-bridge", "", "\x00", "1.2.3.4.5", strings.Repeat("a", 4096)} {
		f.Add(seed)
	}
	salt := make([]byte, anonSaltBytes)
	f.Fuzz(func(t *testing.T, source string) {
		key := anonPrefixKey(source)
		switch {
		case strings.HasPrefix(key, "ip:"):
			p, err := netip.ParsePrefix(key[3:])
			if err != nil {
				t.Fatalf("%q gave unparsable prefix %q", source, key)
			}
			addr, err := netip.ParseAddr(source)
			if err != nil || !p.Contains(addr.WithZone("").Unmap()) || p != p.Masked() {
				t.Fatalf("%q not inside its prefix %q", source, key)
			}
			if bits := p.Bits(); bits != AnonPrefixV4Bits && bits != AnonPrefixV6Bits {
				t.Fatalf("%q: prefix length %d", source, bits)
			}
		case key == "src:"+source:
		default:
			t.Fatalf("%q: key %q", source, key)
		}
		name := anonPseudonym(salt, key)
		if !anonPseudonymRE.MatchString(name) || name != anonPseudonym(salt, anonPrefixKey(source)) {
			t.Fatalf("%q: pseudonym %q", source, name)
		}
	})
}

func TestAnonPrefixOneSubjectPerPrefix(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, AnonymousDailyBytes: 1500, Features: Features{AnonPrefix: true}})
	d0At(s, testTime+3*3600)
	// One IPv6 host rotating through its /64 is one subject with one cap.
	for i, src := range []string{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:fffe"} {
		if _, err := d0Exec(s, src, Command{Operation: "post", Text: fmt.Sprint("rotating ", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d0Exec(s, "2001:db8:1:2::abcd", Command{Operation: "post", Text: "third"}); d0Code(err) != "quota_exhausted" {
		t.Fatalf("third post from the same /64: %v", err)
	}
	if _, err := d0Exec(s, "2001:db8:1:3::1", Command{Operation: "post", Text: "neighbour"}); err != nil {
		t.Fatalf("another /64 shares the cap: %v", err)
	}
	// IPv4 per /24, and an IPv4-mapped address is the same subject.
	a := s.anonymousAccount("198.51.100.7", s.now().Unix(), AnonPrefixV6Bits)
	if a != s.anonymousAccount("198.51.100.200", s.now().Unix(), AnonPrefixV6Bits) || a != s.anonymousAccount("::ffff:198.51.100.1", s.now().Unix(), AnonPrefixV6Bits) || a == s.anonymousAccount("198.51.101.7", s.now().Unix(), AnonPrefixV6Bits) {
		t.Fatal("IPv4 prefixes not keyed per /24")
	}
	if !anonPseudonymRE.MatchString(a) {
		t.Fatalf("pseudonym %q", a)
	}
	rows, err := s.db.Query("SELECT DISTINCT account FROM events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			t.Fatal(err)
		}
		if !anonPseudonymRE.MatchString(account) {
			t.Fatalf("stored account %q", account)
		}
	}
}

func TestAnonClientOnlyNarrows(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, AnonymousDailyBytes: 1500, Features: Features{AnonPrefix: true}})
	src := "203.0.113.40"
	curl, python := WithClient(testContext, "curl"), WithClient(testContext, "python-requests")
	if s.anonymousClient(curl, src, s.now().Unix()) == s.anonymousClient(python, src, s.now().Unix()) || s.anonymousClient(curl, src, s.now().Unix()) != s.anonymousClient(curl, "203.0.113.41", s.now().Unix()) {
		t.Fatal("client descriptor does not follow the User-Agent product within a prefix")
	}
	if c := s.anonymousClient(WithClient(testContext, strings.Repeat("x", 33)), src, s.now().Unix()); c != s.anonymousClient(testContext, src, s.now().Unix()) || len(c) != 32 {
		t.Fatal("an overlong product widened the descriptor")
	}
	// Varying the User-Agent never gives a prefix more than its cap.
	for i, ctx := range []context.Context{curl, python} {
		if _, err := s.Execute(ctx, Command{Operation: "post", Text: fmt.Sprint("ua ", i)}, src); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Execute(WithClient(testContext, "Wget"), Command{Operation: "post", Text: "third"}, src); d0Code(err) != "quota_exhausted" {
		t.Fatalf("a new User-Agent widened the prefix's cap: %v", err)
	}
}

func TestAnonSaltRotatesDailyAndIsDestroyed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	on := Config{Features: Features{AnonPrefix: true}}
	open := func(c Config, now int64) *Store {
		s, err := Open(path, c)
		if err != nil {
			t.Fatal(err)
		}
		d0At(s, now)
		return s
	}
	stored := func(s *Store) int64 {
		return sqlCount(t, s, "SELECT count(*) FROM meta WHERE key LIKE 'anon_salt%'")
	}
	src := "198.51.100.7"
	// A salt an earlier build stored in meta is destroyed at Open.
	s := open(Config{}, testTime)
	if _, err := s.db.Exec("INSERT INTO meta(key,value) VALUES('anon_salt',?),('anon_salt_day','1')", strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(on, testTime+10)
	first := s.anonymousAccount(src, s.now().Unix(), AnonPrefixV6Bits)
	if first == "anon:"+fingerprint([]byte(src)) || stored(s) != 0 {
		t.Fatal("no salted pseudonym, or a salt in the database")
	}
	s.Close()
	// The salt lives in memory only, so a backup cannot recompute a
	// pseudonym: a restart the same day starts a new one.
	s = open(on, testTime+7200)
	restarted := s.anonymousAccount(src, s.now().Unix(), AnonPrefixV6Bits)
	if restarted == first || stored(s) != 0 {
		t.Fatal("a restart recovered the day's salt")
	}
	// The next day's pseudonym differs; yesterday's is kept until 01:00 UTC.
	d0At(s, testTime+86400+60)
	second := s.anonymousAccount(src, s.now().Unix(), AnonPrefixV6Bits)
	if second == restarted || s.previousAnonymousAccount(src, testTime+86400+60, AnonPrefixV6Bits) != restarted || stored(s) != 0 {
		t.Fatal("rotation lost the previous salt inside the grace")
	}
	// After 01:00 the timer (or the next anonymous request) destroys it.
	s.expireSalt(testTime + 86400 + AnonSaltGraceSeconds + 1)
	if s.previousAnonymousAccount(src, testTime+86400+AnonSaltGraceSeconds+1, AnonPrefixV6Bits) != "" || s.design0.salts.prev != nil {
		t.Fatal("yesterday's salt survived 01:00")
	}
	// A day with no anonymous request destroys the day-old current salt too.
	s.expireSalt(testTime + 2*86400 + AnonSaltGraceSeconds + 1)
	if s.design0.salts.cur != nil {
		t.Fatal("a stale current salt survived")
	}
	d0At(s, testTime+2*86400+AnonSaltGraceSeconds+2)
	if third := s.anonymousAccount(src, s.now().Unix(), AnonPrefixV6Bits); third == second || stored(s) != 0 {
		t.Fatal("a new day reused a salt or stored one")
	}
	s.Close()
}

func TestAnonSaltConcurrentFirstUse(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: true}})
	d0At(s, testTime+86400+60)
	names := make(chan string, 16)
	done := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			_, err := d0Exec(s, fmt.Sprintf("203.0.113.%d", i), Command{Operation: "post", Text: fmt.Sprint("race ", i)})
			names <- s.anonymousAccount("203.0.113.1", s.now().Unix(), AnonPrefixV6Bits)
			done <- err
		}(i)
	}
	first := <-names
	for i := 0; i < 16; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if n := <-names; n != first {
				t.Fatal("concurrent first use made two salts")
			}
		}
	}
	if n := sqlCount(t, s, "SELECT count(DISTINCT account) FROM events"); n != 1 {
		t.Fatalf("one /24 became %d subjects", n)
	}
}

func TestAnonRetryAcrossMidnight(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: true}})
	src := "203.0.113.9"
	d0At(s, testTime+86400-5)
	c := Command{Operation: "post", Text: "retry me", RequestID: "across-midnight"}
	first, err := d0Exec(s, src, c)
	if err != nil {
		t.Fatal(err)
	}
	d0At(s, testTime+86400+30)
	again, err := d0Exec(s, src, c)
	if err != nil || !again.Receipt.Duplicate || again.Receipt.ID != first.Receipt.ID {
		t.Fatalf("retry across midnight: %+v %v", again.Receipt, err)
	}
	// A conflicting retry is still refused under yesterday's pseudonym.
	c2 := c
	c2.Text = "different"
	if _, err = d0Exec(s, src, c2); d0Code(err) != "idempotency_conflict" {
		t.Fatalf("conflict across midnight: %v", err)
	}
	// After 01:00 the old pseudonym is gone, so the request is new.
	d0At(s, testTime+86400+AnonSaltGraceSeconds+5)
	late, err := d0Exec(s, src, c)
	if err != nil || late.Receipt.Duplicate || late.Receipt.ID == first.Receipt.ID {
		t.Fatalf("retry after 01:00: %+v %v", late.Receipt, err)
	}
}

// Invariant 6: after anonymous traffic with ANON_PREFIX on, no table contains
// an address, its unsalted hash or its prefix, and yesterday's salt is gone
// after 01:00.
func TestAnonPrefixStoresNoAddress(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{AnonPrefix: true, AllowanceTiers: true}})
	d0At(s, testTime+600)
	sources := []string{"203.0.113.77", "2001:db8:aa:bb::5", "::ffff:198.51.100.9"}
	for i, src := range sources {
		res, err := s.Execute(WithClient(testContext, "curl"), Command{Operation: "post", Text: fmt.Sprint("anon ", i), RequestID: fmt.Sprint("r", i)}, src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d0Exec(s, src, Command{Operation: "report", MessageID: res.Receipt.ID, Reason: "test"}); err != nil {
			t.Fatal(err)
		}
		if _, err = d0Exec(s, src, Command{Operation: "quota.get"}); err != nil {
			t.Fatal(err)
		}
	}
	salt := hex.EncodeToString(s.design0.salts.cur)
	forbidden := []string{"203.0.113", "2001:db8:aa:bb", "198.51.100"}
	for _, src := range append(sources, "198.51.100.9") {
		forbidden = append(forbidden, src, fingerprint([]byte(src)), anonPrefixKey(src), fingerprint([]byte(anonPrefixKey(src))))
	}
	scan := func(extra ...string) {
		t.Helper()
		tables, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table'")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for tables.Next() {
			var name string
			if err = tables.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		tables.Close()
		for _, table := range names {
			rows, err := s.db.Query(`SELECT * FROM "` + table + `"`)
			if err != nil {
				t.Fatal(err)
			}
			columns, _ := rows.Columns()
			for rows.Next() {
				values := make([]any, len(columns))
				ptrs := make([]any, len(columns))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err = rows.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				for i, v := range values {
					text := fmt.Sprint(v)
					if b, ok := v.([]byte); ok {
						text = string(b)
					}
					for _, bad := range append(forbidden, extra...) {
						if bad != "" && strings.Contains(text, bad) {
							t.Fatalf("%s.%s holds %q", table, columns[i], bad)
						}
					}
				}
			}
			rows.Close()
		}
	}
	scan()
	d0At(s, testTime+86400+AnonSaltGraceSeconds+1)
	s.expireSalt(testTime + 86400 + AnonSaltGraceSeconds + 1)
	if s.design0.salts.cur != nil {
		t.Fatal("day-old salt kept in memory after 01:00")
	}
	scan(salt)
}

func TestTierCapsNested(t *testing.T) {
	config := Config{ArchiveDelaySeconds: -1, GlobalDailyBytes: 10000, DailyBytes: 4000, AnonymousDailyBytes: 4000, Features: Features{AllowanceTiers: true}}
	s := openTest(t, config)
	k1, k2 := keyFor(101), keyFor(102)
	d0Identity(t, s, k1)
	d0Identity(t, s, k2)
	if err := s.GrantTier(testContext, keyID(k1), 1, "trusted test"); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantTier(testContext, keyID(k2), 2, "proven test"); err != nil {
		t.Fatal(err)
	}
	// A flood of tier-4 subjects at 00:00 fills only tier 4's half.
	refused := 0
	for i := 0; i < 20; i++ {
		if err := d0Charge(s, d0Anon(fmt.Sprint("flood", i)), 1000); err != nil {
			if d0Code(err) != "global_quota_exhausted" {
				t.Fatal(err)
			}
			refused++
		}
	}
	if d0Used(t, s, "global:t4") != 5000 || refused != 15 {
		t.Fatalf("tier 4 used %d, refused %d", d0Used(t, s, "global:t4"), refused)
	}
	// Fresh signed keys then fill tier 3 up to 80% with tier 4.
	for i := 0; i < 20; i++ {
		err := d0Charge(s, d0Signed(keyFor(byte(120+i))), 1000)
		if err != nil && d0Code(err) != "global_quota_exhausted" {
			t.Fatal(err)
		}
	}
	if d0Used(t, s, "global:t3") != 3000 {
		t.Fatalf("tier 3 used %d", d0Used(t, s, "global:t3"))
	}
	// Tiers 1 and 2 still draw their reserves in full.
	if err := d0Charge(s, d0Signed(k2), 1000); err != nil {
		t.Fatalf("tier 2 reserve: %v", err)
	}
	if err := d0Charge(s, d0Signed(k2), 1); d0Code(err) != "global_quota_exhausted" {
		t.Fatalf("tier 2 past 90%%: %v", err)
	}
	if err := d0Charge(s, d0Signed(k1), 1000); err != nil {
		t.Fatalf("tier 1 reserve: %v", err)
	}
	if err := d0Charge(s, d0Signed(k1), 1); d0Code(err) != "global_quota_exhausted" {
		t.Fatalf("tier 1 past the budget: %v", err)
	}
	if d0Used(t, s, "global") != 10000 || d0Used(t, s, "global") != d0Used(t, s, "global:t1")+d0Used(t, s, "global:t2")+d0Used(t, s, "global:t3")+d0Used(t, s, "global:t4") {
		t.Fatal("the global row is not the sum of the tier rows")
	}

	// Per-subject caps: d_t = 16, 8, 4, 4 × the configured units.
	s = openTest(t, Config{ArchiveDelaySeconds: -1, GlobalDailyBytes: 100000, DailyBytes: 4000, AnonymousDailyBytes: 3000, Features: Features{AllowanceTiers: true}})
	d0Identity(t, s, k1)
	d0Identity(t, s, k2)
	_ = s.GrantTier(testContext, keyID(k1), 1, "trusted test")
	_ = s.GrantTier(testContext, keyID(k2), 2, "proven test")
	k3 := keyFor(103)
	for _, tc := range []struct {
		a   actor
		cap int64
	}{{d0Signed(k1), 16000}, {d0Signed(k2), 8000}, {d0Signed(k3), 4000}, {d0Anon("one"), 3000}} {
		if err := d0Charge(s, tc.a, tc.cap); err != nil {
			t.Fatalf("%s up to its cap: %v", tc.a.account, err)
		}
		if err := d0Charge(s, tc.a, 1); d0Code(err) != "quota_exhausted" {
			t.Fatalf("%s past its cap: %v", tc.a.account, err)
		}
	}
	for _, tc := range []struct {
		key  ed25519.PrivateKey
		want int64
	}{{k1, 16000}, {k2, 8000}, {k3, 4000}} {
		q := run(t, s, signed(tc.key, Command{Operation: "quota.get"}))
		if q.Data["daily_bytes"] != tc.want || q.Data["remaining_bytes"] != int64(0) {
			t.Fatalf("quota.get %v, want daily %d", q.Data, tc.want)
		}
	}
	// A transfer respects the sender's tier cap.
	k4 := keyFor(104)
	d0Identity(t, s, k3)
	d0Identity(t, s, k4)
	_ = s.GrantTier(testContext, keyID(k4), 1, "trusted sender")
	run(t, s, signed(k4, Command{Operation: "credit.transfer", Target: keyID(k3), Amount: 10000}))
}

func TestDesign0Classifier(t *testing.T) {
	s := openTest(t, Config{})
	now := testTime
	classify := func(subj allowance.Subject) allowance.Standing {
		t.Helper()
		st, err := s.classifier().Classify(testContext, s.db, subj, now)
		if err != nil {
			t.Fatal(err)
		}
		if st.WeightPPM != 1_000_000 || st.Source != "design0" || st.Reason == "" {
			t.Fatalf("standing %+v", st)
		}
		return st
	}
	if st := classify(allowance.Subject{ID: "anon:abc"}); st.Tier != allowance.TierAnonymous || st.Root != "anon:abc" {
		t.Fatalf("anonymous: %+v", st)
	}
	key := keyFor(110)
	id := d0Identity(t, s, key)
	subj := allowance.Subject{ID: id, KeyID: id, Signed: true}
	if st := classify(subj); st.Tier != allowance.TierSigned || st.Root != id || st.NewKey {
		t.Fatalf("signed: %+v", st)
	}
	// A verified domain link checked in the last 30 days is proven.
	if _, err := s.db.Exec("INSERT INTO identity_links(agent,kind,value,state,created_at,checked_at) VALUES(?,?,?,?,?,?)", id, "domain", "example.com", "verified", now-40*86400, now-86400); err != nil {
		t.Fatal(err)
	}
	if st := classify(subj); st.Tier != allowance.TierProven || st.Root != "domain:example.com" {
		t.Fatalf("domain: %+v", st)
	}
	for _, update := range []string{"UPDATE identity_links SET checked_at=? WHERE agent=?", "UPDATE identity_links SET state='lapsed',checked_at=? WHERE agent=?"} {
		checked := now - 31*86400
		if strings.Contains(update, "lapsed") {
			checked = now
		}
		if _, err := s.db.Exec(update, checked, id); err != nil {
			t.Fatal(err)
		}
		if st := classify(subj); st.Tier != allowance.TierSigned {
			t.Fatalf("stale or lapsed link still proven: %+v", st)
		}
	}
	// Tier grants: 1 trusted, 2 proven; revocation returns to tier 3.
	if err := s.GrantTier(testContext, id, 1, "trusted"); err != nil {
		t.Fatal(err)
	}
	if st := classify(subj); st.Tier != allowance.TierTrusted {
		t.Fatalf("grant 1: %+v", st)
	}
	if err := s.GrantTier(testContext, id, 2, "proven"); err != nil {
		t.Fatal(err)
	}
	if st := classify(subj); st.Tier != allowance.TierProven {
		t.Fatalf("grant 2: %+v", st)
	}
	if err := s.RevokeTier(testContext, id, "done"); err != nil {
		t.Fatal(err)
	}
	if st := classify(subj); st.Tier != allowance.TierSigned {
		t.Fatalf("revoked: %+v", st)
	}
	// A rotated-away key's link does not count for the account.
	if _, err := s.db.Exec("UPDATE identity_links SET state='verified',checked_at=? WHERE agent=?", now, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE identities SET successor='x' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if st := classify(subj); st.Tier != allowance.TierSigned {
		t.Fatalf("rotated-away link: %+v", st)
	}
}

func TestTierGrantList(t *testing.T) {
	s := openTest(t, Config{})
	if err := s.GrantTier(testContext, keyID(keyFor(1)), 1, "unknown"); d0Code(err) != "agent_not_found" {
		t.Fatalf("unregistered agent: %v", err)
	}
	ids := []string{}
	for i := 0; i < TierGrantsMax+1; i++ {
		ids = append(ids, d0Identity(t, s, keyFor(byte(i+1))))
	}
	for _, bad := range []struct {
		tier   int
		reason string
	}{{3, "x"}, {0, "x"}, {1, ""}, {1, " "}, {1, "two\nlines"}, {1, strings.Repeat("x", TierGrantReasonBytes+1)}, {1, "\xff"}} {
		if err := s.GrantTier(testContext, ids[0], bad.tier, bad.reason); err == nil {
			t.Fatalf("grant %+v accepted", bad)
		}
	}
	for _, id := range ids[:TierGrantsMax] {
		if err := s.GrantTier(testContext, id, 2, "listed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.GrantTier(testContext, ids[TierGrantsMax], 1, "one too many"); err == nil {
		t.Fatal("the tier list grew past its maximum")
	}
	if err := s.GrantTier(testContext, ids[0], 1, "promoted"); err != nil {
		t.Fatalf("regrant at the maximum: %v", err)
	}
	if err := s.RevokeTier(testContext, ids[1], "removed"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeTier(testContext, ids[1], "again"); err == nil {
		t.Fatal("revoked twice")
	}
	if err := s.GrantTier(testContext, ids[TierGrantsMax], 1, "room now"); err != nil {
		t.Fatal(err)
	}
	grants, err := s.TierGrants(testContext)
	if err != nil || len(grants) != TierGrantsMax || grants[0].Tier != 1 {
		t.Fatalf("list: %d %v", len(grants), err)
	}
	// Revocation marks, never deletes; every change is logged.
	if sqlCount(t, s, "SELECT count(*) FROM tier_grants") != TierGrantsMax+1 || sqlCount(t, s, "SELECT count(*) FROM tier_grant_log") != TierGrantsMax+3 {
		t.Fatal("tier list rows or log wrong")
	}
	log, err := s.TierGrantLog(testContext, 2)
	if err != nil || len(log) != 2 || log[0].Action != "grant" || log[1].Action != "revoke" {
		t.Fatalf("log: %+v %v", log, err)
	}
}

func TestReservedHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	s, err := Open(path, Config{ArchiveDelaySeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	d0At(s, testTime)
	holder := keyFor(130)
	run(t, s, signed(holder, Command{Operation: "agent.register", Handle: "Admin"}))
	s.Close()
	s, err = Open(path, Config{ArchiveDelaySeconds: -1, Features: Features{ReservedHandles: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d0At(s, testTime)
	key := keyFor(131)
	for _, handle := range []string{"anon-bob", "K-deadbeef", "NickServ", "guides", "steward", "admin"} {
		fails(t, s, signed(key, Command{Operation: "agent.register", Handle: handle}), "handle_reserved")
	}
	run(t, s, signed(key, Command{Operation: "agent.register", Handle: "bob"}))
	// A handle held today is kept, re-registered and used.
	run(t, s, signed(holder, Command{Operation: "agent.register", Handle: "admin"}))
	if id := run(t, s, signed(holder, Command{Operation: "post", Text: "still admin"})).Receipt.ID; storedHandle(t, s, id) != "admin" {
		t.Fatal("held reserved handle lost")
	}
	// A post's first-use claim is not applied; the post is published.
	fresh := keyFor(132)
	res := run(t, s, signed(fresh, Command{Operation: "post", Text: "hello", Handle: "anon-bob"}))
	if h := res.Receipt.HandleNotApplied; h == nil || h.Reason != "reserved" || h.Requested != "anon-bob" {
		t.Fatalf("hint %+v", h)
	}
	if storedHandle(t, s, res.Receipt.ID) != "" || handleOf(t, s, keyID(fresh)) != "" {
		t.Fatal("reserved handle claimed on post")
	}
	for _, h := range []string{"anon-", "k-1", "memo", "mcp", "operserv", "Security"} {
		if !reservedHandle(h) {
			t.Fatalf("%q not reserved", h)
		}
	}
	for _, h := range []string{"", "anon", "anon_x", "k", "kitty", "anonymous-coward", "archive-curator", "bob"} {
		if reservedHandle(h) {
			t.Fatalf("%q reserved", h)
		}
	}
}

func TestNameGate(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: Features{NameGate: true, ReservedHandles: true}})
	trusted, proven, plain := keyFor(140), keyFor(141), keyFor(142)
	d0Identity(t, s, trusted)
	d0Identity(t, s, proven)
	_ = s.GrantTier(testContext, keyID(trusted), 1, "trusted")
	_ = s.GrantTier(testContext, keyID(proven), 2, "proven")
	run(t, s, signed(trusted, Command{Operation: "post", Room: "lobby", Text: "opening the lobby"}))

	// Tier 3: no new room names, by command or by post; nothing is published or charged.
	fails(t, s, signed(plain, Command{Operation: "room.create", Room: "den"}), "tier_required")
	fails(t, s, signed(plain, Command{Operation: "room.create", Room: "secret", Visibility: "private"}), "tier_required")
	fails(t, s, signed(plain, Command{Operation: "post", Room: "fresh-room", Text: "squat"}), "tier_required")
	if _, err := d0Exec(s, "203.0.113.1", Command{Operation: "post", Room: "anon-room", Text: "squat"}); d0Code(err) != "tier_required" {
		t.Fatalf("anonymous new room: %v", err)
	}
	if n := sqlCount(t, s, "SELECT count(*) FROM rooms WHERE name IN ('den','secret','fresh-room','anon-room')") + sqlCount(t, s, "SELECT count(*) FROM events WHERE room<>'lobby'") + sqlCount(t, s, "SELECT count(*) FROM quota WHERE actor=?", keyID(plain)); n != 0 {
		t.Fatalf("a refused name left %d rows", n)
	}
	// Existing rooms and personal rooms stay open.
	run(t, s, signed(plain, Command{Operation: "post", Room: "lobby", Text: "hi"}))
	if _, err := d0Exec(s, "203.0.113.1", Command{Operation: "post", Room: "lobby", Text: "anon hi"}); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(plain, Command{Operation: "post", Room: "@" + keyID(plain), Text: "my own room"}))
	// No new handle below the name tier: a post is published without it.
	res := run(t, s, signed(plain, Command{Operation: "post", Room: "lobby", Text: "call me", Handle: "newname"}))
	if h := res.Receipt.HandleNotApplied; h == nil || h.Reason != "tier_required" || handleOf(t, s, keyID(plain)) != "" {
		t.Fatalf("hint %+v", h)
	}
	fails(t, s, signed(plain, Command{Operation: "agent.register", Handle: "newname"}), "tier_required")
	run(t, s, signed(plain, Command{Operation: "agent.register"}))
	// Tiers 1-2 name rooms and handles; reserved handles stay reserved.
	run(t, s, signed(proven, Command{Operation: "room.create", Room: "den"}))
	run(t, s, signed(proven, Command{Operation: "post", Room: "den-two", Text: "opened"}))
	run(t, s, signed(proven, Command{Operation: "agent.register", Handle: "prover"}))
	if res := run(t, s, signed(trusted, Command{Operation: "post", Room: "lobby", Text: "named", Handle: "trusty"})); res.Receipt.HandleNotApplied != nil {
		t.Fatalf("trusted claim: %+v", res.Receipt.HandleNotApplied)
	}
	fails(t, s, signed(proven, Command{Operation: "agent.register", Handle: "k-prover"}), "handle_reserved")
	// A handle already held is kept by a later gate.
	d0Identity(t, s, keyFor(143))
	if _, err := s.db.Exec("UPDATE identities SET handle='oldtimer' WHERE id=?", keyID(keyFor(143))); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(keyFor(143), Command{Operation: "agent.register", Handle: "oldtimer"}))
}

func TestHeatAuthors(t *testing.T) {
	scenario := func(f Features) (before, after, crowd []string) {
		s := openTest(t, Config{ArchiveDelaySeconds: -1, Features: f})
		signedAt := func(key ed25519.PrivateKey, room string, at int64, text string) {
			d0At(s, at)
			run(t, s, signed(key, Command{Operation: "post", Room: room, Text: text, Timestamp: at}))
		}
		now := testTime + 86400
		signedAt(keyFor(150), "busy", now-10*3600, "old signed post")
		signedAt(keyFor(151), "mid", now-5*3600, "mid signed post")
		signedAt(keyFor(152), "calm", now-3600, "recent signed post")
		for i := 0; i < 5; i++ {
			signedAt(keyFor(153), "solo", now-2*3600, fmt.Sprint("solo ", i))
		}
		for i := 0; i < 3; i++ {
			signedAt(keyFor(byte(160+i)), "crowd", now-2*3600, fmt.Sprint("crowd ", i))
		}
		order := func(names ...string) []string {
			d0At(s, now)
			want := map[string]bool{}
			for _, n := range names {
				want[n] = true
			}
			out := []string{}
			for _, r := range run(t, s, Command{Operation: "rooms.list", Limit: 50}).Rooms {
				if want[r.Name] {
					out = append(out, r.Name)
				}
			}
			return out
		}
		before, crowd = order("busy", "mid", "calm"), order("solo", "crowd")
		d0At(s, now)
		for i := 0; i < 1000; i++ {
			if _, err := d0Exec(s, "203.0.113.50", Command{Operation: "post", Room: "busy", Text: fmt.Sprint("flood ", i)}); err != nil {
				t.Fatal(err)
			}
		}
		return before, order("busy", "mid", "calm"), crowd
	}
	join := func(v []string) string { return strings.Join(v, ",") }
	before, after, crowd := scenario(Features{HeatAuthors: true})
	if join(before) != "calm,mid,busy" || join(after) != join(before) || join(crowd) != "crowd,solo" {
		t.Fatalf("HEAT_AUTHORS: before %v after %v crowd %v", before, after, crowd)
	}
	// Flags off, heat still counts distinct authors (three voices beat one
	// loud one), but an anonymous flood keeps a room fresh.
	before, after, crowd = scenario(Features{})
	if join(before) != "calm,mid,busy" || after[0] != "busy" || join(crowd) != "crowd,solo" {
		t.Fatalf("flags off: before %v after %v crowd %v", before, after, crowd)
	}
}

func TestDesign0AllFlagsTogether(t *testing.T) {
	s := openTest(t, Config{ArchiveDelaySeconds: -1, GlobalDailyBytes: 1 << 20, AnonymousDailyBytes: 4000,
		Features: Features{ReservedHandles: true, AnonPrefix: true, AllowanceTiers: true, HeatAuthors: true, NameGate: true}})
	d0At(s, testTime+600)
	op := keyFor(170)
	d0Identity(t, s, op)
	if err := s.GrantTier(testContext, keyID(op), 1, "operator of the lobby"); err != nil {
		t.Fatal(err)
	}
	run(t, s, signed(op, Command{Operation: "post", Room: "lobby", Text: "open", Handle: "lobbyist", Timestamp: testTime + 600}))
	anon, err := d0Exec(s, "2001:db8:5:6::1", Command{Operation: "post", Text: "anonymous hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d0Exec(s, "2001:db8:5:6::2", Command{Operation: "post", Room: "new-name", Text: "x"}); d0Code(err) != "tier_required" {
		t.Fatalf("anonymous new room: %v", err)
	}
	q, err := d0Exec(s, "2001:db8:5:6::3", Command{Operation: "quota.get"})
	if err != nil || q.Data["daily_bytes"] != int64(4000) || q.Data["used_bytes"].(int64) <= 0 {
		t.Fatalf("anonymous quota across the /64: %v %v", q.Data, err)
	}
	plain := keyFor(171)
	res := run(t, s, signed(plain, Command{Operation: "post", Text: "hi", Handle: "anon-x", Timestamp: testTime + 600}))
	if res.Receipt.HandleNotApplied == nil || res.Receipt.HandleNotApplied.Reason != "reserved" {
		t.Fatalf("hint %+v", res.Receipt.HandleNotApplied)
	}
	if rooms := run(t, s, Command{Operation: "rooms.list"}).Rooms; len(rooms) != 1 || rooms[0].Name != "lobby" {
		t.Fatalf("rooms %+v", rooms)
	}
	var account string
	if err = s.db.QueryRow("SELECT account FROM events WHERE id=?", anon.Receipt.ID).Scan(&account); err != nil || !anonPseudonymRE.MatchString(account) {
		t.Fatalf("anonymous account %q %v", account, err)
	}
	if d0Used(t, s, "global:t1") == 0 || d0Used(t, s, "global:t3") == 0 || d0Used(t, s, "global:t4") == 0 {
		t.Fatal("tier rows not charged")
	}
}

func TestDesign0Capabilities(t *testing.T) {
	all := Design0Capabilities(Features{ReservedHandles: true, AnonPrefix: true, AllowanceTiers: true, HeatAuthors: true, NameGate: true})
	anon := all["anonymous"].(map[string]any)
	if anon["prefix_v6"] != AnonPrefixV6Bits || anon["prefix_v4"] != AnonPrefixV4Bits || anon["salted_daily"] != true || anon["raw_addresses_stored"] != false {
		t.Fatalf("anonymous: %v", anon)
	}
	if all["heat"] != "signed_authors" || all["name_min_tier"] != 2 || all["tier_caps"].(map[string]any)["enabled"] != true || all["reserved_handles"].(map[string]any)["enabled"] != true {
		t.Fatalf("design0: %v", all)
	}
	one := Design0Capabilities(Features{HeatAuthors: true})
	if one["heat"] != "signed_authors" || one["name_min_tier"] != nil || one["anonymous"].(map[string]any)["salted_daily"] != false ||
		one["anonymous"].(map[string]any)["prefix_v4"] != nil || one["tier_caps"].(map[string]any)["enabled"] != false || one["reserved_handles"].(map[string]any)["enabled"] != false {
		t.Fatalf("heat only: %v", one)
	}
	if Design0Capabilities(Features{Ledger: LedgerOn, VoteRecords: true}) != nil {
		t.Fatal("design0 shown for other builders' flags")
	}
}

func TestDesign0SchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.sqlite")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	// A database from before schema 16 without fragment A's tables.
	if _, err = s.db.Exec("DROP TABLE tier_grants; DROP TABLE tier_grant_log; PRAGMA user_version=15"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		if s, err = Open(path, Config{Features: Features{AllowanceTiers: true, AnonPrefix: true}}); err != nil {
			t.Fatal(err)
		}
		if v := sqlCount(t, s, "PRAGMA user_version"); v != int64(SchemaVersion) || SchemaVersion != 17 {
			t.Fatalf("user_version %d", v)
		}
		if n := sqlCount(t, s, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('tier_grants','tier_grant_log')"); n != 2 {
			t.Fatalf("fragment A tables: %d", n)
		}
		if err = s.Integrity(testContext); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}
