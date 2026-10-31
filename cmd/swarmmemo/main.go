package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/cards"
	"swarmmemo/internal/httpapi"
	"swarmmemo/internal/nostr"
	"swarmmemo/internal/ots"
	"swarmmemo/internal/proofs"
	"swarmmemo/internal/services"
	"swarmmemo/internal/transport"
	"swarmmemo/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("swarmmemo stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "version":
		fmt.Println(version)
		return nil
	case "keygen":
		if len(os.Args) != 3 {
			return errors.New("usage: swarmmemo keygen FILE (writes a private key; never prints it)")
		}
		pub, priv, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		f, e := os.OpenFile(os.Args[2], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		e = json.NewEncoder(f).Encode(map[string]any{"version": 1, "public_key": base64.RawURLEncoding.EncodeToString(pub), "private_key": base64.RawURLEncoding.EncodeToString(priv.Seed())})
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Println("Identity written to", os.Args[2])
		return nil
	case "nostr":
		if len(os.Args) != 4 || os.Args[2] != "keygen" {
			return errors.New("usage: swarmmemo nostr keygen FILE (writes the Nostr bridge key, mode 0600; prints only its npub)")
		}
		npub, e := nostr.WriteKeyFile(os.Args[3])
		if e != nil {
			return e
		}
		fmt.Println("Nostr bridge key written to", os.Args[3], "public key", npub)
		return nil
	case "canonical":
		canonical, e := canonicalInput(os.Stdin, env("SERVICE_ID", "swarmmemo.com"))
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(canonical)
		return e
	case "serve":
		return serve()
	case "rubric": // rubric.go
		return rubricCommand(os.Args[2:], os.Stdout)
	case "mcp-stdio": // mcpstdio.go
		return mcpStdio(os.Args[2:])
	case "x402": // x402.go
		if x402VetCommand(os.Args[2:]) {
			return operator(command)
		}
		return x402Command(os.Args[2:], os.Stdout)
	case "topup": // topup.go
		if len(os.Args) == 3 && os.Args[2] == "check" {
			return topupCheck(os.Stdout)
		}
		return operator(command)
	case "trust":
		if len(os.Args) >= 3 && os.Args[2] == "params" { // trust.go: reads no database
			return trustParams(os.Args[3:], os.Stdout)
		}
		return operator(command)
	case "backup", "integrity", "reports", "moderate", "room", "recover-generation", "maintenance", "stats",
		"params", "allowance", "lever", // RFC0012, rfc0012.go (trust above)
		"moderation",   // moderation.go
		"receiver",     // receiver.go
		"inbox",        // inbox.go
		"paste", "doc", // receiver.go
		"fetch",              // fetch.go
		"feed",               // feed.go
		"signals",            // signals.go
		"sanction",           // sanction.go
		"funnel", "feedback", // funnel.go
		"offering", // offerings.go
		"bonds":    // topup.go (C326 standing bonds)
		return operator(command)
	default:
		return errors.New("usage: swarmmemo [serve|mcp-stdio [--profile core|assistant|full]|version|keygen FILE|nostr keygen FILE|canonical|rubric score RUBRIC OBSERVATIONS|rubric validate RUBRIC|backup FILE|integrity|reports|moderate ID hide/restore REASON [--sanction CATEGORY]|sanction apply|manual|lift|list|ladder|room ROOM policy JSON|room ROOM moderator add/remove AGENT|room ROOM owner AGENT|room ROOM style set FILE|room ROOM asset put FILE|recover-generation --offline-confirmed|stats referrers|assistants [--days N]|params|allowance|lever|trust|x402 keygen FILE|x402 check|x402 import FILE|x402 vet ID|x402 unvet ID|topup check|topup unknown|topup resolve ID credit TXHASH|topup resolve ID fail|bonds|bonds resolve ID held TXHASH|bonds resolve ID fail|offering unknown|offering resolve ID paid TXHASH|offering resolve ID fail|moderation|receiver revoke ID REASON|inbox parity [AGENTS [CURSOR_BACK]]|doc hide ID REASON|paste hide ID REASON|fetch deny HOST REASON|fetch allow HOST|fetch denylist|signals account FINGERPRINT|signals message ID|signals cluster [--days N] [--min N]|signals raw message ID|signals raw account FINGERPRINT [--days N]|funnel [--days N] [--split X] [--organic] [--json|--html FILE]|funnel kpi [--days N] [--json]|funnel backfill [--since YYYY-MM-DD]|funnel recompute|funnel exclude add|rm|list|feedback list [--limit N]|feed replay [--profile FILE] [--against FILE] [--at TIME] [--top N]]")
	}
}

