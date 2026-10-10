package board

// The GitHub half of "Raise your standing": the link checker reads a
// linked account and its gist from GitHub's public REST API, without a
// token. The API allows an unauthenticated caller 60 requests an hour, so
// every request goes through one global budget (githubHourly), a reply
// that says the limit is spent pauses all requests until its reset, users
// are cached for githubUserTTL, and a verified link is rechecked weekly. The
// one host it calls is fixed; the dialer still refuses any address that is
// not public (safenet), so a poisoned resolver cannot turn it inward.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/safenet"
	"swarmmemo/internal/trust"
)

const (
	githubAPI          = "https://api.github.com"
	githubTimeout      = 8 * time.Second
	githubBodyMax      = 1 << 20
	githubHourly       = 50 // of the 60 an unauthenticated caller gets
	githubUserTTL      = 6 * time.Hour
	githubCacheMax     = 4096
	githubGistFilesMax = 10
	githubFileMax      = 64 << 10
	// standingGitHubRecheck is how often a verified GitHub link is read again.
	standingGitHubRecheck = 7 * 86400
)

// githubFetch gets one API path; tests replace it (Store.github.fetch).
type githubFetch func(ctx context.Context, path string) (status int, header http.Header, body []byte, err error)

// githubState is the client's shared budget, pause and cache.
type githubState struct {
	fetch githubFetch

	mu        sync.Mutex
	tokens    float64
	refilled  time.Time
	pausedTil time.Time
	users     map[string]githubUserEntry
}

type githubUser struct {
	Login       string `json:"login"`
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	CreatedAt   string `json:"created_at"`
	PublicRepos int64  `json:"public_repos"`
	Followers   int64  `json:"followers"`
}

type githubUserEntry struct {
	user  githubUser
	until time.Time
}

var githubClient = &http.Client{
	Timeout:       githubTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safenet.Dial(ctx, network, addr, githubTimeout)
		},
		TLSHandshakeTimeout:   githubTimeout,
		ResponseHeaderTimeout: githubTimeout,
		MaxIdleConns:          2,
		IdleConnTimeout:       60 * time.Second,
	},
}

// defaultGitHubFetch is the real API call: GET, JSON, no credentials.
func defaultGitHubFetch(ctx context.Context, path string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "swarmmemo-standing")
	resp, err := githubClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, githubBodyMax+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(body) > githubBodyMax {
		return resp.StatusCode, resp.Header, nil, errGitHubTooLarge
	}
	return resp.StatusCode, resp.Header, body, nil
}

var errGitHubTooLarge = errors.New("github: answer too large")

// admit takes one request from the hourly budget, unless a reply said the
// limit is spent.
func (g *githubState) admit(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.pausedTil) {
		return false
	}
	if g.refilled.IsZero() {
		g.tokens, g.refilled = githubHourly, now
	}
	g.tokens = min(githubHourly, g.tokens+now.Sub(g.refilled).Hours()*githubHourly)
	g.refilled = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// observe reads GitHub's rate headers: a spent limit pauses every request
// until its reset (at most an hour).
func (g *githubState) observe(status int, h http.Header, now time.Time) {
	if h == nil {
		return
	}
	spent := h.Get("X-RateLimit-Remaining") == "0" || status == http.StatusTooManyRequests
	if !spent {
		return
	}
	until := now.Add(time.Hour)
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if t := time.Unix(reset, 0); t.After(now) && t.Before(until) {
			until = t
		}
	}
	g.mu.Lock()
	g.pausedTil = until
	g.mu.Unlock()
}

