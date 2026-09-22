// Package transport serves the board over constrained wires (RFC0007): DNS TXT,
// a raw TCP line protocol, Gemini, Gopher and finger.
//
// Each wire is a thin Adapter that only frames bytes into a Request and renders
// a result back onto its wire. Everything else is shared here and applied to
// every adapter the same way: the frame and response byte caps, read and write
// deadlines, the per-origin request bucket (the same httpapi.Limiter HTTP uses),
// a connection cap, panic recovery, counters, the transport command policy, and
// one call into board.Service with the real peer address as the anonymous
// allowance key. Adapters never verify or trust anything: signed commands are
// decoded by httpapi.DecodeCommand and verified by the board, exactly as on
// /c64/, and plain text comes from httpapi.WriteText, the renderer curl reads.
//
// Every listener is off unless an operator configures its address.
package transport

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

// Wire is what every transport declares: its name, its reduced limits and its
// /capabilities entry.
type Wire interface {
	Name() string
	Limits() Limits
	Capability(host string) httpapi.TransportCapability
}

// Adapter is a one-request wire. It frames nothing itself: the middleware
// hands it one already-bounded frame and writes whatever it renders, capped.
type Adapter interface {
	Wire
	// Parse turns one frame into a Request. It builds commands; it never runs them.
	Parse(frame []byte) (Request, error)
	// Render writes the result, or err, in the wire's own format within
	// req.Budget bytes. A nil return sends nothing.
	Render(req Request, res board.Result, err error) []byte
}

// sourceParser is an Adapter whose parse keeps per-source state (DNS write
// reassembly). The source is only ever a fairness key, never an identity.
type sourceParser interface {
	ParseFrom(source string, frame []byte) (Request, error)
}

// Dialogue is a conversational wire (SMTP). The middleware still owns the
// socket, deadlines, admission, caps and panic recovery; the adapter gets an
// Exchange whose reads are bounded and whose Submit is the same single path
// into the board that every Adapter's Request takes.
type Dialogue interface {
	Wire
	Converse(x *Exchange)
}

// Exchange is one bounded conversation.
type Exchange struct {
	conn   net.Conn
	r      *bufio.Reader
	max    int // bytes per line
	budget int // bytes left to read this session
	stats  *counters
	end    time.Time // the conversation's total deadline
	submit func(Request) (board.Result, error)
}

// ReadLine reads one line within the per-line cap and the session budget,
// renewing the idle deadline.
func (x *Exchange) ReadLine() ([]byte, error) {
	// An idle deadline per line, never past the whole conversation's end.
	idle := time.Now().Add(frameTimeout)
	if !x.end.IsZero() && x.end.Before(idle) {
		idle = x.end
	}
	_ = x.conn.SetReadDeadline(idle)
	max := x.max
	if x.budget < max {
		max = x.budget
	}
	line, err := readLine(x.r, max)
	if errors.Is(err, errTooLarge) {
		x.stats.rejected.Add(1)
	}
	x.budget -= len(line) + 2
	return line, err
}

// Write sends text; errors surface on the next read.
func (x *Exchange) Write(s string) { _, _ = io.WriteString(x.conn, s) }

// Submit runs a request through the shared policy and the board.
func (x *Exchange) Submit(req Request) (board.Result, error) { return x.submit(req) }

// Request is what an adapter framed off its wire.
type Request struct {
	// Command is the one board call this request needs; nil renders a static
	// response (help, a menu, an input prompt) without touching the board.
	Command *board.Command
	// Fallback runs only when Command answers not_found (finger: a room name
	// first, then an agent handle).
	Fallback *board.Command
	Route    string // adapter-private rendering hint
	Arg      string
	State    any // adapter-private parse state (the DNS question to echo)
	// SignedOnly refuses the command unless it carries a key and signature,
	// for wires whose origin cannot key an anonymous allowance (DNS write,
	// mail). The board still does all verification.
	SignedOnly bool
	// Budget is the response size this frame may produce, set by the
	// middleware: the adapter's response limit, or twice the query size for a
	// spoofable UDP datagram.
	Budget int
}

// Limits are an adapter's reduced byte limits, advertised in /capabilities.
type Limits struct {
	Request  int // largest accepted frame
	Response int // largest response on a connection-oriented wire
}

