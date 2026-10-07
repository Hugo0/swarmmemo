package services_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"swarmmemo/internal/services"
)

// migrateReceivers runs MigrateReceivers on db, as a board start does.
func migrateReceivers(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = services.MigrateReceivers(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (r *recvRig) itemCount(id string) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRow("SELECT count(*) FROM receiver_items WHERE receiver=?", id).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// TestReceiverDedupeHeader: without dedupe_header every delivery is stored;
// with it, a repeat of the header's value within the window answers the
// first item as a duplicate and is neither stored, charged, counted nor
// woken on; after the window, with another value, without the header or
// with an oversized value, it is stored.
func TestReceiverDedupeHeader(t *testing.T) {
	r := newRecvRig(t, 1<<30, services.ScreenOff)
	migrateReceivers(t, r.db)

	// Default: no deduplication, three deliveries are three items.
	plain, plainTok := r.create("alice", map[string]any{"label": "plain"})
	for range 3 {
		if rc, err := r.deliver(plain, plainTok, delivery{body: `{"n":1}`, headers: map[string]string{"x-event-id": "evt_1"}}); err != nil || rc.Duplicate {
			t.Fatalf("plain deliver: %+v %v", rc, err)
		}
	}
	if n := r.itemCount(plain); n != 3 {
		t.Fatalf("without dedupe_header every delivery is stored: %d", n)
	}

	id, token := r.create("alice", map[string]any{"label": "dedup", "dedupe_header": "X-Event-Id"})
	listed := r.read("alice", "list", map[string]any{})
	var view map[string]any
	for _, v := range get(listed, "result", "active").([]any) {
		if get(v, "id") == id {
			view = v.(map[string]any)
		}
		if get(v, "id") == plain && get(v, "dedupe_header") != "" {
			t.Fatalf("a plain receiver shows dedupe_header empty: %+v", v)
		}
	}
	if view == nil || view["dedupe_header"] != "x-event-id" || view["duplicates"] != float64(0) {
		t.Fatalf("list shows the dedupe setting: %+v", listed)
	}

	// The first delivery of a value is stored.
	first, err := r.deliver(id, token, delivery{body: `{"n":1}`, headers: map[string]string{"x-event-id": "evt_1"}})
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v %v", first, err)
	}
	chargesBefore, _ := r.spent("deliver")

	// A wake-up set after the first delivery must not fire on a duplicate.
	raw, _ := json.Marshal(map[string]any{"schema": 1, "method": "schedule", "args": map[string]any{"key": "inbox", "on": "received"}, "max_cost": 1})
	tx, _ := r.db.Begin()
	if _, err = r.e.Call(context.Background(), tx, services.Request{Service: "wakeup", Data: string(raw), Subject: subjectOf("alice"), RequestKey: "id:wake-dedupe"}, r.now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r.now += 3600
	dup, err := r.deliver(id, token, delivery{body: `{"n":1,"retry":true}`, headers: map[string]string{"x-event-id": "evt_1"}})
	if err != nil || !dup.Duplicate || dup.Item != first.Item {
		t.Fatalf("a repeat within the window is a duplicate of the first item: %+v %v", dup, err)
	}
	if n := r.itemCount(id); n != 1 {
		t.Fatalf("a duplicate is not stored: %d", n)
	}
	if n, _ := r.spent("deliver"); n != chargesBefore {
		t.Fatalf("a duplicate is not charged: %d -> %d", chargesBefore, n)
	}
	own, _ := r.e.Notices(context.Background(), r.db, services.NoticeQuery{Account: "alice", Caller: "alice", Now: r.now, Own: true})
	if wakes, _ := roundTrip(t, own)["wakeups"].([]any); len(wakes) != 0 {
		t.Fatalf("a duplicate wakes nobody: %+v", wakes)
	}
	var deliveries, duplicates int64
	if err = r.db.QueryRow("SELECT deliveries, duplicates FROM receivers WHERE id=?", id).Scan(&deliveries, &duplicates); err != nil || deliveries != 1 || duplicates != 1 {
		t.Fatalf("counts: deliveries %d duplicates %d %v", deliveries, duplicates, err)
	}

	// Another value, no header, an empty value and an oversized value are
	// all stored.
	for _, h := range []map[string]string{
		{"x-event-id": "evt_2"},
		{},
		{"x-event-id": ""},
		{"x-event-id": strings.Repeat("e", services.ReceiverHeaderBytes+1)},
		{"x-event-id": strings.Repeat("e", services.ReceiverHeaderBytes+1)},
	} {
		if rc, err := r.deliver(id, token, delivery{body: `{"n":2}`, headers: h}); err != nil || rc.Duplicate || rc.Item == first.Item {
			t.Fatalf("stored for %v: %+v %v", h, rc, err)
		}
	}
	if n := r.itemCount(id); n != 6 {
		t.Fatalf("five more items: %d", n)
	}
	own, _ = r.e.Notices(context.Background(), r.db, services.NoticeQuery{Account: "alice", Caller: "alice", Now: r.now, Own: true})
	if wakes, _ := roundTrip(t, own)["wakeups"].([]any); len(wakes) != 1 {
		t.Fatalf("a new item wakes: %+v", wakes)
	}

	// After the window (24h from the stored item) the same value is stored.
	r.now = r.now - 3600 + services.ReceiverDedupeWindow
	late, err := r.deliver(id, token, delivery{body: `{"n":1}`, headers: map[string]string{"x-event-id": "evt_1"}})
	if err != nil || late.Duplicate || late.Item == first.Item {
		t.Fatalf("a repeat after the window is stored: %+v %v", late, err)
	}
	// ...and becomes the item later repeats match.
	r.now += 10
	again, err := r.deliver(id, token, delivery{body: `{"n":1}`, headers: map[string]string{"x-event-id": "evt_1"}})
	if err != nil || !again.Duplicate || again.Item != late.Item {
		t.Fatalf("a repeat of the new item: %+v %v", again, err)
	}

	// Deduplication is per receiver: another receiver stores the same value.
	if rc, err := r.deliver(plain, plainTok, delivery{body: `{}`, headers: map[string]string{"x-event-id": "evt_1"}}); err != nil || rc.Duplicate {
		t.Fatalf("another receiver: %+v %v", rc, err)
	}

	// Only a hash of the value is kept on the item.
	var keys string
	if err = r.db.QueryRow("SELECT group_concat(dedupe_key, ',') FROM receiver_items WHERE receiver=?", id).Scan(&keys); err != nil || strings.Contains(keys, "evt_1") {
		t.Fatalf("dedupe keys: %q %v", keys, err)
	}

	// The lookup is index-backed.
	var plan string
	rows, err := r.db.Query("EXPLAIN QUERY PLAN SELECT id FROM receiver_items WHERE receiver=? AND dedupe_key=? AND dedupe_key<>'' AND received_at>? ORDER BY received_at DESC LIMIT 1", id, "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, b, c any
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + ";"
	}
	rows.Close()
	if !strings.Contains(plan, "receiver_items_dedupe") {
		t.Fatalf("the dedupe lookup uses its index: %s", plan)
	}
}

// A duplicate is answered only after the sender checks: an unsigned repeat
// to an HMAC receiver is refused, not acknowledged.
func TestReceiverDedupeAfterSignature(t *testing.T) {
	r := newRecvRig(t, 1<<30, services.ScreenOff)
	id, token := r.create("alice", map[string]any{"dedupe_header": "idempotency-key", "hmac_secret": "a-shared-secret-of-32-characters"})
	if _, err := r.deliver(id, token, delivery{body: `{}`, headers: map[string]string{"idempotency-key": "k1"}}); code(err) != "receiver_signature_invalid" {
		t.Fatalf("unsigned: %v", err)
	}
}

// HeaderValue, when set, is what the dedupe header is read from: the HTTP
// layer passes the request's header lookup, so any header name works.
func TestReceiverDedupeReadsHeaderValue(t *testing.T) {
	r := newRecvRig(t, 1<<30, services.ScreenOff)
	migrateReceivers(t, r.db)
	id, token := r.create("alice", map[string]any{"dedupe_header": "X-Unique"})
	send := func() services.DeliveryReceipt {
		t.Helper()
		if err := r.e.AdmitDelivery(nil, r.now); err != nil {
			t.Fatal(err)
		}
		tx, _ := r.db.Begin()
		defer tx.Rollback()
		rc, _, err := r.e.Deliver(context.Background(), tx, services.Delivery{ID: id, Token: token, ContentType: "text/plain", Body: []byte("hi"),
			HeaderValue: func(name string) string {
				if strings.EqualFold(name, "X-Unique") {
					return "u-1"
				}
				return ""
			}}, r.now)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return rc
	}
	if a, b := send(), send(); a.Duplicate || !b.Duplicate || a.Item != b.Item {
		t.Fatalf("HeaderValue: %+v %+v", a, b)
	}
}

func TestReceiverDedupeHeaderValidation(t *testing.T) {
	r := newRecvRig(t, 1<<30, services.ScreenOff)
	for _, bad := range []string{"X Event", "x-event-id\r\n", "Authorization", "X-Hub-Signature-256", "Cookie", "X-Api-Key", "X-Webhook-Secret", strings.Repeat("a", 65), "é-id", "x:y", "\u212a-id"} {
		if _, err := r.call("alice", "create", map[string]any{"dedupe_header": bad}, 5); code(err) != "invalid_service_data" {
			t.Errorf("dedupe_header %q: %v", bad, err)
		}
	}
	for _, good := range []string{"X-Event-Id", "idempotency-key", "CE-ID", strings.Repeat("a", 64), "x_custom.id"} {
		out, err := r.call("alice", "create", map[string]any{"dedupe_header": good}, 5)
		if err != nil || get(out, "result", "receiver", "dedupe_header") != strings.ToLower(good) {
			t.Errorf("dedupe_header %q: %+v %v", good, out, err)
		}
		id, _ := get(out, "result", "receiver", "id").(string)
		r.mustCall("alice", "delete", map[string]any{"id": id}, 1)
	}
}

// FuzzReceiverDedupeHeader: whatever create is given, an accepted
// dedupe_header is a lowercase header token of at most 64 bytes naming no
// credential, cookie, signature or secret.
func FuzzReceiverDedupeHeader(f *testing.F) {
	for _, s := range []string{"X-Event-Id", "idempotency-key", "Authorization", "x y", "", "x-event-id\r\n", strings.Repeat("a", 65)} {
		f.Add(s)
	}
	secret := regexp.MustCompile(`authorization|auth|cookie|signature|secret|token|password|credential|api-key|apikey`)
	token := regexp.MustCompile("^[!#$%&'*+.^_`|~0-9a-z-]+$")
	f.Fuzz(func(t *testing.T, name string) {
		raw, _ := json.Marshal(map[string]any{"dedupe_header": name})
		got, err := services.ReceiverDedupeHeaderForTest(raw)
		if err != nil || name == "" {
			if got != "" {
				t.Fatalf("refused or empty %q stored %q", name, got)
			}
			return
		}
		if got != strings.ToLower(name) || len(got) > 64 || !token.MatchString(got) || secret.MatchString(got) {
			t.Fatalf("accepted %q as %q", name, got)
		}
	})
}

// TestReceiverMigrationAddsDedupe: receiver tables from before dedupe gain
// the columns and the index, their rows stay, and the migration runs twice
// without change.
func TestReceiverMigrationAddsDedupe(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/old.db")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE receivers (
 id TEXT PRIMARY KEY, account TEXT NOT NULL, key_id TEXT NOT NULL DEFAULT '', hosted INTEGER NOT NULL DEFAULT 0,
 label TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL, hmac_secret TEXT NOT NULL DEFAULT '',
 allow_from TEXT NOT NULL DEFAULT '', screen INTEGER NOT NULL DEFAULT 1,
 state TEXT NOT NULL CHECK(state IN ('active','deleted','revoked')), reason TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, rotated_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0,
 deliveries INTEGER NOT NULL DEFAULT 0, last_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE receiver_items (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, receiver TEXT NOT NULL, account TEXT NOT NULL,
 content_type TEXT NOT NULL, body TEXT NOT NULL, bytes INTEGER NOT NULL, headers TEXT NOT NULL DEFAULT '{}',
 verified INTEGER NOT NULL DEFAULT 0, cost INTEGER NOT NULL DEFAULT 0, received_at INTEGER NOT NULL,
 event_seq INTEGER NOT NULL DEFAULT 0,
 screen TEXT NOT NULL CHECK(screen IN ('off','pending','done','failed','unpaid','unavailable')),
 verdict TEXT NOT NULL DEFAULT '', screen_cost INTEGER NOT NULL DEFAULT 0, screened_at INTEGER NOT NULL DEFAULT 0);
INSERT INTO receivers(id,account,token_hash,state,created_at) VALUES('` + strings.Repeat("a", 32) + `','alice','h','active',1);
INSERT INTO receiver_items(id,receiver,account,content_type,body,bytes,received_at,screen) VALUES('` + strings.Repeat("b", 32) + `','` + strings.Repeat("a", 32) + `','alice','text/plain','kept',4,1,'off');`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(services.Schema); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		migrateReceivers(t, db)
	}
	var columns, index, rows int
	if err = db.QueryRow("SELECT (SELECT count(*) FROM pragma_table_info('receivers') WHERE name IN ('dedupe_header','duplicates')) + (SELECT count(*) FROM pragma_table_info('receiver_items') WHERE name='dedupe_key'), (SELECT count(*) FROM sqlite_master WHERE type='index' AND name='receiver_items_dedupe'), (SELECT count(*) FROM receiver_items WHERE body='kept' AND dedupe_key='')").Scan(&columns, &index, &rows); err != nil || columns != 3 || index != 1 || rows != 1 {
		t.Fatalf("columns %d index %d rows %d %v", columns, index, rows, err)
	}
}