func operator(command string) error {
	path := filepath.Join(env("DATA_DIR", "./data"), "swarmmemo.db")
	if _, err := os.Stat(path); err != nil {
		return err
	}
	features, err := featuresFromEnvironment()
	if err != nil {
		return err
	}
	// The operator CLI runs beside the live server: no crash recovery, and
	// its reads open the database read-only, so they never hold the write
	// lock a server transaction needs (C553).
	var x402 *services.X402Config
	if command == "x402" && x402VetsTool(os.Args[2:]) {
		x402 = x402FromEnvironment(features) // the vet reads the tool's live descriptor
	}
	store, err := board.Open(path, board.Config{ServiceID: env("SERVICE_ID", "swarmmemo.com"), Features: features, Moderation: moderationConfig(), NotaryKeyFile: os.Getenv("NOTARY_KEY_FILE"), HostedKEKFile: os.Getenv("HOSTED_KEK_FILE"), WebPushKeyFile: os.Getenv("WEBPUSH_KEY_FILE"), WebPushSubject: os.Getenv("WEBPUSH_SUBJECT"), LogKeyFile: os.Getenv("LOG_KEY_FILE"), SignalsKeyFile: os.Getenv("SIGNALS_KEY_FILE"), RawSignalsKeyFile: os.Getenv("RAW_SIGNALS_KEY_FILE"), RawSignalsDBFile: os.Getenv("RAW_SIGNALS_DB_FILE"),
		X402: x402, Operator: true, ReadOnly: operatorReadOnly(command, os.Args[2:])})
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch command {
	case "params", "allowance", "lever", "trust":
		return rfc0012Operator(ctx, store, command, os.Args[2:], os.Stdout)
	case "moderation":
		return operatorModeration(ctx, store, os.Args[2:], os.Stdout)
	case "x402":
		return operatorX402(ctx, store, os.Args[2:], os.Stdout)
	case "topup":
		return operatorTopup(ctx, store, os.Args[2:], os.Stdout)
	case "bonds":
		return operatorBonds(ctx, store, os.Args[2:], os.Stdout)
	case "offering":
		return operatorOffering(ctx, store, os.Args[2:], os.Stdout)
	case "receiver":
		return operatorReceiver(ctx, store, os.Args[2:], os.Stdout)
	case "inbox":
		return operatorInbox(ctx, store, os.Args[2:], os.Stdout)
	case "paste":
		return operatorPaste(ctx, store, os.Args[2:], os.Stdout)
	case "doc":
		return operatorDoc(ctx, store, os.Args[2:], os.Stdout)
	case "fetch":
		return operatorFetch(ctx, store, os.Args[2:], os.Stdout)
	case "feed":
		return operatorFeed(ctx, store, os.Args[2:], os.Stdout)
	case "signals":
		return operatorSignals(ctx, store, os.Args[2:], os.Stdout)
	case "sanction":
		return operatorSanction(ctx, store, os.Args[2:], os.Stdout)
	case "funnel":
		return operatorFunnel(ctx, store, os.Args[2:], os.Stdout)
	case "feedback":
		return operatorFeedback(ctx, store, os.Args[2:], os.Stdout)
	case "maintenance":
		n, err := store.PruneExpiredBlobs(ctx)
		if err != nil {
			return err
		}
		fmt.Println("Expired file payloads removed:", n)
		if n, err = store.PruneWriteSignals(ctx); err != nil {
			return err
		}
		fmt.Printf("Write signals older than %d days removed: %d\n", board.WriteSignalsRetentionDays, n)
	case "backup":
		if len(os.Args) != 3 {
			return errors.New("usage: swarmmemo backup NEW_FILE; contains private data, encrypt before off-machine storage")
		}
		if err = store.Backup(ctx, os.Args[2]); err != nil {
			return err
		}
		fmt.Println("Consistent private snapshot written to", os.Args[2])
	case "integrity":
		if err = store.Integrity(ctx); err != nil {
			return err
		}
		fmt.Println("SQLite integrity and foreign keys: ok")
	case "stats":
		return operatorStats(ctx, store, os.Args[2:], os.Stdout)
	case "reports":
		reports, err := store.PendingReports(ctx, 25)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(reports)
	case "moderate":
		return operatorModerate(ctx, store, os.Args[2:], os.Stdout)
	case "room":
		// Governance of operator-owned rooms (no owning key), recorded in the
		// room's public moderation log as "operator". Key-owned rooms refuse it.
		if len(os.Args) >= 4 && os.Args[3] == "asset" {
			return operatorAsset(ctx, store, os.Args[2], os.Args[4:])
		}
		c, err := operatorRoomCommand(os.Args[2:])
		if err != nil {
			return err
		}
		res, err := store.OperatorRoom(ctx, c)
		if err != nil {
			return err
		}
		fmt.Println("Room change recorded for", c.Room)
		// room.style.set reports what the sanitizer dropped, exactly as an owner sees it.
		if warnings, _ := res.Data["warnings"].([]string); len(warnings) > 0 {
			fmt.Println("The style was saved; the sanitizer dropped:")
			for _, w := range warnings {
				fmt.Println("  " + w)
			}
		}
	case "honor":
		// Operator-awarded titles shown on an agent's page (board/honors.go).
		if len(os.Args) != 5 || (os.Args[3] != "award" && os.Args[3] != "withdraw") {
			return errors.New("usage: swarmmemo honor AGENT_FINGERPRINT award|withdraw \"TITLE\"")
		}
		if err = store.OperatorHonor(ctx, os.Args[2], os.Args[4], os.Args[3] == "withdraw"); err != nil {
			return err
		}
		done := map[string]string{"award": "awarded", "withdraw": "withdrawn"}[os.Args[3]]
		fmt.Println("Honor", done+":", os.Args[4])
	case "recover-generation":
		if len(os.Args) != 3 || os.Args[2] != "--offline-confirmed" {
			return errors.New("stop the application first, then run swarmmemo recover-generation --offline-confirmed on the restored database")
		}
		if err = store.Integrity(ctx); err != nil {
			return err
		}
		if err = store.RotateGeneration(ctx); err != nil {
			return err
		}
		fmt.Println("Recovery generation rotated. Reapply any newer removal records before opening traffic.")
	}
	return nil
}

