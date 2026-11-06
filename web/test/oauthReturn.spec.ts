import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { fromBase64URL, toBase64URL } from '@thehappieco/kit/bytes'
import { importPrivateKey, type PrivateKey } from '@thehappieco/kit/hpke'
import { checkGrantShape, openGrant } from '@thehappieco/kit/profiles/mailie'
import type { SessionReply, User } from '../src/api/types'
import type { Edition } from '../src/edition'
import { FLOW_LIFETIME_MS, captureOAuthReturn, oauthBack, parseOAuthReturn, peekOAuthStart, pendingOAuthReturn, rememberOAuthStart, sweepOAuthStorage, takeOAuthReturn, takeOAuthStart } from '../src/state/oauthReturn'
import { REASON_DECLINED, REASON_DUPLICATE, REASON_MAILBOX_REFUSED, REASON_NOT_GRANTED } from '../src/ui/reasons'
import { account, ana, failure, freshModules, json, now, ORIGIN, reply, serve, stubPage } from './support'

// A return is also held in module memory until taken; take it so no test sees another's.
afterEach(() => { takeOAuthReturn(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

const STATE = 'Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA'
const refused = () => ({ getItem() { throw new Error('denied') }, setItem() { throw new Error('denied') }, removeItem() { throw new Error('denied') } })

describe('what the provider sends back', () => {
  it('accepts a code or an error with exactly one state, from the query only', () => {
    expect(parseOAuthReturn(`${ORIGIN}/oauth/return?code=4%2F0Ab_x-y&state=${STATE}&scope=mail`)?.url)
      .toBe(`${ORIGIN}/oauth/return?code=4%2F0Ab_x-y&state=${STATE}&scope=mail`)
    expect(parseOAuthReturn(`${ORIGIN}/oauth/return?error=access_denied&state=${STATE}`)?.url)
      .toBe(`${ORIGIN}/oauth/return?error=access_denied&state=${STATE}`)
  })

  it('never forwards the fragment', () => {
    expect(parseOAuthReturn(`${ORIGIN}/oauth/return?code=abc&state=${STATE}#token=leak`)?.url).toBe(`${ORIGIN}/oauth/return?code=abc&state=${STATE}`)
  })

  it.each([
    ['no state', `/oauth/return?code=abc`],
    ['a short state', `/oauth/return?code=abc&state=short`],
    ['a state with odd characters', `/oauth/return?code=abc&state=${STATE}%3Cscript%3E`],
    ['two states', `/oauth/return?code=abc&state=${STATE}&state=${STATE}`],
    ['both a code and an error', `/oauth/return?code=abc&error=access_denied&state=${STATE}`],
    ['neither a code nor an error', `/oauth/return?state=${STATE}`],
    ['a code in the fragment', `/oauth/return#code=abc&state=${STATE}`],
    ['a code with whitespace', `/oauth/return?code=a%20b&state=${STATE}`],
  ])('treats a return with %s as malformed, and posts nothing', (_, path) => {
    const found = parseOAuthReturn(ORIGIN + path)
    expect(found).not.toBeNull()
    expect(found!.url).toBe('')
  })

  it('ignores every page that is not the return route', () => {
    expect(parseOAuthReturn(`${ORIGIN}/?code=abc&state=${STATE}`)).toBeNull()
    expect(parseOAuthReturn(`${ORIGIN}/oauth/callback?code=abc&state=${STATE}`)).toBeNull()
  })
})

describe('keeping a return across sign-in', () => {
  beforeEach(() => { stubPage(`/oauth/return?code=abc&state=${STATE}`) })

  it('takes the code out of the address bar at once and keeps it in this tab only', () => {
    expect(captureOAuthReturn()).toBe(true)
    expect(history.replaceState).toHaveBeenCalledWith(null, '', '/')
    expect(pendingOAuthReturn()).toBe(true)
    expect(takeOAuthReturn()?.url).toBe(`${ORIGIN}/oauth/return?code=abc&state=${STATE}`)
    expect(takeOAuthReturn()).toBeNull()
    expect(pendingOAuthReturn()).toBe(false)
  })

  it('lets a waiting return lapse with the provider flow', () => {
    captureOAuthReturn()
    expect(pendingOAuthReturn(Date.now() + FLOW_LIFETIME_MS + 1000)).toBe(false)
  })

  it('does nothing on an ordinary page', () => {
    vi.stubGlobal('location', new URL(ORIGIN + '/'))
    expect(captureOAuthReturn()).toBe(false)
    expect(history.replaceState).not.toHaveBeenCalled()
  })

  it('remembers which mailbox a flow was for, for ten minutes, and only once', () => {
    rememberOAuthStart({ account_id: 'acc_1', provider: 'microsoft', email: 'ana@example.test' })
    expect(takeOAuthStart()).toMatchObject({ account_id: 'acc_1', provider: 'microsoft', email: 'ana@example.test' })
    expect(takeOAuthStart()).toBeNull()
    rememberOAuthStart({ account_id: 'acc_1', provider: 'microsoft', email: 'ana@example.test' })
    expect(takeOAuthStart(Date.now() + FLOW_LIFETIME_MS + 1000)).toBeNull()
    sessionStorage.setItem('mailie.oauthStart', JSON.stringify({ account_id: 'acc_1', provider: 'yahoo', email: 'x', started_at: Date.now() }))
    expect(takeOAuthStart()).toBeNull()
  })

  it('takes a link’s start, which names no address, as fresh, and never one that mixes a link with a mailbox', () => {
    rememberOAuthStart({ link_id: 'lnk_0000000000000001', provider: 'microsoft', workspace_id: 'wsp_000000000000bbbb', back: '/settings' })
    expect(oauthBack(JSON.parse(sessionStorage.getItem('mailie.oauthStart')!), ORIGIN)).toBe('/settings')
    expect(peekOAuthStart()).toMatchObject({ link_id: 'lnk_0000000000000001', provider: 'microsoft', workspace_id: 'wsp_000000000000bbbb' })
    expect(takeOAuthStart()).toMatchObject({ link_id: 'lnk_0000000000000001' })
    expect(takeOAuthStart()).toBeNull()
    for (const record of [
      { link_id: 'lnk_0000000000000001', provider: 'microsoft', email: 'ana@example.test' },
      { link_id: 'lnk_0000000000000001', provider: 'gmail', account_id: 'acc_1' },
      { link_id: 'lnk_0000000000000001', provider: 'icloud' },
      { link_id: '', provider: 'gmail' },
      { link_id: 'lnk_0000000000000001', provider: 'gmail', workspace_id: '' },
    ]) {
      sessionStorage.setItem('mailie.oauthStart', JSON.stringify({ ...record, started_at: Date.now() }))
      expect(takeOAuthStart(), JSON.stringify(record)).toBeNull()
    }
  })

  it('drops a return still waiting when a new flow starts, which it does not belong to', () => {
    captureOAuthReturn()
    expect(pendingOAuthReturn()).toBe(true)
    rememberOAuthStart({ link_id: 'lnk_0000000000000002', provider: 'gmail' })
    expect(pendingOAuthReturn()).toBe(false)
    expect(sessionStorage.getItem('mailie.oauthReturn')).toBeNull()
  })

  it('still finishes a return on the same page load when storage is refused, and only once', () => {
    vi.stubGlobal('sessionStorage', refused())
    expect(captureOAuthReturn()).toBe(true)
    expect(history.replaceState).toHaveBeenCalledWith(null, '', '/')
    expect(pendingOAuthReturn()).toBe(true)
    expect(takeOAuthReturn()?.url).toBe(`${ORIGIN}/oauth/return?code=abc&state=${STATE}`)
    expect(takeOAuthReturn()).toBeNull()
    expect(pendingOAuthReturn()).toBe(false)
  })
})

describe('coming back to the page the flow left from', () => {
  beforeEach(() => { stubPage(`/oauth/return?code=abc&state=${STATE}`) })
  const start = { account_id: 'acc_1', provider: 'gmail', email: 'ana@example.test' } as const
  const kept = () => JSON.parse(sessionStorage.getItem('mailie.oauthStart')!) as Record<string, unknown>

  it('comes back to the path it left from, with the code out of the address bar, and leaves the start to be taken once', () => {
    // As a browser does: the address becomes what replaceState names.
    vi.mocked(history.replaceState).mockImplementation((_state, _unused, url) => { vi.stubGlobal('location', new URL(String(url), ORIGIN)) })
    rememberOAuthStart({ ...start, back: '/settings/mailboxes' })
    expect(kept().back).toBe('/settings/mailboxes')
    expect(captureOAuthReturn()).toBe(true)
    expect(history.replaceState).toHaveBeenCalledExactlyOnceWith(null, '', '/settings/mailboxes')
    expect(JSON.stringify(vi.mocked(history.replaceState).mock.calls)).not.toContain('code')
    // Read, not taken: finishing the return takes it, once.
    expect(takeOAuthStart()).toMatchObject({ ...start, back: '/settings/mailboxes' })
    expect(takeOAuthStart()).toBeNull()
    // The same page load again: no return any more, nothing replaced.
    expect(captureOAuthReturn()).toBe(false)
    expect(history.replaceState).toHaveBeenCalledTimes(1)
  })

  it.each([
    ['nothing', undefined],
    ['a path from the root of another origin', '//evil.example/settings'],
    ['a backslash, which a browser reads as a slash', '/\\evil.example/settings'],
    ['a control character', '/settings\u0000/mailboxes'],
    ['a tab', '/settings\t/mailboxes'],
    ['a relative path', 'settings/mailboxes'],
    ['an absolute URL', 'https://evil.example/settings'],
    ['the provider’s return itself', '/oauth/return'],
    ['dot segments', '/settings/../mailboxes'],
    ['what the browser would encode', '/configurações'],
    ['a query, which may name a workspace', '/settings/mailboxes?workspace=wsp_000000000000bbbb'],
    ['a fragment', '/settings/mailboxes#keys'],
    ['more than 1024 bytes', `/${'a'.repeat(1024)}`],
  ])('keeps no back given %s, and comes back to /', (_, back) => {
    rememberOAuthStart({ ...start, back })
    expect(kept()).not.toHaveProperty('back')
    expect(JSON.stringify(kept())).not.toContain('wsp_')
    captureOAuthReturn()
    expect(history.replaceState).toHaveBeenCalledExactlyOnceWith(null, '', '/')
  })

  it('comes back to / when the start is older than the flow, written in the future, not this page’s, or gone', () => {
    rememberOAuthStart({ ...start, back: '/settings' }, Date.now() - FLOW_LIFETIME_MS - 1000)
    captureOAuthReturn()
    expect(history.replaceState).toHaveBeenLastCalledWith(null, '', '/')
    for (const record of [
      { ...start, back: '/settings', started_at: Date.now() + 60_000 },
      { ...start, provider: 'yahoo', back: '/settings', started_at: Date.now() },
      // Written by something else, with a back no page here would keep.
      { ...start, back: '//evil.example/', started_at: Date.now() },
    ]) {
      sessionStorage.setItem('mailie.oauthStart', JSON.stringify(record))
      vi.stubGlobal('location', new URL(`${ORIGIN}/oauth/return?code=abc&state=${STATE}`))
      captureOAuthReturn()
      expect(history.replaceState).toHaveBeenLastCalledWith(null, '', '/')
    }
    sessionStorage.removeItem('mailie.oauthStart')
    vi.stubGlobal('location', new URL(`${ORIGIN}/oauth/return?code=abc&state=${STATE}`))
    captureOAuthReturn()
    expect(history.replaceState).toHaveBeenLastCalledWith(null, '', '/')
  })

  it('answers a back only for a fresh start of this origin', () => {
    const at = Date.now()
    expect(oauthBack({ ...start, back: '/settings', started_at: at }, ORIGIN, at)).toBe('/settings')
    expect(oauthBack({ ...start, back: '/settings', started_at: at }, ORIGIN, at + FLOW_LIFETIME_MS + 1)).toBeNull()
    expect(oauthBack({ ...start, started_at: at }, ORIGIN, at)).toBeNull()
    expect(oauthBack(null, ORIGIN, at)).toBeNull()
    expect(oauthBack('/settings', ORIGIN, at)).toBeNull()
  })
})

describe('nothing outlives the provider flow', () => {
  beforeEach(() => { stubPage(`/oauth/return?code=abc&state=${STATE}`) })

  it('removes an unused start and return once ten minutes are up, rather than only ignoring them', () => {
    rememberOAuthStart({ account_id: 'acc_1', provider: 'gmail', email: 'ana@example.test' })
    captureOAuthReturn()
    const left = sweepOAuthStorage()
    expect(left).toBeGreaterThan(FLOW_LIFETIME_MS - 1000)
    expect(left).toBeLessThanOrEqual(FLOW_LIFETIME_MS)
    expect(sessionStorage.getItem('mailie.oauthStart')).not.toBeNull()
    expect(sessionStorage.getItem('mailie.oauthReturn')).not.toBeNull()

    expect(sweepOAuthStorage(Date.now() + FLOW_LIFETIME_MS)).toBeNull()
    expect(sessionStorage.getItem('mailie.oauthStart')).toBeNull()
    expect(sessionStorage.getItem('mailie.oauthReturn')).toBeNull()
    // The in-memory copy of the code goes too.
    expect(takeOAuthReturn()).toBeNull()
  })

  it('answers when the next record expires, so the page can come back for it', () => {
    const later = Date.now() + 4 * 60_000
    rememberOAuthStart({ account_id: 'acc_1', provider: 'gmail', email: 'ana@example.test' }, later)
    captureOAuthReturn()
    // Six minutes on: the return has four left, the start ten.
    const left = sweepOAuthStorage(Date.now() + 6 * 60_000)!
    expect(left).toBeGreaterThan(4 * 60_000 - 1000)
    expect(left).toBeLessThanOrEqual(4 * 60_000)
  })

  it('removes what this page did not write, and copes with storage that refuses', () => {
    sessionStorage.setItem('mailie.oauthReturn', 'not json')
    sessionStorage.setItem('mailie.oauthStart', JSON.stringify({ account_id: 'acc_1', started_at: 'yesterday' }))
    expect(sweepOAuthStorage()).toBeNull()
    expect(sessionStorage.getItem('mailie.oauthReturn')).toBeNull()
    expect(sessionStorage.getItem('mailie.oauthStart')).toBeNull()

    vi.stubGlobal('sessionStorage', refused())
    expect(() => sweepOAuthStorage()).not.toThrow()
  })
})

describe('finishing a return', () => {
  async function signedIn(route: Parameters<typeof serve>[0]) {
    await freshModules()
    const session = await import('../src/state/session')
    const accounts = await import('../src/state/accounts')
    const returns = await import('../src/state/oauthReturn')
    const fetch = serve(request => request.path === '/v1/auth/login' ? json(reply()) : route(request))
    await session.adoptSession(reply())
    return { session, accounts, returns, fetch }
  }

  beforeEach(() => { stubPage(`/oauth/return?code=abc&state=${STATE}#x=1`) })

  it('posts the return URL, with the bearer token, exactly once, and says which mailbox connected', async () => {
    const { accounts, returns, fetch } = await signedIn(() => json(account({ state: 'active', email: 'ana.souza@example.test' })))
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'microsoft', email: 'ana.souza@example.test' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    await accounts.finishOAuthReturn()
    const posts = fetch.mock.calls.filter(([url]) => String(url).endsWith('/v1/accounts/oauth/callback'))
    expect(posts).toHaveLength(1)
    const [url, init] = posts[0]!
    expect(url).toBe(`${ORIGIN}/v1/accounts/oauth/callback`)
    expect(init!.method).toBe('POST')
    expect(JSON.parse(String(init!.body))).toEqual({ redirect_url: `${ORIGIN}/oauth/return?code=abc&state=${STATE}` })
    expect((init!.headers as Record<string, string>).Authorization).toBe('Bearer ' + reply().token)
    expect(accounts.accounts.notice).toEqual({ kind: 'connected', email: 'ana.souza@example.test', syncing: false })
    expect(accounts.accounts.list.map(item => item.email)).toEqual(['ana.souza@example.test'])
  })

  it('finishes a return that comes back before the person’s workspaces were read, and shows the one its mailbox is in', async () => {
    const PERSONAL = 'wsp_000000000000aaaa'
    const TEAM = 'wsp_000000000000bbbb'
    const listed = [
      { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 },
      { id: TEAM, kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1_790_000_000 },
    ]
    const { accounts, returns, fetch } = await signedIn(({ path }) => path === '/v1/workspaces'
      ? json(listed) : json(account({ state: 'active', email: 'suporte@example.test', workspace_id: TEAM })))
    const { workspaces } = await import('../src/state/workspaces')
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    returns.captureOAuthReturn()
    // A page back from the provider: the workspace shown is first known while the return is finished.
    expect(workspaces.loaded).toBe(false)
    await accounts.finishOAuthReturn()
    expect(fetch.mock.calls.filter(([url]) => String(url).endsWith('/v1/accounts/oauth/callback'))).toHaveLength(1)
    expect(workspaces.currentID).toBe(TEAM)
    expect(accounts.accounts.notice).toEqual({ kind: 'connected', email: 'suporte@example.test', syncing: false })
  })

  it('posts nothing for a return whose person signed out while the workspaces were read, not even with the next person’s session', async () => {
    let release!: () => void
    const held = new Promise<void>(resolve => { release = resolve })
    const { accounts, returns, fetch, session } = await signedIn(async ({ path }) => {
      if (path !== '/v1/workspaces') return json(account({ state: 'active' }))
      await held
      return json([{ id: 'wsp_000000000000aaaa', kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }])
    })
    returns.captureOAuthReturn()
    const finishing = accounts.finishOAuthReturn()
    await session.signOut()
    await session.adoptSession(reply('tok_second_000000000000000000000000000000000', { ...ana, id: 'usr_00000000000000b1', email: 'bea@example.test' }))
    release()
    await finishing
    expect(fetch.mock.calls.filter(([url]) => String(url).endsWith('/v1/accounts/oauth/callback'))).toHaveLength(0)
  })

  it('posts a return exactly once in a browser that refuses site storage', async () => {
    vi.stubGlobal('sessionStorage', refused())
    const { accounts, returns, fetch } = await signedIn(({ path }) => path.endsWith('/oauth/callback') ? failure('not_found', 404) : json(account()))
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    expect(returns.captureOAuthReturn()).toBe(true)
    expect(returns.pendingOAuthReturn()).toBe(true)
    await accounts.finishOAuthReturn()
    await accounts.finishOAuthReturn()
    const posts = fetch.mock.calls.filter(([url]) => String(url).endsWith('/v1/accounts/oauth/callback'))
    expect(posts).toHaveLength(1)
    expect(JSON.parse(String(posts[0]![1]!.body))).toEqual({ redirect_url: `${ORIGIN}/oauth/return?code=abc&state=${STATE}` })
    // Without storage the start is lost, so the notice cannot name the mailbox.
    expect(accounts.accounts.notice).toEqual({ kind: 'failed', email: '', failure: { op: 'complete-auth', code: 'not_found' } })
  })

  it('waits longer for the exchange than the server takes over it', async () => {
    const timeout = vi.spyOn(AbortSignal, 'timeout')
    const { accounts, returns } = await signedIn(() => json(account({ state: 'active' })))
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    // The server's exchange takes up to 40 s, after up to 20 s of a link's checks, inside a 70 s route.
    expect(timeout).toHaveBeenLastCalledWith(75_000)
  })

  it('believes the account, not a lost reply, when the exchange finished after the page stopped waiting', async () => {
    const { accounts, returns } = await signedIn(({ path }) => path.endsWith('/oauth/callback') ? failure('unavailable', 503) : json(account({ state: 'active' })))
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(accounts.accounts.notice).toEqual({ kind: 'connected', email: 'suporte@example.test', syncing: false })
    expect(accounts.accounts.list.map(item => item.state)).toEqual(['active'])
  })

  it('still reports the failure when the account did not change', async () => {
    const { accounts, returns } = await signedIn(({ path }) => path.endsWith('/oauth/callback') ? failure('unavailable', 503) : json(account({ state: 'pending_auth' })))
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(accounts.accounts.notice).toEqual({ kind: 'failed', email: 'suporte@example.test', failure: { op: 'complete-auth', code: 'unavailable' } })
  })

  it('reports a flow the server does not recognise for this person, naming the mailbox it was for', async () => {
    const { accounts, returns } = await signedIn(({ path }) => path.endsWith('/oauth/callback') ? failure('not_found', 404) : json(account({ state: 'pending_auth' })))
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(accounts.accounts.notice).toEqual({ kind: 'failed', email: 'suporte@example.test', failure: { op: 'complete-auth', code: 'not_found' } })
  })

  it('names what to do when the provider’s grant came back without the mailbox, in the person’s language', async () => {
    const { accounts, returns } = await signedIn(({ path }) => path.endsWith('/oauth/callback') ? failure('bad_request', 400) : json(account({ state: 'error', state_reason: REASON_NOT_GRANTED })))
    const { locale } = await import('../src/ui/i18n')
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(accounts.accounts.notice).toEqual({ kind: 'failed', email: 'suporte@example.test', failure: { op: 'complete-auth', code: 'bad_request', account: { provider: 'gmail', email: 'suporte@example.test', state: 'error', state_reason: REASON_NOT_GRANTED } } })
    // The card below it has the same account, now failing, with its button to try again.
    expect(accounts.accounts.list.map(item => item.state)).toEqual(['error'])
    locale.value = 'pt'
    expect(noticeText(accounts.accounts.notice!)).toBe('suporte@example.test não foi conectada. O Google não deu ao Mailie acesso a esta caixa de email. Tente novamente e, na tela do Google, permita o acesso ao Gmail (marque a caixa, se aparecer uma).')
    locale.value = 'en'
  })

  it('never calls an account connected when the server stored the grant but the mailbox refused it', async () => {
    const refused = account({ state: 'needs_reauth', state_reason: REASON_MAILBOX_REFUSED })
    const { accounts, returns } = await signedIn(() => json(refused))
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ account_id: 'acc_0000000000000001', provider: 'gmail', email: 'suporte@example.test' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(accounts.accounts.notice).toMatchObject({ kind: 'failed', failure: { op: 'complete-auth', account: { state: 'needs_reauth', state_reason: REASON_MAILBOX_REFUSED } } })
    expect(noticeText(accounts.accounts.notice!)).toBe('suporte@example.test was not connected. The mail server refused this authorization. Try again and choose the account suporte@example.test on Google’s sign-in screen.')
  })

  it('posts nothing for a malformed return and says so', async () => {
    vi.stubGlobal('location', new URL(`${ORIGIN}/oauth/return?code=abc`))
    const { accounts, returns, fetch } = await signedIn(() => json(account()))
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(fetch.mock.calls.some(([url]) => String(url).endsWith('/oauth/callback'))).toBe(false)
    expect(accounts.accounts.notice).toMatchObject({ kind: 'failed', failure: { code: 'return_invalid' } })
  })
})

describe('finishing a link at the provider', () => {
  const LINK = 'lnk_0000000000000001'
  const PERSONAL = 'wsp_000000000000aaaa'
  const TEAM = 'wsp_000000000000bbbb'
  const listed = [
    { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 },
    { id: TEAM, kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1_790_000_000 },
  ]
  const link = (fields: Record<string, unknown> = {}) => ({ id: LINK, provider: 'gmail', state: 'waiting', expires_at: now() + 600, ...fields })
  const callbacks = (fetch: ReturnType<typeof serve>) => fetch.mock.calls
    .filter(([url]) => String(url).endsWith('/v1/accounts/oauth/callback'))
    .map(([, init]) => JSON.parse(String(init!.body)) as Record<string, string>)
  const reads = (fetch: ReturnType<typeof serve>) => fetch.mock.calls.filter(([url]) => String(url).endsWith(`/v1/accounts/oauth/links/${LINK}`)).length

  type Steps = typeof import('../src/state/stepUp')
  /**
   * A new page load signed in as signedInAs, with the edition's own step-up
   * when one is given (made with this page's modules); route answers all but
   * sign-in.
   */
  async function signedIn(route: Parameters<typeof serve>[0], signedInAs: SessionReply = reply(), stepUp?: (steps: Steps) => NonNullable<Edition['stepUp']>) {
    await freshModules()
    const steps = await import('../src/state/stepUp')
    if (stepUp) {
      const [{ configureEdition }, { openEdition }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
      configureEdition({ ...openEdition, stepUp: stepUp(steps) })
    }
    const session = await import('../src/state/session')
    const accounts = await import('../src/state/accounts')
    const returns = await import('../src/state/oauthReturn')
    const { workspaces } = await import('../src/state/workspaces')
    const fetch = serve(request => request.path === '/v1/auth/login' ? json(reply()) : route(request))
    await session.adoptSession(signedInAs)
    return { session, accounts, returns, steps, workspaces, fetch }
  }
  /** A person with a real account key: the public half the server serves, and the private half only their browser holds. */
  async function linker(): Promise<{ user: User; account: PrivateKey }> {
    const keys = await generateAccountKeys()
    return { user: { ...ana, public_key: toBase64URL(keys.publicKey) }, account: await importPrivateKey(keys.privateKey) }
  }

  /** Lets the page's promises run, with the fake timers left alone, until check holds. */
  async function until(check: () => boolean) {
    for (let round = 0; round < 2_000 && !check(); round++) await new Promise(resolve => setImmediate(resolve))
    expect(check()).toBe(true)
  }

  beforeEach(() => { stubPage(`/oauth/return?code=abc&state=${STATE}`) })

  it('sends the mailbox’s first key with a link’s return, sealed to the person who started it, and takes the return once answered', async () => {
    const person = await linker()
    const made = account({ id: 'acc_0000000000000008', email: 'ana.lima@gmail.com', state: 'active' })
    const { accounts, returns, fetch } = await signedIn(({ path }) => path.endsWith('/oauth/callback') ? json(made) : failure('not_found', 404), reply(undefined, person.user))
    returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    const [body, ...more] = callbacks(fetch)
    expect(more).toEqual([])
    expect(Object.keys(body!).sort()).toEqual(['grant', 'namespace', 'public_key', 'redirect_url'])
    expect(body!.redirect_url).toBe(`${ORIGIN}/oauth/return?code=abc&state=${STATE}`)
    const grant = fromBase64URL(body!.grant!, 88)
    checkGrantShape(grant, 1)
    expect(await openGrant(person.account, body!.namespace!, person.user.seal_id!, 1, fromBase64URL(body!.public_key!, 32), grant)).toHaveLength(32)
    expect(returns.pendingOAuthReturn()).toBe(false)
    expect(returns.takeOAuthStart()).toBeNull()
    expect(accounts.accounts.notice).toEqual({ kind: 'connected', email: 'ana.lima@gmail.com', syncing: false })
    // Answered: the same page posts nothing again.
    await accounts.finishOAuthReturn()
    expect(callbacks(fetch)).toHaveLength(1)
  })

  it.each([
    ['gmail', 'You did not allow access, so nothing was connected.'],
    ['microsoft', 'Access was not allowed, so nothing was connected. If Microsoft said your organization’s administrator must approve Mailie, ask them, or enter the mailbox’s address instead.'],
  ] as const)('posts a %s link’s refusal at once, with no key and no step-up, and says why it ended', async (provider, said) => {
    stubPage(`/oauth/return?error=access_denied&state=${STATE}`)
    const { accounts, returns, steps, fetch } = await signedIn(({ path }) => {
      if (path.endsWith('/oauth/callback')) return failure('bad_request', 400)
      if (path === `/v1/accounts/oauth/links/${LINK}`) return json(link({ provider, state: 'failed', reason: REASON_DECLINED }))
      return failure('not_found', 404)
    }, { ...reply(), authenticated_at: 0 })
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ link_id: LINK, provider, back: '/' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(steps.stepUpPrompt.open).toBe(false)
    expect(callbacks(fetch)).toEqual([{ redirect_url: `${ORIGIN}/oauth/return?error=access_denied&state=${STATE}` }])
    expect(accounts.accounts.notice).toEqual({ kind: 'failed', email: '', failure: { op: 'complete-link', code: 'bad_request', link: { provider, reason: REASON_DECLINED } } })
    expect(noticeText(accounts.accounts.notice!)).toBe(said)
    expect(returns.pendingOAuthReturn()).toBe(false)
  })

  it('asks for the step-up before it takes a link’s return, and posts the return once it is given', async () => {
    // A minute of the ten left: too little for the server's own checks once the code is exchanged.
    const { accounts, returns, session, steps, fetch } = await signedIn(({ path }) => path.endsWith('/oauth/callback')
      ? json(account({ id: 'acc_0000000000000008', email: 'ana.lima@gmail.com', state: 'active' })) : failure('not_found', 404), { ...reply(), authenticated_at: now() - 9 * 60 })
    returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    returns.captureOAuthReturn()
    const finishing = accounts.finishOAuthReturn()
    await vi.waitFor(() => expect(steps.stepUpPrompt.open).toBe(true))
    expect(callbacks(fetch)).toEqual([])
    // Still in the tab while the person proves themselves.
    expect(returns.pendingOAuthReturn()).toBe(true)
    expect(returns.peekOAuthStart()).toMatchObject({ link_id: LINK })
    session.steppedUp(ana.id, now())
    steps.stepUpGiven()
    await finishing
    expect(callbacks(fetch)).toHaveLength(1)
    expect(callbacks(fetch)[0]).toHaveProperty('grant')
    expect(returns.pendingOAuthReturn()).toBe(false)
  })

  it('keeps a link’s return in the tab when its step-up is closed or sends the tab away, and finishes it on the page that comes back', async () => {
    const answer = ({ path }: { path: string }) => path.endsWith('/oauth/callback')
      ? json(account({ id: 'acc_0000000000000008', email: 'ana.lima@gmail.com', state: 'active' })) : failure('not_found', 404)
    // The edition's step-up, whose window the browser blocked: it stops the flows, and sends the tab to prove the person again.
    const away = vi.fn()
    const first = await signedIn(answer, { ...reply(), authenticated_at: 0 }, steps => async (reason, margin) => { away(reason, margin); throw new steps.StepUpCancelled() })
    const { noticeText } = await import('../src/ui/notices')
    first.returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    first.returns.captureOAuthReturn()
    await first.accounts.finishOAuthReturn()
    expect(away).toHaveBeenCalledWith('link', 120)
    expect(callbacks(first.fetch)).toEqual([])
    expect(first.returns.pendingOAuthReturn()).toBe(true)
    expect(noticeText(first.accounts.accounts.notice!)).toBe('Mailie needs to confirm it is you to finish connecting the mailbox. Start again.')
    // The page the tab comes back to, in the same tab, its step-up fresh: the same return, posted once, with a key.
    const kept = ['mailie.oauthStart', 'mailie.oauthReturn'].map(key => [key, sessionStorage.getItem(key)!] as const)
    expect(kept.every(([, value]) => value)).toBe(true)
    vi.restoreAllMocks()
    stubPage('/')
    for (const [key, value] of kept) sessionStorage.setItem(key, value)
    const second = await signedIn(answer)
    expect(second.returns.captureOAuthReturn()).toBe(false)
    await second.accounts.finishOAuthReturn()
    expect(callbacks(second.fetch)).toHaveLength(1)
    expect(callbacks(second.fetch)[0]).toMatchObject({ redirect_url: `${ORIGIN}/oauth/return?code=abc&state=${STATE}`, grant: expect.any(String) })
    expect(second.accounts.accounts.notice).toEqual({ kind: 'connected', email: 'ana.lima@gmail.com', syncing: false })
    expect(second.returns.pendingOAuthReturn()).toBe(false)
  })

  it('asks a link still finishing every 2 s after its callback went unanswered, and shows the mailbox it connected', async () => {
    const made = account({ id: 'acc_0000000000000008', email: 'ana.lima@gmail.com', state: 'active' })
    const states = ['finishing', 'finishing', 'connected']
    const { accounts, returns, fetch } = await signedIn(({ path }) => {
      if (path.endsWith('/oauth/callback')) return failure('unavailable', 503)
      if (path === `/v1/accounts/oauth/links/${LINK}`) {
        const state = states.shift()!
        return json(link(state === 'connected' ? { state, account_id: made.id } : { state }))
      }
      if (path === `/v1/accounts/${made.id}`) return json(made)
      return failure('not_found', 404)
    })
    returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    returns.captureOAuthReturn()
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      const finishing = accounts.finishOAuthReturn()
      await until(() => reads(fetch) === 1)
      await vi.advanceTimersByTimeAsync(1_900)
      expect(reads(fetch)).toBe(1)
      await vi.advanceTimersByTimeAsync(100)
      await until(() => reads(fetch) === 2)
      await vi.advanceTimersByTimeAsync(2_000)
      await until(() => reads(fetch) === 3)
      await finishing
    } finally { vi.useRealTimers() }
    expect(reads(fetch)).toBe(3)
    expect(callbacks(fetch)).toHaveLength(1)
    expect(accounts.accounts.notice).toEqual({ kind: 'connected', email: 'ana.lima@gmail.com', syncing: false })
    expect(accounts.accounts.list.map(item => item.id)).toEqual([made.id])
  })

  it('shows the team a failed link went into, and says why it failed, naming the address the provider named', async () => {
    const { accounts, returns, workspaces, fetch } = await signedIn(({ path }) => {
      if (path === '/v1/workspaces') return json(listed)
      if (path === '/v1/accounts') return json([])
      if (path.endsWith('/oauth/callback')) return failure('bad_request', 400)
      if (path === `/v1/accounts/oauth/links/${LINK}`) return json(link({ provider: 'microsoft', workspace_id: TEAM, state: 'failed', email: 'vendas@example.test', reason: REASON_MAILBOX_REFUSED }))
      return failure('not_found', 404)
    })
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ link_id: LINK, provider: 'microsoft', workspace_id: TEAM, back: '/' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(reads(fetch)).toBe(1)
    expect(workspaces.currentID).toBe(TEAM)
    expect(accounts.accounts.notice).toEqual({ kind: 'failed', email: 'vendas@example.test', failure: { op: 'complete-link', code: 'bad_request', link: { provider: 'microsoft', reason: REASON_MAILBOX_REFUSED } } })
    expect(noticeText(accounts.accounts.notice!)).toBe('vendas@example.test was not connected. Microsoft’s mail server did not accept this account, so nothing was connected. Check that it has an Outlook or Exchange mailbox with IMAP turned on, or enter the mailbox’s address instead.')
  })

  it('names the address of a link to a mailbox the workspace holds with the one text that never tells a removed one apart', async () => {
    const { accounts, returns, workspaces } = await signedIn(({ path }) => {
      if (path === '/v1/workspaces') return json(listed)
      if (path === '/v1/accounts') return json([])
      if (path.endsWith('/oauth/callback')) return failure('conflict', 409)
      if (path === `/v1/accounts/oauth/links/${LINK}`) return json(link({ state: 'failed', email: 'ana.lima@gmail.com', reason: REASON_DUPLICATE }))
      return failure('not_found', 404)
    })
    const { locale } = await import('../src/ui/i18n')
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    // Into the personal workspace: that is the one shown.
    expect(workspaces.currentID).toBe(PERSONAL)
    expect(noticeText(accounts.accounts.notice!)).toBe('ana.lima@gmail.com was not connected. This address is already connected in this workspace, or was removed from it and is still kept. If it is waiting for authorization, finish it from its card; if it was removed, an owner of the workspace can recover it, or delete it for good to connect it again.')
    locale.value = 'pt'
    expect(noticeText(accounts.accounts.notice!)).toMatch(/^ana\.lima@gmail\.com não foi conectada\. /)
    locale.value = 'en'
  })

  it('says a link that failed for a reason the console does not know as a flow that did not finish, never in the server’s words', async () => {
    const { accounts, returns } = await signedIn(({ path }) => {
      if (path.endsWith('/oauth/callback')) return failure('bad_request', 400)
      if (path === `/v1/accounts/oauth/links/${LINK}`) return json(link({ state: 'failed', reason: 'the provider issued no refresh token' }))
      return failure('not_found', 404)
    })
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(noticeText(accounts.accounts.notice!)).toBe('The provider did not complete the authorization. Try again.')
  })

  it('says a link that ended with its starter’s place is gone, and asks the link nothing more', async () => {
    const { accounts, returns, fetch } = await signedIn(() => failure('not_found', 404))
    const { noticeText } = await import('../src/ui/notices')
    returns.rememberOAuthStart({ link_id: LINK, provider: 'gmail', back: '/' })
    returns.captureOAuthReturn()
    await accounts.finishOAuthReturn()
    expect(reads(fetch)).toBe(1)
    expect(noticeText(accounts.accounts.notice!)).toBe('This authorization expired or was started by another sign-in. Start again.')
  })
})
