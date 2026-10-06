package httpapi

// Fuzzing the OAuth parsers: dynamic client registration and the token
// endpoint take any bytes from anyone, so neither may panic, accept a
// redirect URI validRedirectURI refuses, or answer anything but RFC 6749
// JSON. Run with -fuzz for at most 60 s.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"swarmmemo/internal/board"
)

func FuzzOAuthRegistration(f *testing.F) {
	for _, seed := range []string{
		`{"client_name":"Test App","redirect_uris":["https://app.example/cb"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`,
		`{"redirect_uris":["http://127.0.0.1:8123/cb","http://[::1]/cb","http://localhost/cb"]}`,
		`{"redirect_uris":["javascript:alert(1)"]}`,
		`{"redirect_uris":["https://a.example/cb#x"],"client_name":"‮evil"}`,
		`{"redirect_uris":[]}`, `[]`, `null`, ``, `{"redirect_uris":"https://a.example/cb"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		name, redirects, err := parseRegistration(body)
		if err != nil {
			if _, ok := err.(*board.OAuthError); !ok {
				t.Fatalf("a registration error that is not an OAuth error: %v", err)
			}
			return
		}
		if len(redirects) == 0 || len(redirects) > board.OAuthRedirectURIsMax {
			t.Fatalf("accepted %d redirect URIs", len(redirects))
		}
		for _, u := range redirects {
			if !validRedirectURI(u) || strings.Contains(u, "#") || !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) {
				t.Fatalf("accepted redirect URI %q", u)
			}
		}
		if name == "" || len(name) > board.OAuthClientNameBytes || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\n\r‮") {
			t.Fatalf("client name %q", name)
		}
	})
}

func FuzzOAuthTokenRequest(f *testing.F) {
	for _, seed := range []struct{ contentType, body, auth string }{
		{"application/x-www-form-urlencoded", "grant_type=authorization_code&code=smc1_" + strings.Repeat("A", 43) + "&client_id=smcl_x&redirect_uri=https%3A%2F%2Fapp.example%2Fcb&code_verifier=" + strings.Repeat("v", 50), ""},
		{"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&refresh_token=smo1_" + strings.Repeat("B", 43) + "&resource=https%3A%2F%2Fswarmmemo.com%2Fmcp", ""},
		{"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&client_id=y", ""},
		{"application/x-www-form-urlencoded; charset=utf-8", "grant_type=password&%zz", ""},
		{"application/json", `{"grant_type":"refresh_token"}`, ""},
		{"application/x-www-form-urlencoded", "grant_type=refresh_token&client_id=x&refresh_token=y", "Basic eDp5"},
	} {
		f.Add(seed.contentType, seed.body, seed.auth)
	}
	_, s := hostedServer(f)
	// The per-network budget would answer 429 after a few hundred inputs.
	s.oauth.token = NewLimiterRate(1<<30, 1<<30)
	f.Fuzz(func(t *testing.T, contentType, body, auth string) {
		r := httptest.NewRequest("POST", "https://swarmmemo.com/oauth/token", strings.NewReader(body))
		r.RemoteAddr = "198.51.100.30:4000"
		r.Header.Set("Content-Type", contentType)
		if auth != "" && !strings.ContainsAny(auth, "\r\n\x00") {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %d %q", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("token answer cacheable: %q", w.Header().Get("Cache-Control"))
		}
		switch w.Code {
		case http.StatusOK:
			// Only a real code or refresh token gets tokens; fuzz inputs never hold one.
			t.Fatalf("tokens for fuzz input %q", body)
		case 400, 401, 405:
			if code, _ := out["error"].(string); code == "" || code == "server_error" {
				t.Fatalf("error answer %d %v", w.Code, out)
			}
		case http.StatusTooManyRequests:
			// The server-wide per-network budget: still a JSON answer.
		default:
			t.Fatalf("status %d %v", w.Code, out)
		}
	})
}