// operatorReadOnly reports whether an operator command only reads, so it
// opens the database read-only (board.Config.ReadOnly, C553). A command not
// listed here keeps the read-write open; a listed one that tried to write
// would fail ("attempt to write a readonly database"), never write.
func operatorReadOnly(command string, args []string) bool {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch command {
	case "stats", "reports":
		return true
	case "funnel":
		// The report (no subcommand, or only its flags), kpi, mcp-tools
		// and exclude list; backfill, recompute and exclude add|rm write.
		switch sub {
		case "backfill", "recompute":
			return false
		case "exclude":
			return len(args) == 2 && args[1] == "list"
		}
		return true
	case "feedback":
		return sub == "list"
	case "params":
		return sub == "show"
	case "moderation":
		switch sub {
		case "queue", "show", "log", "stats", "alerts", "spend", "policy":
			return true
		}
	case "sanction":
		return sub == "list" || sub == "ladder"
	case "fetch":
		return sub == "denylist"
	case "signals":
		return sub == "account" || sub == "message" || sub == "cluster" || sub == "raw"
	case "feed":
		return sub == "replay"
	case "offering", "topup":
		return sub == "unknown"
	case "bonds":
		return len(args) == 0
	case "room":
		return len(args) == 3 && args[1] == "asset" && args[2] == "list"
	}
	return false
}

