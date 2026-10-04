// Whether the API keys section shows the MCP address: only where the edition
// offers it and the server says it answers at /mcp (GET /v1/me/mcp), asked
// once per person, and never by an edition that offers no MCP address.
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
  await session.signIn('ana@example.test', 'correct-password')
  const asked = () => fetch.mock.calls.filter(([url]) => new URL(String(url)).pathname === '/v1/me/mcp').length
  return { session, mcp, asked }
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('the MCP address', () => {
  it('is offered once the server says it serves MCP over HTTP, asking it once', async () => {
    const { mcp, asked } = await signedIn(() => json({ http: true }))
    expect(mcp.mcpOffered()).toBe(false)
    await mcp.loadMcpAccess()
    await mcp.loadMcpAccess()
    expect(mcp.mcpOffered()).toBe(true)
    expect(asked()).toBe(1)
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

  it('is never asked about by an edition that offers no MCP address', async () => {
    const { mcp, asked } = await signedIn(() => json({ http: true }), { mcp: false })
    await mcp.loadMcpAccess()
    expect(asked()).toBe(0)
    expect(mcp.mcpOffered()).toBe(false)
  })

  it('is asked about again for the next person', async () => {
    const { session, mcp, asked } = await signedIn(() => json({ http: true }))
    await mcp.loadMcpAccess()
    await session.signOut()
    expect(mcp.mcpAccess).toMatchObject({ loaded: false, served: false })
    await session.signIn('ana@example.test', 'correct-password')
    await mcp.loadMcpAccess()
    expect(asked()).toBe(2)
  })
})
