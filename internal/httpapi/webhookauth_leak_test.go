package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// leakCanary is the auth header value TestWebhookAuthHeaderNeverLeaks
// follows; leakToken is its token part.
const (
	leakToken  = "sk_live_CANARY7c2e9f41d0b3a8e5"
	leakCanary = "Bearer " + leakToken
	// leakURLCanary is the value of a create refused on a URL wire: it
	// travels in the caller's own URL, and must go no further.
	leakURLCanary = "Bearer URLCANARY93d1c0ffee42"
)

// leakForms are the canary's spellings a leak could take: as is, its token,
// base64 (both alphabets, padded or not) and hex of either.
func leakForms(values ...string) []string {
	var forms []string
	for _, v := range values {
		for _, s := range []string{v, strings.TrimPrefix(v, "Bearer ")} {
			b := []byte(s)
			forms = append(forms, s, base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
				base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b),
				hex.EncodeToString(b), strings.ToUpper(hex.EncodeToString(b)))
		}
	}
	return forms
}

// leakLog is a slog handler that keeps every record, attributes included,
// as text. slog.SetDefault also routes the standard log package here.
type leakLog struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
	h   slog.Handler
}

func newLeakLog() *leakLog {
	l := &leakLog{mu: &sync.Mutex{}, buf: &bytes.Buffer{}}
	l.h = slog.NewTextHandler(lockedWriter{l}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return l
}

type lockedWriter struct{ l *leakLog }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	return w.l.buf.Write(p)
}