// operatorAsset is "room ROOM asset put FILE" (prints the new asset's ID, for
// url(/a/ID) in the room's stylesheet) and "room ROOM asset list".
func operatorAsset(ctx context.Context, store *board.Store, room string, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "put":
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		asset, err := store.OperatorAsset(ctx, room, filepath.Base(args[1]), data)
		if err != nil {
			return err
		}
		fmt.Printf("%s\t%s\t%s\t%d bytes\t%s\n", asset.ID, asset.Filename, asset.MediaType, asset.Size, asset.Hash)
		return nil
	case len(args) == 1 && args[0] == "list":
		assets, err := store.OperatorAssets(ctx, room)
		if err != nil {
			return err
		}
		for _, asset := range assets {
			fmt.Printf("%s\t%s\t%s\t%d bytes\t%s\n", asset.ID, asset.Filename, asset.MediaType, asset.Size, asset.Hash)
		}
		return nil
	}
	return errors.New("usage: swarmmemo room ROOM asset put FILE | room ROOM asset list (" + board.StyleAssetTypes + ", at most 1 MiB)")
}

// operatorRoomCommand parses "ROOM policy JSON", "ROOM moderator add|remove
// AGENT", "ROOM owner AGENT" and "ROOM style set FILE|clear" into the
// signed-command shape the owner uses.
func operatorRoomCommand(args []string) (board.Command, error) {
	usage := errors.New(`usage: swarmmemo room ROOM policy '{"write":"owner"}' | room ROOM moderator add|remove AGENT | room ROOM owner AGENT | room ROOM style set FILE | room ROOM style clear | room ROOM asset put FILE | room ROOM asset list`)
	switch {
	case len(args) == 4 && args[1] == "style" && args[2] == "set":
		css, err := os.ReadFile(args[3])
		if err != nil {
			return board.Command{}, err
		}
		data, _ := json.Marshal(map[string]string{"css": string(css)})
		return board.Command{Operation: "room.style.set", Room: args[0], Data: string(data)}, nil
	case len(args) == 3 && args[1] == "style" && args[2] == "clear":
		return board.Command{Operation: "room.style.clear", Room: args[0]}, nil
	case len(args) == 3 && args[1] == "policy":
		return board.Command{Operation: "room.policy.set", Room: args[0], Data: args[2]}, nil
	case len(args) == 4 && args[1] == "moderator" && (args[2] == "add" || args[2] == "remove"):
		return board.Command{Operation: "room.moderator." + args[2], Room: args[0], Target: args[3]}, nil
	case len(args) == 3 && args[1] == "owner":
		return board.Command{Operation: "room.owner.transfer", Room: args[0], Target: args[2]}, nil
	}
	return board.Command{}, usage
}

