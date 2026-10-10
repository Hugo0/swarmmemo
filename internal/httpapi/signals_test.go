package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C160: an accepted write over HTTP records its User-Agent, the Referer's
// origin and path, Accept-Language and client hints, with the address the
// server trusts: X-Forwarded-For only from the loopback proxy, else the
// peer. A verified email bridge adds its sending domain; anyone else's
// claim of one is ignored. None of it reaches a public answer.
func TestWriteSignalsOverHTTP(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "board.sqlite"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.SetAllowanceParams(t.Context(), board.PostingParamsNamespace, []byte(`{"anonymous_top_level_per_hour":1000}`), "test", 0); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("b", 40)
	s := New(store, nil, Config{PublicURL: "https://swarmmemo.com", ServiceID: "swarmmemo.com", Version: "test", TrustLoopbackProxy: true, BridgeTokens: map[string]string{"email": token}})
	const canary = "canary-c160-http/1.0"
	post := func(remote, xff string, headers map[string]string, text string) board.WriteSignal {
		t.Helper()
		r := httptest.NewRequest("POST", "https://swarmmemo.com/v1/command", strings.NewReader(`{"operation":"post","room":"lobby","text":"`+text+`","request_id":"`+text+`"}`))
		r.RemoteAddr = remote + ":4000"
		r.Header.Set("Content-Type", "application/json")
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var res board.Result
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.Receipt == nil {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), canary) || strings.Contains(w.Body.String(), "ip_hash") {
			t.Fatalf("a signal in the answer: %s", w.Body.String())
		}
		sig, ok, err := store.SignalMessage(t.Context(), res.Receipt.ID)
		if err != nil || !ok {
			t.Fatalf("no signal for %s: %v", res.Receipt.ID, err)
		}
		return sig
	}
	browser := map[string]string{"User-Agent": canary, "Referer": "https://blog.example/post/1?utm=x&text=secret", "Accept-Language": "fr-FR,fr;q=0.8",
		"Sec-CH-UA": `"Chromium";v="130"`, "Sec-CH-UA-Platform": `"Linux"`, "Sec-CH-UA-Mobile": "?0"}
	proxied := post("127.0.0.1", "1.2.3.4, 198.51.100.6", browser, "proxied")
	if proxied.UserAgent != canary || proxied.Referer != "https://blog.example/post/1" || proxied.AcceptLanguage != "fr-FR,fr;q=0.8" || proxied.Via != "command" ||
		!strings.Contains(proxied.SecCHUA, `Sec-CH-UA-Platform="Linux"`) || !strings.Contains(proxied.SecCHUA, "Sec-CH-UA-Mobile=?0") {
		t.Fatalf("headers not captured: %+v", proxied)
	}
	direct := post("198.51.100.6", "", nil, "direct")
	if direct.IPHashFull == "" || direct.IPHashFull != proxied.IPHashFull {
		t.Fatalf("the proxy's last X-Forwarded-For hop is the address: %s vs %s", proxied.IPHashFull, direct.IPHashFull)
	}
	spoof := post("198.51.100.5", "198.51.100.6", map[string]string{"X-SwarmMemo-Bridge-Sender-Domain": "spoof.example"}, "spoof")
	if spoof.IPHashFull == direct.IPHashFull || spoof.IPHash24 != direct.IPHash24 || spoof.Origin != "" {
		t.Fatalf("X-Forwarded-For or a sender domain trusted from outside the proxy: %+v", spoof)
	}
	mail := post("127.0.0.1", "192.0.2.8", map[string]string{"X-SwarmMemo-Bridge": "email", "X-SwarmMemo-Bridge-Token": token, "X-SwarmMemo-Bridge-Sender-Domain": "Mail.Example", "X-Forwarded-Proto": "https"}, "mail")
	if mail.Via != "email" || mail.Origin != "email:mail.example" {
		t.Fatalf("verified bridge: %+v", mail)
	}
	for _, path := range []string{"/api/stats", "/api/stats/daily", "/capabilities", "/v1/export", "/api/messages?room=lobby", "/e/" + "lobby"} {
		r := httptest.NewRequest("GET", "https://swarmmemo.com"+path, nil)
		r.RemoteAddr = "203.0.113.1:4000"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		for _, leak := range []string{canary, "fr-FR", "blog.example", "ip_hash", "write_signal", store.SignalsKeyID()} {
			if strings.Contains(w.Body.String(), leak) {
				t.Fatalf("GET %s carries %q", path, leak)
			}
		}
	}
}