// Framing is how a listener cuts one request off its wire.
type Framing int

const (
	// Line reads one line ending in LF or CRLF (or EOF).
	Line Framing = iota
	// LengthPrefixed reads one DNS-over-TCP message: a two-byte length, then it.
	LengthPrefixed
	// Datagram is one UDP packet.
	Datagram
	// Conversation hands the connection to a Dialogue adapter.
	Conversation
)

const (
	frameTimeout  = 10 * time.Second // to deliver the request, TLS handshake included
	totalTimeout  = 30 * time.Second
	commandTimout = 10 * time.Second
	maxConns      = 64 // concurrent connections per listener
	maxPeerConns  = 4  // concurrent connections per peer, across all listeners
	udpWorkers    = 8
	dialogueBytes = 64 << 10 // everything one conversation may send
)

var (
	errRate     = &board.Error{Status: 429, Code: "request_rate", Message: "Too many requests from this address. Wait briefly before retrying.", RetryAfter: 2}
	errTooLarge = &board.Error{Status: 413, Code: "request_too_large", Message: "The request exceeds this transport's size limit; see /capabilities."}
	errFrame    = &board.Error{Status: 400, Code: "invalid_request", Message: "Malformed request."}
	errInternal = &board.Error{Status: 500, Code: "internal", Message: "Request failed."}
)

func bad(msg string) *board.Error {
	return &board.Error{Status: 400, Code: "invalid_request", Message: msg}
}

// Config enables listeners. An empty address leaves that transport off.
type Config struct {
	Host          string // public host for links and /capabilities
	DNSAddr       string // UDP and TCP
	DNSZone       string
	DNSNameServer string
	TCPAddr       string
	GeminiAddr    string
	GeminiCert    string
	GeminiKey     string
	GopherAddr    string
	FingerAddr    string
	// DNSWrite also accepts signed commands tunnelled through queries
	// (RFC0007 rung C). Off unless set, and never anonymous.
	DNSWrite bool
	// SMTPAddr receives mail for ROOM@SMTPDomain. SMTPAnonymous also turns
	// plain-text bodies into anonymous posts; without it only signed
	// commands are accepted.
	SMTPAddr      string
	SMTPDomain    string
	SMTPAnonymous bool
}

// ConfigFromEnv reads SWARMMEMO_TRANSPORT_* variables. Nothing is enabled by
// default: deploying the binary changes nothing until an address is set.
func ConfigFromEnv(getenv func(string) string, publicURL string) Config {
	host := "swarmmemo.com"
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	value := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv("SWARMMEMO_TRANSPORT_" + key)); v != "" {
			return v
		}
		return fallback
	}
	host = value("HOST", host)
	return Config{
		Host:          host,
		DNSAddr:       value("DNS_ADDR", ""),
		DNSZone:       value("DNS_ZONE", "q."+host),
		DNSNameServer: value("DNS_NS", host),
		TCPAddr:       value("TCP_ADDR", ""),
		GeminiAddr:    value("GEMINI_ADDR", ""),
		GeminiCert:    value("GEMINI_CERT", ""),
		GeminiKey:     value("GEMINI_KEY", ""),
		GopherAddr:    value("GOPHER_ADDR", ""),
		FingerAddr:    value("FINGER_ADDR", ""),
		DNSWrite:      value("DNS_WRITE", "") == "true",
		SMTPAddr:      value("SMTP_ADDR", ""),
		SMTPDomain:    value("SMTP_DOMAIN", "post."+host),
		SMTPAnonymous: value("SMTP_ANONYMOUS", "") == "true",
	}
}

type listener struct {
	network string // "tcp" or "udp"
	addr    string
	framing Framing
	tls     *tls.Config
	adapter Wire
}

type counters struct {
	requests, rateLimited, rejected, failed, panics atomic.Int64
}

// Core is the shared middleware and the set of enabled listeners.
type Core struct {
	service board.Service
	// limiter is shared with HTTP: a connection-oriented peer has completed a
	// handshake, so its address is real and it keeps one budget on every wire.
	limiter *httpapi.Limiter
	// datagrams is the same machinery with its own table: a UDP source can be
	// forged, and must not spend or crowd out a real client's HTTP budget.
	datagrams *httpapi.Limiter
	listeners []listener
	caps      []httpapi.TransportCapability
	names     []string
	stats     map[string]*counters
	peersMu   sync.Mutex
	peers     map[string]int // open connections per peer; entries leave at zero
	bound     []net.Addr     // actual addresses, in listener order, once started
}

