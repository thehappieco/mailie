import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ana, failure, json, now, reply, serve, settle, stubPage } from './support'

// Each test gets fresh modules: the session is module state, and a leftover
// token from one test must not be the reason another passes.
async function load() {
  vi.resetModules()
  const session = await import('../src/state/session')
  const vault = await import('../src/state/sessionVault')
  // The same graph's ApiError: instanceof across reset modules would never match.
  const { ApiError } = await import('../src/api/http')
  return { ...session, vault, ApiError }
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

const me = (token = 'tok_first_0000000000000000000000000000000000') => ({ user: ana, session: { id: 'ses_0000000000000001', created_at: now(), expires_at: now() + 86_400, token } })

describe('restoring a remembered session', () => {
  it('opens the console only after the server confirms the stored token', async () => {
    const s = await load()
    await s.vault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_stored', expiresAt: now() + 3600, userID: ana.id })
    const fetch = serve(({ path, token }) => path === '/v1/auth/me' && token === 'tok_stored' ? json({ user: ana, session: me().session }) : failure('unauthorized', 401))
    await s.restore()
    expect(s.session.phase).toBe('ready')
    expect(s.session.user?.email).toBe(ana.email)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(JSON.stringify(s.session)).not.toContain('tok_stored')
  })

  it('forgets a stored session the server refuses', async () => {
    const s = await load()
    await s.vault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_revoked', expiresAt: now() + 3600, userID: ana.id })
    serve(() => failure('unauthorized', 401))
    await s.restore()
    expect(s.session.phase).toBe('signed-out')
    expect(s.session.notice).toBe('')
    expect(await s.vault.loadLocalSession()).toBeNull()
  })

  it('keeps a stored session when the server cannot be reached, and offers a retry', async () => {
    const s = await load()
    await s.vault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_offline', expiresAt: now() + 3600, userID: ana.id })
    vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('Failed to fetch'))
    await s.restore()
    expect(s.session.phase).toBe('restoring')
    expect(s.session.restoreFailed).toBe(true)
    expect((await s.vault.loadLocalSession())?.token).toBe('tok_offline')
  })

  it('treats an expired record as no session without asking the server', async () => {
    const s = await load()
    await s.vault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_old', expiresAt: now() + 60, userID: ana.id })
    const fetch = vi.spyOn(globalThis, 'fetch')
    vi.spyOn(Date, 'now').mockReturnValue((now() + 3600) * 1000)
    await s.restore()
    expect(s.session.phase).toBe('signed-out')
    expect(fetch).not.toHaveBeenCalled()
  })

  it('refuses a stored token the server says belongs to someone else', async () => {
    const s = await load()
    await s.vault.saveLocalSession({ id: crypto.randomUUID(), token: 'tok_swapped', expiresAt: now() + 3600, userID: 'usr_somebody_else0' })
    serve(() => json({ user: ana, session: me().session }))
    await s.restore()
    expect(s.session.phase).toBe('signed-out')
    expect(await s.vault.loadLocalSession()).toBeNull()
  })
})

describe('a session in use', () => {
  it('ends once, with a notice, when the server refuses the token mid-use, and tells the other tabs', async () => {
    const s = await load()
    serve(({ path }) => path === '/v1/auth/login' ? json(reply()) : failure('unauthorized', 401))
    await s.signIn('ana@example.test', 'correct-password')
    expect(s.session.phase).toBe('ready')
    const cleared: string[] = []
    s.vault.observeLocalSession(change => cleared.push(change.id))
    const call = vi.fn(async () => { throw new s.ApiError('unauthorized', 401) })
    await expect(s.authorized(call)).rejects.toMatchObject({ code: 'unauthorized' })
    await expect(s.authorized(call)).rejects.toMatchObject({ code: 'unauthorized' })
    await settle()
    expect(s.session.phase).toBe('signed-out')
    expect(s.session.notice).toBe('expired')
    expect(call).toHaveBeenCalledTimes(1)
    // The stored login is cleared in the background; other tabs hear of it once that lands.
    await vi.waitFor(() => expect(cleared).toHaveLength(1))
    expect(await s.vault.loadLocalSession()).toBeNull()
  })

  it('does not end a newer session because an older token was refused', async () => {
    const s = await load()
    serve(({ path }) => path === '/v1/auth/password' ? json(reply('tok_second_000000000000000000000000000000000')) : json(reply()))
    await s.signIn('ana@example.test', 'correct-password')
    let refuse!: () => void
    const late = s.authorized(() => new Promise<never>((_, reject) => { refuse = () => reject(new s.ApiError('unauthorized', 401)) }))
    await s.changePassword('correct-password', 'another-password')
    refuse()
    await expect(late).rejects.toMatchObject({ code: 'unauthorized' })
    expect(s.session.phase).toBe('ready')
    const tokens: string[] = []
    await s.authorized(async token => { tokens.push(token) })
    expect(tokens).toEqual(['tok_second_000000000000000000000000000000000'])
    expect((await s.vault.loadLocalSession())?.token).toBe('tok_second_000000000000000000000000000000000')
  })

  it('leaves this browser at once on sign-out, even when the server cannot be told', async () => {
    const s = await load()
    serve(({ path }) => path === '/v1/auth/login' ? json(reply()) : Promise.reject(new TypeError('offline')))
    await s.signIn('ana@example.test', 'correct-password')
    await s.signOut()
    expect(s.session.phase).toBe('signed-out')
    expect(s.session.user).toBeNull()
    expect(await s.vault.loadLocalSession()).toBeNull()
    await expect(s.authorized(async () => 'never')).rejects.toMatchObject({ code: 'unauthorized' })
  })

  it('stays signed in when signing out everywhere could not reach the server', async () => {
    const s = await load()
    serve(({ path }) => path === '/v1/auth/login' ? json(reply()) : Promise.reject(new TypeError('offline')))
    await s.signIn('ana@example.test', 'correct-password')
    await expect(s.signOut({ everywhere: true })).rejects.toMatchObject({ code: 'unavailable' })
    expect(s.session.phase).toBe('ready')
  })

  it('asks the server to end every session when signing out everywhere', async () => {
    const s = await load()
    const fetch = serve(({ path }) => path === '/v1/auth/login' ? json(reply()) : new Response(null, { status: 204 }))
    await s.signIn('ana@example.test', 'correct-password')
    await s.signOut({ everywhere: true })
    const logout = fetch.mock.calls.find(([url]) => String(url).endsWith('/v1/auth/logout'))!
    expect(JSON.parse(String(logout[1]!.body))).toEqual({ everywhere: true })
    expect(s.session.phase).toBe('signed-out')
  })

  it('signs this tab out when another tab clears its login', async () => {
    const s = await load()
    serve(() => json(reply()))
    await s.signIn('ana@example.test', 'correct-password')
    const stored = await s.vault.loadLocalSession()
    await s.vault.clearLocalSession(stored!.id)
    expect(s.session.phase).toBe('signed-out')
  })

  it('shows the invitation form without restoring, whatever the browser remembers', async () => {
    const s = await load()
    s.showSignIn()
    expect(s.session.phase).toBe('signed-out')
  })
})
