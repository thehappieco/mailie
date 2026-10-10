import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick, watch } from 'vue'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { fromBase64URL, toBase64URL } from '@thehappieco/kit/bytes'
import { importPrivateKey, type PrivateKey } from '@thehappieco/kit/hpke'
import { checkGrantShape, openGrant } from '@thehappieco/kit/profiles/mailie'
import type { Account, AddAccountRequest, SessionReply, User } from '../src/api/types'
import { draftRequest, emptyDraft } from '../src/ui/accountDraft'
import { REASON_MAILBOX_REFUSED, REASON_NOT_GRANTED, REASON_TOKEN_REJECTED } from '../src/ui/reasons'
import { account, ana, failure, freshModules, json, now, reply, type Route, serve, stubPage } from './support'

// Fresh modules per test: the session and the accounts store are module
// state, and the point of several tests is what happens when they change.
async function signedIn(route: Route, signedInAs: SessionReply = reply()) {
  await freshModules()
  const session = await import('../src/state/session')
  const store = await import('../src/state/accounts')
  const stepUp = await import('../src/state/stepUp')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    // A server without workspaces: every list is the person's whole.
    if (request.path === '/v1/workspaces') return failure('not_found', 404)
    return route(request)
  })
  await session.adoptSession(signedInAs)
  // Only timers the polling uses are faked; IndexedDB runs on setImmediate.
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'] })
  const navigate = vi.fn()
  store.navigation.assign = navigate
  return { session, store, stepUp, fetch, navigate }
}

/** A person with a real account key: the public half the server serves, and the private half only their browser holds. */
async function linker(): Promise<{ user: User; account: PrivateKey }> {
  const keys = await generateAccountKeys()
  return { user: { ...ana, public_key: toBase64URL(keys.publicKey) }, account: await importPrivateKey(keys.privateKey) }
}

/** The mailbox's first key a link sent opens for its linker, at epoch 1, to the private half of the public key it sent: answers the mailbox key. */
async function opensForLinker(body: AddAccountRequest, person: { user: User; account: PrivateKey }): Promise<Uint8Array> {
  const grant = fromBase64URL(body.grant!, 88)
  checkGrantShape(grant, 1)
  return openGrant(person.account, body.namespace!, person.user.seal_id!, 1, fromBase64URL(body.public_key!, 32), grant)
}

/** A person's link carries exactly these fields beside the details: the first key, never a private one. */
const KEY_FIELDS = ['grant', 'namespace', 'public_key']

const polls = (fetch: ReturnType<typeof serve>, id = 'acc_0000000000000001') =>
  fetch.mock.calls.filter(([url, init]) => String(url).endsWith(`/v1/accounts/${id}`) && (init?.method ?? 'GET') === 'GET').length