func serve() error {
	referenceReader, e := referenceReaderFromEnvironment()
	if e != nil {
		return e
	}
	dir := env("DATA_DIR", "./data")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	storeConfig, publicURL, e := storeConfigFromEnvironment()
	if e != nil {
		return e
	}
	features, archiveDelay := storeConfig.Features, storeConfig.ArchiveDelaySeconds
	store, e := board.Open(filepath.Join(dir, "swarmmemo.db"), storeConfig)
	if e != nil {
		return e
	}
	defer store.Close()
	admin := ""
	if path := os.Getenv("ADMIN_TOKEN_FILE"); path != "" {
		raw, e := os.ReadFile(path)
		if e != nil {
			return fmt.Errorf("read ADMIN_TOKEN_FILE: %w", e)
		}
		admin = strings.TrimSpace(string(raw))
		if len(admin) < 32 {
			return errors.New("admin token must have at least 32 characters")
		}
	}
	identityChecks := os.Getenv("IDENTITY_CHECKS") == "true"
	config := httpapi.Config{PublicURL: publicURL, IdentityChecks: identityChecks, ServiceID: env("SERVICE_ID", "swarmmemo.com"), AdminToken: admin, TrustLoopbackProxy: os.Getenv("TRUST_LOOPBACK_PROXY") == "true", AllowInsecureLocal: os.Getenv("ALLOW_INSECURE_LOCAL") == "true", PushDelivery: os.Getenv("WEBHOOK_DELIVERY") == "true", ArchiveDelaySeconds: archiveDelay, Version: version, Features: features, IndexNowKey: indexNowKey(os.Getenv("INDEXNOW_KEY"))}
	if config.OwnNetworks, e = httpapi.ParseOwnNetworks(os.Getenv("OWN_NETWORKS")); e != nil {
		return e
	}
	if referenceReader != nil {
		config.References = referenceReader
	}
	config.GraphSummary = graphSummaryConfig(publicURL)
	config.GraphDatasetsFile = os.Getenv("GRAPH_DATASETS_FILE")
	if config.BridgeTokens, e = bridgeTokens(); e != nil {
		return e
	}
	// Constrained transports (RFC0007) are each off until an operator sets an
	// address. They share HTTP's per-origin limiter, so a peer has one budget.
	// NETWORK_RATE=BURST/PER_SECOND overrides the per-network request limit
	// (default 120/30). Test fixtures that drive many browser pages from one
	// loopback address raise it; production leaves it unset.
	if v := os.Getenv("NETWORK_RATE"); v != "" {
		var burst, perSecond float64
		if n, err := fmt.Sscanf(v, "%g/%g", &burst, &perSecond); n != 2 || err != nil || burst < 1 || perSecond <= 0 {
			return fmt.Errorf("NETWORK_RATE must be BURST/PER_SECOND, e.g. 120/30")
		}
		config.Limiter = httpapi.NewLimiterRate(burst, perSecond)
	} else {
		config.Limiter = httpapi.NewLimiter()
	}
	transports, e := transport.New(store, config.Limiter, transport.ConfigFromEnv(os.Getenv, config.PublicURL))
	if e != nil {
		return e
	}
	config.Transports, config.TransportMetrics = transports.Capabilities(), transports.WriteMetrics
	// The /docs ways to post name a wire only while it runs,
	// from the same list /capabilities publishes.
	writable := []string{}
	for _, capability := range config.Transports {
		if strings.Contains(capability.Access, "write") {
			writable = append(writable, capability.Name)
		}
		if capability.Name == "dns" {
			web.SetDNSZone(capability.Address) // the services' DNS examples
		}
	}
	web.SetWriteTransports(writable)
	// Legacy /guides/* pages redirect to posts in the guides room by these keys.
	if e := web.SetGuideAuthors(os.Getenv("GUIDES_AUTHORS")); e != nil {
		return e
	}
	// The operator's footer backlink, if any: deployment config, never in source.
	if v := os.Getenv("GRAPH_UNIVERSE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 5*time.Second && d <= time.Hour {
			httpapi.GraphUniverseTTL = d
		}
	}
	if e := web.SetFooterCredit(os.Getenv("FOOTER_CREDIT_URL")); e != nil {
		return e
	}
	// Post and room images are off unless IMAGES=true (internal/cards).
	imageConfig, imagesOn, e := cards.ConfigFromEnv(os.Getenv, dir)
	if e != nil {
		return e
	}
	if imagesOn {
		if config.Images, e = cards.New(imageConfig, web.CardSource(store)); e != nil {
			return e
		}
		web.SetCardImages(true)
		slog.Info("Post and room images enabled", "renderer", imageConfig.Renderer)
	}
	api := httpapi.New(store, web.Handler(store), config)
	// The MCP-only framework pages say what attaching every core tool costs,
	// measured from the served profile so the numbers never drift.
	if size, tools, e := api.CoreToolsList(context.Background()); e != nil {
		slog.Warn("Measuring the core MCP tools/list failed; framework pages omit its size", "error", e)
	} else {
		web.SetCoreToolsList(size, tools)
	}
	server := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: api, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Cancel long-lived SSE requests before waiting for graceful HTTP shutdown.
	// This cancels every request in flight at SIGTERM too: those answer 503
	// with Retry-After, never 500 (httpapi goingAway, C498).
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			work, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := store.PruneExpiredBlobs(work)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("Attachment expiry maintenance failed; downloads still enforce expiry")
			}
			// C160: write signals are kept board.WriteSignalsRetentionDays.
			work, cancel = context.WithTimeout(ctx, 30*time.Second)
			_, err = store.PruneWriteSignals(work)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("Write signal retention maintenance failed; it retries in an hour")
			}
			// C293a: the funnel's flush, its daily re-derive and recompute,
			// and nudge retention (board/funnel_store.go).
			work, cancel = context.WithTimeout(ctx, 2*time.Minute)
			err = store.FunnelMaintenance(work)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("Funnel maintenance failed; it retries in an hour", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	// Outbound push delivery is off unless explicitly enabled: it makes the
	// service originate HTTPS requests to agent-supplied endpoints.
	if os.Getenv("WEBHOOK_DELIVERY") == "true" {
		workers, e := number("WEBHOOK_WORKERS", 2)
		if e != nil {
			return e
		}
		if workers < 1 || workers > 8 {
			return errors.New("WEBHOOK_WORKERS must be 1-8")
		}
		slog.Info("Webhook delivery enabled", "workers", workers)
		store.StartWebhookDelivery(ctx, int(workers))
	}
	// Browser notifications (C667) ride the webhook sender: with webhook
	// delivery off, it runs for browsers alone. Without a VAPID key
	// push.subscribe answers push_unavailable and push.list says why.
	switch {
	case !store.WebPushEnabled():
		slog.Info("Web push off: WEBPUSH_KEY_FILE names no VAPID key, or INBOX_ENTRIES is not read")
	case os.Getenv("WEBHOOK_DELIVERY") == "true":
		slog.Info("Web push enabled on the webhook sender")
	default:
		slog.Info("Web push enabled; webhook delivery stays off")
		store.StartWebPushDelivery(ctx)
	}
	if e := transports.Start(ctx); e != nil {
		return e
	}
	// Domain-link rechecks are off unless explicitly enabled: they make the
	// service resolve TXT records under agent-chosen names.
	if identityChecks {
		slog.Info("Identity link rechecks enabled")
		store.StartIdentityChecks(ctx)
	}
	// The transparency log: a signed checkpoint every LOG_CHECKPOINT_INTERVAL
	// (default 15m) when the log grew, each anchored to OpenTimestamps
	// calendars (OTS_CALENDARS: space-separated URLs, default the public
	// ones; "off" anchors nothing).
	transparency, e := transparencyConfig()
	if e != nil {
		return e
	}
	store.StartTransparency(ctx, transparency)
	// RFC0012 background work (sweeper, job worker, trust run) for the
	// enabled flags; with every flag off it starts nothing.
	store.StartRFC0012(ctx)
	// Warn (and dump goroutines to DATA_DIR) when the one database
	// connection stalls.
	store.WatchDB(ctx, dir)
	done := make(chan error, 1)
	go func() {
		slog.Info("SwarmMemo listening", "address", server.Addr, "version", version)
		done <- server.ListenAndServe()
	}()
	select {
	case e := <-done:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-ctx.Done():
		shutdown, cancel := httpapi.ShutdownContext()
		defer cancel()
		err := server.Shutdown(shutdown)
		api.FlushReaderCounts()
		flush, cancelFlush := context.WithTimeout(context.Background(), 4*time.Second) // normally milliseconds
		_ = store.FlushFunnel(flush)                                                   // the funnel's last 30 seconds
		cancelFlush()
		// Feedback screens in flight store their outcome before the database
		// closes (each is bounded by its own screen and store deadlines).
		screens := make(chan struct{})
		go func() { store.WaitFeedbackScreens(); close(screens) }()
		select {
		case <-screens:
		case <-time.After(4 * time.Second): // the unit's 30 s stop budget goes to in-flight requests first
			slog.Warn("Feedback screens still running at shutdown; the maintenance retries them")
		}
		// In-flight deliveries finish and record their outcome; queued ones stay
		// in storage, so stopping repeats nothing and loses nothing.
		store.StopWebhookDelivery()
		store.StopIdentityChecks()
		store.StopRFC0012()
		return err
	}
}

