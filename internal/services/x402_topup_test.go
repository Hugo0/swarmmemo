package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	topupTestConfig = `{"schema":1,"enabled":true,"network":"eip155:8453","asset":"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913","asset_name":"USD Coin","asset_version":"2","pay_to":"0x1111111111111111111111111111111111111111","facilitator_url":"https://facilitator.example.com"}`
	topupTestFrom   = "0x2222222222222222222222222222222222222222"
)

func topupTestPayment(cfg *TopupConfig, r TopupRequirement, mutate func(m map[string]any)) string {
	accepted, _ := json.Marshal(cfg.wire(cfg.topup(r)))
	var acc map[string]any
	_ = json.Unmarshal(accepted, &acc)
	m := map[string]any{"x402Version": 2, "accepted": acc, "payload": map[string]any{
		"signature": "0x" + strings.Repeat("1f", 65),
		"authorization": map[string]any{"from": topupTestFrom, "to": cfg.PayTo.String(), "value": strconv.FormatInt(r.Amount, 10),
			"validAfter": "1000", "validBefore": "5000", "nonce": "0x" + strings.Repeat("ab", 32)}}}
	if mutate != nil {
		mutate(m)
	}
	raw, _ := json.Marshal(m)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestTopupConfigParsing(t *testing.T) {
	cfg, err := ParseTopupConfig([]byte(topupTestConfig), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Min != TopupMinDefault || cfg.Max != TopupMaxDefault || cfg.AccountDaily != TopupAccountDailyDefault || cfg.BoardDaily != TopupBoardDailyDefault {
		t.Fatalf("defaults %d %d %d %d", cfg.Min, cfg.Max, cfg.AccountDaily, cfg.BoardDaily)
	}
	// The conservative defaults: 0.10 to 5 USDC a top-up, 10 USDC per
	// account and 100 USDC per board per UTC day.
	if cfg.Min != 100_000 || cfg.Max != 5_000_000 || cfg.AccountDaily != 10_000_000 || cfg.BoardDaily != 100_000_000 {
		t.Fatalf("defaults are not the conservative ones: %d %d %d %d", cfg.Min, cfg.Max, cfg.AccountDaily, cfg.BoardDaily)
	}
	if _, err := ParseTopupConfig([]byte(strings.Replace(topupTestConfig, `"enabled":true`, `"enabled":false`, 1)), nil); !errors.Is(err, ErrTopupDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	for name, bad := range map[string]string{
		"no pay_to":           strings.Replace(topupTestConfig, `"pay_to":"0x1111111111111111111111111111111111111111",`, "", 1),
		"zero pay_to":         strings.Replace(topupTestConfig, "0x1111111111111111111111111111111111111111", "0x0000000000000000000000000000000000000000", 1),
		"pay_to is the asset": strings.Replace(topupTestConfig, "0x1111111111111111111111111111111111111111", "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", 1),
		"http facilitator":    strings.Replace(topupTestConfig, "https://facilitator", "http://facilitator", 1),
		"IP facilitator":      strings.Replace(topupTestConfig, "facilitator.example.com", "10.0.0.1", 1),
		"unknown field":       strings.Replace(topupTestConfig, `"schema":1`, `"schema":1,"wallet_key_file":"k"`, 1),
		"bad network":         strings.Replace(topupTestConfig, "eip155:8453", "base", 1),
		"token without file":  strings.Replace(topupTestConfig, `"schema":1`, `"schema":1,"facilitator_token_file":"/x"`, 1),
	} {
		if _, err := ParseTopupConfig([]byte(bad), nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A facilitator token file is read through readKey (owner-only on disk).
	withToken := strings.Replace(topupTestConfig, `"schema":1`, `"schema":1,"facilitator_token_file":"/token"`, 1)
	if _, err := ParseTopupConfig([]byte(withToken), func(string) ([]byte, error) { return nil, fs.ErrNotExist }); err == nil {
		t.Fatal("a missing token file accepted")
	}
	cfg, err = ParseTopupConfig([]byte(withToken), func(string) ([]byte, error) { return []byte("secret-token\n"), nil })
	if err != nil || cfg.facilitatorToken != "secret-token" {
		t.Fatalf("token: %v", err)
	}
}

func TestTopupPaymentCheck(t *testing.T) {
	cfg, _ := ParseTopupConfig([]byte(topupTestConfig), nil)
	r := TopupRequirement{Amount: 250_000, Quote: "4000.salt.mac", Expires: 4000}
	p, err := ParseTopupPayment(topupTestPayment(cfg, r, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Check(p, r, 2000); err != nil {
		t.Fatal(err)
	}
	code := func(err error) string {
		var te *TopupError
		if errors.As(err, &te) {
			return te.Code
		}
		return ""
	}
	if code(cfg.Check(p, r, 4001)) != "payment_expired" {
		t.Fatal("expired quote accepted")
	}
	if code(cfg.Check(p, r, 999)) != "payment_expired" {
		t.Fatal("authorization not yet valid accepted")
	}
	if code(cfg.Check(p, TopupRequirement{Amount: 250_001, Quote: r.Quote, Expires: r.Expires}, 2000)) != "payment_mismatch" {
		t.Fatal("another amount accepted")
	}
	// Mixed-case addresses must be valid EIP-55 checksums.
	bad := topupTestPayment(cfg, r, func(m map[string]any) {
		m["payload"].(map[string]any)["authorization"].(map[string]any)["from"] = "0xAbCdEf0000000000000000000000000000000000"
	})
	if _, err = ParseTopupPayment(bad); code(err) != "payment_invalid" {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err = ParseTopupPayment(strings.Repeat("A", TopupPaymentBytes+4)); code(err) != "payment_invalid" {
		t.Fatal("oversized header accepted")
	}
}

func TestTopupSettleFailsClosed(t *testing.T) {
	cfg, _ := ParseTopupConfig([]byte(topupTestConfig), nil)
	r := TopupRequirement{Amount: 250_000, Quote: "4000.salt.mac", Expires: 4000}
	p, _ := ParseTopupPayment(topupTestPayment(cfg, r, nil))
	cases := []struct {
		name           string
		verify, settle func(w http.ResponseWriter)
		code           string
		definite       bool
	}{
		{"verify down", func(w http.ResponseWriter) { w.WriteHeader(502) }, nil, "payment_unsettled", true},
		{"verify not JSON", func(w http.ResponseWriter) { _, _ = w.Write([]byte("ok")) }, nil, "payment_unsettled", true},
		{"payer differs", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"isValid":true,"payer":"0x3333333333333333333333333333333333333333"}`))
		}, nil, "payment_rejected", true},
		{"settle down", nil, func(w http.ResponseWriter) { w.WriteHeader(500) }, "payment_unsettled", false},
		{"settle huge", nil, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"success":true,"x":"` + strings.Repeat("a", 20<<10) + `"}`))
		}, "payment_unsettled", false},
		{"settle other network", nil, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"success":true,"transaction":"0x` + strings.Repeat("1", 64) + `","network":"eip155:1"}`))
		}, "payment_unsettled", false},
		{"settle refused", nil, func(w http.ResponseWriter) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"success":false,"errorReason":"x"}`))
		}, "payment_rejected", true},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.URL.Path == "/verify" && c.verify != nil:
				c.verify(w)
			case req.URL.Path == "/verify":
				_, _ = w.Write([]byte(`{"isValid":true}`))
			case req.URL.Path == "/settle" && c.settle != nil:
				c.settle(w)
			default:
				_, _ = w.Write([]byte(`{"success":true,"transaction":"0x` + strings.Repeat("1", 64) + `"}`))
			}
		}))
		cfg.UseTestFacilitator(srv.Client(), srv.URL)
		_, err := cfg.Settle(context.Background(), nil, p, r)
		var te *TopupError
		if !errors.As(err, &te) || te.Code != c.code || te.Definite != c.definite {
			t.Errorf("%s: %v", c.name, err)
		}
		srv.Close()
	}
}

