package service

import (
	"context"

	"github.com/thehappieco/mailie/internal/auth"
)

// MCPAccess is how a tool holding a key reaches this server's MCP server, as
// GET /v1/me/mcp reports it.
type MCPAccess struct {
	// HTTP is whether this server answers MCP over Streamable HTTP at /mcp,
	// beside the REST API (MAIL_MCP_HTTP). Off, /mcp answers the API's 404,
	// and only a client that starts the daemon itself (serve --mcp-stdio,
	// with MAIL_MCP_KEY) speaks MCP to it.
	HTTP bool `json:"http"`
}

// MCPAccess says how keys reach the MCP server, so a console never shows an
// address the server does not answer at. It is the same for every caller.
func (s *Service) MCPAccess(_ context.Context, p Principal) (MCPAccess, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return MCPAccess{}, err
	}
	return MCPAccess{HTTP: s.mcpHTTP}, nil
}