// New validates the configuration and builds the enabled adapters. It opens
// no socket; Start does.
func New(service board.Service, limiter *httpapi.Limiter, cfg Config) (*Core, error) {
	if limiter == nil {
		limiter = httpapi.NewLimiter()
	}
	c := &Core{service: service, limiter: limiter, datagrams: httpapi.NewLimiter(), stats: map[string]*counters{}, peers: map[string]int{}}
	host := strings.ToLower(cfg.Host)
	add := func(a Wire, ls ...listener) {
		for i := range ls {
			ls[i].adapter = a
			c.listeners = append(c.listeners, ls[i])
		}
		capability := a.Capability(host)
		if capability.Address == "" {
			capability.Address = net.JoinHostPort(host, port(ls[0].addr))
		}
		c.caps = append(c.caps, capability)
		c.names = append(c.names, a.Name())
		c.stats[a.Name()] = &counters{}
	}
	if cfg.DNSAddr != "" {
		d, err := newDNS(cfg.DNSZone, cfg.DNSNameServer, host)
		if err != nil {
			return nil, err
		}
		if cfg.DNSWrite {
			d.writes = newReassembly()
		}
		add(d, listener{network: "udp", addr: cfg.DNSAddr, framing: Datagram}, listener{network: "tcp", addr: cfg.DNSAddr, framing: LengthPrefixed})
	}
	if cfg.TCPAddr != "" {
		add(lineProtocol{host: host, port: port(cfg.TCPAddr)}, listener{network: "tcp", addr: cfg.TCPAddr, framing: Line})
	}
	if cfg.GeminiAddr != "" {
		if cfg.GeminiCert == "" || cfg.GeminiKey == "" {
			return nil, errors.New("SWARMMEMO_TRANSPORT_GEMINI_ADDR needs SWARMMEMO_TRANSPORT_GEMINI_CERT and SWARMMEMO_TRANSPORT_GEMINI_KEY")
		}
		cert, err := tls.LoadX509KeyPair(cfg.GeminiCert, cfg.GeminiKey)
		if err != nil {
			return nil, fmt.Errorf("gemini certificate: %w", err)
		}
		tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		add(gemini{host: host, port: port(cfg.GeminiAddr)}, listener{network: "tcp", addr: cfg.GeminiAddr, framing: Line, tls: tlsConfig})
	}
	if cfg.GopherAddr != "" {
		add(gopher{host: host, port: port(cfg.GopherAddr)}, listener{network: "tcp", addr: cfg.GopherAddr, framing: Line})
	}
	if cfg.FingerAddr != "" {
		add(finger{host: host}, listener{network: "tcp", addr: cfg.FingerAddr, framing: Line})
	}
	if cfg.SMTPAddr != "" {
		domain := strings.TrimSuffix(strings.ToLower(cfg.SMTPDomain), ".")
		if _, err := encodeName(domain); err != nil {
			return nil, fmt.Errorf("SWARMMEMO_TRANSPORT_SMTP_DOMAIN %q is not a DNS name", domain)
		}
		add(&smtp{host: host, domain: domain, anonymous: cfg.SMTPAnonymous}, listener{network: "tcp", addr: cfg.SMTPAddr, framing: Conversation})
	}
	return c, nil
}

func port(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return p
}

// Capabilities is exactly the enabled transports, for /capabilities.
func (c *Core) Capabilities() []httpapi.TransportCapability {
	return append([]httpapi.TransportCapability(nil), c.caps...)
}

// WriteMetrics appends per-transport counters to the loopback /metrics page.
func (c *Core) WriteMetrics(w io.Writer) {
	for _, name := range c.names {
		s := c.stats[name]
		fmt.Fprintf(w, "swarmmemo_transport_requests_total{transport=%q} %d\nswarmmemo_transport_rate_limited_total{transport=%q} %d\nswarmmemo_transport_rejected_total{transport=%q} %d\nswarmmemo_transport_errors_total{transport=%q} %d\nswarmmemo_transport_panics_total{transport=%q} %d\n",
			name, s.requests.Load(), name, s.rateLimited.Load(), name, s.rejected.Load(), name, s.failed.Load(), name, s.panics.Load())
	}
}