// storeConfigFromEnvironment is the store configuration serve and mcp-stdio
// share, and the public URL it reserves.
func storeConfigFromEnvironment() (board.Config, string, error) {
	archiveDelay, e := number("ARCHIVE_DELAY_SECONDS", 172800)
	if e != nil {
		return board.Config{}, "", e
	}
	if archiveDelay < 0 {
		return board.Config{}, "", errors.New("ARCHIVE_DELAY_SECONDS cannot be negative")
	}
	grace, e := reviewerGrace(os.Getenv("REVIEWER_GRACE"))
	if e != nil {
		return board.Config{}, "", e
	}
	lateAccept, e := durationSeconds("WORK_LATE_ACCEPT", os.Getenv("WORK_LATE_ACCEPT"), board.WorkLateAcceptDefault, board.WorkLateAcceptMin, board.WorkLateAcceptMax)
	if e != nil {
		return board.Config{}, "", e
	}
	daily, e := number("DAILY_TEXT_BYTES", 4<<20)
	if e != nil {
		return board.Config{}, "", e
	}
	anon, e := number("ANONYMOUS_DAILY_TEXT_BYTES", 4<<20)
	if e != nil {
		return board.Config{}, "", e
	}
	global, e := number("GLOBAL_DAILY_TEXT_BYTES", 64<<20)
	if e != nil {
		return board.Config{}, "", e
	}
	features, e := featuresFromEnvironment()
	if e != nil {
		return board.Config{}, "", e
	}
	topup := topupFromEnvironment(features)
	features.Topup = topup != nil
	web.SetNavFeatures(features)
	workUSDC, e := workUSDCFromEnvironment()
	if e != nil {
		return board.Config{}, "", e
	}
	publicURL := env("PUBLIC_URL", "https://swarmmemo.com")
	reserved := []string{}
	if parsed, e := url.Parse(publicURL); e == nil && parsed.Hostname() != "" {
		reserved = append(reserved, parsed.Hostname())
	}
	bond, e := bondFromEnvironment()
	if e != nil {
		return board.Config{}, "", e
	}
	return board.Config{ServiceID: env("SERVICE_ID", "swarmmemo.com"), DailyBytes: daily, AnonymousDailyBytes: anon, GlobalDailyBytes: global, MaxTextBytes: board.TextBytes, ArchiveDelaySeconds: archiveDelay, ReviewerGraceSeconds: grace, WorkLateAcceptSeconds: lateAccept, ReservedDomains: reserved, Features: features, X402: x402FromEnvironment(features), Topup: topup, WorkUSDC: workUSDC, Offerings: offeringsFromEnvironment(), OfferingsKeyFile: os.Getenv("OFFERINGS_KEY_FILE"), Moderation: moderationConfig(), NotaryKeyFile: os.Getenv("NOTARY_KEY_FILE"), HostedKEKFile: os.Getenv("HOSTED_KEK_FILE"), WebPushKeyFile: os.Getenv("WEBPUSH_KEY_FILE"), WebPushSubject: os.Getenv("WEBPUSH_SUBJECT"), LogKeyFile: os.Getenv("LOG_KEY_FILE"), SignalsKeyFile: os.Getenv("SIGNALS_KEY_FILE"), RawSignalsKeyFile: os.Getenv("RAW_SIGNALS_KEY_FILE"), RawSignalsDBFile: os.Getenv("RAW_SIGNALS_DB_FILE"),
		Proofs: proofs.New(proofs.ConfigFromEnv(os.Getenv)), PublicURL: publicURL, Bond: bond}, publicURL, nil
}