const loopback = () => ({ flow: 'loopback', auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?state=x', state: 'x', expires_at: now() + 600 })

const starts = (fetch: ReturnType<typeof serve>) => fetch.mock.calls.filter(([url]) => String(url).endsWith('/oauth/start')).length
const deletes = (fetch: ReturnType<typeof serve>) => fetch.mock.calls.filter(([, init]) => init?.method === 'DELETE').map(([url]) => String(url))

/** A daemon that creates one pending Gmail account and then answers polls from a script. */
function daemon(states: Account['state'][], flow: object = loopback()) {
  const script = [...states]
  return (({ path, method }) => {
    if (path === '/v1/accounts' && method === 'POST') return json({ account: account(), auth: flow }, 201)
    if (path === '/v1/accounts/acc_0000000000000001' && method === 'DELETE') return new Response(null, { status: 204 })
    if (path === '/v1/accounts/acc_0000000000000001') return json(account({ state: script.length > 1 ? script.shift()! : script[0]! }))
    return failure('not_found', 404)
  }) satisfies Route
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('waiting for a provider', () => {
  it('asks for the account every 3 seconds until it is active, and never sooner', async () => {
    const { store, fetch } = await signedIn(daemon(['pending_auth', 'pending_auth', 'active']))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(store.connect.phase).toBe('waiting')
    expect(store.connect.authURL).toMatch(/^https:\/\/accounts\.google\.com\//)
    await vi.advanceTimersByTimeAsync(2_900)
    expect(polls(fetch)).toBe(0)
    await vi.advanceTimersByTimeAsync(100)
    expect(polls(fetch)).toBe(1)
    await vi.advanceTimersByTimeAsync(6_000)
    expect(polls(fetch)).toBe(3)
    expect(store.connect.phase).toBe('done')
    await vi.advanceTimersByTimeAsync(30_000)
    expect(polls(fetch)).toBe(3)
  })

  it('stops and says so when the account fails', async () => {
    const { store, fetch } = await signedIn(daemon(['pending_auth', 'error']))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    await vi.advanceTimersByTimeAsync(6_000)
    expect(store.connect.phase).toBe('failed')
    expect(store.connect.failure).toEqual({ op: 'wait-auth', code: 'flow_failed', account: { provider: 'gmail', email: 'suporte@example.test', state: 'error' } })
    await vi.advanceTimersByTimeAsync(30_000)
    expect(polls(fetch)).toBe(2)
  })

  it('says why the consent failed when the account records a reason the console knows', async () => {
    const { store } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'POST') return json({ account: account(), auth: loopback() }, 201)
      return json(account({ state: 'error', state_reason: REASON_NOT_GRANTED }))
    })
    const { describe: describeFailure } = await import('../src/ui/errors')
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    await vi.advanceTimersByTimeAsync(3_000)
    expect(store.connect.phase).toBe('failed')
    expect(store.connect.failure).toEqual({ op: 'wait-auth', code: 'flow_failed', account: { provider: 'gmail', email: 'suporte@example.test', state: 'error', state_reason: REASON_NOT_GRANTED } })
    expect(describeFailure(store.connect.failure!)).toBe('Google did not give Mailie access to this mailbox. Try again and, on Google’s screen, allow access to Gmail (tick the box if one is shown).')
  })

  it('says which account to choose when a re-authorization was refused by the mail server again', async () => {
    const existing = account({ provider: 'microsoft', email: 'ana.souza@example.test', state: 'needs_reauth', state_reason: REASON_TOKEN_REJECTED })
    let polled = 0
    const { store } = await signedIn(({ path }) => {
      if (path.endsWith('/oauth/start')) return json(loopback())
      // Pending while the person is at the provider, then a failed consent:
      // the new grant does not open the mailbox either.
      return json(++polled === 1 ? { ...existing, state: 'pending_auth', state_reason: undefined } : { ...existing, state: 'error', state_reason: REASON_MAILBOX_REFUSED })
    })
    const { describe: describeFailure } = await import('../src/ui/errors')
    await store.resumeAuthorization(existing)
    await vi.advanceTimersByTimeAsync(6_000)
    expect(store.connect.phase).toBe('failed')
    expect(describeFailure(store.connect.failure!)).toBe('The mail server refused this authorization. Try again and choose the account ana.souza@example.test on Microsoft’s sign-in screen. If you already did, check that IMAP is turned on for this mailbox.')
  })

  it('stops when the flow window closes', async () => {
    const { store, fetch } = await signedIn(daemon(['pending_auth'], { ...loopback(), expires_at: now() + 5 }))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    await vi.advanceTimersByTimeAsync(6_000)
    expect(store.connect.failure).toEqual({ op: 'wait-auth', code: 'flow_expired' })
    const asked = polls(fetch)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(polls(fetch)).toBe(asked)
  })

  it('stops the moment the person signs out, and forgets their accounts', async () => {
    const { session, store, fetch } = await signedIn(daemon(['pending_auth']))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    await vi.advanceTimersByTimeAsync(3_000)
    expect(polls(fetch)).toBe(1)
    await session.signOut()
    expect(store.connect.phase).toBe('idle')
    expect(store.accounts.list).toEqual([])
    await vi.advanceTimersByTimeAsync(60_000)
    expect(polls(fetch)).toBe(1)
  })

  it('waits as long as a rate limit asks before asking again', async () => {
    let asked = 0
    const { store, fetch } = await signedIn(request => {
      if (request.path === '/v1/accounts/acc_0000000000000001' && ++asked === 1) return failure('rate_limited', 429, { 'Retry-After': '10' })
      return daemon(['pending_auth'])(request)
    })
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    await vi.advanceTimersByTimeAsync(3_000)
    expect(polls(fetch)).toBe(1)
    await vi.advanceTimersByTimeAsync(9_000)
    expect(polls(fetch)).toBe(1)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(polls(fetch)).toBe(2)
    expect(store.connect.phase).toBe('waiting')
  })

  it('does not mistake an account that was already failing for a new failure', async () => {
    const failing = account({ state: 'error', state_reason: 'consent denied' })
    const { store, fetch } = await signedIn(({ path }) => {
      if (path.endsWith('/oauth/start')) return json(loopback())
      return json(polls(fetch) < 3 ? failing : account({ state: 'active' }))
    })
    await store.resumeAuthorization(failing)
    await vi.advanceTimersByTimeAsync(6_000)
    expect(store.connect.phase).toBe('waiting')
    await vi.advanceTimersByTimeAsync(3_000)
    expect(store.connect.phase).toBe('done')
  })
})

describe('an account removed while it was being authorized', () => {
  it('drops its card and does not offer to try again', async () => {
    const existing = account({ state: 'needs_reauth' })
    let polled = 0
    const { store, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([existing])
      if (path.endsWith('/oauth/start')) return json(loopback())
      // Removed in another browser after the first poll.
      if (path === '/v1/accounts/acc_0000000000000001' && ++polled === 1) return json(existing)
      return failure('not_found', 404)
    })
    await store.loadAccounts()
    await store.resumeAuthorization(existing)
    await vi.advanceTimersByTimeAsync(6_000)
    expect(store.connect.failure).toEqual({ op: 'wait-auth', code: 'not_found' })
    expect(store.accounts.list).toEqual([])
    expect(await store.retryConnect()).toBe(false)
    expect(starts(fetch)).toBe(1)
  })

  it('drops its card when consent cannot even start for it', async () => {
    const existing = account({ state: 'error' })
    const { store } = await signedIn(({ path, method }) => path === '/v1/accounts' && method === 'GET' ? json([existing]) : failure('not_found', 404))
    await store.loadAccounts()
    await store.resumeAuthorization(existing)
    expect(store.connect.failure).toEqual({ op: 'start-auth', code: 'not_found' })
    expect(store.accounts.list).toEqual([])
  })
})

