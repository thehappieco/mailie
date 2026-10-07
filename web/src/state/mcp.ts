// What GET /v1/me/mcp says: whether this server answers MCP over HTTP at
// /mcp, for an edition that shows how to connect a tool there
// (edition().mcp), and whether its API keys may send email
// (MAIL_KEYS_MAY_SEND), which decides whether a key may be given the send
// scope or Send on a mailbox. Until the server has said so, and if it could
// not be asked, nothing about /mcp is shown (an address that answers 404 is
// worse than none) and sending is not offered to a key. It resets when the
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
  /** Its API keys may send email. */
  keysSend: boolean
}

const fresh = (): McpState => ({ loaded: false, loading: false, served: false, keysSend: false })

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

/** Whether a key may be offered the send scope and Send on a mailbox: the server said its keys may send. */
export function keysMaySend(): boolean {
  return mcpAccess.loaded && mcpAccess.keysSend
}

/**
 * Asks the server, once per person: every edition does, for whether keys may
 * send, and one that offers no MCP address shows none whatever the answer.
 */
export async function loadMcpAccess(): Promise<void> {
  if (mcpAccess.loading || mcpAccess.loaded) return
  const at = generation
  mcpAccess.loading = true
  try {
    const access = await authorized(token => getMcpAccess(token))
    if (at !== generation) return
    mcpAccess.served = access.http
    mcpAccess.keysSend = access.keys_send === true
    mcpAccess.loaded = true
  } catch {
    // Not shown, and asked again the next time the section is opened or refreshed.
  } finally {
    if (at === generation) mcpAccess.loading = false
  }
}
