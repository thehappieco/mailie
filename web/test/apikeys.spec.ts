// API keys: listing a person's keys, creating one under the text the console
// shows, revoking one. What must never happen is as much the subject: a new
// key's secret kept anywhere but in the caller's hands, a key asked for past
// the limit, a changed text agreed to from here, a key drawn live after the
// server revoked it.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { CreatedKey, PersonalKey } from '../src/api/types'
import { isCreatedKey, isPersonalKey } from '../src/api/types'
import { KEY_TERMS_VERSION } from '../src/open/versions'
import { MAX_LIVE_KEYS, claudeCommand, keyMailboxes, keyStanding, mcpEndpoint } from '../src/ui/apikeys'
import { describe as say } from '../src/ui/errors'
import { account, failure, freshModules, json, now, reply, type Route, serve, settle, stubPage } from './support'

const PREFIX = '3f9a0c1d2e4b5a6c'
const SECRET = `${PREFIX}.Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXg`

function listed(prefix: string, fields: Partial<PersonalKey> = {}): PersonalKey {
  return { prefix, name: `Key ${prefix}`, scope: 'read', account_ids: [], created_at: now() - 86_400, expires_at: now() + 89 * 86_400, terms_version: KEY_TERMS_VERSION, ...fields }
}
function made(fields: Partial<PersonalKey> = {}): CreatedKey {
  return { ...listed(PREFIX, { name: 'Claude Code', created_at: now(), expires_at: now() + 30 * 86_400, ...fields }), key: SECRET }
}
/** n keys that still work, with prefixes of their own. */
const live = (n: number, from = 0) => Array.from({ length: n }, (_, i) => listed(`a${String(from + i).padStart(15, '0')}`))

// Fresh modules per test: the session and the keys store are module state,
// and some tests are about what happens when the person changes.
async function signedIn(route: Route) {
  await freshModules()
  const session = await import('../src/state/session')
  const keys = await import('../src/state/apikeys')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    return route(request)
  })
  await session.signIn('ana@example.test', 'correct-password')
  return { session, keys, fetch }
}

const calls = (fetch: ReturnType<typeof serve>, method: string, path = '/v1/me/apikeys') => fetch.mock.calls
  .filter(([url, init]) => new URL(String(url)).pathname === path && (init?.method ?? 'GET') === method)
  .map(([, init]) => ({ body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined, headers: init?.headers as Record<string, string> }))

