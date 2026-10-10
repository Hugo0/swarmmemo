package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

// stdioSession runs mcp-stdio in process against a fresh DATA_DIR and
// returns a connected client session.
func stdioSession(t *testing.T, args ...string) *mcp.ClientSession {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	serverT, clientT := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- runMCPStdio(ctx, args, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v (server: %v)", err, <-done)
	}
	t.Cleanup(func() {
		session.Close()
		cancel()
		<-done
	})
	return session
}

func toolNames(tools []*mcp.Tool) []string {
	names := []string{}
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// httpToolNames lists a profile's tools over the Streamable HTTP handler,
// with the same environment mcp-stdio reads.
func httpToolNames(t *testing.T, path string) []string {
	t.Helper()
	storeConfig, publicURL, err := storeConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	store, err := board.Open(filepath.Join(t.TempDir(), "swarmmemo.db"), storeConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := httpapi.New(store, nil, httpapi.Config{PublicURL: publicURL, ServiceID: storeConfig.ServiceID, ArchiveDelaySeconds: storeConfig.ArchiveDelaySeconds, Version: version, Features: storeConfig.Features})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	var out struct {
		Result struct {
			Tools []*mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Result.Tools) == 0 {
		t.Fatalf("tools/list over HTTP %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return toolNames(out.Result.Tools)
}

func TestMCPStdioServesTheHTTPProfiles(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{nil, "/mcp/core"},
		{[]string{"--profile", "assistant"}, "/mcp/assistant"},
		{[]string{"--profile", "full"}, "/mcp"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			session := stdioSession(t, tc.args...)
			init := session.InitializeResult()
			if init == nil || init.ServerInfo.Name != "swarmmemo" || init.Instructions == "" {
				t.Fatalf("initialize: %+v", init)
			}
			list, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, want := toolNames(list.Tools), httpToolNames(t, tc.path)
			if !slices.Equal(got, want) {
				t.Fatalf("stdio tools differ from %s:\nstdio %v\nhttp  %v", tc.path, got, want)
			}
		})
	}
}

func TestMCPStdioReadToolOnFreshStore(t *testing.T) {
	session := stdioSession(t)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_rooms", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(res.StructuredContent)
	if res.IsError || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("list_rooms: error=%v %s", res.IsError, body)
	}
}

func TestMCPStdioRefusesBadInvocations(t *testing.T) {
	serverT, _ := mcp.NewInMemoryTransports()
	t.Setenv("DATA_DIR", "")
	if err := runMCPStdio(context.Background(), nil, serverT); err == nil || !strings.Contains(err.Error(), "DATA_DIR") {
		t.Fatalf("no DATA_DIR: %v", err)
	}
	t.Setenv("DATA_DIR", t.TempDir())
	for _, args := range [][]string{{"--profile", "nope"}, {"extra"}, {"--port", "1"}} {
		if err := runMCPStdio(context.Background(), args, serverT); err == nil || err.Error() != mcpStdioUsage {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
