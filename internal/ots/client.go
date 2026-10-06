package ots

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultCalendars are the public calendars the reference client uses.
var DefaultCalendars = []string{
	"https://alice.btc.calendar.opentimestamps.org",
	"https://bob.btc.calendar.opentimestamps.org",
	"https://finney.calendar.eternitywall.com",
}

// MaxResponseBytes bounds a calendar's answer.
const MaxResponseBytes = 16 << 10

// Client submits digests to calendars and upgrades pending proofs. A pending
// attestation is followed only to a calendar in Calendars (same scheme and
// host), never to an address a calendar's answer names.
type Client struct {
	Calendars []string
	HTTP      *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Stamp submits digest (32 bytes) to every calendar and returns the .ots
// file combining the answers, with one error per calendar that failed. It
// fails only when no calendar answered.
func (c *Client) Stamp(ctx context.Context, digest []byte) (*File, []error) {
	if len(digest) != 32 {
		return nil, []error{errors.New("ots: digest must be 32 bytes")}
	}
	root := &Timestamp{Msg: bytes.Clone(digest)}
	var errs []error
	for _, cal := range c.Calendars {
		body, status, err := c.do(ctx, http.MethodPost, strings.TrimSuffix(cal, "/")+"/digest", digest)
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("status %d", status)
		}
		var t *Timestamp
		if err == nil {
			t, err = ParseTimestamp(body, digest)
		}
		if err == nil {
			err = root.Merge(t)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cal, err))
		}
	}
	if len(root.Attestations) == 0 && len(root.Branches) == 0 {
		if len(errs) == 0 {
			errs = append(errs, errors.New("ots: no calendars configured"))
		}
		return nil, errs
	}
	return &File{Digest: bytes.Clone(digest), Stamp: root}, errs
}

// Upgrade asks each pending attestation's calendar for its completed
// timestamp and merges every answer it gets. changed reports whether the
// file grew; a calendar that has not finished answers 404, which is no error.
func (c *Client) Upgrade(ctx context.Context, f *File) (changed bool, err error) {
	var pending []*Timestamp
	f.Stamp.Walk(func(t *Timestamp) {
		for _, a := range t.Attestations {
			if a.PendingURI() != "" {
				pending = append(pending, t)
				return
			}
		}
	})
	var errs []error
	for _, t := range pending {
		for _, a := range t.Attestations {
			uri := a.PendingURI()
			if uri == "" || !c.allowed(uri) {
				continue
			}
			body, status, err := c.do(ctx, http.MethodGet, strings.TrimSuffix(uri, "/")+"/timestamp/"+hex.EncodeToString(t.Msg), nil)
			if err == nil && status == http.StatusNotFound {
				continue
			}
			if err == nil && status != http.StatusOK {
				err = fmt.Errorf("status %d", status)
			}
			var up *Timestamp
			if err == nil {
				up, err = ParseTimestamp(body, t.Msg)
			}
			if err == nil {
				before := len(f.Bytes())
				if err = t.Merge(up); err == nil && len(f.Bytes()) != before {
					changed = true
				}
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", uri, err))
			}
		}
	}
	if len(f.Bytes()) > MaxFileBytes {
		return false, errors.New("ots: upgraded proof too large")
	}
	return changed, errors.Join(errs...)
}

func (c *Client) allowed(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	for _, cal := range c.Calendars {
		if v, err := url.Parse(cal); err == nil && v.Scheme == u.Scheme && v.Host == u.Host {
			return true
		}
	}
	return false
}

func (c *Client) do(ctx context.Context, method, target string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.opentimestamps.v1")
	req.Header.Set("User-Agent", "swarmmemo-ots/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > MaxResponseBytes {
		return nil, 0, errors.New("ots: calendar answer too large")
	}
	return data, resp.StatusCode, nil
}
