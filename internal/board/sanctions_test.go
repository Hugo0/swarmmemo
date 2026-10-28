package board

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/trust"
)

// sanctionWorld drives writes at a movable clock, over HTTP-like signals.
type sanctionWorld struct {
	t   *testing.T
	s   *Store
	seq int
}

func newSanctionWorld(t *testing.T) *sanctionWorld {
	return &sanctionWorld{t: t, s: openTest(t, Config{})}
}

func (w *sanctionWorld) advance(d time.Duration) {
	at := w.s.now().Add(d)
	w.s.now = func() time.Time { return at }
}

// post writes a post from source with client ua, signed by key (nil:
// anonymous); it returns the receipt id or the error.
func (w *sanctionWorld) post(key ed25519.PrivateKey, source, ua string) (string, error) {
	w.t.Helper()
	w.seq++
	id := fmt.Sprintf("r%d", w.seq)
	c := Command{Operation: "post", Room: "lobby", Text: "text " + id, RequestID: id}
	if key != nil {
		c.Timestamp = w.s.now().Unix()
		c = signed(key, c)
	}
	ctx := WithRequestSignals(WithVia(context.Background(), "command"), RequestSignals{UserAgent: ua, AcceptLanguage: "en-GB,en;q=0.9"})
	res, err := w.s.Execute(ctx, c, source)
	if err != nil {
		return "", err
	}
	return res.Receipt.ID, nil
}

func (w *sanctionWorld) mustPost(key ed25519.PrivateKey, source, ua string) string {
	w.t.Helper()
	id, err := w.post(key, source, ua)
	if err != nil {
		w.t.Fatalf("post from %s (%s): %v", source, ua, err)
	}
	return id
}

func (w *sanctionWorld) refused(key ed25519.PrivateKey, source, ua string) *Error {
	w.t.Helper()
	_, err := w.post(key, source, ua)
	var e *Error
	if !errors.As(err, &e) || e.Code != "sanctioned" || e.Status != 403 {
		w.t.Fatalf("post from %s (%s): want 403 sanctioned, got %v", source, ua, err)
	}
	return e
}

func (w *sanctionWorld) apply(msg, category string) SanctionOutcome {
	w.t.Helper()
	out, err := w.s.ApplySanction(context.Background(), msg, category, "", "operator")
	if err != nil {
		w.t.Fatalf("apply %s on %s: %v", category, msg, err)
	}
	return out
}

// The ladder: a first injection covers the account and the exact address
// for 7 days; the same key is refused from any network, an anonymous post
// from that address is refused, a neighbour in the /24 is not, and reads
// still work. The refusal names the category, the end and the appeal, never
// what matched.
func TestSanctionFirstOffence(t *testing.T) {
	w := newSanctionWorld(t)
	alice := keyFor(71)
	msg := w.mustPost(alice, "198.51.100.7", "evil/1.2")
	out := w.apply(msg, "injection")
	if out.Rung != 1 || out.Days != 7 || out.Subjects["account"] != 1 || out.Subjects["exact"] != 1 || len(out.Subjects) != 2 {
		t.Fatalf("first offence %+v", out)
	}
	if out.Penalty == "" || out.PenaltyExisting {
		t.Fatalf("no trust penalty filed: %+v", out)
	}
	e := w.refused(alice, "203.0.113.9", "other/9") // the same key from elsewhere
	w.refused(nil, "198.51.100.7", "curl/8.5")      // anonymous, the same address
	w.mustPost(nil, "198.51.100.8", "evil/1.2")     // a neighbour in the /24, rung 1
	w.mustPost(keyFor(72), "198.51.100.8", "evil/1.2")
	if !strings.Contains(e.Message, "prompt injection aimed at AI readers") || !strings.Contains(e.Message, out.ID) || !strings.Contains(e.Message, "feedback") {
		t.Fatalf("refusal %q", e.Message)
	}
	// The refusal echoes the author's own flagged message, but a ladder
	// (classifier) category never echoes a reason.
	d := e.Details.(map[string]any)
	if d["message"] != msg {
		t.Fatalf("flagged message id not echoed: %v", d)
	}
	if _, ok := d["reason"]; ok {
		t.Fatalf("a ladder sanction leaked its reason: %v", d)
	}
	sig, _, _ := w.s.SignalMessage(context.Background(), msg)
	b, _ := json.Marshal(e)
	for _, secret := range []string{sig.IPHashFull, sig.IPHash24, sig.ClientHash, keyID(alice), "198.51.100", "address", "network"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("the refusal reveals %q: %s", secret, b)
		}
	}
	// Reads still work, signed or not, from the sanctioned address.
	for _, c := range []Command{{Operation: "messages.list", Room: "lobby"}, {Operation: "message.get", MessageID: msg}} {
		if _, err := w.s.Execute(context.Background(), c, "198.51.100.7"); err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
	}
	if _, err := w.s.Execute(context.Background(), signed(alice, Command{Operation: "journal.get"}), "198.51.100.7"); err != nil {
		t.Fatalf("signed read: %v", err)
	}
	// The appeal path stays open.
	if _, err := w.s.Execute(context.Background(), signed(alice, Command{Operation: "feedback", Target: "general", Kind: "not_useful", Text: "appeal " + out.ID, RequestID: "appeal"}), "198.51.100.7"); err != nil {
		t.Fatalf("feedback while sanctioned: %v", err)
	}
	// One sanction per message.
	if _, err := w.s.ApplySanction(context.Background(), msg, "phishing", "", "operator"); err == nil || !strings.Contains(err.Error(), "already sanctioned") {
		t.Fatalf("second sanction for one message: %v", err)
	}
	// The trust penalty is in the public log, naming the message, once.
	entries, _, err := trust.PenaltyLog(context.Background(), w.s.db, 0, 10, "", w.s.now().Unix())
	if err != nil || len(entries) != 1 || entries[0].Trigger != trust.TriggerModeration || entries[0].Evidence != "message "+msg || entries[0].Members[0] != keyID(alice) {
		t.Fatalf("penalty log %+v %v", entries, err)
	}
}