var topupHexRE = regexp.MustCompile(`^0x[0-9a-f]+$`)

// FuzzParseTopupPayment: the payment header parser never panics, and what
// it accepts is well formed.
func FuzzParseTopupPayment(f *testing.F) {
	cfg, _ := ParseTopupConfig([]byte(topupTestConfig), nil)
	r := TopupRequirement{Amount: 250_000, Quote: "4000.salt.mac", Expires: 4000}
	good := topupTestPayment(cfg, r, nil)
	raw, _ := base64.StdEncoding.DecodeString(good)
	f.Add(good)
	f.Add(string(raw))
	f.Add(base64.RawURLEncoding.EncodeToString(raw))
	f.Add("")
	f.Add("eyJ4NDAyVmVyc2lvbiI6Mn0=")
	f.Add(base64.StdEncoding.EncodeToString([]byte(`{"x402Version":2,"accepted":{},"payload":{}}`)))
	f.Fuzz(func(t *testing.T, header string) {
		p, err := ParseTopupPayment(header)
		if err != nil {
			return
		}
		if len(header) > TopupPaymentBytes || p.Value <= 0 || p.From == (EVMAddress{}) || len(p.Nonce) != 66 || len(p.Signature) != 132 ||
			!topupHexRE.MatchString(p.Nonce) || !topupHexRE.MatchString(p.Signature) || p.Quote == "" || p.ValidBefore < 0 || p.ValidAfter < 0 {
			t.Fatalf("accepted a malformed payment: %+v", p)
		}
		// What we would send the facilitator is valid JSON.
		body, err := cfg.facilitatorBody(p, cfg.topup(r))
		if err != nil || !json.Valid(body) {
			t.Fatalf("facilitator body: %v", err)
		}
	})
}