describe('an add the server half made', () => {
  /** The server stored the pending row, then failed to start consent. */
  function halfMade(code: string, status: number) {
    return (({ path, method }) => {
      if (path === '/v1/accounts' && method === 'POST') return failure(code, status)
      if (path === '/v1/accounts' && method === 'GET') return json([account()])
      if (path.endsWith('/oauth/start')) return json(loopback())
      if (method === 'DELETE') return new Response(null, { status: 204 })
      return json(account())
    }) satisfies Route
  }

  it('finds the pending account left behind, so Try again resumes it and closing removes it', async () => {
    const { store, fetch } = await signedIn(halfMade('internal', 500))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'Suporte@Example.test ' })
    expect(store.connect.failure).toEqual({ op: 'add-account', code: 'internal' })
    expect(store.accounts.list.map(item => item.email)).toEqual(['suporte@example.test'])
    expect(await store.retryConnect()).toBe(true)
    expect(starts(fetch)).toBe(1)
    expect(store.connect.phase).toBe('waiting')
    store.cancelConnect()
    await vi.advanceTimersByTimeAsync(0)
    expect(deletes(fetch)).toEqual([expect.stringMatching(/\/v1\/accounts\/acc_0000000000000001\?confirm=acc_0000000000000001$/)])
  })

  it('shows an address that was already there, but never takes it over', async () => {
    const { store, fetch } = await signedIn(halfMade('conflict', 409))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(store.connect.failure).toEqual({ op: 'add-account', code: 'conflict' })
    expect(store.accounts.list).toHaveLength(1)
    expect(await store.retryConnect()).toBe(false)
    store.cancelConnect()
    await vi.advanceTimersByTimeAsync(0)
    expect(deletes(fetch)).toEqual([])
    expect(starts(fetch)).toBe(0)
  })

  it('does not look for a row when the request was refused before anything was stored', async () => {
    const { store, fetch } = await signedIn(halfMade('bad_request', 400))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(store.connect.failure).toEqual({ op: 'add-account', code: 'bad_request' })
    expect(fetch.mock.calls.some(([url, init]) => String(url).endsWith('/v1/accounts') && (init?.method ?? 'GET') === 'GET')).toBe(false)
    expect(store.accounts.list).toEqual([])
  })
})

