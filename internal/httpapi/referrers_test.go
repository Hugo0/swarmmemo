package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

func TestReferrerKeyKeepsOnlyTheDomain(t *testing.T) {
	c := newReferrerCounter("https://board.example")
	for raw, want := range map[string]string{
		"":                                             "",
		"https://swarmmemo.com/r/lobby":                "",
		"https://www.swarmmemo.com/":                   "",
		"https://board.example/e/1":                    "",
		"https://api.board.example/":                   "",
		"https://evilswarmmemo.com/":                   "host:evilswarmmemo.com",
		"https://swarmmemo.com.evil.example/x":         "host:evil.example",
		"https://news.ycombinator.com/item?id=1":       "host:ycombinator.com",
		"https://WWW.Example.COM./Path?q=secret#f":     "host:example.com",
		"http://user:password@example.org:8443/a":      "host:example.org",
		"https://www.bbc.co.uk/news":                   "host:bbc.co.uk",
		"https://co.uk/":                               "other",
		"https://alice.github.io/private-blog":         "host:github.io",
		"https://xn--bcher-kva.de/":                    "host:xn--bcher-kva.de",
		"https://bücher.de/":                           "other",
		"https://еxample.com/":                         "other", // Cyrillic е
		"https://93.184.216.34/":                       "other",
		"http://127.0.0.1:8080/admin":                  "other",
		"http://[2001:db8::1]:443/":                    "other",
		"http://[::ffff:10.0.0.1]/":                    "other",
		"http://localhost:3000/":                       "other",
		"http://1.2.3/":                                "other",
		"http://0x7f.1/":                               "other",
		"android-app://com.google.android.gm/":         "other",
		"javascript:alert(1)":                          "other",
		"data:text/html,<script>":                      "other",
		"/relative/path":                               "other",
		"//example.com/no-scheme":                      "other",
		"not a url at all":                             "other",
		"https://exa mple.com/":                        "other",
		"https://example.com\r\nX-Injected: 1":         "other",
		"https://%65xample.com/":                       "other",
		"https://-bad-.com/":                           "other",
		"https://" + strings.Repeat("a", 64) + ".com/": "other",
		"https://example.com/" + strings.Repeat("p", referrerHeaderBytes): "other",
	} {
		if got := c.referrerKey(raw); got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
		if got := c.referrerKey(raw); strings.Contains(got, "/") || strings.Contains(got, "?") || strings.Contains(got, "@") || strings.Contains(got, "secret") {
			t.Errorf("%q leaked more than a domain: %q", raw, got)
		}
	}
}

func TestUserAgentFamilies(t *testing.T) {
	for raw, want := range map[string]string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)":                                                "Googlebot",
		"Mozilla/5.0 (Linux; Android 6.0.1) Chrome/41 Mobile Safari/537.36 (compatible; Googlebot/2.1)":                           "Googlebot",
		"Mozilla/5.0 (compatible; GoogleOther)":                                                                                   "GoogleOther",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)":                                                 "Bingbot",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)":                  "GPTBot",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; OAI-SearchBot/1.0; +https://openai.com/searchbot":        "OAI-SearchBot",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0; +https://openai.com/bot":               "ChatGPT-User",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)":                 "ClaudeBot",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)":             "Claude-User",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-SearchBot/1.0)":                                    "Claude-SearchBot",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)": "PerplexityBot",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Perplexity-User/1.0)":                                     "Perplexity-User",
		"Mozilla/5.0 (Macintosh) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/13.1 Safari/605.1.15 (Applebot/0.1)":            "Applebot",
		"CCBot/2.0 (https://commoncrawl.org/faq/)":                                                                                "CCBot",
		"meta-externalagent/1.1 (+https://developers.facebook.com/docs/sharing/webmasters/crawler)":                               "meta-externalagent",
		"facebookexternalhit/1.1":                 "FacebookBot",
		"Mozilla/5.0 (compatible; YandexBot/3.0)": "YandexBot",
		"DuckAssistBot/1.2":                       "DuckAssistBot",
		"curl/8.5.0":                              "curl",
		"Wget/1.21.4":                             "Wget",
		"python-requests/2.32.3":                  "python-requests",
		"python-httpx/0.27.0":                     "python-httpx",
		"Python-urllib/3.12":                      "Python-urllib",
		"Go-http-client/2.0":                      "Go-http-client",
		"node":                                    "node",
		"axios/1.7.2":                             "axios",
		"SomeNewCrawler/1.0 (+https://crawler.example)":              "other-bot",
		"Mozilla/5.0 (X11; Linux x86_64) Chrome/140.0 Safari/537.36": "",
		"":             "",
		"nodejs-thing": "",
		strings.Repeat("x", userAgentBytes) + "ClaudeBot": "", // past the inspected prefix
	} {
		got := userAgentFamily(raw)
		if got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
	// Every family the matcher can produce is one storage accepts.
	known := map[string]bool{}
	for _, family := range board.UserAgentFamilies {
		known[family] = true
	}
	for _, m := range userAgentMarkers {
		if !known[m.family] {
			t.Errorf("family %s is not in board.UserAgentFamilies", m.family)
		}
	}
	for _, family := range []string{"node", "other-bot"} {
		if !known[family] {
			t.Errorf("family %s is not in board.UserAgentFamilies", family)
		}
	}
}

