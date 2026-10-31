package httpapi

// The authenticated live stream (C665, HexArena platform S4): GET
// /api/stream with a credential is the caller's own stream of stream.read
// (board/livestream.go): new messages in its app rooms and inbox, as
// server-sent events. The credential is an app's token (Authorization:
// Bearer, within its scopes exactly) or a signed stream.read command in the
// query (public_key, signature, timestamp, nonce, and cursor if signed).
//
// It is /api/stream again, not a second system: the same endpoint and
// capacity (streams), and the board's one committed-write signal (Changes)
// wakes it, so a message reaches it well under a second on one box. Every
// read is a whole command, checked again (a revoked or expired token, or a
// signature past its window, ends the stream with event: end). Each message
// event's id is its position; a client resumes with Last-Event-ID, as
// EventSource does by itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"swarmmemo/internal/board"
)

const (
	// liveStreamsPerSource bounds open authenticated streams per network
	// address and per key.
	liveStreamsPerSource = 4
	// liveStreamsTotal bounds open authenticated streams on the server: half
	// the shared stream capacity, so credentials anyone can mint never take
	// every slot from the public /api/stream.
	liveStreamsTotal = 32
	// liveStreamMax is the longest one stream stays open; a signed one ends
	// with its signature's window, an app token's with the token.
	liveStreamMax = 10 * time.Minute
	// liveKeepalive is shorter than the proxy's 60-second read timeout.
	liveKeepalive = 25 * time.Second
	// liveReadGap is the least time between two reads of one stream, so a
	// burst of writes costs one read, not one per write.
	liveReadGap = 100 * time.Millisecond
	// liveRetryMillis is the reconnect delay the stream asks of EventSource.
	liveRetryMillis = 1000
)

// liveStreamRequest reports a /api/stream request carrying a credential.
func liveStreamRequest(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.URL.Query().Has("public_key") || r.URL.Query().Has("signature")
}

// liveReader runs stream.read for the stream's credential at a position.
type liveReader func(ctx context.Context, cursor string) (board.Result, error)

func (s *Server) liveStream(w http.ResponseWriter, r *http.Request) {
	read, key, deadline, err := s.liveCredential(r)
	if err != nil {
		s.bearerError(w, err)
		return
	}
	release, full := s.lives.Acquire(s.peer(r), key)
	if full != "" {
		writeError(w, &board.Error{Status: 429, Code: "request_rate", RetryAfter: 10, Message: fmt.Sprintf("This network or credential already holds %d live streams, the most at once; close one first.", liveStreamsPerSource)})
		return
	}
	defer release()
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		writeError(w, &board.Error{Status: 503, Code: "stream_capacity", RetryAfter: 5, Message: "Live stream capacity reached. Read stream.read with a cursor instead, or retry shortly."})
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, bad("Streaming unavailable; read stream.read instead."))
		return
	}
	notifier, _ := s.service.(changeNotifier)
	changes := func() <-chan struct{} {
		if notifier == nil {
			return nil
		}
		return notifier.Changes()
	}
	// The position: Last-Event-ID (a reconnect), else the cursor, else now.
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("cursor")
	}
	changed := changes()
	first, err := read(r.Context(), cursor)
	if err != nil {
		s.bearerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(text string) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprint(w, text); err != nil {
			return false
		}
		f.Flush()
		return true
	}
	// sent is the last position the client was given. A position that
	// moved past messages this stream does not carry (every other write on
	// the board) is sent with the keepalive, not on every write.
	sent := ""
	// emit writes one page; more reports a page that was full.
	emit := func(res board.Result) (ok, more bool) {
		positions, _ := res.Data["positions"].([]string)
		var b []byte
		for i, m := range res.Messages {
			raw, _ := json.Marshal(m)
			if i < len(positions) {
				b = fmt.Appendf(b, "id: %s\n", positions[i])
				sent = positions[i]
			}
			b = fmt.Appendf(b, "event: message\ndata: %s\n\n", raw)
		}
		if res.NextCursor != "" {
			cursor = res.NextCursor
		}
		more, _ = res.Data["has_more"].(bool)
		return len(b) == 0 || write(string(b)), more
	}
	ready, _ := json.Marshal(map[string]any{"cursor": first.NextCursor, "keepalive_seconds": int(liveKeepalive.Seconds())})
	if !write(fmt.Sprintf("retry: %d\nid: %s\nevent: ready\ndata: %s\n\n", liveRetryMillis, first.NextCursor, ready)) {
		return
	}
	sent = first.NextCursor
	ok, more := emit(first)
	if !ok {
		return
	}
	// end says why the stream ends: an error's code, or "expired" at its
	// time limit.
	end := func(err error) {
		code := "expired"
		var be *board.Error
		if errors.As(err, &be) {
			code = be.Code
		} else if err != nil {
			code = "internal"
		}
		raw, _ := json.Marshal(map[string]string{"code": code, "cursor": cursor})
		write(fmt.Sprintf("event: end\ndata: %s\n\n", raw))
	}
	// A store without the signal (a test double) is read every few seconds.
	var poll <-chan time.Time
	if notifier == nil {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		poll = ticker.C
	}
	keepalive := time.NewTicker(liveKeepalive)
	defer keepalive.Stop()
	stop := time.NewTimer(time.Until(deadline))
	defer stop.Stop()
	last := time.Now()
	for {
		if !more {
			select {
			case <-r.Context().Done():
				return
			case <-stop.C:
				end(nil)
				return
			case <-keepalive.C:
				text := ": keepalive\n\n"
				if cursor != sent {
					raw, _ := json.Marshal(map[string]string{"cursor": cursor})
					text, sent = fmt.Sprintf("id: %s\nevent: cursor\ndata: %s\n\n", cursor, raw), cursor
				}
				if !write(text) {
					return
				}
				continue
			case <-changed:
			case <-poll:
			}
		}
		if gap := liveReadGap - time.Since(last); gap > 0 {
			time.Sleep(gap)
		}
		changed = changes()
		last = time.Now()
		res, err := read(r.Context(), cursor)
		if err != nil {
			if r.Context().Err() == nil {
				end(err)
			}
			return
		}
		if ok, more = emit(res); !ok {
			return
		}
	}
}

