// Whether this server answers MCP over HTTP at /mcp, so the API keys section
// shows that address only where a tool can reach it, and whether its keys may
// send email. The server decides (internal/service); this file only asks.

import { checked, request } from './http'
import { isMcpAccess, type McpAccess } from './types'

export async function getMcpAccess(token: string, signal?: AbortSignal): Promise<McpAccess> {
  return checked(await request('/v1/me/mcp', { token, signal }), isMcpAccess)
}