describe('abandoning a connection', () => {
  it('removes the account the attempt created, and stops asking about it', async () => {
    const { store, fetch } = await signedIn(daemon(['pending_auth']))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    store.cancelConnect()
    await vi.advanceTimersByTimeAsync(0)
    const deletes = fetch.mock.calls.filter(([, init]) => init?.method === 'DELETE').map(([url]) => String(url))
    expect(deletes).toEqual([expect.stringMatching(/\/v1\/accounts\/acc_0000000000000001\?confirm=acc_0000000000000001$/)])
    expect(store.accounts.list).toEqual([])
    await vi.advanceTimersByTimeAsync(30_000)
    expect(polls(fetch)).toBe(0)
  })

  it('never removes an existing account whose re-authorization was abandoned', async () => {
    const existing = account({ state: 'needs_reauth' })
    const { store, fetch } = await signedIn(({ path }) => path.endsWith('/oauth/start') ? json(loopback()) : json(existing))
    await store.resumeAuthorization(existing)
    store.cancelConnect()
    await vi.advanceTimersByTimeAsync(0)
    expect(fetch.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(false)
  })
})

describe('leaving for the provider', () => {
  it('notes which mailbox it was for, then goes to the provider page', async () => {
    const web = { flow: 'web', auth_url: 'https://login.microsoftonline.com/common/oauth2/v2.0/authorize?state=x', state: 'x', expires_at: now() + 600 }
    const { store, navigate } = await signedIn(daemon(['pending_auth'], web))
    await store.connectOAuthAccount({ provider: 'microsoft', email: 'ana.souza@example.test' })
    expect(store.connect.phase).toBe('redirecting')
    expect(navigate).toHaveBeenCalledWith(web.auth_url)
    expect(JSON.parse(sessionStorage.getItem('mailie.oauthStart')!)).toMatchObject({ account_id: 'acc_0000000000000001', provider: 'microsoft', email: 'ana.souza@example.test' })
  })

  it.each(['javascript:alert(1)', 'http://accounts.google.com/', 'https://user:pass@evil.example/', 'not a url'])('never sends the page to %s', async auth_url => {
    const { store, navigate } = await signedIn(daemon(['pending_auth'], { flow: 'web', auth_url, state: 'x', expires_at: now() + 600 }))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(navigate).not.toHaveBeenCalled()
    expect(store.connect.failure).toEqual({ op: 'start-auth', code: 'flow_unsupported' })
  })
})

describe('flows the person finishes elsewhere', () => {
  it('shows a device code and its https page, then waits like any other flow', async () => {
    const device = { flow: 'device', user_code: 'ABCD-EFGH', verification_uri: 'https://microsoft.com/devicelogin', state: 'x', expires_at: now() + 900 }
    const { store, fetch } = await signedIn(daemon(['pending_auth', 'active'], device))
    await store.connectOAuthAccount({ provider: 'microsoft', email: 'ana.souza@example.test' })
    expect(store.connect).toMatchObject({ phase: 'waiting', flow: 'device', userCode: 'ABCD-EFGH', verificationURI: 'https://microsoft.com/devicelogin' })
    await vi.advanceTimersByTimeAsync(6_000)
    expect(polls(fetch)).toBe(2)
    expect(store.connect.phase).toBe('done')
  })

  it('refuses the pasted flow, which needs a terminal to paste into', async () => {
    const { store, fetch } = await signedIn(daemon(['pending_auth'], { flow: 'pasted', auth_url: 'https://accounts.google.com/o/oauth2/v2/auth', state: 'x', expires_at: now() + 600 }))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(store.connect.failure).toEqual({ op: 'start-auth', code: 'flow_unsupported' })
    await vi.advanceTimersByTimeAsync(30_000)
    expect(polls(fetch)).toBe(0)
  })
})

describe('a password account', () => {
  it('ends active, or with nothing stored and a reason', async () => {
    let refuse = true
    const { store } = await signedIn(({ path, body }) => {
      if (path !== '/v1/accounts') return failure('not_found', 404)
      if (refuse) return failure('bad_request', 400)
      return json({ account: account({ provider: 'imap', auth_kind: 'password', state: 'active', email: (body as { email: string }).email }) }, 201)
    })
    const body = { email: 'vendas@example.test', provider: 'imap' as const, password: 'app-password', imap_host: 'imap.example.test', imap_port: 993, smtp_host: 'smtp.example.test', smtp_port: 465, smtp_tls: 'implicit' as const }
    expect(await store.connectPasswordAccount(body)).toBe(false)
    expect(store.connect.failure).toEqual({ op: 'test-login', code: 'bad_request' })
    expect(store.accounts.list).toEqual([])
    refuse = false
    expect(await store.connectPasswordAccount(body)).toBe(true)
    expect(store.connect.phase).toBe('done')
    expect(store.accounts.list.map(item => item.state)).toEqual(['active'])
  })
})

describe('an iCloud account', () => {
  it('is sent as its address and password alone, and a refusal is described as iCloud’s', async () => {
    const sent: unknown[] = []
    let refuse = true
    const { store } = await signedIn(({ path, body }) => {
      if (path !== '/v1/accounts') return failure('not_found', 404)
      sent.push(body)
      if (refuse) return failure('bad_request', 400)
      return json({ account: account({ provider: 'icloud', auth_kind: 'password', state: 'active', email: 'ana@icloud.com', save_sent_copy: true }) }, 201)
    })
    const body = draftRequest('icloud', { ...emptyDraft(), email: 'ana@icloud.com', password: 'abcd-efgh-ijkl-mnop', imapHost: 'imap.icloud.com', smtpHost: 'smtp.icloud.com' })
    expect(await store.connectPasswordAccount(body)).toBe(false)
    expect(store.connect.failure).toEqual({ op: 'test-login-icloud', code: 'bad_request' })
    expect(store.accounts.list).toEqual([])
    // Nothing was stored, so Try again goes back to the form rather than resuming anything.
    expect(await store.retryConnect()).toBe(false)
    expect(store.connect.phase).toBe('idle')
    refuse = false
    expect(await store.connectPasswordAccount(body)).toBe(true)
    expect(sent).toEqual([
      { email: 'ana@icloud.com', provider: 'icloud', password: 'abcd-efgh-ijkl-mnop', public_key: expect.any(String), namespace: expect.any(String), grant: expect.any(String) },
      { email: 'ana@icloud.com', provider: 'icloud', password: 'abcd-efgh-ijkl-mnop', public_key: expect.any(String), namespace: expect.any(String), grant: expect.any(String) },
    ])
    expect(store.connect.phase).toBe('done')
    expect(store.accounts.list.map(item => item.provider)).toEqual(['icloud'])
  })
})

describe('listing an account’s folders', () => {
  it('asks for the account again before reporting a refused listing, so the sheet already knows it needs authorizing', async () => {
    const { store, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([account({ state: 'active' })])
      if (path.endsWith('/folders')) return failure('conflict', 409)
      return json(account({ state: 'needs_reauth', state_reason: REASON_TOKEN_REJECTED }))
    })
    await store.loadAccounts()
    const seen: Account['state'][] = []
    const stop = watch(() => store.accounts.folders['acc_0000000000000001']?.failure, found => { if (found) seen.push(store.accounts.list[0]!.state) }, { flush: 'sync' })
    await store.loadFolders('acc_0000000000000001')
    stop()
    expect(seen).toEqual(['needs_reauth'])
    expect(store.accounts.folders['acc_0000000000000001']).toMatchObject({ loading: false, failure: { op: 'folders', code: 'conflict' } })
    expect(store.accounts.list[0]).toMatchObject({ state: 'needs_reauth', state_reason: REASON_TOKEN_REJECTED })
    expect(polls(fetch)).toBe(1)
  })

  it('does not ask for the account again when the listing failed for another reason', async () => {
    const { store, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([account({ state: 'active' })])
      if (path.endsWith('/folders')) return failure('unavailable', 503)
      return json(account({ state: 'active' }))
    })
    await store.loadAccounts()
    await store.loadFolders('acc_0000000000000001')
    expect(store.accounts.folders['acc_0000000000000001']?.failure).toEqual({ op: 'folders', code: 'unavailable' })
    expect(polls(fetch)).toBe(0)
  })
})