// Start binds every enabled listener before returning, so a bad address or a
// missing capability fails startup instead of silently serving nothing, then
// serves until ctx ends.
func (c *Core) Start(ctx context.Context) error {
	var opened []io.Closer
	fail := func(err error) error {
		for _, o := range opened {
			o.Close()
		}
		return err
	}
	var serve []func()
	for _, l := range c.listeners {
		l := l
		if l.network == "udp" {
			pc, err := net.ListenPacket("udp", l.addr)
			if err != nil {
				return fail(fmt.Errorf("%s listener %s: %w", l.adapter.Name(), l.addr, err))
			}
			opened, c.bound = append(opened, pc), append(c.bound, pc.LocalAddr())
			serve = append(serve, func() { c.servePackets(ctx, pc, l) })
			continue
		}
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			return fail(fmt.Errorf("%s listener %s: %w", l.adapter.Name(), l.addr, err))
		}
		if l.tls != nil {
			ln = tls.NewListener(ln, l.tls)
		}
		opened, c.bound = append(opened, ln), append(c.bound, ln.Addr())
		serve = append(serve, func() { c.serveStream(ctx, ln, l) })
	}
	for i, run := range serve {
		slog.Info("Constrained transport listening", "transport", c.listeners[i].adapter.Name(), "network", c.listeners[i].network, "address", c.bound[i].String())
		go run()
	}
	go func() {
		<-ctx.Done()
		for _, o := range opened {
			o.Close()
		}
	}()
	return nil
}

func (c *Core) serveStream(ctx context.Context, ln net.Listener, l listener) {
	slots := make(chan struct{}, maxConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				c.serveConn(ctx, conn, l)
			}()
		default:
			// Full: refuse at once rather than queue unbounded work.
			c.stats[l.adapter.Name()].rateLimited.Add(1)
			conn.Close()
		}
	}
}

// serveConn is the whole middleware for one connection: one frame in, one
// bounded response out, close.
func (c *Core) serveConn(ctx context.Context, conn net.Conn, l listener) {
	defer conn.Close()
	a := l.adapter
	stats := c.stats[a.Name()]
	stats.requests.Add(1)
	defer func() {
		if recover() != nil {
			stats.panics.Add(1)
			slog.Warn("Constrained transport request panicked", "transport", a.Name())
		}
	}()
	start := time.Now()
	_ = conn.SetDeadline(start.Add(totalTimeout))
	_ = conn.SetReadDeadline(start.Add(frameTimeout))
	peer := peerOf(conn.RemoteAddr())
	if !c.acquirePeer(peer) {
		stats.rateLimited.Add(1)
		return
	}
	defer c.releasePeer(peer)
	limits := a.Limits()
	send := func(out []byte) {
		if len(out) > limits.Response {
			out = out[:limits.Response]
		}
		if l.framing == LengthPrefixed {
			if len(out) == 0 {
				return
			}
			out = append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...)
		}
		_, _ = conn.Write(out)
	}
	if dialogue, ok := a.(Dialogue); ok {
		if !c.limiter.Admit(peer) {
			stats.rateLimited.Add(1)
			return
		}
		x := &Exchange{conn: conn, r: bufio.NewReaderSize(conn, 1024), max: limits.Request, budget: dialogueBytes, stats: stats, end: start.Add(totalTimeout),
			submit: func(req Request) (board.Result, error) {
				// Each submitted message is its own request against the shared
				// budget; admitting the connection once paid for one call only.
				if !c.limiter.Admit(peer) {
					stats.rateLimited.Add(1)
					return board.Result{}, errRate
				}
				ctx, cancel := context.WithTimeout(ctx, commandTimout)
				defer cancel()
				res, err := c.run(ctx, peer, req)
				if err != nil {
					stats.failed.Add(1)
				}
				return res, err
			}}
		dialogue.Converse(x)
		return
	}
	framed := a.(Adapter)
	if !c.limiter.Admit(peer) {
		stats.rateLimited.Add(1)
		send(framed.Render(Request{Budget: limits.Response}, board.Result{}, errRate))
		return
	}
	frame, err := readFrame(bufio.NewReaderSize(conn, 1024), l.framing, limits.Request)
	if err != nil {
		stats.rejected.Add(1)
		if errors.Is(err, errTooLarge) {
			send(framed.Render(Request{Budget: limits.Response}, board.Result{}, errTooLarge))
		}
		return
	}
	send(c.handle(ctx, framed, peer, frame, limits.Response))
}

