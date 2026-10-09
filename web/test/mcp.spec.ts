// What GET /v1/me/mcp says, asked once per person: the API keys section shows
// the MCP address only where the edition offers it and the server says it
// answers at /mcp, and offers a key the send scope only where the server says
// its keys may send, whatever the edition.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { freshModules, json, reply, serve, stubPage, type Route } from './support'

async function signedIn(route: Route, options: { mcp?: boolean } = {}) {
  await freshModules()
  if (options.mcp === false) {
    const [{ configureEdition }, { openEdition }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
    configureEdition({ ...openEdition, mcp: false })
  }
  const session = await import('../src/state/session')
  const mcp = await import('../src/state/mcp')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    return route(request)
  })
  await session.adoptSession(reply())
  const asked = () => fetch.mock.calls.filter(([url]) => new URL(String(url)).pathname === '/v1/me/mcp').length
  return { session, mcp, asked }
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('the MCP address', () => {
  it('is offered once the server says it serves MCP over HTTP, asking it once', async () => {
    const { mcp, asked } = await signedIn(() => json({ http: true, keys_send: true }))
    expect(mcp.mcpOffered()).toBe(false)
    expect(mcp.keysMaySend()).toBe(false)
    await mcp.loadMcpAccess()
    await mcp.loadMcpAccess()
    expect(mcp.mcpOffered()).toBe(true)
    expect(mcp.keysMaySend()).toBe(true)
    expect(asked()).toBe(1)
  })

  it('offers keys no sending where the server says its keys do not send, or does not say', async () => {
    for (const answer of [{ http: true, keys_send: false }, { http: true }]) {
      const { mcp } = await signedIn(() => json(answer))
      await mcp.loadMcpAccess()
      expect(mcp.mcpOffered(), JSON.stringify(answer)).toBe(true)
      expect(mcp.keysMaySend(), JSON.stringify(answer)).toBe(false)
      vi.restoreAllMocks()
    }
  })

  it('is not offered where the server does not serve it, nor while it could not be asked, which is tried again', async () => {
    let answer: Response | null = new Response('<html>502</html>', { status: 502 })
    const { mcp, asked } = await signedIn(() => answer ?? json({ http: false }))
    await mcp.loadMcpAccess()
    expect(mcp.mcpOffered()).toBe(false)
    expect(mcp.mcpAccess.loaded).toBe(false)
    answer = null
    await mcp.loadMcpAccess()
    expect(mcp.mcpAccess.loaded).toBe(true)
    expect(mcp.mcpOffered()).toBe(false)
    expect(asked()).toBe(2)
  })

  it('is never shown by an edition that offers no MCP address, which still asks whether keys may send', async () => {
    const { mcp, asked } = await signedIn(() => json({ http: true, keys_send: true }), { mcp: false })
    await mcp.loadMcpAccess()
    expect(asked()).toBe(1)
    expect(mcp.mcpOffered()).toBe(false)
    expect(mcp.keysMaySend()).toBe(true)
  })

  it('is asked about again for the next person', async () => {
    const { session, mcp, asked } = await signedIn(() => json({ http: true }))
    await mcp.loadMcpAccess()
    await session.signOut()
    expect(mcp.mcpAccess).toMatchObject({ loaded: false, served: false, keysSend: false })
    await session.adoptSession(reply())
    await mcp.loadMcpAccess()
    expect(asked()).toBe(2)
  })
})