describe('the accounts list', () => {
  it('drops a reply that arrives after the person signed out', async () => {
    let answer: ((response: Response) => void) | undefined
    const { session, store } = await signedIn(() => new Promise<Response>(resolve => { answer = resolve }))
    const loading = store.loadAccounts()
    // The list is asked for once the workspaces are known (none, on this server).
    await vi.waitFor(() => expect(answer).toBeTypeOf('function'))
    await session.signOut()
    answer!(json([account({ email: 'someone.else@example.test' })]))
    await loading
    expect(store.accounts.list).toEqual([])
    expect(store.accounts.loaded).toBe(false)
  })

  it('treats removing an account that is already gone as done', async () => {
    const { store } = await signedIn(({ method }) => method === 'DELETE' ? failure('not_found', 404) : json([account()]))
    await store.loadAccounts()
    expect(await store.removeAccount('acc_0000000000000001')).toBe(true)
    expect(store.accounts.list).toEqual([])
    expect(store.accounts.notice).toEqual({ kind: 'removed', email: 'suporte@example.test' })
  })

  it('says a removal through the live region, not only on screen', async () => {
    const { store } = await signedIn(({ method }) => method === 'DELETE' ? new Response(null, { status: 204 }) : json([account()]))
    const { announcement } = await import('../src/ui/announce')
    await store.loadAccounts()
    await store.removeAccount('acc_0000000000000001')
    await nextTick()
    expect(announcement.value).toBe('suporte@example.test was removed from Mailie.')
  })

  it('survives a password change, which replaces the token but not the person', async () => {
    const { session, store } = await signedIn(({ path }) => {
      if (path === '/v1/accounts') return json([account({ state: 'active' })])
      if (path === '/v1/providers') return json([{ id: 'gmail', oauth: true, password: false, flows: ['web'] }])
      return failure('not_found', 404)
    })
    await store.loadAccounts()
    await store.loadProviders()
    const seen: string[] = []
    const stop = watch(session.identity, value => { seen.push(value) }, { flush: 'sync' })
    await session.replaceSession(reply('tok_second_000000000000000000000000000000000'), false)
    stop()
    expect(seen).toEqual([])
    expect(store.accounts.loaded).toBe(true)
    expect(store.accounts.list).toHaveLength(1)
    expect(store.accounts.providersLoaded).toBe(true)
  })
})

