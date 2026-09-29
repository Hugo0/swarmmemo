package cards

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/safenet"
)

// The Cloudflare renderer is Browser Rendering's REST /screenshot endpoint.
// It is given the card page's bytes as "html", never a URL, so the remote
// browser renders exactly what this server wrote and fetches nothing: the
// page's own policy forbids every request, JavaScript is switched off, and
// every subresource type is rejected on Cloudflare's side as well.
//
// The client can reach exactly one host, api.cloudflare.com on 443, through a
// dialer that re-checks every resolved address is public (safenet.PublicIP,
// the rule every outbound request shares), follows no redirect, uses no proxy
// and reads a bounded response.

const (
	cloudflareHost     = "api.cloudflare.com"
	cloudflareBase     = "https://" + cloudflareHost + "/client/v4"
	cloudflareTimeout  = 25 * time.Second
	cloudflareDial     = 5 * time.Second
	cloudflareErrBytes = 4 << 10
)

// MaxImageBytes bounds one card image from any renderer, and so each cache entry.
const MaxImageBytes = 1 << 20

var (
	errCloudflareHost    = errors.New("cards: the renderer may reach only " + cloudflareHost)
	errCloudflareAddress = errors.New("cards: " + cloudflareHost + " resolved to a non-public address")
	// ErrRendererLimited is Cloudflare's 429: its own quota, not ours.
	ErrRendererLimited = errors.New("cards: Cloudflare rate limited the render")
)

type cloudflare struct {
	base, account, token string
	client               *http.Client
	lookup               func(context.Context, string) ([]net.IP, error)
}

func newCloudflare(account, token string) *cloudflare {
	c := &cloudflare{base: cloudflareBase, account: account, token: token, lookup: func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}}
	c.client = &http.Client{
		Timeout:       cloudflareTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           c.dial,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cloudflareHost},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: cloudflareTimeout,
			DisableKeepAlives:     true,
			MaxIdleConns:          0,
		},
	}
	return c
}

// dial connects only to api.cloudflare.com:443, and only to public addresses.
func (c *cloudflare) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !strings.EqualFold(host, cloudflareHost) || port != "443" {
		return nil, errCloudflareHost
	}
	ips, err := c.lookup(ctx, cloudflareHost)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cards: resolve %s: %w", cloudflareHost, err)
	}
	for _, ip := range ips {
		if safenet.PublicIP(ip) != nil {
			return nil, errCloudflareAddress
		}
	}
	dialer := &net.Dialer{Timeout: cloudflareDial}
	var last error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// rejectedResources are every subresource type a page could request.
var rejectedResources = []string{"stylesheet", "image", "media", "font", "script", "texttrack", "xhr", "fetch", "eventsource", "websocket", "manifest", "other"}

// screenshotRequest is the /screenshot body. It has an html field and no url
// field: a URL cannot be expressed.
type screenshotRequest struct {
	HTML                string         `json:"html"`
	Viewport            map[string]int `json:"viewport"`
	ScreenshotOptions   map[string]any `json:"screenshotOptions"`
	GotoOptions         map[string]any `json:"gotoOptions"`
	SetJavaScriptEnable bool           `json:"setJavaScriptEnabled"`
	RejectResourceTypes []string       `json:"rejectResourceTypes"`
}

func (c *cloudflare) screenshot(ctx context.Context, page []byte) ([]byte, error) {
	body, err := json.Marshal(screenshotRequest{
		HTML:                string(page),
		Viewport:            map[string]int{"width": Width, "height": Height, "deviceScaleFactor": 1},
		ScreenshotOptions:   map[string]any{"type": "png", "fullPage": false, "omitBackground": false},
		GotoOptions:         map[string]any{"waitUntil": "load", "timeout": 15000},
		SetJavaScriptEnable: false,
		RejectResourceTypes: rejectedResources,
	})
	if err != nil {
		return nil, err
	}
	endpoint := c.base + "/accounts/" + c.account + "/browser-rendering/screenshot"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "image/png")
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRendererLimited
	}
	if res.StatusCode != http.StatusOK {
		// The error body is JSON from Cloudflare; only its size is trusted.
		raw, _ := io.ReadAll(io.LimitReader(res.Body, cloudflareErrBytes))
		return nil, fmt.Errorf("cards: Cloudflare answered %d: %s", res.StatusCode, oneLineASCII(raw, 300))
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxImageBytes {
		return nil, errors.New("cards: Cloudflare image exceeds the size cap")
	}
	if err := checkPNG(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// checkPNG accepts only a PNG of the card's exact size: whatever the remote
// renderer returns is served from our origin, so it is verified, not trusted.
func checkPNG(raw []byte) error {
	if board.ImageMediaType(raw) != "image/png" {
		return errors.New("cards: renderer did not return a PNG")
	}
	w, h, ok := pngSize(raw)
	if !ok || w != Width || h != Height {
		return fmt.Errorf("cards: renderer returned %dx%d, want %dx%d", w, h, Width, Height)
	}
	return nil
}

func pngSize(raw []byte) (int, int, bool) {
	// The IHDR chunk follows the 8-byte signature and its 8-byte header.
	if len(raw) < 24 || string(raw[12:16]) != "IHDR" {
		return 0, 0, false
	}
	be := func(b []byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
	return be(raw[16:20]), be(raw[20:24]), true
}

func oneLineASCII(raw []byte, n int) string {
	var b strings.Builder
	for _, c := range raw {
		if b.Len() >= n {
			break
		}
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}