// acquirePeer bounds one address's concurrent connections, so a single peer
// cannot hold every slot open with slow requests. The map is bounded by the
// total connection slots, because an entry is removed when it reaches zero.
func (c *Core) acquirePeer(peer string) bool {
	c.peersMu.Lock()
	defer c.peersMu.Unlock()
	if c.peers[peer] >= maxPeerConns {
		return false
	}
	c.peers[peer]++
	return true
}

func (c *Core) releasePeer(peer string) {
	c.peersMu.Lock()
	defer c.peersMu.Unlock()
	if c.peers[peer]--; c.peers[peer] <= 0 {
		delete(c.peers, peer)
	}
}

// servePackets answers UDP datagrams. A forged source address is the whole
// threat here: the rate limit is keyed on it in its own table, an over-limit
// source gets silence rather than a reflected answer, and no answer may exceed
// twice the bytes that arrived.
func (c *Core) servePackets(ctx context.Context, pc net.PacketConn, l listener) {
	var wg sync.WaitGroup
	for i := 0; i < udpWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, l.adapter.Limits().Request+1)
			for {
				n, addr, err := pc.ReadFrom(buf)
				if err != nil {
					if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
						return
					}
					continue
				}
				c.answerPacket(ctx, pc, addr, buf[:n], l.adapter.(Adapter))
			}
		}()
	}
	wg.Wait()
}

func (c *Core) answerPacket(ctx context.Context, pc net.PacketConn, addr net.Addr, frame []byte, a Adapter) {
	stats := c.stats[a.Name()]
	stats.requests.Add(1)
	defer func() {
		if recover() != nil {
			stats.panics.Add(1)
			slog.Warn("Constrained transport request panicked", "transport", a.Name())
		}
	}()
	if len(frame) > a.Limits().Request {
		stats.rejected.Add(1)
		return
	}
	if !c.datagrams.Admit(peerOf(addr)) {
		stats.rateLimited.Add(1)
		return
	}
	budget := 2 * len(frame)
	if out := c.handle(ctx, a, peerOf(addr), frame, budget); len(out) > 0 && len(out) <= budget {
		_ = pc.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = pc.WriteTo(out, addr)
	}
}

// handle parses, applies the shared command policy, runs the one board call
// and renders. It is the single path from any wire into the board.
func (c *Core) handle(ctx context.Context, a Adapter, peer string, frame []byte, budget int) []byte {
	stats := c.stats[a.Name()]
	var req Request
	var err error
	if sp, ok := a.(sourceParser); ok {
		req, err = sp.ParseFrom(peer, frame)
	} else {
		req, err = a.Parse(frame)
	}
	req.Budget = budget
	if err != nil {
		stats.rejected.Add(1)
		return a.Render(req, board.Result{}, err)
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimout)
	defer cancel()
	res, err := c.run(ctx, peer, req)
	if err != nil {
		stats.failed.Add(1)
	}
	return a.Render(req, res, err)
}

func (c *Core) run(ctx context.Context, peer string, req Request) (board.Result, error) {
	if req.Command == nil {
		return board.Result{}, nil
	}
	if req.SignedOnly && (req.Command.PublicKey == "" || req.Command.Signature == "") {
		return board.Result{}, &board.Error{Status: 401, Code: "signature_required", Message: "This transport accepts signed commands only."}
	}
	res, err := c.execute(ctx, peer, *req.Command)
	var be *board.Error
	if req.Fallback != nil && errors.As(err, &be) && be.Status == 404 {
		return c.execute(ctx, peer, *req.Fallback)
	}
	return res, err
}