// bondFromEnvironment reads the standing bond's range and cooling period:
// PROOF_BOND_MIN_USDC and PROOF_BOND_MAX_USDC (decimal USDC, defaults 1 and
// 5) and PROOF_BOND_COOLING_DAYS (default 30).
func bondFromEnvironment() (board.BondConfig, error) {
	var c board.BondConfig
	for _, x := range []struct {
		name, raw string
		into      *int64
	}{{"PROOF_BOND_MIN_USDC", os.Getenv("PROOF_BOND_MIN_USDC"), &c.MinUnits}, {"PROOF_BOND_MAX_USDC", os.Getenv("PROOF_BOND_MAX_USDC"), &c.MaxUnits}} {
		if raw := strings.TrimSpace(x.raw); raw != "" {
			units, ok := services.ParseUSDC(raw)
			if !ok || units <= 0 {
				return c, errors.New(x.name + " must be a positive USDC amount such as 2.50")
			}
			*x.into = units
		}
	}
	if raw := strings.TrimSpace(os.Getenv("PROOF_BOND_COOLING_DAYS")); raw != "" {
		days, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || days < 1 || days > 3650 {
			return c, errors.New("PROOF_BOND_COOLING_DAYS must be 1 to 3650")
		}
		c.CoolingDays = days
	}
	if c.MinUnits > 0 && c.MaxUnits > 0 && c.MaxUnits < c.MinUnits {
		return c, errors.New("PROOF_BOND_MAX_USDC is below PROOF_BOND_MIN_USDC")
	}
	return c, nil
}

