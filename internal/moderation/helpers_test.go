package moderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const testKey = "test-jev-key-0123456789"

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "m.db")}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// As the board does: one connection, so a nested query would deadlock here.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeJev is a Jev stand-in: it checks the request shape and the key, and
// answers every question it was asked with p[question] (default 0.01).
type fakeJev struct {
	srv      *httptest.Server
	mu       sync.Mutex
	p        map[string]float64
	status   int // non-zero: answer every request with this status
	requests atomic.Int64
	tokens   int64
	lastBody map[string]any
	lastAsks map[string]bool // the question keys of the last request
	raw      map[string]any  // answers sent as they are (nil: leave the question out)
}

func newFakeJev(t *testing.T) *fakeJev {
	f := &fakeJev{p: map[string]float64{}, tokens: 1000}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(401)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string                     `json:"model"`
			State     map[string]any             `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.Unmarshal(raw, &req); err != nil || req.Model == "" || len(req.Questions) == 0 {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastBody = req.State
		f.lastAsks = map[string]bool{}
		for k := range req.Questions {
			f.lastAsks[k] = true
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		answers := map[string]any{}
		for k := range req.Questions {
			if v, ok := f.raw[k]; ok {
				if v != nil {
					answers[k] = v
				}
				continue
			}
			p, ok := f.p[k]
			if !ok {
				p = 0.01
			}
			answers[k] = map[string]float64{"noul": p}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int64{"input_tokens": f.tokens}, "model": req.Model})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJev) set(p map[string]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p = p
}

// answerRaw sends answer as it is for question k (nil leaves k out).
func (f *fakeJev) answerRaw(k string, answer any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.raw == nil {
		f.raw = map[string]any{}
	}
	f.raw[k] = answer
}

func (f *fakeJev) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func writeKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jev.key")
	if err := os.WriteFile(path, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePolicy(t *testing.T, dir string, body string) string {
	t.Helper()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	e      *Engine
	jev    *fakeJev
	db     *sql.DB
	clock  *clock
	alerts []Alert
	amu    sync.Mutex
	opts   Options
}

func (v *env) alertKinds() []string {
	v.amu.Lock()
	defer v.amu.Unlock()
	var out []string
	for _, a := range v.alerts {
		out = append(out, a.Kind)
	}
	return out
}

// newEnv builds an engine over a fresh database and a fake Jev. policy, when
// not empty, is written to a policy file.
func newEnv(t *testing.T, policy string) *env {
	t.Helper()
	v := &env{jev: newFakeJev(t), db: openDB(t), clock: &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}}
	o := Options{DB: v.db, JevKeyFile: writeKey(t), Now: v.clock.now, Alert: func(a Alert) {
		v.amu.Lock()
		v.alerts = append(v.alerts, a)
		v.amu.Unlock()
	}}
	if policy != "" {
		o.PolicyFile = writePolicy(t, t.TempDir(), policy)
	}
	o = withFakeJev(o, v.jev.srv.URL)
	v.opts = o
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	v.e = e
	return v
}

func (v *env) screen(t *testing.T, s Surface, id, text string) Decision {
	t.Helper()
	return v.e.Screen(context.Background(), s, Subject{ID: id, Agent: "agent-" + id}, Content{Text: text})
}

// recorder is an Actuator that records what it was asked.
type recorder struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recorder) Apply(ctx context.Context, subject string, hide bool, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	verb := "restore"
	if hide {
		verb = "hide"
	}
	r.calls = append(r.calls, verb+" "+subject+" "+reason)
	return nil
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}
