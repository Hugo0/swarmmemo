package board

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestWebhookHeaderChallengeUnobservable is the oracle test of C1108b
// (section 4): an attacker holding a stolen token registers the platform's
// URL with it and watches what SwarmMemo shows. Two boards that differ only
// in their endpoint's answer to the header challenge (200 with the nonce
// echoed, which would activate an echo subscription, versus 401) must show
// the same create result, the same webhook.list at every poll until expiry,
// and the same rows in every table of both databases except auth_sealed,
// after masking ids, secrets and the random-per-record ciphertext.
func TestWebhookHeaderChallengeUnobservable(t *testing.T) {
	keys := t.TempDir()
	cfg := Config{
		SignalsKeyFile:    filepath.Join(keys, "signals.key"),
		RawSignalsKeyFile: filepath.Join(keys, "signals-raw.key"),
		LogKeyFile:        filepath.Join(keys, "log.key"),
		OfferingsKeyFile:  filepath.Join(keys, "offerings.key"),
	}
	type run struct {
		name    string
		s       *Store
		rec     *authReceiver
		answers []string
	}
	replies := map[string]func(authRequest) (int, string){
		"accepts": func(req authRequest) (int, string) {
			var body struct{ Nonce string }
			_ = json.Unmarshal(req.body, &body)
			return 200, `{"nonce":"` + body.Nonce + `","challenge":"` + body.Nonce + `"}`
		},
		"rejects": func(authRequest) (int, string) { return 401, `{"error":"invalid token"}` },
	}
	runs := []*run{{name: "accepts"}, {name: "rejects"}}
	for _, r := range runs {
		r.s = openTest(t, cfg)
		r.s.config.Features.WebhookAuthHeaders = true
		r.rec = pointSender(t, r.s, replies[r.name])
	}
	attacker := keyFor(160)
	step := 0
	// observe runs the same signed command on both boards at their (equal)
	// clock and records its masked answer.
	observe := func(c Command) {
		t.Helper()
		step++
		for _, r := range runs {
			c := c
			c.Timestamp, c.Nonce = r.s.now().Unix(), fmt.Sprintf("oracle-%03d", step)
			if mutation(c.Operation) {
				c.RequestID = fmt.Sprintf("oracle-req-%03d", step)
			}
			res, err := r.s.Execute(testContext, signed(attacker, c), "198.51.100.7")
			raw, _ := json.Marshal(map[string]any{"result": res, "error": errCode(err)})
			r.answers = append(r.answers, maskOracle(string(raw)))
		}
	}
	at := func(when int64) {
		for _, r := range runs {
			r.s.now = func() time.Time { return time.Unix(when, 0) }
		}
	}
	list := Command{Operation: "webhook.list"}
	observe(Command{Operation: "webhook.create", Data: authCreateData("https://hooks.example.org/platform", `{"Authorization":"`+testAuthValue+`"}`)})
	observe(list)
	for _, offset := range []int64{0, 1, 5, 30, 60, 120, 600, 1800, WebhookPendingTTL - 1} {
		at(testTime + offset)
		for _, r := range runs {
			for range 3 {
				if _, err := r.s.deliverOnce(testContext); err != nil {
					t.Fatal(err)
				}
			}
		}
		observe(list)
	}
	for _, r := range runs {
		got := r.rec.seen()
		if len(got) != 1 || got[0].header.Get("Authorization") != testAuthValue {
			t.Fatalf("%s: the endpoint saw %d requests; want the one challenge with its header", r.name, len(got))
		}
	}
	// A wrong nonce is answered alike too.
	observe(Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + strings.Repeat("0", 32) + `","nonce":"guess"}`})
	at(testTime + WebhookPendingTTL + 1)
	for _, r := range runs {
		r.s.expireWebhooks(testContext)
	}
	observe(list)
	at(testTime + 2*WebhookPendingTTL)
	for _, r := range runs {
		r.s.expireWebhooks(testContext)
	}
	observe(list)

	for i := range runs[0].answers {
		if runs[0].answers[i] != runs[1].answers[i] {
			t.Fatalf("answer %d differs by the endpoint's answer:\naccepts: %s\nrejects: %s", i, runs[0].answers[i], runs[1].answers[i])
		}
	}
	if !strings.Contains(runs[0].answers[1], `"state":"pending"`) || !strings.Contains(runs[0].answers[len(runs[0].answers)-1], `"state":"expired"`) {
		t.Fatalf("the polls did not see pending then expired: %v", runs[0].answers)
	}
	for _, leak := range []string{"401", "invalid token", "status ", "pending_deliveries", "last_error"} {
		for _, answer := range runs[1].answers {
			if strings.Contains(answer, leak) {
				t.Fatalf("a poll shows %q: %s", leak, answer)
			}
		}
	}
	a, b := oracleDump(t, runs[0].s.db, "auth_sealed"), oracleDump(t, runs[1].s.db, "auth_sealed")
	if diff := dumpDiff(a, b); diff != "" {
		t.Fatalf("the board databases differ by the endpoint's answer:\n%s", diff)
	}
	// The raw signals database: its clear columns (each sealed record has
	// its own random nonce, and holds the request, not the answer).
	if runs[0].s.rawSignals.db != nil {
		a, b = oracleDump(t, runs[0].s.rawSignals.db, "sealed"), oracleDump(t, runs[1].s.rawSignals.db, "sealed")
		if diff := dumpDiff(a, b); diff != "" {
			t.Fatalf("the raw signals databases differ:\n%s", diff)
		}
	}
	if len(a) == 0 {
		t.Fatal("nothing compared")
	}
}

var (
	oracleHex = regexp.MustCompile(`[0-9a-f]{16,}`)
	oracleB64 = regexp.MustCompile(`[A-Za-z0-9_-]{40,}`)
)

// maskOracle replaces what differs between two boards by construction: ids
// and keys (hex), secrets and signatures (base64url).
func maskOracle(s string) string {
	return oracleB64.ReplaceAllString(oracleHex.ReplaceAllString(s, "<hex>"), "<b64>")
}

// oracleDump is every row of every table of db, masked, sorted per table,
// leaving out the named column.
func oracleDump(t *testing.T, db *sql.DB, skip string) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var out []string
	for _, table := range tables {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var parts []string
			for i, col := range cols {
				if col == skip {
					continue
				}
				v := values[i]
				if b, ok := v.([]byte); ok {
					v = fmt.Sprintf("%x", b)
				}
				parts = append(parts, col+"="+maskOracle(fmt.Sprint(v)))
			}
			lines = append(lines, table+": "+strings.Join(parts, " "))
		}
		rows.Close()
		slices.Sort(lines)
		out = append(out, lines...)
	}
	return out
}

func dumpDiff(a, b []string) string {
	var diff []string
	for _, line := range a {
		if !slices.Contains(b, line) {
			diff = append(diff, "only accepts: "+line)
		}
	}
	for _, line := range b {
		if !slices.Contains(a, line) {
			diff = append(diff, "only rejects: "+line)
		}
	}
	return strings.Join(diff, "\n")
}
