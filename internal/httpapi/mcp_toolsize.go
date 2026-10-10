package httpapi

// The size of the core profile's tools/list (C134b): what a client takes
// into its context when it attaches every tool. The framework pages state it
// (web.SetCoreToolsList), measured from the very server /mcp/core serves, so
// the number follows the tools instead of drifting.

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CoreToolsList measures /mcp/core's tools/list: the bytes of its JSON-RPC
// answer and how many tools it lists. It lists over an in-memory session, as
// an anonymous client would.
func (s *Server) CoreToolsList(ctx context.Context) (size, tools int, err error) {
	ctx, cancel := context.WithCancel(context.WithValue(ctx, peerContextKey{}, stdioPeer))
	defer cancel()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	served, err := s.mcpCore.Connect(ctx, serverSide, nil)
	if err != nil {
		return 0, 0, err
	}
	defer served.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "tools-list-size", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		return 0, 0, err
	}
	defer session.Close()
	all := &mcp.ListToolsResult{Tools: []*mcp.Tool{}}
	params := &mcp.ListToolsParams{}
	for {
		page, err := session.ListTools(ctx, params)
		if err != nil {
			return 0, 0, err
		}
		all.Tools = append(all.Tools, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		params.Cursor = page.NextCursor
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": all})
	if err != nil {
		return 0, 0, err
	}
	return len(raw), len(all.Tools), nil
}
