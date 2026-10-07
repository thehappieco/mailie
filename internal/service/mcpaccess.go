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
	// KeysSend is whether this server's API keys may send email
	// (MAIL_KEYS_MAY_SEND): a console offers the send scope, and the send
	// flag on a key's mailboxes, only then.
	KeysSend bool `json:"keys_send"`
}

// MCPAccess says how keys reach the MCP server, so a console never shows an
// address the server does not answer at, and whether they may send. It is the
// same for every caller.
func (s *Service) MCPAccess(_ context.Context, p Principal) (MCPAccess, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return MCPAccess{}, err
	}
	return MCPAccess{HTTP: s.mcpHTTP, KeysSend: !s.keysMayNotSend}, nil
}