func (l *leakLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *leakLog) Handle(ctx context.Context, r slog.Record) error {
	return l.h.Handle(ctx, r)
}
func (l *leakLog) WithAttrs(a []slog.Attr) slog.Handler {
	return &leakLog{mu: l.mu, buf: l.buf, h: l.h.WithAttrs(a)}
}
func (l *leakLog) WithGroup(n string) slog.Handler {
	return &leakLog{mu: l.mu, buf: l.buf, h: l.h.WithGroup(n)}
}
func (l *leakLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// leakEndpoint is the platform: a TLS receiver for any host that records
// each request's headers and body and answers with status.
type leakEndpoint struct {
	mu       sync.Mutex
	requests []struct {
		header http.Header
		body   []byte
	}
	status atomic.Int64
}

// TestWebhookAuthHeaderNeverLeaks follows one auth header value through the
// whole life of a subscription (C1108b section 4): create by HTTP POST, list,
// a wrong and a right webhook.confirm, a delivered event, events answered 401
// until the subscription disables itself, the account export, every
// operation in the registry, the operator's reads and the account's erasure.
// Then every column of every table of the board and raw signals databases,
// every decrypted raw record, every captured response (headers and body),
// every log line (slog and the standard log, which slog.SetDefault routes to
// it) and every operator answer is searched for the value, its token and
// their base64 and hex forms. The value may appear only in the platform's
// request headers: once on the challenge and once per event attempt.
//
// There is no Sentry (or other error-reporting) client in this binary; what
// one would receive is what slog and the standard log write, both captured
// here, and survive's recovered panics, which carry no value.
func TestWebhookAuthHeaderNeverLeaks(t *testing.T) {
	logs := newLeakLog()
	previous := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(previous) })

	dir := t.TempDir()
	features := board.Features{WebhookAuthHeaders: true}
	store, err := board.Open(filepath.Join(dir, "board.sqlite"), board.Config{ServiceID: "swarmmemo.com", PublicURL: "https://swarmmemo.com", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000000,"anonymous_top_level_per_room_day":1000000,"anonymous_mentions_per_hour":1000000,"repeat_flood_copies":1000000,"new_key_posts_per_hour":1000000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	server := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Version: "test", Features: features, PushDelivery: true})

	platform := &leakEndpoint{}
	platform.status.Store(200)
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		platform.mu.Lock()
		platform.requests = append(platform.requests, struct {
			header http.Header
			body   []byte
		}{r.Header.Clone(), raw})
		platform.mu.Unlock()
		w.WriteHeader(int(platform.status.Load()))
		_, _ = io.WriteString(w, `{"echo":"`+r.Header.Get("Authorization")+`","body":`+string(raw)+`}`)
	}))
	t.Cleanup(tlsServer.Close)
	pool := x509.NewCertPool()
	pool.AddCert(tlsServer.Certificate())
	addr := tlsServer.Listener.Addr().String()
	deliver := store.WebhookSenderForTest(&http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: "example.com"},
			DisableKeepAlives: true,
		},
	})
	drainSender := func() {
		t.Helper()
		for i := 0; i < 64; i++ {
			worked, err := deliver(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				return
			}
		}
		t.Fatal("the sender never ran dry")
	}

	// The recording HTTP client: every request and answer, as an access
	// log and a response log would keep them.
	var captured []string
	seq := 0
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://swarmmemo.com"+path, strings.NewReader(body))
		r.RemoteAddr = "203.0.113.50:4000"
		r.Header.Set("User-Agent", "leak-test/1")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		var headers bytes.Buffer
		_ = w.Header().Write(&headers)
		captured = append(captured, method+" "+path+" -> "+fmt.Sprint(w.Code)+"\n"+headers.String()+"\n"+w.Body.String())
		return w
	}
	sign := func(key ed25519.PrivateKey, c board.Command) board.Command {
		seq++
		c.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
		c.Timestamp, c.Nonce = time.Now().Unix(), fmt.Sprintf("leak-nonce-%04d", seq)
		if op, ok := board.LookupOperation(c.Operation); ok && op.Mutation && c.RequestID == "" {
			c.RequestID = fmt.Sprintf("leak-req-%04d", seq)
		}
		c.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, board.Canonical("swarmmemo.com", c)))
		return c
	}
	command := func(key ed25519.PrivateKey, c board.Command) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(sign(key, c))
		w := do("POST", "/v1/command", string(raw))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	errorCode := func(out map[string]any) string {
		e, _ := out["error"].(map[string]any)
		code, _ := e["code"].(string)
		return code
	}
	_, ownerKey, _ := ed25519.GenerateKey(rand.Reader)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	ownerFP := fingerprintOf(ownerKey)

	// A create on a URL wire is refused, and goes no further than the URL.
	urlCreate := sign(ownerKey, board.Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/url","headers":{"Authorization":"` + leakURLCanary + `"}}`})
	rawURL, _ := json.Marshal(urlCreate)
	r := httptest.NewRequest("GET", "https://swarmmemo.com/c64/"+base64.RawURLEncoding.EncodeToString(rawURL), nil)
	r.RemoteAddr = "203.0.113.50:4000"
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "webhook_headers_post_only") {
		t.Fatalf("GET /c64/ create with headers: %d %s", w.Code, w.Body.String())
	}
	captured = append(captured, "GET /c64/ -> "+w.Body.String())

	// Create by HTTP POST, then list.
	createCmd := sign(ownerKey, board.Command{Operation: "webhook.create", Data: `{"schema":1,"url":"https://hooks.example.org/platform","headers":{"authorization":"` + leakCanary + `"}}`, RequestID: "leak-create"})
	rawCreate, _ := json.Marshal(createCmd)
	createAnswer := do("POST", "/v1/command", string(rawCreate))
	var created map[string]any
	_ = json.Unmarshal(createAnswer.Body.Bytes(), &created)
	if createAnswer.Code != 200 {
		t.Fatalf("create: %d %v", createAnswer.Code, created)
	}
	// The unsalted hashes a dictionary attack on a database copy would
	// match: the command's (as requests digests it under each key) and the
	// value's own.
	unsalted := func(c board.Command) string {
		sum := sha256.Sum256(board.Canonical("swarmmemo.com", c))
		return hex.EncodeToString(sum[:])
	}
	fresh := createCmd
	fresh.Timestamp, fresh.Nonce = 0, ""
	hashes := []string{unsalted(createCmd), unsalted(fresh)}
	for _, v := range []string{leakCanary, leakToken} {
		sum := sha256.Sum256([]byte(v))
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	data := created["data"].(map[string]any)
	sub := data["subscription_id"].(string)
	if fmt.Sprint(data["headers"]) != "[Authorization]" || data["activation"] != "confirm" {
		t.Fatalf("create: %v", data)
	}
	command(ownerKey, board.Command{Operation: "webhook.list"})
	// The one challenge; its answer echoes the header back, unread.
	drainSender()
	platform.mu.Lock()
	if len(platform.requests) != 1 {
		t.Fatalf("challenge requests: %d", len(platform.requests))
	}
	var challenge struct{ Nonce string }
	_ = json.Unmarshal(platform.requests[0].body, &challenge)
	platform.mu.Unlock()
	command(ownerKey, board.Command{Operation: "webhook.list"})
	if _, out := command(ownerKey, board.Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + sub + `","nonce":"` + leakToken + `"}`}); errorCode(out) != "webhook_nonce_mismatch" {
		t.Fatalf("wrong confirm: %v", out)
	}
	if code, out := command(ownerKey, board.Command{Operation: "webhook.confirm", Data: `{"schema":1,"subscription_id":"` + sub + `","nonce":"` + challenge.Nonce + `"}`}); code != 200 {
		t.Fatalf("confirm: %d %v", code, out)
	}
	// A delivered event, then events answered 401 until it disables.
	topics := []string{"glacier survey notes", "harbor tide tables", "orchard pruning plan", "violin repair log", "comet sighting report", "bread starter diary"}
	post := func(i int) {
		t.Helper()
		if code, out := command(otherKey, board.Command{Operation: "post", Room: "lobby", To: ownerFP, Text: fmt.Sprintf("Event %d for you: %s, item %d.", i, topics[i], i*7919)}); code != 200 {
			t.Fatalf("post %d: %d %v", i, code, out)
		}
	}
	post(0)
	drainSender()
	platform.status.Store(401)
	for i := 1; i <= board.WebhookDisableFailures; i++ {
		post(i)
		drainSender()
	}
	_, listed := command(ownerKey, board.Command{Operation: "webhook.list"})
	item := listed["data"].(map[string]any)["subscriptions"].([]any)[0].(map[string]any)
	if item["state"] != "disabled" || item["disabled_reason"] != "status 401" {
		t.Fatalf("after %d 401s: %v", board.WebhookDisableFailures, item)
	}
	// The account export, and every operation in the registry, twice.
	command(ownerKey, board.Command{Operation: "account.export"})
	for _, op := range board.Operations() {
		for _, payload := range []string{"", `{"schema":1,"subscription_id":"` + sub + `","nonce":"` + challenge.Nonce + `"}`} {
			command(ownerKey, board.Command{Operation: op.Name, Target: sub, MessageID: sub, Data: payload, Room: "lobby"})
		}
	}
	for _, path := range []string{"/capabilities", "/capabilities?full=1", "/openapi.json", "/llms.txt", "/graph", "/api/graph/summary", "/@" + ownerFP, "/api/agents/" + ownerFP, "/api/messages?room=lobby", "/v1/export", "/me", "/stats"} {
		do("GET", path, "")
	}
	// The operator's reads, and the erasure.
	operator := map[string]any{}
	account := ownerFP
	if v, err := store.SignalAccount(t.Context(), account, 30); err == nil {
		operator["signals_account"] = v
	}
	raws, err := store.RawSignalsForAccount(t.Context(), account, 30)
	if err != nil || len(raws) == 0 {
		t.Fatalf("raw signals: %d %v", len(raws), err)
	}
	operator["raw_signals"] = raws
	if v, err := store.RawSignalsForObject(t.Context(), sub); err == nil {
		operator["raw_signals_object"] = v
	}
	preview, err := store.OperatorEraseAccount(t.Context(), account, "req-leak-1", false)
	if err != nil {
		t.Fatal(err)
	}
	operator["erase_preview"] = preview
	receipt, err := store.OperatorEraseAccount(t.Context(), account, "req-leak-1", true)
	if err != nil {
		t.Fatal(err)
	}
	operator["erase_receipt"] = receipt
	if v, err := store.ErasureLog(t.Context()); err == nil {
		operator["erasure_log"] = v
	}
	if v, err := store.RawSignalsForAccount(t.Context(), account, 30); err == nil {
		operator["raw_signals_after"] = v
	}
	operatorJSON, _ := json.Marshal(operator)

	// Every column of every table of both databases.
	var dumps []string
	for _, file := range []string{"board.sqlite", board.RawSignalsDBFileName} {
		dumps = append(dumps, dumpDatabase(t, filepath.Join(dir, file)))
	}
	if !strings.Contains(dumps[0], "webhook_subscriptions") || !strings.Contains(dumps[1], "raw_signals") {
		t.Fatal("the dumps miss their tables")
	}
	haystacks := map[string]string{
		"responses": strings.Join(captured, "\n"), "logs": logs.text(), "operator": string(operatorJSON),
		"board database": dumps[0], "raw signals database": dumps[1],
	}
	if !strings.Contains(haystacks["logs"], "command_problem") {
		t.Fatal("the log capture saw no problem lines; is it wired?")
	}
	for name, text := range haystacks {
		for _, form := range append(leakForms(leakCanary, leakURLCanary), hashes...) {
			if strings.Contains(text, form) {
				i := strings.Index(text, form)
				t.Fatalf("the header value (as %q) leaked into %s: ...%s...", form, name, text[max(0, i-200):min(len(text), i+200)])
			}
		}
	}
	// The platform saw it exactly once per request: the challenge and each
	// event attempt, in a header and never in a body.
	platform.mu.Lock()
	defer platform.mu.Unlock()
	want := 1 + 1 + board.WebhookDisableFailures
	if len(platform.requests) != want {
		t.Fatalf("the platform got %d requests, want %d", len(platform.requests), want)
	}
	for i, req := range platform.requests {
		if req.header.Get("Authorization") != leakCanary || len(req.header.Values("Authorization")) != 1 {
			t.Fatalf("request %d: Authorization %q", i, req.header.Values("Authorization"))
		}
		var headers bytes.Buffer
		h := req.header.Clone()
		h.Del("Authorization")
		_ = h.Write(&headers)
		for _, form := range leakForms(leakCanary) {
			if bytes.Contains(req.body, []byte(form)) || strings.Contains(headers.String(), form) {
				t.Fatalf("request %d carries the value outside its header", i)
			}
		}
	}
}

// dumpDatabase is every column of every row of every table of the SQLite
// file at path, text as is and blobs as their bytes, hex and base64.
func dumpDatabase(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
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
	var out strings.Builder
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out.WriteString(table + ":")
			for i, v := range values {
				out.WriteString(" " + cols[i] + "=")
				if b, ok := v.([]byte); ok {
					out.Write(b)
					out.WriteString(" " + hex.EncodeToString(b) + " " + base64.StdEncoding.EncodeToString(b))
					continue
				}
				fmt.Fprint(&out, v)
			}
			out.WriteString("\n")
		}
		rows.Close()
	}
	return out.String()
}