describe('linking a mailbox with its first key', () => {
  const imap = { email: 'vendas@example.test', provider: 'imap' as const, password: 'app-password', imap_host: 'imap.example.test', imap_port: 993, smtp_host: 'smtp.example.test', smtp_port: 465, smtp_tls: 'implicit' as const }
  const posted = (fetch: ReturnType<typeof serve>) => fetch.mock.calls
    .filter(([url, init]) => String(url).endsWith('/v1/accounts') && init?.method === 'POST')
    .map(([, init]) => JSON.parse(String(init!.body)) as AddAccountRequest)

  it('sends the key with the form, a fresh pair and namespace on every attempt, sealed to the linker alone', async () => {
    const person = await linker()
    let refuse = true
    const { store, fetch } = await signedIn(({ path, body }) => {
      if (path !== '/v1/accounts') return failure('not_found', 404)
      if (refuse) return failure('bad_request', 400)
      return json({ account: account({ provider: 'imap', auth_kind: 'password', state: 'active', email: (body as { email: string }).email }) }, 201)
    }, reply(undefined, person.user))
    expect(await store.connectPasswordAccount(imap)).toBe(false)
    refuse = false
    expect(await store.connectPasswordAccount(imap)).toBe(true)
    const [first, second] = posted(fetch)
    for (const body of [first!, second!]) {
      expect(Object.keys(body).filter(key => !(key in imap)).sort()).toEqual(KEY_FIELDS)
      expect(await opensForLinker(body, person)).toHaveLength(32)
    }
    expect(second!.public_key).not.toBe(first!.public_key)
    expect(second!.namespace).not.toBe(first!.namespace)
    // Another person's account key opens none of it.
    await expect(opensForLinker(second!, await linker().then(other => ({ ...other, user: person.user })))).rejects.toThrow()
  })

  it('sends the key with an OAuth link before leaving for the provider, and none when an abandoned link is resumed', async () => {
    const person = await linker()
    const web = { flow: 'web', auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?state=x', state: 'x', expires_at: now() + 600 }
    const { store, fetch, navigate } = await signedIn(daemon(['pending_auth'], web), reply(undefined, person.user))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(navigate).toHaveBeenCalledWith(web.auth_url)
    const [body] = posted(fetch)
    expect(Object.keys(body!).sort()).toEqual(['email', ...KEY_FIELDS, 'provider'].sort())
    expect(await opensForLinker(body!, person)).toHaveLength(32)
    await store.resumeAuthorization(account())
    const resumed = fetch.mock.calls.filter(([url]) => String(url).endsWith('/oauth/start')).map(([, init]) => JSON.parse(String(init!.body)))
    expect(resumed).toEqual([{}])
  })

  it('links nothing for a person without an account key, saying so before any step-up or request', async () => {
    const { store, stepUp, fetch } = await signedIn(daemon(['pending_auth']), reply(undefined, { ...ana, public_key: undefined }))
    await store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    expect(store.connect.failure).toEqual({ op: 'add-account', code: 'not_enrolled' })
    expect(await store.connectPasswordAccount(imap)).toBe(false)
    expect(store.connect.failure).toEqual({ op: 'test-login', code: 'not_enrolled' })
    expect(stepUp.stepUpPrompt.open).toBe(false)
    expect(posted(fetch)).toEqual([])
    const { describe: describeFailure } = await import('../src/ui/errors')
    expect(describeFailure(store.connect.failure!)).toBe('Your account has no account key yet. Sign out and sign in again with your password to set it up, then try again.')
  })

  it('asks for the step-up first when the session’s has less than two minutes left, and sends nothing until it is given', async () => {
    const person = await linker()
    const { session, store, stepUp, fetch } = await signedIn(daemon(['active']), { ...reply(undefined, person.user), authenticated_at: now() - 9 * 60 })
    const linking = store.connectOAuthAccount({ provider: 'gmail', email: 'suporte@example.test' })
    await vi.waitFor(() => expect(stepUp.stepUpPrompt.open).toBe(true))
    expect(posted(fetch)).toEqual([])
    // The prompt's step-up went through.
    session.steppedUp(person.user.id, now())
    stepUp.stepUpGiven()
    await linking
    expect(posted(fetch)).toHaveLength(1)
    expect(store.connect.phase).toBe('waiting')
  })

  it('leaves the form as it was, and sends nothing, when the step-up is closed', async () => {
    const person = await linker()
    const { store, stepUp, fetch } = await signedIn(daemon(['active']), { ...reply(undefined, person.user), authenticated_at: 0 })
    const linking = store.connectPasswordAccount(imap)
    await vi.waitFor(() => expect(stepUp.stepUpPrompt.open).toBe(true))
    stepUp.stepUpRefused()
    expect(await linking).toBe(false)
    expect(store.connect.phase).toBe('idle')
    expect(store.connect.failure).toBeNull()
    expect(posted(fetch)).toEqual([])
  })

  it('asks again and sends a new key when the server refuses a link whose step-up went stale on the way', async () => {
    const person = await linker()
    let refused = 0
    const { session, store, stepUp, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'POST' && ++refused === 1) {
        // The login took long enough for the step-up to pass its ten minutes.
        session.session.authenticatedAt = now() - 11 * 60
        return failure('not_authorized', 403)
      }
      return json({ account: account({ provider: 'imap', auth_kind: 'password', state: 'active', email: 'vendas@example.test' }) }, 201)
    }, reply(undefined, person.user))
    const linking = store.connectPasswordAccount(imap)
    await vi.waitFor(() => expect(stepUp.stepUpPrompt.open).toBe(true))
    session.steppedUp(person.user.id, now())
    stepUp.stepUpGiven()
    expect(await linking).toBe(true)
    const [first, second] = posted(fetch)
    expect(second!.public_key).not.toBe(first!.public_key)
    expect(await opensForLinker(second!, person)).toHaveLength(32)
  })

  it('takes a refusal while the step-up is still fresh as the server’s answer, and asks for nothing', async () => {
    const person = await linker()
    const { store, stepUp, fetch } = await signedIn(() => failure('not_authorized', 403), reply(undefined, person.user))
    expect(await store.connectPasswordAccount(imap)).toBe(false)
    expect(store.connect.failure).toEqual({ op: 'test-login', code: 'not_authorized' })
    expect(stepUp.stepUpPrompt.open).toBe(false)
    expect(posted(fetch)).toHaveLength(1)
  })
})