// Repeats: the second offence within 90 days is 30 days and, from one exact
// address, covers the /24 only together with the offender's client; the
// third is 90 days.
func TestSanctionRepeatAndClientCoSignal(t *testing.T) {
	w := newSanctionWorld(t)
	alice := keyFor(73)
	w.apply(w.mustPost(alice, "198.51.100.7", "evil/1.2"), "phishing")
	w.advance(8 * 24 * time.Hour) // the first has ended
	second := w.apply(w.mustPost(alice, "198.51.100.7", "evil/1.4"), "phishing")
	if second.Rung != 2 || second.Days != 30 || second.Subjects["net24"] != 0 || second.Subjects["net24_client"] != 1 {
		t.Fatalf("second offence %+v", second)
	}
	w.refused(nil, "198.51.100.40", "evil/1.9") // same /24, same client family and major version
	w.mustPost(nil, "198.51.100.40", "honest/3.0")
	w.mustPost(nil, "192.0.2.40", "evil/1.9") // another network, same client: never alone
	w.advance(31 * 24 * time.Hour)
	third := w.apply(w.mustPost(alice, "198.51.100.7", "evil/1.4"), "phishing")
	if third.Rung != 3 || third.Days != 90 {
		t.Fatalf("third offence %+v", third)
	}
	// Outside the window it starts over.
	w.advance(200 * 24 * time.Hour)
	fresh := w.apply(w.mustPost(alice, "198.51.100.7", "evil/1.4"), "phishing")
	if fresh.Rung != 1 || fresh.Days != 7 {
		t.Fatalf("after the window %+v", fresh)
	}
}

// The /24 widens only when two or more distinct exact addresses in it
// offended: an anonymous attacker who hops addresses inside one /24 with the
// same client is a repeat (network and client), and its second address
// widens the sanction to the whole /24.
func TestSanctionWidensOnlyWithTwoAddresses(t *testing.T) {
	w := newSanctionWorld(t)
	first := w.apply(w.mustPost(nil, "198.51.100.20", "atk/2.0"), "malware")
	if first.Rung != 1 || first.Subjects["exact"] != 1 || len(first.Subjects) != 1 {
		t.Fatalf("anonymous first offence %+v", first)
	}
	w.mustPost(nil, "198.51.100.30", "bystander/1") // rung 1: the /24 is open
	second := w.apply(w.mustPost(nil, "198.51.100.21", "atk/2.1"), "malware")
	if second.Rung != 2 || second.Subjects["net24"] != 1 || second.Subjects["exact"] != 1 {
		t.Fatalf("hop inside the /24 %+v", second)
	}
	w.refused(nil, "198.51.100.30", "bystander/1")
	w.refused(keyFor(74), "198.51.100.99", "anything/1")
	w.mustPost(nil, "198.51.101.30", "atk/2.1") // the next /24
	// The same attacker repeating from one address does not widen.
	w2 := newSanctionWorld(t)
	w2.apply(w2.mustPost(nil, "203.0.113.5", "atk/2.0"), "malware")
	w2.advance(8 * 24 * time.Hour)
	again := w2.apply(w2.mustPost(nil, "203.0.113.5", "atk/2.0"), "malware")
	if again.Rung != 2 || again.Subjects["net24"] != 0 || again.Subjects["net24_client"] != 1 {
		t.Fatalf("one address, repeat %+v", again)
	}
}