// A flood of distinct referrers cannot grow memory: past the bound, new
// domains count as other until the next write.
func TestReferrerMemoryIsBounded(t *testing.T) {
	s := New(&fakeService{}, nil, Config{})
	for i := 0; i < 3*referrerPendingHosts; i++ {
		r := httptestRequest("GET", "/", "https://site"+strconv.Itoa(i)+".example/")
		s.countReferrer(r)
	}
	s.referrers.mu.Lock()
	defer s.referrers.mu.Unlock()
	if len(s.referrers.pending) != 1 {
		t.Fatalf("days pending: %d", len(s.referrers.pending))
	}
	for _, d := range s.referrers.pending {
		if d.hosts != referrerPendingHosts || len(d.counts) != referrerPendingHosts+1 || d.counts["other"] != int64(2*referrerPendingHosts) {
			t.Fatalf("hosts %d, keys %d, other %d", d.hosts, len(d.counts), d.counts["other"])
		}
	}
}

// Counted end to end, stored as domains only, and never served publicly.
func TestReferrersStoredPrivately(t *testing.T) {
	store, err := board.Open(filepath.Join(t.TempDir(), "referrers.sqlite"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store, web.Handler(store), Config{PublicURL: "https://swarmmemo.com"})
	for i := 0; i < 3; i++ {
		r := httptestRequest("GET", "/for-agents", "https://news.ycombinator.com/item?id=secret-thread")
		r.Header.Set("User-Agent", "Mozilla/5.0 (compatible; ClaudeBot/1.0)")
		serve(s, r)
	}
	serve(s, httptestRequest("GET", "/", "https://swarmmemo.com/r/lobby"))
	s.FlushReaderCounts()
	days, err := store.ReadReferrerStats(context.Background(), time.Now(), 1)
	if err != nil || len(days) != 1 {
		t.Fatal(err)
	}
	day := days[0]
	if len(day.Hosts) != 1 || day.Hosts[0] != (board.ReferrerCount{Name: "ycombinator.com", Count: 3}) || day.Other != 0 {
		t.Fatalf("hosts: %+v other %d", day.Hosts, day.Other)
	}
	if len(day.Agents) != 1 || day.Agents[0] != (board.ReferrerCount{Name: "ClaudeBot", Count: 3}) {
		t.Fatalf("agents: %+v", day.Agents)
	}
	for _, path := range []string{"/api/stats/daily", "/capabilities", "/api/stats", "/llms.txt", "/metrics"} {
		body := makeRequest(s, "GET", path, "", "").Body.String()
		if strings.Contains(body, "ycombinator") || strings.Contains(body, "secret-thread") {
			t.Fatalf("%s exposes referrer data", path)
		}
	}
}

func httptestRequest(method, path, referer string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "198.51.100.9:12345"
	r.Header.Set("Accept", "text/html")
	if referer != "" {
		r.Header.Set("Referer", referer)
	}
	return r
}

func serve(s http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