let page: ReturnType<typeof stubPage>
beforeEach(() => { page = stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('a person’s API keys', () => {
  it('are listed with the session’s bearer and nothing in the URL', async () => {
    const { keys, fetch } = await signedIn(({ path }) => path === '/v1/me/apikeys' ? json([listed(PREFIX, { last_used_at: now() - 60 })]) : failure('not_found', 404))
    await keys.loadKeys()
    expect(keys.apiKeys.loaded).toBe(true)
    expect(keys.apiKeys.list.map(key => key.prefix)).toEqual([PREFIX])
    const [call] = calls(fetch, 'GET')
    expect(call!.headers.Authorization).toBe(`Bearer ${reply().token}`)
    expect(fetch.mock.calls.map(([url]) => new URL(String(url)).search).filter(Boolean)).toEqual([])
  })

  it('never keep a field the list should not have carried, a secret above all', async () => {
    const { keys } = await signedIn(() => json([{ ...listed(PREFIX, { restricted: false }), key: SECRET, hash: '$argon2id$…' }]))
    await keys.loadKeys()
    expect(Object.keys(keys.apiKeys.list[0]!).sort()).toEqual(['account_ids', 'created_at', 'expires_at', 'name', 'prefix', 'restricted', 'scope', 'terms_version'])
    expect(JSON.stringify(keys.apiKeys)).not.toContain(SECRET)
  })
})

describe('creating a key', () => {
  it('names the text the console shows, the scope, the mailboxes and the lifetime chosen, and hands the secret to the caller alone', async () => {
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/me/apikeys' && method === 'POST') return json(made({ scope: 'write', account_ids: ['acc_0000000000000001'] }), 201)
      return json([])
    })
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'Claude Code', scope: 'write', accountIDs: ['acc_0000000000000001'], lifetime: 30 })
    expect(calls(fetch, 'POST').map(call => call.body)).toEqual([{ name: 'Claude Code', scope: 'write', account_ids: ['acc_0000000000000001'], ttl_days: 30, terms_version: KEY_TERMS_VERSION }])
    expect('created' in outcome && outcome.created.key).toBe(SECRET)
    // The list has the key as it is listed; the secret is nowhere in the store.
    expect(keys.apiKeys.list.map(key => key.prefix)).toEqual([PREFIX])
    expect(keys.apiKeys.list[0]).not.toHaveProperty('key')
    expect(JSON.stringify(keys.apiKeys)).not.toContain(SECRET)
    // Nor in the page's storage, its address or its history.
    expect(sessionStorage.length).toBe(0)
    expect(location.href).not.toContain(PREFIX)
    expect(page.history.replaceState).not.toHaveBeenCalled()
  })

  it('asks for every mailbox of the person’s by naming none', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? json(made(), 201) : json([]))
    await keys.loadKeys()
    await keys.createKey({ name: 'Claude Code', scope: 'read', accountIDs: null, lifetime: 90 })
    expect(calls(fetch, 'POST').map(call => call.body)).toEqual([{ name: 'Claude Code', scope: 'read', ttl_days: 90, terms_version: KEY_TERMS_VERSION }])
  })

  it('asks nothing past the most keys a person may hold, and revoked or expired ones do not count', async () => {
    let list = live(MAX_LIVE_KEYS)
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? json(made(), 201) : json(list))
    await keys.loadKeys()
    expect(keys.atKeyLimit()).toBe(true)
    expect(await keys.createKey({ name: 'One more', scope: 'read', accountIDs: null, lifetime: 90 })).toEqual({ failure: { op: 'create-key', code: 'key_limit' } })
    expect(calls(fetch, 'POST')).toHaveLength(0)
    list = [...live(MAX_LIVE_KEYS - 1), listed('b000000000000001', { revoked_at: now() - 10 }), listed('b000000000000002', { expires_at: now() - 10 })]
    await keys.loadKeys()
    expect(keys.atKeyLimit()).toBe(false)
    expect('created' in await keys.createKey({ name: 'One more', scope: 'read', accountIDs: null, lifetime: 90 })).toBe(true)
    expect(calls(fetch, 'POST')).toHaveLength(1)
  })

  it('tells the limit from a changed text after a 409 by reading the list again', async () => {
    let list = live(3)
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? failure('conflict', 409) : json(list))
    await keys.loadKeys()
    // Another tab made keys up to the limit meanwhile.
    list = live(MAX_LIVE_KEYS)
    expect(await keys.createKey({ name: 'x', scope: 'read', accountIDs: null, lifetime: 90 })).toEqual({ failure: { op: 'create-key', code: 'key_limit' } })
    list = live(3)
    await keys.loadKeys()
    // Under the limit, a 409 is the server asking about a newer text than the one shown.
    expect(await keys.createKey({ name: 'x', scope: 'read', accountIDs: null, lifetime: 90 })).toEqual({ failure: { op: 'create-key', code: 'terms_changed' } })
    expect(calls(fetch, 'GET').length).toBe(4)
    expect(say({ op: 'create-key', code: 'terms_changed' })).toBe('The terms for API keys changed while this page was open. Reload the page to read the current text.')
  })

  it('reads the list again when the answer never came, since the key may have been made', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? failure('internal', 500) : json([]))
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'x', scope: 'read', accountIDs: null, lifetime: 90 })
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'internal' } })
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(say({ op: 'create-key', code: 'internal' })).toContain('If it is in the list, revoke it and create another')
  })

  it('refuses an answer whose secret is not under the prefix it is listed by, and reads the list again for the key it made', async () => {
    let list: PersonalKey[] = []
    const { keys, fetch } = await signedIn(({ method }) => {
      if (method !== 'POST') return json(list)
      list = [listed(PREFIX)]
      return json({ ...made(), key: `ffffffffffffffff.${SECRET.split('.')[1]}` }, 201)
    })
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'x', scope: 'read', accountIDs: null, lifetime: 90 })
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'invalid_response' } })
    // Nothing from the answer is kept; the list as the server has it now shows the key.
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(keys.apiKeys.list.map(key => key.prefix)).toEqual([PREFIX])
    expect(JSON.stringify(keys.apiKeys)).not.toContain(SECRET.split('.')[1])
  })

  it('reads the list again after a 2xx it cannot read, and says the key may exist rather than to try again', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST'
      ? new Response('<!doctype html><title>Gateway</title>', { status: 201, headers: { 'Content-Type': 'text/html' } })
      : json([]))
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'x', scope: 'read', accountIDs: null, lifetime: 90 })
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'invalid_response' } })
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(say({ op: 'create-key', code: 'invalid_response' })).toBe('Mailie could not confirm that the key was created. If it is in the list, revoke it and create another: its secret cannot be shown again.')
  })

  it('drops a key made for a person who is no longer signed in here', async () => {
    let answer: (response: Response) => void = () => {}
    const { keys, session } = await signedIn(({ method }) => method === 'POST' ? new Promise<Response>(resolve => { answer = resolve }) : json([]))
    await keys.loadKeys()
    const pending = keys.createKey({ name: 'x', scope: 'read', accountIDs: null, lifetime: 90 })
    await settle()
    await session.signOut()
    answer(json(made(), 201))
    const outcome = await pending
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'aborted' } })
    expect(keys.apiKeys.list).toEqual([])
  })
})

