package services

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/corroborate"
)

const corroborateVitalik = "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"

// fakeCorroborate is a sidecar answering every resolve with answer (or
// status), counting the requests it served.
func fakeCorroborate(t *testing.T, status int, answer string) (*corroborateSvc, *atomic.Int64, *[]map[string]any) {
	t.Helper()
	var hits atomic.Int64
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		sent = append(sent, body)
		if status != 200 {
			w.Header().Set("Retry-After", "15")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	client, err := corroborate.New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return newCorroborate(Deps{Corroborate: client}).(*corroborateSvc), &hits, &sent
}

const corroborateFakeAnswer = `{"addresses":["0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"],"score":3.4914,"total_cents":3100,"independent_roots":2,
"roots":[{"root":"iris-registry:world-orb","contribution_cents":2500,"saturated":false,"adapters":["world-id-orb"],"strongest":{"adapter":"world-id-orb","forge_cents":50000,"rent_cents":50,"age_curve":"Decay","age_days":10,"age_weight":0.99}},
{"root":"kyc-vendor:persona","contribution_cents":600,"saturated":false,"adapters":["coinbase-verification"],"strongest":null}],
"checks":{"total":18,"held":2,"unavailable":0},"unavailable":[],"caveats":[],
"registry":{"address":"0x977b028b900cce8ee89c46877e814eff3060aa07","chain":"sepolia","chain_id":11155111,"revision":44,"block":11883996,"block_time":1791628836,"sha256":"147b29c2d3787e8b2c1a11accedb0ba2a5ff982942c2d554cca1bc9d52fa6e53"},
"as_of":null,"computed_at":1791628932}`

func corroborateCall(args string, subject string, signed bool, now int64) Call {
	return Call{Service: CorroborateID, Method: "resolve", Args: json.RawMessage(args), Subject: allowance.Subject{ID: subject, Signed: signed}, Now: now}
}

func refusalCode(err error) (string, int) {
	var e *allowance.Err
	if errors.As(err, &e) {
		return e.Code, e.RetryAfter
	}
	return "", 0
}

func TestCorroborateParse(t *testing.T) {
	lower := strings.ToLower(corroborateVitalik)
	upper := "0x" + strings.ToUpper(corroborateVitalik[2:])
	for _, args := range []string{
		`{"addresses":"` + corroborateVitalik + `"}`, `{"addresses":"` + lower + `"}`, `{"addresses":"` + upper + `"}`,
		`{"addresses":" ` + lower + ` , ` + corroborateVitalik + `,"}`, `{"addresses":["` + lower + `"]}`,
	} {
		got, asOf, err := parseCorroborate(json.RawMessage(args))
		if err != nil || len(got) != 1 || got[0] != corroborateVitalik || asOf != 0 {
			t.Errorf("%s: %v %d %v", args, got, asOf, err)
		}
	}
	two, _, err := parseCorroborate(json.RawMessage(`{"addresses":"0x2222222222222222222222222222222222222222,0x1111111111111111111111111111111111111111","as_of":11400000}`))
	if err != nil || strings.Join(two, ",") != "0x1111111111111111111111111111111111111111,0x2222222222222222222222222222222222222222" {
		t.Fatalf("sorted set: %v %v", two, err)
	}
	badChecksum := strings.Replace(corroborateVitalik, "dA6", "Da6", 1)
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "0x" + strings.Repeat("0", 39) + string(rune('0'+i%10))
		if i == 10 {
			eleven[i] = "0x" + strings.Repeat("0", 38) + "aa"
		}
	}
	for _, args := range []string{
		`{}`, `{"addresses":""}`, `{"addresses":","}`, `{"addresses":"` + badChecksum + `"}`, `{"addresses":"vitalik.eth"}`, `{"addresses":"0x123"}`,
		`{"addresses":"0X` + corroborateVitalik[2:] + `"}`, `{"addresses":"bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"}`,
		`{"addresses":"` + strings.Join(eleven, ",") + `"}`, `{"addresses":[1]}`, `{"addresses":{"a":1}}`,
		`{"addresses":"` + lower + `","as_of":5}`, `{"addresses":"` + lower + `","as_of":"11400000"}`, `{"addresses":"` + lower + `","as_of":-1}`,
		`{"addresses":"` + lower + `","extra":1}`,
	} {
		if _, _, err := parseCorroborate(json.RawMessage(args)); err == nil {
			t.Errorf("%s accepted", args)
		} else if code, _ := refusalCode(err); code != "invalid_service_data" {
			t.Errorf("%s: %v", args, err)
		}
	}
}

