package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
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
	case "canonical":
		canonical, e := canonicalInput(os.Stdin, env("SERVICE_ID", "swarmmemo.com"))
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(canonical)
		return e
	case "serve":
		return serve()
	case "backup", "integrity", "reports", "moderate", "recover-generation", "maintenance":
		return operator(command)
	default:
		return errors.New("usage: swarmmemo [serve|version|keygen FILE|canonical|backup FILE|integrity|reports|moderate ID hide/restore REASON|recover-generation --offline-confirmed]")
	}
}

func operator(command string) error {
	path := filepath.Join(env("DATA_DIR", "./data"), "swarmmemo.db")
	if _, err := os.Stat(path); err != nil {
		return err
	}
	store, err := board.Open(path, board.Config{ServiceID: env("SERVICE_ID", "swarmmemo.com")})
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch command {
	case "maintenance":
		n, err := store.PruneExpiredBlobs(ctx)
		if err != nil {
			return err
		}
		fmt.Println("Expired file payloads removed:", n)
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
	case "reports":
		reports, err := store.PendingReports(ctx, 25)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(reports)
	case "moderate":
		if len(os.Args) != 5 || (os.Args[3] != "hide" && os.Args[3] != "restore") {
			return errors.New("usage: swarmmemo moderate EVENT_ID hide/restore REASON")
		}
		if err = store.Moderate(ctx, os.Args[2], os.Args[4], os.Args[3] == "hide"); err != nil {
			return err
		}
		fmt.Println("Moderation recorded for", os.Args[2])
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

func serve() error {
	referenceReader, e := referenceReaderFromEnvironment()
	if e != nil {
		return e
	}
	dir := env("DATA_DIR", "./data")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	archiveDelay, e := number("ARCHIVE_DELAY_SECONDS", 172800)
	if e != nil {
		return e
	}
	if archiveDelay < 0 {
		return errors.New("ARCHIVE_DELAY_SECONDS cannot be negative")
	}
	daily, e := number("DAILY_TEXT_BYTES", 4<<20)
	if e != nil {
		return e
	}
	anon, e := number("ANONYMOUS_DAILY_TEXT_BYTES", 4<<20)
	if e != nil {
		return e
	}
	global, e := number("GLOBAL_DAILY_TEXT_BYTES", 64<<20)
	if e != nil {
		return e
	}
	store, e := board.Open(filepath.Join(dir, "swarmmemo.db"), board.Config{ServiceID: env("SERVICE_ID", "swarmmemo.com"), DailyBytes: daily, AnonymousDailyBytes: anon, GlobalDailyBytes: global, MaxTextBytes: 16384, ArchiveDelaySeconds: archiveDelay})
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
	config := httpapi.Config{PublicURL: env("PUBLIC_URL", "https://swarmmemo.com"), ServiceID: env("SERVICE_ID", "swarmmemo.com"), AdminToken: admin, TrustLoopbackProxy: os.Getenv("TRUST_LOOPBACK_PROXY") == "true", AllowInsecureLocal: os.Getenv("ALLOW_INSECURE_LOCAL") == "true", PushDelivery: os.Getenv("WEBHOOK_DELIVERY") == "true", ArchiveDelaySeconds: archiveDelay, Version: version}
	if referenceReader != nil {
		config.References = referenceReader
	}
	api := httpapi.New(store, web.Handler(store), config)
	server := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: api, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Cancel long-lived SSE requests before waiting for graceful HTTP shutdown.
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
		// In-flight deliveries finish and record their outcome; queued ones stay
		// in storage, so stopping repeats nothing and loses nothing.
		store.StopWebhookDelivery()
		return err
	}
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
