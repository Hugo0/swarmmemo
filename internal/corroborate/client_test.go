package corroborate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const vitalik = "0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"

// Answer is a sidecar answer as workers/corroborate/src/core.mjs shapes it.
const Answer = `{"addresses":["0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"],"score":0.1489,"total_cents":0.41,"independent_roots":0,
"roots":[{"root":"social-account:lens","contribution_cents":0.41,"saturated":false,"adapters":["lens-account"],
"strongest":{"adapter":"lens-account","name":"Lens account (Lens Chain)","evidence_class":"Behavioral","observed_on":"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045",
"forge_cents":1,"rent_cents":1,"live":true,"age_curve":"Ramp","half_life_days":730,"issued_at":1743762999,"age_days":554,"age_weight":0.4091,"source":"research/protocols/lens-onchain-read.md"}}],
"checks":{"total":18,"held":1,"unavailable":2},"unavailable":[{"adapter":"idena","address":"0xd8dA6BF26964aF9D7eEd9e03E53415D37aA96045"}],
"caveats":[{"code":"independent-control-not-attested","message":"x"}],
"registry":{"address":"0x977b028b900cce8ee89c46877e814eff3060aa07","chain":"sepolia","chain_id":11155111,"revision":44,"block":11883996,"block_time":1791628836,"sha256":"147b29c2d3787e8b2c1a11accedb0ba2a5ff982942c2d554cca1bc9d52fa6e53"},
"as_of":null,"computed_at":1791628932,"cached":false,"human":true}`

func sidecar(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestValidURL(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:8787", "http://[::1]:8787", "http://127.0.0.1:8787/"} {
		if !ValidURL(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"https://127.0.0.1:8787", "http://10.0.0.1:8787", "http://example.com:8787", "http://localhost:8787",
		"http://127.0.0.1", "http://127.0.0.1:8787/resolve", "http://u:p@127.0.0.1:8787", "http://127.0.0.1:8787?x=1", "http://0.0.0.0:8787"} {
		if ValidURL(bad) {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := New("http://192.0.2.1:8787"); err == nil {
		t.Fatal("New took a non-loopback URL")
	}
}

func TestResolve(t *testing.T) {
	var got map[string]any
	c := sidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/resolve" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = io.WriteString(w, Answer)
	})
	r, err := c.Resolve(t.Context(), []string{vitalik}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := got["as_of"]; has || len(got["addresses"].([]any)) != 1 {
		t.Fatalf("sent %v", got)
	}
	if r.TotalCents != 0.41 || r.Score != 0.1489 || r.Registry.Revision != 44 || len(r.Roots) != 1 || r.Roots[0].Strongest.ForgeCents != 1 ||
		r.Roots[0].Strongest.AgeCurve != "Ramp" || *r.Roots[0].Strongest.AgeDays != 554 || r.Checks.Unavailable != 2 || r.AsOf != nil {
		t.Fatalf("result %+v", r)
	}
	// Re-encoded, a field the sidecar adds (here, a forbidden verdict) does
	// not pass through.
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "human") {
		t.Fatalf("passed through: %s", out)
	}
	if _, err = c.Resolve(t.Context(), []string{vitalik}, RegistryGenesisBlock+1); err != nil || got["as_of"] != float64(RegistryGenesisBlock+1) {
		t.Fatalf("as_of: %v %v", got, err)
	}
}

func TestResolveRefusesBadInputBeforeAsking(t *testing.T) {
	c := sidecar(t, func(http.ResponseWriter, *http.Request) { t.Error("asked the sidecar") })
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = "0x" + strings.Repeat("1", 39) + string(rune('a'+i%6))
	}
	for _, in := range [][]string{nil, eleven, {"vitalik.eth"}, {"0x123"}, {"0X" + vitalik[2:]}} {
		var bad *ErrRequest
		if _, err := c.Resolve(t.Context(), in, 0); !errors.As(err, &bad) {
			t.Errorf("%v: %v", in, err)
		}
	}
	var bad *ErrRequest
	if _, err := c.Resolve(t.Context(), []string{vitalik}, 5); !errors.As(err, &bad) {
		t.Errorf("as_of before the registry: %v", err)
	}
}

// Every failure is ErrUnavailable with a retry_after, never a zero score.
func TestResolveUnavailable(t *testing.T) {
	unavailable := func(t *testing.T, c *Client, ctx context.Context, wantRetry int) {
		t.Helper()
		r, err := c.Resolve(ctx, []string{vitalik}, 0)
		var u *ErrUnavailable
		if r != nil || !errors.As(err, &u) || (wantRetry > 0 && u.RetryAfter != wantRetry) || u.RetryAfter <= 0 {
			t.Fatalf("got %v %v", r, err)
		}
	}
	t.Run("sidecar 503", func(t *testing.T) {
		c := sidecar(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "15")
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"error":"timeout","retry_after":15}`)
		})
		unavailable(t, c, t.Context(), 15)
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		c, _ := New(srv.URL)
		srv.Close()
		unavailable(t, c, t.Context(), 30)
	})
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		c := sidecar(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		defer close(release)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		unavailable(t, c, ctx, 15)
	})
	t.Run("nothing read is not a zero", func(t *testing.T) {
		c := sidecar(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Replace(Answer, `"checks":{"total":18,"held":1,"unavailable":2}`, `"checks":{"total":18,"held":0,"unavailable":18}`, 1))
		})
		unavailable(t, c, t.Context(), 30)
	})
	t.Run("garbage", func(t *testing.T) {
		c := sidecar(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"score":-1}`) })
		unavailable(t, c, t.Context(), 30)
	})
	t.Run("another pin", func(t *testing.T) {
		c := sidecar(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, Answer) })
		c.Pin = strings.Repeat("0", 64)
		unavailable(t, c, t.Context(), 300)
		c.Pin = "147b29c2d3787e8b2c1a11accedb0ba2a5ff982942c2d554cca1bc9d52fa6e53"
		if _, err := c.Resolve(t.Context(), []string{vitalik}, 0); err != nil {
			t.Fatal(err)
		}
	})
}

func TestResolveRequestRefusal(t *testing.T) {
	c := sidecar(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":"as_of_unavailable","message":"leave as_of out"}`)
	})
	var bad *ErrRequest
	if _, err := c.Resolve(t.Context(), []string{vitalik}, RegistryGenesisBlock); !errors.As(err, &bad) || bad.Code != "as_of_unavailable" {
		t.Fatalf("%v", err)
	}
}