func TestCorroborateResolveAndCache(t *testing.T) {
	s, hits, sent := fakeCorroborate(t, 200, corroborateFakeAnswer)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	run := func(args string) (json.RawMessage, error) {
		after, err := s.ReadRemote(t.Context(), nil, corroborateCall(args, "anon:net-a", false, now.Unix()))
		if err != nil {
			return nil, err
		}
		return after(t.Context())
	}
	body, err := run(`{"addresses":"` + strings.ToLower(corroborateVitalik) + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil || got["total_cents"] != float64(3100) || got["score"] != 3.4914 || got["cached"] != false {
		t.Fatalf("answer %s %v", body, err)
	}
	if addrs := (*sent)[0]["addresses"].([]any); len(addrs) != 1 || addrs[0] != corroborateVitalik {
		t.Fatalf("sent %v", (*sent)[0])
	}
	// The same set in another case and order is the cached answer.
	body, err = run(`{"addresses":["` + corroborateVitalik + `"]}`)
	if err != nil || !strings.Contains(string(body), `"cached":true`) || hits.Load() != 1 {
		t.Fatalf("cache: %s %v hits=%d", body, err, hits.Load())
	}
	// A block is its own entry; an hour later the answer is asked again.
	if _, err = run(`{"addresses":"` + corroborateVitalik + `","as_of":11400000}`); err != nil || hits.Load() != 2 || (*sent)[1]["as_of"] != float64(11400000) {
		t.Fatalf("as_of: %v hits=%d", err, hits.Load())
	}
	now = now.Add(CorroborateCacheTTL)
	if _, err = run(`{"addresses":"` + corroborateVitalik + `"}`); err != nil || hits.Load() != 3 {
		t.Fatalf("expiry: %v hits=%d", err, hits.Load())
	}
}

// A partial answer (some checks unread) is kept a minute, not an hour.
func TestCorroboratePartialAnswerShortCache(t *testing.T) {
	partial := strings.Replace(corroborateFakeAnswer, `"unavailable":0}`, `"unavailable":3}`, 1)
	s, hits, _ := fakeCorroborate(t, 200, partial)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	for i, step := range []time.Duration{0, 30 * time.Second, 31 * time.Second} {
		now = now.Add(step)
		after, err := s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+corroborateVitalik+`"}`, "anon:net-a", false, now.Unix()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = after(t.Context()); err != nil {
			t.Fatal(err)
		}
		if want := []int64{1, 1, 2}[i]; hits.Load() != want {
			t.Fatalf("step %d: hits %d, want %d", i, hits.Load(), want)
		}
	}
}

// The sidecar's failures are corroborate_unavailable with its retry_after
// (the board answers 503 service_unavailable), and nothing is cached.
func TestCorroborateUnavailable(t *testing.T) {
	s, hits, _ := fakeCorroborate(t, 503, `{"error":"timeout","retry_after":15}`)
	for i := 0; i < 2; i++ {
		after, err := s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+corroborateVitalik+`"}`, "anon:net-a", false, 1_800_000_000))
		if err != nil {
			t.Fatal(err)
		}
		body, err := after(t.Context())
		if code, retry := refusalCode(err); body != nil || code != RefusalCorroborateUnavailable || retry != 15 {
			t.Fatalf("got %s %v", body, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("a failure was cached: hits %d", hits.Load())
	}
	// Not configured at all: refused before anything runs.
	none := newCorroborate(Deps{}).(*corroborateSvc)
	if _, err := none.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+corroborateVitalik+`"}`, "anon:net-a", false, 1)); err == nil {
		t.Fatal("unconfigured answered")
	} else if code, retry := refusalCode(err); code != RefusalCorroborateUnavailable || retry <= 0 {
		t.Fatalf("unconfigured: %v", err)
	}
	if none.CatalogueExtra()["available"] != false || s.CatalogueExtra()["available"] != true {
		t.Fatal("available")
	}
}

// Fresh resolves are counted per network (or key) a minute and a day;
// cached answers are not.
func TestCorroborateRateLimit(t *testing.T) {
	s, _, _ := fakeCorroborate(t, 200, corroborateFakeAnswer)
	now := int64(1_800_000_000)
	s.now = func() time.Time { return time.Unix(now, 0) }
	addr := func(i int) string {
		return "0x" + strings.Repeat("0", 36) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16]) + "00"
	}
	limit := corroborateLimits[false].PerMinute
	for i := range int(limit) {
		if _, err := s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+addr(i)+`"}`, "anon:net-a", false, now)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+addr(99)+`"}`, "anon:net-a", false, now))
	if code, retry := refusalCode(err); code != "request_rate" || retry <= 0 || retry > 60 {
		t.Fatalf("over the minute: %v", err)
	}
	// Another network has its own window, and a key a larger one.
	if _, err = s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+addr(99)+`"}`, "anon:net-b", false, now)); err != nil {
		t.Fatal(err)
	}
	for i := range int(corroborateLimits[true].PerMinute) {
		if _, err = s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+addr(i)+`"}`, "acct-1", true, now)); err != nil {
			t.Fatalf("signed call %d: %v", i, err)
		}
	}
	// A cached answer is served past the window.
	after, err := s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+corroborateVitalik+`"}`, "anon:net-c", false, now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = after(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < int(limit)+5; i++ {
		if _, err = s.ReadRemote(t.Context(), nil, corroborateCall(`{"addresses":"`+strings.ToLower(corroborateVitalik)+`"}`, "anon:net-a", false, now)); err != nil {
			t.Fatalf("cached call %d: %v", i, err)
		}
	}
}