// The other rungs, promotion's explicit exemption and unknown categories.
func TestSanctionCategories(t *testing.T) {
	w := newSanctionWorld(t)
	for i, c := range []struct {
		category string
		days     int
	}{{"hate", 1}, {"sexual", 1}, {"minors", 30}, {"doxxing", 30}, {"manipulation", 7}} {
		out := w.apply(w.mustPost(keyFor(byte(80+i)), fmt.Sprintf("192.0.%d.1", i), "c/1"), c.category)
		if out.Rung != 1 || out.Days != c.days {
			t.Fatalf("%s: %+v", c.category, out)
		}
	}
	msg := w.mustPost(keyFor(90), "192.0.2.90", "ad/1")
	for _, cat := range []string{"promotion", "spam", SanctionManual} {
		if _, err := w.s.ApplySanction(context.Background(), msg, cat, "", "operator"); err == nil {
			t.Fatalf("%s was sanctioned", cat)
		}
	}
	if r, _ := SanctionRuleFor("promotion"); r.NoSanction == "" || r.Days != [3]int{} {
		t.Fatal("promotion must stay out of the ladder")
	}
	if r, _ := SanctionRuleFor("minors"); r.Days[1] != 180 {
		t.Fatal("minors repeat is 180 days")
	}
	for op := range sanctionExempt {
		if _, ok := LookupOperation(op); !ok || !mutation(op) {
			t.Errorf("exempt %q is not a write", op)
		}
	}
	for _, op := range []string{"post", "conversation.open", "conversation.respond", "work.create", "work.claim", "work.submit", "agent.register", "hosted.create", "vote"} {
		if sanctionExempt[op] {
			t.Errorf("%s must be refused while sanctioned", op)
		}
	}
}

// A lift ends it at once and lifts its trust penalty; expiry ends it by
// time. Rows stay, and their network hashes are blanked once the sanction
// has been over for the signal retention.
func TestSanctionLiftExpiryRedaction(t *testing.T) {
	w := newSanctionWorld(t)
	ctx := context.Background()
	alice := keyFor(75)
	out := w.apply(w.mustPost(alice, "198.51.100.7", "evil/1"), "hate")
	w.refused(alice, "198.51.100.7", "evil/1")
	lift, err := w.s.LiftSanction(ctx, out.ID, "wrong call", "operator")
	if err != nil || lift == "" {
		t.Fatalf("lift: %q %v", lift, err)
	}
	if _, err = w.s.LiftSanction(ctx, out.ID, "again", "operator"); err == nil {
		t.Fatal("lifted twice")
	}
	w.mustPost(alice, "198.51.100.7", "evil/1")
	list, err := w.s.Sanctions(ctx, false, 10)
	if err != nil || len(list) != 1 || list[0].State != "lifted" || list[0].LiftReason != "wrong call" {
		t.Fatalf("list %+v %v", list, err)
	}
	// A lifted sanction is not a prior offence.
	second := w.apply(w.mustPost(alice, "198.51.100.7", "evil/1"), "hate")
	if second.Rung != 1 {
		t.Fatalf("a lifted sanction counted: %+v", second)
	}
	w.advance(25 * time.Hour)
	w.mustPost(alice, "198.51.100.7", "evil/1") // a day's sanction has ended
	if active, _ := w.s.Sanctions(ctx, true, 10); len(active) != 0 {
		t.Fatalf("active after expiry: %+v", active)
	}
	w.advance(WriteSignalsRetentionDays * 24 * time.Hour)
	if _, err = w.s.PruneWriteSignals(ctx); err != nil {
		t.Fatal(err)
	}
	var rows, hashes, subjects int
	if err = w.s.db.QueryRow("SELECT count(*),sum(addr_hash<>'' OR net_hash<>'' OR client_hash<>'') FROM sanctions").Scan(&rows, &hashes); err != nil {
		t.Fatal(err)
	}
	if err = w.s.db.QueryRow("SELECT count(*) FROM sanction_subjects WHERE kind<>'account' AND subject<>''").Scan(&subjects); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || hashes != 0 || subjects != 0 {
		t.Fatalf("after redaction: %d rows, %d with hashes, %d network subjects", rows, hashes, subjects)
	}
	if list, _ = w.s.Sanctions(ctx, false, 10); len(list) != 2 || list[0].Account != keyID(alice) || list[0].State != "ended" {
		t.Fatalf("rows kept: %+v", list)
	}
}

