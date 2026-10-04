// Whether this server answers MCP over HTTP at /mcp (GET /v1/me/mcp), for an
// edition that shows how to connect a tool there (edition().mcp). Until the
// server has said so, and if it could not be asked, nothing about /mcp is
// shown: an address that answers 404 is worse than none. It resets when the
// person changes, like the other stores.

import { reactive, watch } from 'vue'
import { getMcpAccess } from '../api/mcp'
import { edition } from '../edition'
import { authorized, identity } from './session'

interface McpState {
  /** The server answered. */
  loaded: boolean
  loading: boolean
  /** It serves MCP over HTTP at this origin + /mcp. */
  served: boolean
}

const fresh = (): McpState => ({ loaded: false, loading: false, served: false })

export const mcpAccess = reactive<McpState>(fresh())

let generation = 0
watch(identity, () => {
  generation++
  Object.assign(mcpAccess, fresh())
}, { flush: 'sync' })

/** Whether to show the MCP address and the Claude Code command: the edition offers them, and the server serves /mcp. */
export function mcpOffered(): boolean {
  return edition().mcp && mcpAccess.served
}

/** Asks the server, once it is worth asking: an edition that offers no MCP address never does. */
export async function loadMcpAccess(): Promise<void> {
  if (!edition().mcp || mcpAccess.loading || mcpAccess.loaded) return
  const at = generation
  mcpAccess.loading = true
  try {
    const access = await authorized(token => getMcpAccess(token))
    if (at !== generation) return
    mcpAccess.served = access.http
    mcpAccess.loaded = true
  } catch {
    // Not shown, and asked again the next time the section is opened or refreshed.
  } finally {
    if (at === generation) mcpAccess.loading = false
  }
}