describe('revoking a key', () => {
  it('names the key by its prefix in the path, and draws it revoked once the server confirms', async () => {
    let revoked = 0
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === `/v1/me/apikeys/${PREFIX}` && method === 'DELETE') { revoked = now(); return new Response(null, { status: 204 }) }
      return json([listed(PREFIX, revoked ? { revoked_at: revoked } : {})])
    })
    await keys.loadKeys()
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('live')
    expect(await keys.revokeKey(PREFIX)).toBeNull()
    expect(calls(fetch, 'DELETE', `/v1/me/apikeys/${PREFIX}`)).toHaveLength(1)
    // Drawn revoked at once, then as the list says.
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('revoked')
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(keys.apiKeys.list[0]!.revoked_at).toBe(revoked)
  })

  it('reads the list again when the key is already gone, and says so', async () => {
    let list = [listed(PREFIX)]
    const { keys, fetch } = await signedIn(({ method }) => method === 'DELETE' ? failure('not_found', 404) : json(list))
    await keys.loadKeys()
    list = []
    const failed = await keys.revokeKey(PREFIX)
    expect(failed).toEqual({ op: 'revoke-key', code: 'not_found' })
    expect(say(failed!)).toBe('This key no longer exists.')
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(keys.apiKeys.list).toEqual([])
  })

  it('never calls a key revoked when the server did not confirm it', async () => {
    const { keys } = await signedIn(({ method }) => method === 'DELETE' ? failure('internal', 500) : json([listed(PREFIX)]))
    await keys.loadKeys()
    const failed = await keys.revokeKey(PREFIX)
    expect(say(failed!)).toBe('Could not confirm that the key was revoked. Try again: doing it twice is safe.')
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('live')
  })
})

it('forgets the keys when the person changes, and draws nothing a slow answer brings for the one before', async () => {
  let answer: (response: Response) => void = () => {}
  let slow = false
  const { keys, session } = await signedIn(() => slow ? new Promise<Response>(resolve => { answer = resolve }) : json([listed(PREFIX)]))
  await keys.loadKeys()
  expect(keys.apiKeys.list).toHaveLength(1)
  slow = true
  const pending = keys.loadKeys()
  await settle()
  await session.signOut()
  expect(keys.apiKeys.list).toEqual([])
  expect(keys.apiKeys.loaded).toBe(false)
  answer(json([listed(PREFIX)]))
  await pending
  expect(keys.apiKeys.list).toEqual([])
})