// Without a signals key network sanctions fail open; account ones hold.
func TestSanctionFailOpenWithoutKey(t *testing.T) {
	w := newSanctionWorld(t)
	alice := keyFor(76)
	w.apply(w.mustPost(alice, "198.51.100.7", "evil/1"), "injection")
	w.apply(w.mustPost(nil, "198.51.100.9", "evil/1"), "injection")
	w.s.signals = signalsState{}
	w.mustPost(nil, "198.51.100.9", "evil/1")
	w.refused(alice, "198.51.100.200", "x/1")
}

// Manual sanctions, the key cluster from a trust detector, a hosted
// identity's account, a relayed write's network and the public counts.
func TestSanctionManualClusterAndStats(t *testing.T) {
	w := newSanctionWorld(t)
	ctx := context.Background()
	alice, dave, erin := keyFor(77), keyFor(78), keyFor(79)
	w.mustPost(dave, "203.0.113.50", "d/1")
	if _, err := trust.NewPenalty(ctx, w.s.db, trust.TriggerOverride, []string{keyID(alice), keyID(dave)}, "", "ring found by hand", "operator", w.s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	// Only detector findings make a cluster; an override does not.
	out := w.apply(w.mustPost(alice, "198.51.100.7", "a/1"), "phishing")
	if out.Subjects["cluster"] != 0 {
		t.Fatalf("an override made a cluster: %+v", out)
	}
	if _, err := w.s.db.Exec(`INSERT INTO trust_penalty_log(id,trigger,state,members,reason,author,starts_at,ends_at,created_at) VALUES('ring-1','ring','applied',?,'ring','detector',?,?,?)`,
		`["`+keyID(alice)+`","`+keyID(dave)+`"]`, w.s.now().Unix(), w.s.now().Unix()+86400*30, w.s.now().Unix()); err != nil {
		t.Fatal(err)
	}
	out = w.apply(w.mustPost(erin, "192.0.2.77", "e/1"), "phishing")
	if out.Subjects["cluster"] != 0 {
		t.Fatalf("erin is in no ring: %+v", out)
	}
	if _, err := w.s.LiftSanction(ctx, w.firstSanctionOf(keyID(alice)), "redo with the ring", "operator"); err != nil {
		t.Fatal(err)
	}
	out = w.apply(w.mustPost(alice, "198.51.100.7", "a/1"), "phishing")
	if out.Subjects["cluster"] != 1 {
		t.Fatalf("ring member not covered: %+v", out)
	}
	w.refused(dave, "203.0.113.50", "d/1")
	// Manual: an account for 2 days, and a message's /24.
	frank := keyFor(80)
	m, err := w.s.ApplyManualSanction(ctx, ManualSanction{Account: keyID(frank), Days: 2, Reason: "spam wave", Author: "operator"})
	if err != nil || m.Category != SanctionManual || m.Subjects["account"] != 1 {
		t.Fatalf("manual account %+v %v", m, err)
	}
	// A manual operator sanction echoes the operator's reason back to its own
	// author; an account sanction carries no flagged message.
	ef := w.refused(frank, "192.0.2.200", "f/1")
	fd := ef.Details.(map[string]any)
	if fd["reason"] != "spam wave" {
		t.Fatalf("manual sanction reason not echoed: %v", fd)
	}
	if _, ok := fd["message"]; ok {
		t.Fatalf("an account sanction has no flagged message to echo: %v", fd)
	}
	anon := w.mustPost(nil, "198.18.0.5", "g/1")
	if _, err = w.s.ApplyManualSanction(ctx, ManualSanction{MessageID: anon, Scope: "net24", Days: 1, Reason: "flood", Author: "operator"}); err != nil {
		t.Fatal(err)
	}
	// A message-scoped manual sanction echoes both the flagged message and the
	// reason.
	ad := w.refused(nil, "198.18.0.99", "other/1").Details.(map[string]any)
	if ad["message"] != anon || ad["reason"] != "flood" {
		t.Fatalf("manual message sanction details %v", ad)
	}
	for _, bad := range []ManualSanction{{Account: keyID(frank), Days: 0, Reason: "x"}, {Account: "nope", Days: 1, Reason: "x"}, {MessageID: anon, Scope: "planet", Days: 1, Reason: "x"}, {Account: keyID(frank), Days: 1}} {
		if _, err = w.s.ApplyManualSanction(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// A write relayed by DNS carries no network sanction.
	dctx := WithVia(context.Background(), "dns")
	res, err := w.s.Execute(dctx, signed(keyFor(81), Command{Operation: "post", Room: "lobby", Text: "via dns", RequestID: "dns1", Timestamp: w.s.now().Unix()}), "8.8.8.8")
	if err != nil {
		t.Fatal(err)
	}
	dns := w.apply(res.Receipt.ID, "hate")
	if dns.Subjects["exact"] != 0 || dns.Subjects["account"] != 1 || len(dns.Notes) == 0 {
		t.Fatalf("dns write %+v", dns)
	}
	st, err := w.s.SanctionStats(ctx)
	if err != nil || st.Active["phishing"] != 2 || st.Active[SanctionManual] != 2 || st.Active["hate"] != 1 || st.ActiveTotal != 5 || st.AppliedTotal != 6 {
		t.Fatalf("stats %+v %v", st, err)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), keyID(alice)) || strings.Contains(string(b), "hash") {
		t.Fatalf("stats name more than counts: %s", b)
	}
}

func (w *sanctionWorld) firstSanctionOf(account string) string {
	var id string
	if err := w.s.db.QueryRow("SELECT id FROM sanctions WHERE account=? ORDER BY seq LIMIT 1", account).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// Structural: only sanctions.go and signals.go name the sanctions' hash
// columns and subjects table, so no other code can serve them.
func TestSanctionHashesNeverPublic(t *testing.T) {
	for _, dir := range []string{"..", "../../cmd"} {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			if base := filepath.Base(path); (base == "sanctions.go" || base == "signals.go") && filepath.Base(filepath.Dir(path)) == "board" {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, name := range []string{"sanction_subjects", "addr_hash", "net_hash", "client_hash"} {
				if strings.Contains(string(src), name) {
					t.Errorf("%s names %s: only board/sanctions.go may", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// sanctionLadderProtocolSection is docs/PROTOCOL.md's ladder table, from
// SanctionLadder: categories with the same lengths share a row.
func sanctionLadderProtocolSection() string {
	days := func(n int) string {
		if n == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", n)
	}
	var b strings.Builder
	b.WriteString("| Category | First | Repeat | Third |\n|---|---|---|---|\n")
	var order [][3]int
	rows := map[[3]int][]string{}
	for _, r := range SanctionLadder {
		if r.NoSanction != "" {
			continue
		}
		if rows[r.Days] == nil {
			order = append(order, r.Days)
		}
		rows[r.Days] = append(rows[r.Days], "`"+r.Category+"`")
	}
	for _, d := range order {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", strings.Join(rows[d], ", "), days(d[0]), days(d[1]), days(d[2]))
	}
	return b.String()
}

// The admission lookup reads the subject index, never scans.
func TestSanctionLookupIndexed(t *testing.T) {
	s := openTest(t, Config{})
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+sanctionLookupSQL, "a", "b", "c", "d", 1, "a", "b", "k", "c", "d", "d", "e")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "sanction_subjects_lookup") || strings.Contains(joined, "SCAN ss") || strings.Contains(joined, "SCAN s ") {
		t.Fatalf("plan: %s", joined)
	}
}

// The client tuple: versions cut to their major, language weights dropped.
func TestSignalClientTuple(t *testing.T) {
	a := signalClientTuple(RequestSignals{UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/126.0.6478.126 Safari/537.36", AcceptLanguage: "en-GB,en;q=0.9"})
	b := signalClientTuple(RequestSignals{UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/126.0.6478.61 Safari/537.36", AcceptLanguage: "en-GB, en;q=0.8"})
	c := signalClientTuple(RequestSignals{UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/127.0.1 Safari/537.36", AcceptLanguage: "en-GB,en;q=0.9"})
	if a != b || a == c || !strings.Contains(a, "chrome/126 ") || !strings.Contains(a, "x86_64") {
		t.Fatalf("tuples %q %q %q", a, b, c)
	}
	if signalClientTuple(RequestSignals{Origin: "email:x.example"}) != "" {
		t.Fatal("a bridge's origin is not a client")
	}
}
