import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { FLOW_LIFETIME_MS, captureOAuthReturn, oauthBack, parseOAuthReturn, pendingOAuthReturn, rememberOAuthStart, sweepOAuthStorage, takeOAuthReturn, takeOAuthStart } from '../src/state/oauthReturn'
import { REASON_MAILBOX_REFUSED, REASON_NOT_GRANTED } from '../src/ui/reasons'
import { account, failure, freshModules, json, ORIGIN, reply, serve, stubPage } from './support'

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
    captureOAuthReturn()
    const later = Date.now() + 4 * 60_000
    rememberOAuthStart({ account_id: 'acc_1', provider: 'gmail', email: 'ana@example.test' }, later)
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
    // The server's exchange takes up to 40 s inside a 45 s route.
    expect(timeout).toHaveBeenLastCalledWith(50_000)
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