describe('what the console says about keys', () => {
  it('names the MCP server at the page’s own origin, which serves the console, the API and /mcp', () => {
    expect(mcpEndpoint('http://localhost:5174')).toBe('http://localhost:5174/mcp')
    expect(mcpEndpoint('https://mail.example.org')).toBe('https://mail.example.org/mcp')
    expect(mcpEndpoint('https://mail.example.org:8443/oauth/return?x=1')).toBe('https://mail.example.org:8443/mcp')
  })

  it('writes the Claude Code command with the key in the Authorization header only', () => {
    expect(claudeCommand('https://mail.example.org/mcp', '<your key>'))
      .toBe('claude mcp add --transport http mailie https://mail.example.org/mcp --header "Authorization: Bearer <your key>"')
  })

  it('calls a key live until it is revoked or reaches its expiry', () => {
    const at = Date.UTC(2026, 8, 27) / 1000
    expect(keyStanding({ expires_at: at + 1 }, at * 1000)).toBe('live')
    expect(keyStanding({ expires_at: at }, at * 1000)).toBe('expired')
    expect(keyStanding({ expires_at: at + 100, revoked_at: at - 5 }, at * 1000)).toBe('revoked')
  })

  it('names the mailboxes a key reaches by address, all of them when it names none, and one removed since as removed', () => {
    const accounts = [{ id: 'acc_1', email: 'suporte@example.test' }]
    expect(keyMailboxes({}, accounts)).toEqual(['All your mailboxes'])
    expect(keyMailboxes({ account_ids: [] }, accounts)).toEqual(['All your mailboxes'])
    expect(keyMailboxes({ account_ids: [], restricted: false }, accounts)).toEqual(['All your mailboxes'])
    expect(keyMailboxes({ account_ids: ['acc_1', 'acc_gone'] }, accounts)).toEqual(['suporte@example.test', 'A removed mailbox'])
  })

  it('never says a key made for chosen mailboxes reached all of them once those are removed', () => {
    const accounts = [{ id: 'acc_1', email: 'suporte@example.test' }]
    // Removing the mailbox took it out of the key's list, and revoked the key.
    expect(keyMailboxes({ account_ids: [], restricted: true }, accounts)).toEqual(['Only mailboxes removed since'])
    expect(keyMailboxes({ restricted: true }, accounts)).toEqual(['Only mailboxes removed since'])
    expect(keyMailboxes({ account_ids: ['acc_1'], restricted: true }, accounts)).toEqual(['suporte@example.test'])
  })

  it('reads the keys again when a mailbox one of them names is removed, and draws the key the server revoked with it', async () => {
    let removed = false
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json(removed ? [account()] : [account(), account({ id: 'acc_0000000000000002', email: 'loja@example.test' })])
      if (path === '/v1/accounts/acc_0000000000000002' && method === 'DELETE') { removed = true; return new Response(null, { status: 204 }) }
      if (path === '/v1/me/apikeys') return json([removed
        ? listed(PREFIX, { name: 'Script', account_ids: [], restricted: true, revoked_at: now() })
        : listed(PREFIX, { name: 'Script', account_ids: ['acc_0000000000000002'], restricted: true })])
      return failure('not_found', 404)
    })
    const mailboxes = await import('../src/state/accounts')
    await mailboxes.loadAccounts()
    await keys.loadKeys()
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('live')
    expect(keyMailboxes(keys.apiKeys.list[0]!, mailboxes.accounts.list)).toEqual(['loja@example.test'])
    expect(await mailboxes.removeAccount('acc_0000000000000002')).toBe(true)
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('revoked')
    expect(keyMailboxes(keys.apiKeys.list[0]!, mailboxes.accounts.list)).toEqual(['Only mailboxes removed since'])
  })

  it('leaves the keys alone when a mailbox none of them names is removed', async () => {
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([account(), account({ id: 'acc_0000000000000002', email: 'loja@example.test' })])
      if (path === '/v1/accounts/acc_0000000000000002' && method === 'DELETE') return new Response(null, { status: 204 })
      if (path === '/v1/me/apikeys') return json([listed(PREFIX, { account_ids: ['acc_0000000000000001'], restricted: true })])
      return failure('not_found', 404)
    })
    const mailboxes = await import('../src/state/accounts')
    await mailboxes.loadAccounts()
    await keys.loadKeys()
    await mailboxes.removeAccount('acc_0000000000000002')
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(1)
  })

  it('reads a key the running console can draw, and holds the contract to read and write', () => {
    expect(isPersonalKey({ ...listed(PREFIX), scope: 'send' })).toBe(true)
    expect(isPersonalKey({ ...listed(PREFIX), scope: 'send' }, true)).toBe(false)
    expect(isPersonalKey({ ...listed(PREFIX), prefix: '../x' })).toBe(false)
    expect(isCreatedKey(made(), true)).toBe(true)
    // The daemon always names the mailboxes, [] for all of them; the running console also reads an answer without.
    const { account_ids: _, ...unnamed } = made()
    expect(isCreatedKey(unnamed, true)).toBe(false)
    expect(isCreatedKey(unnamed)).toBe(true)
    expect(isCreatedKey({ ...made(), key: `${PREFIX}.` })).toBe(false)
    expect(isCreatedKey({ ...made(), key: `${PREFIX}.a b` })).toBe(false)
    // Whether a key was made for chosen mailboxes: a flag, when the daemon says.
    expect(isPersonalKey({ ...listed(PREFIX), restricted: true }, true)).toBe(true)
    expect(isPersonalKey({ ...listed(PREFIX), restricted: 'yes' })).toBe(false)
  })
})