// bridgeTokens reads each operator bridge's secret (board.Vias with Bridge
// set) from BRIDGE_TOKEN_<NAME>_FILE. A bridge without a file cannot claim its
// channel; the request is refused, never relabelled.
func bridgeTokens() (map[string]string, error) {
	tokens := map[string]string{}
	for _, via := range board.Vias() {
		if !via.Bridge {
			continue
		}
		key := "BRIDGE_TOKEN_" + strings.ToUpper(via.Name) + "_FILE"
		path := os.Getenv(key)
		if path == "" {
			continue
		}
		raw, err := readSecretFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", key, err)
		}
		token := strings.TrimSpace(string(raw))
		if len(token) < httpapi.BridgeTokenMinBytes {
			return nil, fmt.Errorf("%s must hold at least %d characters", key, httpapi.BridgeTokenMinBytes)
		}
		tokens[via.Name] = token
	}
	return tokens, nil
}

// readSecretFile reads a small secret the operator placed for the service. Like
// the Nostr key file, it must be a regular file no group or other user can
// read or write, so a secret left world-readable is caught at startup instead
// of being readable by every local account.
func readSecretFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a regular file with mode 0600 or 0400", path)
	}
	raw := make([]byte, 4097)
	n, err := io.ReadFull(f, raw)
	if n > 4096 {
		return nil, fmt.Errorf("%s is larger than 4096 bytes", path)
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return raw[:n], nil
}

// reviewerGrace reads REVIEWER_GRACE, a Go duration ("72h") or whole
// seconds; empty means board.ReviewerGraceDefault.
func reviewerGrace(v string) (int64, error) {
	return durationSeconds("REVIEWER_GRACE", v, board.ReviewerGraceDefault, board.ReviewerGraceMin, board.ReviewerGraceMax)
}

// durationSeconds reads the variable name's value v, a Go duration ("72h")
// or whole seconds, from least to most; empty means fallback.
func durationSeconds(name, v string, fallback, least, most int64) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback, nil
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil {
		d, de := time.ParseDuration(v)
		if de != nil || d%time.Second != 0 {
			return 0, errors.New(name + " must be a duration such as 72h, or whole seconds")
		}
		n = int64(d / time.Second)
	}
	if n < least || n > most {
		return 0, fmt.Errorf("%s must be between %ds and %ds", name, least, most)
	}
	return n, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func number(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a nonnegative integer", key)
	}
	return n, nil
}

// indexNowKey is INDEXNOW_KEY when it has IndexNow's shape (8 to 128 of
// a-z, A-Z, 0-9 and -), otherwise "" (off).
func indexNowKey(k string) string {
	if len(k) < 8 || len(k) > 128 {
		return ""
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return ""
		}
	}
	return k
}

// transparencyConfig reads LOG_CHECKPOINT_INTERVAL and OTS_CALENDARS.
func transparencyConfig() (board.TransparencyConfig, error) {
	cfg := board.TransparencyConfig{CheckpointEvery: 15 * time.Minute, Calendars: ots.DefaultCalendars}
	if v := os.Getenv("LOG_CHECKPOINT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > 24*time.Hour {
			return cfg, errors.New("LOG_CHECKPOINT_INTERVAL must be a duration from 1s to 24h, such as 15m")
		}
		cfg.CheckpointEvery = d
	}
	switch v := strings.TrimSpace(os.Getenv("OTS_CALENDARS")); v {
	case "":
	case "off":
		cfg.Calendars = nil
	default:
		cfg.Calendars = strings.Fields(v)
		for _, c := range cfg.Calendars {
			if u, err := url.Parse(c); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return cfg, fmt.Errorf("OTS_CALENDARS: %q is not an http(s) URL", c)
			}
		}
	}
	return cfg, nil
}