// execute is the transport command policy. Constrained wires carry public
// reads and posts to public rooms only. Everything that needs HTTPS on the web
// (private rooms, identity management, delegation, private reads) stays there:
// Gemini's certificate is trust-on-first-use and the rest are plaintext.
func (c *Core) execute(ctx context.Context, peer string, cmd board.Command) (board.Result, error) {
	if err := permitted(cmd); err != nil {
		return board.Result{}, err
	}
	if cmd.Operation == "post" {
		room := cmd.Room
		if room == "" {
			room = "lobby"
		}
		// The same visibility probe HTTP makes before a plaintext signed post:
		// anonymous, so a private room is simply not found.
		res, err := c.service.Execute(ctx, board.Command{Operation: "room.get", Room: room}, peer)
		if err != nil || res.Room == nil || res.Room.Visibility != "public" {
			return board.Result{}, &board.Error{Status: 403, Code: "public_rooms_only", Message: "Constrained transports post to existing public rooms only. Use HTTPS for anything else."}
		}
	}
	return c.service.Execute(ctx, cmd, peer)
}

func permitted(cmd board.Command) error {
	if cmd.PrivateRead != nil || cmd.Delegation != nil {
		return &board.Error{Status: 400, Code: "https_required", Message: "Private reads and delegated commands use HTTPS JSON POST /v1/command."}
	}
	switch cmd.Operation {
	case "post":
		return nil
	case "messages.list", "message.get", "thread.get", "rooms.list", "room.get", "agent.get":
		if cmd.PublicKey != "" || cmd.Signature != "" {
			return &board.Error{Status: 400, Code: "https_required", Message: "Authenticated reads use HTTPS."}
		}
		return nil
	}
	return &board.Error{Status: 400, Code: "unsupported_operation", Message: "Constrained transports carry public reads and posts only. Use HTTPS /v1/command for other operations."}
}

// readFrame reads exactly one request, never more than max bytes.
func readFrame(r *bufio.Reader, framing Framing, max int) ([]byte, error) {
	switch framing {
	case LengthPrefixed:
		var size [2]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(size[:]))
		if n > max {
			return nil, errTooLarge
		}
		frame := make([]byte, n)
		if _, err := io.ReadFull(r, frame); err != nil {
			return nil, err
		}
		return frame, nil
	case Line:
		return readLine(r, max)
	}
	return nil, errFrame
}

// readLine accepts LF, CRLF, or a final line closed by EOF (a netcat that
// half-closes), and never buffers more than max bytes.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	line := make([]byte, 0, 128)
	for {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		if b == '\n' {
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			return line, nil
		}
		if len(line) >= max {
			return nil, errTooLarge
		}
		line = append(line, b)
	}
}

func peerOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); ip != nil {
		// The same textual form HTTP's peer() produces, so one origin is one
		// anonymous allowance whichever wire it posts on.
		return ip.String()
	}
	return host
}

// Text is the shared plain-text rendering for every text wire: the lines
// httpapi.WriteText gives curl, with terminal control characters neutralized
// and cut at a line boundary to fit max bytes.
func Text(res board.Result, max int) string {
	var b strings.Builder
	httpapi.WriteText(&b, res)
	return fit(clean(b.String()), max)
}

// ErrorText is the one plain-text error line.
func ErrorText(err error) string {
	be := boardError(err)
	return clean("error " + be.Code + ": " + be.Message + "\n")
}

func boardError(err error) *board.Error {
	var be *board.Error
	if errors.As(err, &be) {
		return be
	}
	return errInternal
}

const truncated = "[truncated; read the rest over HTTPS]\n"

func fit(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(truncated)
	if cut < 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl >= 0 {
		cut = nl + 1
	}
	return s[:cut] + truncated
}

// clean replaces control characters other than newline and tab, invalid
// UTF-8, and bidirectional overrides, so hostile message text cannot drive a
// terminal or reorder what a reader sees on a raw wire.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0), r == utf8.RuneError:
			return '�'
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return '�'
		}
		return r
	}, s)
}

// oneLine is text safe for a single protocol line (a status meta, a menu
// display string): no tabs or newlines at all.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, clean(s))
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

func limitArg(s string, fallback, max int) (int, error) {
	if s == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > max {
		return 0, bad(fmt.Sprintf("Count must be 1-%d.", max))
	}
	return n, nil
}
