package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

const mcpStdioUsage = "usage: swarmmemo mcp-stdio [--profile core|assistant|full] (DATA_DIR must be set; serves MCP over stdin and stdout)"

// mcpStdio serves an MCP profile over stdin and stdout against the store in
// DATA_DIR (created when empty), with no listener: the same server and tools
// /mcp/core, /mcp/assistant or /mcp serve over HTTP, for clients and
// directories that run the server as a local process. Stdout carries only
// the protocol; logs go to stderr.
func mcpStdio(args []string) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runMCPStdio(ctx, args, &mcp.StdioTransport{})
}

func runMCPStdio(ctx context.Context, args []string, transport mcp.Transport) error {
	flags := flag.NewFlagSet("mcp-stdio", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "core", "core, assistant or full")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !slices.Contains(httpapi.MCPProfiles, *profile) {
		return errors.New(mcpStdioUsage)
	}
	dir := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if dir == "" {
		return errors.New("mcp-stdio needs DATA_DIR, the directory for its SQLite store (created when missing)")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	storeConfig, publicURL, err := storeConfigFromEnvironment()
	if err != nil {
		return err
	}
	store, err := board.Open(filepath.Join(dir, "swarmmemo.db"), storeConfig)
	if err != nil {
		return err
	}
	defer store.Close()
	api := httpapi.New(store, nil, httpapi.Config{PublicURL: publicURL, ServiceID: storeConfig.ServiceID, ArchiveDelaySeconds: storeConfig.ArchiveDelaySeconds, Version: version, Features: storeConfig.Features})
	slog.Info("SwarmMemo MCP over stdio", "profile", *profile, "version", version)
	return api.RunMCP(ctx, *profile, transport)
}
