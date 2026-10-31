package board

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"swarmmemo/internal/proofs"
)

// Provider sign-in (login.go) against a fake X: one provider account is one
// hosted identity, also when two callbacks race; removing the link locks
// the account out without making another; an unconfigured provider is
// refused cleanly.
func TestLoginOneAccountOneIdentity(t *testing.T) {
	x := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.x.com/2/oauth2/token":
			_ = r.ParseForm()
			if r.PostForm.Get("redirect_uri") != "https://swarmmemo.com/login/x/callback" || r.PostForm.Get("code_verifier") == "" {
				http.Error(w, `{"error":"invalid_grant"}`, 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + r.PostForm.Get("code"), "token_type": "bearer"})
		case "/api.x.com/2/users/me":
			user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": map[string]string{"dana": "7001", "eve": "7002"}[user], "username": user}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(x.Close)
	reg := proofs.New(proofs.Config{OAuth: map[string]proofs.OAuthCredentials{"x": {ClientID: "id", ClientSecret: "secret"}}, Client: x.Client(), TestBaseURL: x.URL})
	s := openTest(t, Config{HostedKEKFile: writeKEK(t, 0o600), Proofs: reg, PublicURL: "https://swarmmemo.com"})
	verifier := strings.Repeat("v", 43)

	if _, err := s.LoginAuthURL("google", "state", verifier); errCode(err) != "sign_in_provider_off" {
		t.Fatalf("google not set up: %v", err)
	}
	if _, err := s.LoginAuthURL("discord", "state", verifier); errCode(err) != "not_found" {
		t.Fatalf("discord is no sign-in provider: %v", err)
	}
	if got := s.LoginProviders(); len(got) != 3 || !got[0].Enabled || got[1].Enabled || got[2].Enabled {
		t.Fatalf("providers: %+v", got)
	}

	// Two callbacks for one new account at once make one identity.
	var wg sync.WaitGroup
	results := make([]LoginResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.LoginComplete(testContext, "x", "dana", verifier, "198.51.100.40")
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].Account != results[1].Account || results[0].Created == results[1].Created {
		t.Fatalf("racing sign-ins: %+v %v", results, errs)
	}
	first := results[0]
	if !first.Created {
		first = results[1]
	}
	if first.Handle != "dana" || first.RecoveryCode == "" || first.Token == "" || first.Root != "x:7001" {
		t.Fatalf("created: %+v", first)
	}
	again, err := s.LoginComplete(testContext, "x", "dana", verifier, "198.51.100.41")
	if err != nil || again.Created || again.Account != first.Account || again.Token != "" || again.RecoveryCode != "" {
		t.Fatalf("again: %+v %v", again, err)
	}
	// A web sign-in's token replaces the previous one.
	tok1, _, err := s.LoginToken(testContext, first.Account, "x")
	if err != nil {
		t.Fatal(err)
	}
	tok2, _, err := s.LoginToken(testContext, first.Account, "x")
	if err != nil || tok1 == tok2 {
		t.Fatal(err)
	}
	if _, err := s.HostedAccount(testContext, tok1, 2); err == nil {
		t.Fatal("the replaced sign-in token still works")
	}
	if acct, err := s.HostedAccount(testContext, tok2, 2); err != nil || acct != first.Account {
		t.Fatalf("the new sign-in token: %q %v", acct, err)
	}

	// Removing the link locks the account out, and it makes no new identity.
	if _, err := s.db.Exec("DELETE FROM identity_links WHERE agent=? AND kind='x'", first.Account); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoginComplete(testContext, "x", "dana", verifier, "198.51.100.40"); errCode(err) != "sign_in_unlinked" {
		t.Fatalf("after unlink: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM hosted_keys").Scan(&n); err != nil || n != 1 {
		t.Fatalf("hosted identities: %d %v", n, err)
	}
	// Another account makes its own.
	other, err := s.LoginComplete(testContext, "x", "eve", verifier, "198.51.100.40")
	if err != nil || !other.Created || other.Account == first.Account {
		t.Fatalf("eve: %+v %v", other, err)
	}
	// A refused code creates nothing.
	if _, err := s.LoginComplete(testContext, "x", "dana", "", "198.51.100.40"); errCode(err) != "sign_in_failed" {
		t.Fatalf("no verifier: %v", err)
	}
}
