package httpapi

// The stdio transport: `swarmmemo mcp-stdio` serves one of the MCP profiles
// over a process's stdin and stdout, for clients and directories that run a
// server locally. It serves the very servers the HTTP handlers serve (built
// in initMCP), so the tools, descriptions and instructions are the same.

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPProfiles are the profile names RunMCP takes: core is /mcp/core,
// assistant is /mcp/assistant and full is /mcp.
var MCPProfiles = []string{"core", "assistant", "full"}

// stdioPeer is the caller's address for limits: the local process, as a
// loopback HTTP client would be.
const stdioPeer = "127.0.0.1"

// mcpServerFor is the MCP server of a profile name in MCPProfiles.
func (s *Server) mcpServerFor(profile string) (*mcp.Server, error) {
	switch profile {
	case "core":
		return s.mcpCore, nil
	case "assistant":
		return s.mcpAssistant, nil
	case "full":
		return s.mcpServerNow(), nil
	}
	return nil, fmt.Errorf("unknown MCP profile %q: use core, assistant or full", profile)
}

// RunMCP serves a profile's MCP server over the transport t until the client
// disconnects or ctx ends. The caller is anonymous, as an HTTP client that
// presents no token.
func (s *Server) RunMCP(ctx context.Context, profile string, t mcp.Transport) error {
	server, err := s.mcpServerFor(profile)
	if err != nil {
		return err
	}
	return server.Run(context.WithValue(ctx, peerContextKey{}, stdioPeer), t)
}