// get fetches path within the budget: the answer, or linkUnreachable when
// nothing conclusive came back (budget spent, network, 5xx, rate limit).
func (s *Store) githubGet(ctx context.Context, path string, into any) (int, linkOutcome) {
	g := &s.github
	if !g.admit(s.now()) {
		return 0, linkUnreachable
	}
	fetch := g.fetch
	if fetch == nil {
		fetch = defaultGitHubFetch
	}
	status, header, body, err := fetch(ctx, path)
	g.observe(status, header, s.now())
	switch {
	case errors.Is(err, errGitHubTooLarge):
		return status, linkFailed
	case err != nil, status == http.StatusTooManyRequests, status == http.StatusForbidden, status >= 500:
		return status, linkUnreachable
	case status == http.StatusNotFound:
		return status, linkFailed
	case status != http.StatusOK:
		return status, linkUnreachable
	}
	if json.Unmarshal(body, into) != nil {
		return status, linkUnreachable
	}
	return status, linkPassed
}

// githubUserOf reads an account, from the cache when it is recent.
func (s *Store) githubUserOf(ctx context.Context, login string) (githubUser, linkOutcome) {
	g := &s.github
	now := s.now()
	g.mu.Lock()
	if e, ok := g.users[login]; ok && now.Before(e.until) {
		g.mu.Unlock()
		return e.user, linkPassed
	}
	g.mu.Unlock()
	var u githubUser
	if _, outcome := s.githubGet(ctx, "/users/"+login, &u); outcome != linkPassed {
		return u, outcome
	}
	if u.ID <= 0 || !strings.EqualFold(u.Login, login) {
		return u, linkFailed
	}
	g.mu.Lock()
	if g.users == nil {
		g.users = map[string]githubUserEntry{}
	}
	if len(g.users) >= githubCacheMax {
		for k, e := range g.users {
			if !now.Before(e.until) || len(g.users) >= githubCacheMax {
				delete(g.users, k)
			}
		}
	}
	g.users[login] = githubUserEntry{user: u, until: now.Add(githubUserTTL)}
	g.mu.Unlock()
	return u, linkPassed
}

// gistAuthorizes reports whether a gist is owned by user id and one of its
// first files holds statement. Files and their text are hostile input: only
// the first githubGistFilesMax files and githubFileMax bytes of each are read.
func gistAuthorizes(raw gistAnswer, ownerID int64, statement string) bool {
	if raw.Owner.ID != ownerID || ownerID <= 0 {
		return false
	}
	n := 0
	for _, f := range raw.Files {
		if n++; n > githubGistFilesMax {
			break
		}
		text := f.Content
		if len(text) > githubFileMax {
			text = text[:githubFileMax]
		}
		if strings.Contains(text, statement) {
			return true
		}
	}
	return false
}

type gistAnswer struct {
	Owner struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"owner"`
	Files map[string]struct {
		Content string `json:"content"`
	} `json:"files"`
}

// checkGitHubLink is the github link's live check: the gist the link names
// is owned by the linked account and publishes the link's statement. A pass
// records the account's assessment: its numeric id (the root, so a rename
// keeps it), age and public counts, all public.
func (s *Store) checkGitHubLink(ctx context.Context, fingerprint, login, proof string) linkOutcome {
	r, ok := decodeLinkRecord(proof)
	gist, okGist := gistID(r.Proof)
	if !ok || !okGist || r.Message == "" {
		return linkFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 2*githubTimeout)
	defer cancel()
	user, outcome := s.githubUserOf(ctx, login)
	if outcome != linkPassed {
		return outcome
	}
	var g gistAnswer
	if _, outcome = s.githubGet(ctx, "/gists/"+gist, &g); outcome != linkPassed {
		return outcome
	}
	if !gistAuthorizes(g, user.ID, r.Message) {
		return linkFailed
	}
	now := s.now().Unix()
	age := int64(0)
	if t, err := time.Parse(time.RFC3339, user.CreatedAt); err == nil && t.Unix() < now {
		age = (now - t.Unix()) / 86400
	}
	cents := trust.GitHubCents(age, user.PublicRepos, user.Followers)
	_ = s.saveAssessment(ctx, fingerprint, "github", login, "github:"+strconv.FormatInt(user.ID, 10), cents, map[string]any{
		"source": "api.github.com", "github_id": user.ID, "created_at": user.CreatedAt, "age_days": age,
		"public_repos": user.PublicRepos, "followers": user.Followers, "gist": gist}, now)
	return linkPassed
}