// liveCredential is the stream's reader, the key its slot counts under and
// when it ends.
func (s *Server) liveCredential(r *http.Request) (liveReader, string, time.Time, error) {
	end := time.Now().Add(liveStreamMax)
	if auth := r.Header.Get("Authorization"); auth != "" {
		token := bearerToken(auth)
		o := s.oauthStore()
		if o == nil {
			return nil, "", end, board.HostedOff()
		}
		if r.URL.RawQuery != "" && r.URL.Query().Encode() != "" {
			for key := range r.URL.Query() {
				if key != "cursor" {
					return nil, "", end, bad("With an app's token the stream takes cursor only (or Last-Event-ID).")
				}
			}
		}
		if !board.LooksLikeJWT(token) {
			return nil, "", end, &board.Error{Status: 401, Code: "invalid_token", Message: "The live stream takes an app's token (a JWT from Sign in with SwarmMemo) or a signed stream.read command."}
		}
		_, _, claims, err := o.BearerSigner(r.Context(), token, time.Now().Unix())
		if err != nil {
			return nil, "", end, err
		}
		if !s.hostedLimiter.Admit("oauth:" + claims.Session) {
			return nil, "", end, errSignInRate()
		}
		read := func(ctx context.Context, cursor string) (board.Result, error) {
			ctx = board.WithStreamCursor(board.WithBearerToken(board.WithVia(ctx, "command"), token), cursor)
			now := time.Now().Unix()
			key, publicKey, _, err := o.BearerSigner(ctx, token, now)
			if err != nil {
				return board.Result{}, err
			}
			return s.service.Execute(ctx, signAs(s.cfg.ServiceID, key, publicKey, board.Command{Operation: "stream.read"}, now, now), s.peer(r))
		}
		return read, "oauth:" + claims.Session, end, nil
	}
	q := r.URL.Query()
	for key, values := range q {
		if len(values) != 1 {
			return nil, "", end, bad("Duplicate stream query field.")
		}
		switch key {
		case "public_key", "signature", "timestamp", "nonce", "cursor", "limit":
		default:
			return nil, "", end, bad("A signed stream takes public_key, signature, timestamp, nonce, cursor and limit only: a signed stream.read command.")
		}
	}
	c, err := queryCommand(q)
	if err != nil {
		return nil, "", end, err
	}
	c.Operation = "stream.read"
	if signatureEnd := time.Unix(c.Timestamp+board.SignatureWindowSeconds, 0); signatureEnd.Before(end) {
		end = signatureEnd
	}
	read := func(ctx context.Context, cursor string) (board.Result, error) {
		return s.service.Execute(board.WithStreamCursor(ctx, cursor), c, s.peer(r))
	}
	return read, c.PublicKey, end, nil
}
