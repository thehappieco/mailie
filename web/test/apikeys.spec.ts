// API keys: a workspace's, which its owners and admins (in a personal
// workspace, its person) list, create under the text the console shows,
// give mailboxes to and revoke, and the ones a person created, which they
// list and revoke from their account. What must never happen is as much the
// subject: a new key's secret kept anywhere but in the caller's hands, a key
// asked for past the limit, a changed text agreed to from here, a key drawn
// live after the server revoked it, a member of a team asking for its keys,
// a key offered Read on a mailbox its giver does not read.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { CreatedKey, KeyMailbox, Workspace, WorkspaceKey } from '../src/api/types'
import { isCreatedKey, isWorkspaceKey } from '../src/api/types'
import { KEY_TERMS_VERSION } from '../src/open/versions'
import {
  MAX_LIVE_KEYS, claudeCommand, keyAccessRules, keyFlagEditable, keyOriginNote, keyPerson, keyStanding, mcpEndpoint, scopeLabel, toggleKeyFlag,
  type KeyFacts,
} from '../src/ui/apikeys'
import { describe as say } from '../src/ui/errors'
import { account, failure, freshModules, json, now, reply, type Route, serve, settle, stubPage } from './support'

const PREFIX = '3f9a0c1d2e4b5a6c'
const SECRET = `${PREFIX}.Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXg`
const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const ONE = 'acc_0000000000000001'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support = (role: string): Workspace => ({ id: TEAM, kind: 'team', source: 'local', name: 'Support', role, status: 'active', created_at: 1_790_000_000 })
const keysPath = (workspace = PERSONAL) => `/v1/workspaces/${workspace}/apikeys`

const held = (accountID = ONE, fields: Partial<KeyMailbox> = {}): KeyMailbox => ({ account_id: accountID, workspace_id: PERSONAL, read: true, act: false, send: false, granted_by: 'usr_00000000000000a1', updated_at: now() - 60, ...fields })
function listed(prefix: string, fields: Partial<WorkspaceKey> = {}): WorkspaceKey {
  return { prefix, name: `Key ${prefix}`, scope: 'read', workspace_id: PERSONAL, mailboxes: [held()], created_by: 'usr_00000000000000a1', created_at: now() - 86_400, expires_at: now() + 89 * 86_400, live: true, terms_version: KEY_TERMS_VERSION, sends: false, ...fields }
}
function made(fields: Partial<WorkspaceKey> = {}): CreatedKey {
  return { ...listed(PREFIX, { name: 'Claude Code', created_at: now(), expires_at: now() + 30 * 86_400, ...fields }), key: SECRET }
}
/** n keys that still work, with prefixes of their own. */
const live = (n: number, from = 0) => Array.from({ length: n }, (_, i) => listed(`a${String(from + i).padStart(15, '0')}`))

// Fresh modules per test: the session and the keys store are module state,
// and some tests are about what happens when the person changes.
async function signedIn(route: Route, list: Workspace[] = [personal], shown = PERSONAL) {
  await freshModules()
  const session = await import('../src/state/session')
  const keys = await import('../src/state/apikeys')
  const workspaces = await import('../src/state/workspaces')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (request.path === '/v1/workspaces' && request.method === 'GET') return json(list)
    return route(request)
  })
  await session.adoptSession(reply())
  await workspaces.loadWorkspaces()
  workspaces.selectWorkspace(shown)
  return { session, keys, workspaces, fetch }
}

const calls = (fetch: ReturnType<typeof serve>, method: string, path = keysPath()) => fetch.mock.calls
  .filter(([url, init]) => new URL(String(url)).pathname === path && (init?.method ?? 'GET') === method)
  .map(([url, init]) => ({ body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined, headers: init?.headers as Record<string, string>, search: new URL(String(url)).search }))

let page: ReturnType<typeof stubPage>
beforeEach(() => { page = stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('a workspace’s API keys', () => {
  it('are listed for the workspace shown, with the session’s bearer and nothing in the URL', async () => {
    const { keys, fetch } = await signedIn(({ path }) => path === keysPath() ? json([listed(PREFIX, { last_used_at: now() - 60 })]) : failure('not_found', 404))
    await keys.loadKeys()
    expect(keys.apiKeys.loaded).toBe(true)
    expect(keys.apiKeys.list.map(key => key.prefix)).toEqual([PREFIX])
    const [call] = calls(fetch, 'GET')
    expect(call!.headers.Authorization).toBe(`Bearer ${reply().token}`)
    expect(call!.search).toBe('')
  })

  it('never keep a field the list should not have carried, a secret above all', async () => {
    const { keys } = await signedIn(() => json([{ ...listed(PREFIX), key: SECRET, hash: '$argon2id$…', mailboxes: [{ ...held(), secret: 'x' }] }]))
    await keys.loadKeys()
    expect(Object.keys(keys.apiKeys.list[0]!).sort()).toEqual(['created_at', 'created_by', 'expires_at', 'live', 'mailboxes', 'name', 'prefix', 'scope', 'sends', 'terms_version', 'workspace_id'])
    expect(Object.keys(keys.apiKeys.list[0]!.mailboxes[0]!).sort()).toEqual(['account_id', 'act', 'granted_by', 'read', 'send', 'updated_at', 'workspace_id'])
    expect(JSON.stringify(keys.apiKeys)).not.toContain(SECRET)
  })

  it('are never asked for by a member of a team, and are by its owners and admins', async () => {
    for (const [role, asked] of [['member', 0], ['admin', 1], ['owner', 1]] as const) {
      const { keys, fetch } = await signedIn(() => json([]), [personal, support(role)], TEAM)
      await keys.loadKeys()
      expect(calls(fetch, 'GET', keysPath(TEAM)), role).toHaveLength(asked)
      expect(keys.apiKeys.loaded, role).toBe(asked === 1)
      vi.restoreAllMocks()
    }
  })

  it('are forgotten when another workspace is shown, and an answer for the one before never lands', async () => {
    let answer: (response: Response) => void = () => {}
    const { keys, workspaces } = await signedIn(({ path }) => path === keysPath(TEAM)
      ? new Promise<Response>(resolve => { answer = resolve })
      : json([listed(PREFIX)]), [personal, support('owner')], PERSONAL)
    await keys.loadKeys()
    expect(keys.apiKeys.list).toHaveLength(1)
    workspaces.selectWorkspace(TEAM)
    expect(keys.apiKeys.loaded).toBe(false)
    const pending = keys.loadKeys()
    await settle()
    workspaces.selectWorkspace(PERSONAL)
    answer(json([listed('b000000000000001', { workspace_id: TEAM, mailboxes: [] })]))
    await pending
    expect(keys.apiKeys.list).toEqual([])
    expect(keys.apiKeys.workspace).toBe('')
  })
})

describe('creating a key', () => {
  it('names the text the console shows, the scope, what it holds on each mailbox and the lifetime chosen, and hands the secret to the caller alone', async () => {
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === keysPath() && method === 'POST') return json(made({ scope: 'write', mailboxes: [held(ONE, { act: true })] }), 201)
      return json([])
    })
    await keys.loadKeys()
    const outcome = await keys.createKey({
      name: 'Claude Code', scope: 'write', lifetime: 30,
      mailboxes: [{ account_id: ONE, read: true, act: true, send: false }, { account_id: 'acc_0000000000000002', read: false, act: false, send: false }],
    })
    // A mailbox given nothing is not asked for.
    expect(calls(fetch, 'POST').map(call => call.body)).toEqual([{ name: 'Claude Code', scope: 'write', ttl_days: 30, terms_version: KEY_TERMS_VERSION, mailboxes: [{ account_id: ONE, read: true, act: true, send: false }] }])
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

  it('asks for a key that reaches nothing yet by naming no mailbox', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? json(made({ mailboxes: [] }), 201) : json([]))
    await keys.loadKeys()
    await keys.createKey({ name: 'Claude Code', scope: 'read', mailboxes: [], lifetime: 90 })
    expect(calls(fetch, 'POST').map(call => call.body)).toEqual([{ name: 'Claude Code', scope: 'read', ttl_days: 90, terms_version: KEY_TERMS_VERSION }])
  })

  it('asks nothing past the most keys a workspace may hold, and revoked, expired, carried-over or moved-in ones do not count', async () => {
    let list = live(MAX_LIVE_KEYS)
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? json(made(), 201) : json(list))
    await keys.loadKeys()
    expect(keys.atKeyLimit()).toBe(true)
    expect(await keys.createKey({ name: 'One more', scope: 'read', mailboxes: [], lifetime: 90 })).toEqual({ failure: { op: 'create-key', code: 'key_limit' } })
    expect(calls(fetch, 'POST')).toHaveLength(0)
    const { workspace_id: _, ...carried } = listed('c000000000000001')
    // The persons' keys the upgrade moved into the workspace (origin) may be more than the limit, and do not count either.
    const moved = (prefix: string, origin: 'person' | 'person-all') => listed(prefix, { origin })
    list = [...live(MAX_LIVE_KEYS - 1), listed('b000000000000001', { revoked_at: now() - 10, live: false }), listed('b000000000000002', { expires_at: now() - 10, live: false }), { ...carried, carried_over: true, other_workspaces: 1 }, moved('d000000000000001', 'person'), moved('d000000000000002', 'person-all')]
    await keys.loadKeys()
    expect(keys.atKeyLimit()).toBe(false)
    expect('created' in await keys.createKey({ name: 'One more', scope: 'read', mailboxes: [], lifetime: 90 })).toBe(true)
    expect(calls(fetch, 'POST')).toHaveLength(1)
  })

  it('tells the limit from a changed text after a 409 by reading the list again', async () => {
    let list = live(3)
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? failure('conflict', 409) : json(list))
    await keys.loadKeys()
    // Another tab made keys up to the limit meanwhile.
    list = live(MAX_LIVE_KEYS)
    expect(await keys.createKey({ name: 'x', scope: 'read', mailboxes: [], lifetime: 90 })).toEqual({ failure: { op: 'create-key', code: 'key_limit' } })
    list = live(3)
    await keys.loadKeys()
    // Under the limit, a 409 is the server asking about a newer text than the one shown.
    expect(await keys.createKey({ name: 'x', scope: 'read', mailboxes: [], lifetime: 90 })).toEqual({ failure: { op: 'create-key', code: 'terms_changed' } })
    expect(calls(fetch, 'GET').length).toBe(4)
    expect(say({ op: 'create-key', code: 'terms_changed' })).toBe('The terms for API keys changed while this page was open. Reload the page to read the current text.')
    expect(say({ op: 'create-key', code: 'key_limit' })).toBe('This workspace has 20 active keys, the most it can have. Revoke one to create another.')
  })

  it('says, in the console’s words, why the server refused a key', async () => {
    const { keys } = await signedIn(({ method }) => method === 'POST' ? failure('not_authorized', 403) : json([]))
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'x', scope: 'send', mailboxes: [{ account_id: ONE, read: true, act: false, send: true }], lifetime: 90 })
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'not_authorized' } })
    expect(say({ op: 'create-key', code: 'not_authorized' })).toBe('The server refused this key: only owners and admins create keys, Read is given only on a mailbox you read yourself, and Send only where this server’s keys may send.')
    expect(say({ op: 'create-key', code: 'not_found' })).toBe('A chosen mailbox is no longer in this workspace. Choose again.')
  })

  it('reads the list again when the answer never came, since the key may have been made', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'POST' ? failure('internal', 500) : json([]))
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'x', scope: 'read', mailboxes: [], lifetime: 90 })
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'internal' } })
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(say({ op: 'create-key', code: 'internal' })).toContain('If it is in the list, revoke it and create another')
  })

  it('refuses an answer whose secret is not under the prefix it is listed by, and reads the list again for the key it made', async () => {
    let list: WorkspaceKey[] = []
    const { keys, fetch } = await signedIn(({ method }) => {
      if (method !== 'POST') return json(list)
      list = [listed(PREFIX)]
      return json({ ...made(), key: `ffffffffffffffff.${SECRET.split('.')[1]}` }, 201)
    })
    await keys.loadKeys()
    const outcome = await keys.createKey({ name: 'x', scope: 'read', mailboxes: [], lifetime: 90 })
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
    const outcome = await keys.createKey({ name: 'x', scope: 'read', mailboxes: [], lifetime: 90 })
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'invalid_response' } })
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(say({ op: 'create-key', code: 'invalid_response' })).toBe('Mailie could not confirm that the key was created. If it is in the list, revoke it and create another: its secret cannot be shown again.')
  })

  it('drops a key made for a person who is no longer signed in here', async () => {
    let answer: (response: Response) => void = () => {}
    const { keys, session } = await signedIn(({ method }) => method === 'POST' ? new Promise<Response>(resolve => { answer = resolve }) : json([]))
    await keys.loadKeys()
    const pending = keys.createKey({ name: 'x', scope: 'read', mailboxes: [], lifetime: 90 })
    await settle()
    await session.signOut()
    answer(json(made(), 201))
    const outcome = await pending
    expect(outcome).toEqual({ failure: { op: 'create-key', code: 'aborted' } })
    expect(keys.apiKeys.list).toEqual([])
  })
})

describe('revoking a key', () => {
  it('names the key by its prefix in the workspace’s path, and draws it revoked once the server confirms', async () => {
    let revoked = 0
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === `${keysPath()}/${PREFIX}` && method === 'DELETE') { revoked = now(); return new Response(null, { status: 204 }) }
      return json([listed(PREFIX, revoked ? { revoked_at: revoked, live: false } : {})])
    })
    await keys.loadKeys()
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('live')
    expect(await keys.revokeKey(PREFIX)).toBeNull()
    expect(calls(fetch, 'DELETE', `${keysPath()}/${PREFIX}`)).toHaveLength(1)
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

  it('reads the person’s workspaces again when the server says their role no longer lets them', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'DELETE' ? failure('not_authorized', 403) : json([listed(PREFIX, { workspace_id: TEAM, mailboxes: [] })]), [personal, support('admin')], TEAM)
    await keys.loadKeys()
    const before = calls(fetch, 'GET', '/v1/workspaces').length
    expect(await keys.revokeKey(PREFIX)).toEqual({ op: 'revoke-key', code: 'not_authorized' })
    await settle()
    expect(calls(fetch, 'GET', '/v1/workspaces').length).toBe(before + 1)
  })
})

describe('what a key holds on a mailbox', () => {
  it('is set to exactly the flags ticked, and the key’s row takes the answer', async () => {
    const answer = held(ONE, { act: true, updated_at: now() })
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === `${keysPath()}/${PREFIX}/accounts/${ONE}` && method === 'PUT') return json(answer)
      return json([listed(PREFIX, { scope: 'write' })])
    })
    await keys.loadKeys()
    expect(await keys.setKeyMailbox(PREFIX, ONE, { read: true, act: false, send: false }, { read: true, act: true, send: false })).toBeNull()
    expect(calls(fetch, 'PUT', `${keysPath()}/${PREFIX}/accounts/${ONE}`).map(call => call.body)).toEqual([{ read: true, act: true, send: false }])
    expect(keys.apiKeys.list[0]!.mailboxes).toEqual([answer])
  })

  it('takes the mailbox out of the key when nothing is left ticked', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'DELETE' ? new Response(null, { status: 204 }) : json([listed(PREFIX)]))
    await keys.loadKeys()
    expect(await keys.setKeyMailbox(PREFIX, ONE, { read: true, act: false, send: false }, { read: false, act: false, send: false })).toBeNull()
    expect(calls(fetch, 'DELETE', `${keysPath()}/${PREFIX}/accounts/${ONE}`)).toHaveLength(1)
    expect(calls(fetch, 'PUT', `${keysPath()}/${PREFIX}/accounts/${ONE}`)).toHaveLength(0)
    expect(keys.apiKeys.list[0]!.mailboxes).toEqual([])
  })

  it('asks nothing when nothing changed, and says a refusal in the console’s words, reading the keys again', async () => {
    const { keys, fetch } = await signedIn(({ method }) => method === 'PUT' ? failure('conflict', 409) : json([listed(PREFIX)]))
    await keys.loadKeys()
    expect(await keys.setKeyMailbox(PREFIX, ONE, { read: true, act: false, send: false }, { read: true, act: false, send: false })).toBeNull()
    expect(fetch.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0)
    const failed = await keys.setKeyMailbox(PREFIX, ONE, { read: true, act: false, send: false }, { read: true, act: false, send: true })
    expect(say(failed!)).toBe('This key no longer works: it was revoked, or it expired.')
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(say({ op: 'change-key-access', code: 'not_authorized' })).toBe('The server refused this: you can give a key Read only on a mailbox you read yourself, and Send only where this server’s keys may send.')
  })

  it('reads the keys again when a mailbox one of them holds is removed', async () => {
    let removed = false
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json(removed ? [account()] : [account(), account({ id: 'acc_0000000000000002', email: 'loja@example.test' })])
      if (path === '/v1/accounts/acc_0000000000000002' && method === 'DELETE') { removed = true; return new Response(null, { status: 204 }) }
      if (path === keysPath()) return json([listed(PREFIX, { mailboxes: removed ? [] : [held('acc_0000000000000002')] })])
      return failure('not_found', 404)
    })
    const mailboxes = await import('../src/state/accounts')
    await mailboxes.loadAccounts()
    await keys.loadKeys()
    expect(await mailboxes.removeAccount('acc_0000000000000002')).toBe(true)
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(keys.apiKeys.list[0]!.mailboxes).toEqual([])
  })

  it('leaves the keys alone when a mailbox none of them holds is removed', async () => {
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === '/v1/accounts' && method === 'GET') return json([account(), account({ id: 'acc_0000000000000002', email: 'loja@example.test' })])
      if (path === '/v1/accounts/acc_0000000000000002' && method === 'DELETE') return new Response(null, { status: 204 })
      if (path === keysPath()) return json([listed(PREFIX)])
      return failure('not_found', 404)
    })
    const mailboxes = await import('../src/state/accounts')
    await mailboxes.loadAccounts()
    await keys.loadKeys()
    await mailboxes.removeAccount('acc_0000000000000002')
    await settle()
    expect(calls(fetch, 'GET')).toHaveLength(1)
  })

  it('reads a key’s sends from the workspace’s path', async () => {
    const send = { account_id: ONE, idempotency_key: 'k1', state: 'unknown', message_id: 'm@example.test', attempts: 1, recipients: 2, sent_copy: 'n/a', created_at: now(), updated_at: now() }
    const { keys, fetch } = await signedIn(({ path }) => path === `${keysPath()}/${PREFIX}/sends` ? json([send]) : json([]))
    expect(await keys.loadKeySends(PREFIX)).toEqual({ list: [send] })
    expect(calls(fetch, 'GET', `${keysPath()}/${PREFIX}/sends`)).toHaveLength(1)
  })
})

describe('the keys a person created', () => {
  it('are listed from every workspace, and revoked by their prefix, read again where else they show', async () => {
    let revoked = 0
    const { keys, fetch } = await signedIn(({ path, method }) => {
      if (path === `/v1/me/apikeys/${PREFIX}` && method === 'DELETE') { revoked = now(); return new Response(null, { status: 204 }) }
      const key = listed(PREFIX, revoked ? { revoked_at: revoked, live: false } : {})
      if (path === '/v1/me/apikeys') return json([key, listed('b000000000000001', { workspace_id: TEAM, mailboxes: [] })])
      if (path === keysPath()) return json([key])
      return failure('not_found', 404)
    })
    await keys.loadMyKeys()
    await keys.loadKeys()
    expect(keys.myKeys.list.map(key => key.workspace_id)).toEqual([PERSONAL, TEAM])
    expect(await keys.revokeMyKey(PREFIX)).toBeNull()
    expect(calls(fetch, 'DELETE', `/v1/me/apikeys/${PREFIX}`)).toHaveLength(1)
    expect(keyStanding(keys.myKeys.list[0]!)).toBe('revoked')
    await settle()
    expect(calls(fetch, 'GET', '/v1/me/apikeys')).toHaveLength(2)
    expect(calls(fetch, 'GET')).toHaveLength(2)
    expect(keyStanding(keys.apiKeys.list[0]!)).toBe('revoked')
  })

  it('are forgotten when the person changes, and a slow answer for the one before draws nothing', async () => {
    let answer: (response: Response) => void = () => {}
    let slow = false
    const { keys, session } = await signedIn(() => slow ? new Promise<Response>(resolve => { answer = resolve }) : json([listed(PREFIX)]))
    await keys.loadMyKeys()
    await keys.loadKeys()
    expect(keys.myKeys.list).toHaveLength(1)
    slow = true
    const pending = keys.loadMyKeys()
    await settle()
    await session.signOut()
    expect(keys.myKeys.list).toEqual([])
    expect(keys.apiKeys.list).toEqual([])
    expect(keys.apiKeys.loaded).toBe(false)
    answer(json([listed(PREFIX)]))
    await pending
    expect(keys.myKeys.list).toEqual([])
  })
})

describe('what an owner or an admin may give a key', () => {
  const key = (fields: Partial<KeyFacts> = {}): KeyFacts => ({ scope: 'send', sends: true, live: true, ...fields })

  it('gives Read only on a mailbox the giver reads, whatever their role', () => {
    expect(keyAccessRules({ key: key(), viewerReads: true, keysSend: true }).canAdd.read).toBe(true)
    expect(keyAccessRules({ key: key(), viewerReads: false, keysSend: true }).canAdd.read).toBe(false)
  })

  it('gives Act only to a key whose scope acts, and Send only to one that sends on a server whose keys may send', () => {
    expect(keyAccessRules({ key: key({ scope: 'read', sends: false }), viewerReads: true, keysSend: true }).canAdd).toEqual({ read: true, act: false, send: false })
    expect(keyAccessRules({ key: key({ scope: 'write', sends: false }), viewerReads: true, keysSend: true }).canAdd).toEqual({ read: true, act: true, send: false })
    expect(keyAccessRules({ key: key(), viewerReads: false, keysSend: true }).canAdd).toEqual({ read: false, act: true, send: true })
    expect(keyAccessRules({ key: key(), viewerReads: true, keysSend: false }).canAdd.send).toBe(false)
    // A key of the send scope made under terms that never sent.
    expect(keyAccessRules({ key: key({ sends: false }), viewerReads: true, keysSend: true }).canAdd.send).toBe(false)
  })

  it('gives a key carried over from before nothing, and lets anything be taken from it', () => {
    const rules = keyAccessRules({ key: key({ carried_over: true }), viewerReads: true, keysSend: true })
    expect(rules.canAdd).toEqual({ read: false, act: false, send: false })
    expect(rules.canRemove).toEqual({ read: true, act: true, send: true })
  })

  it('changes nothing on a key that no longer works', () => {
    const rules = keyAccessRules({ key: key({ live: false }), viewerReads: true, keysSend: true })
    expect(rules.canAdd).toEqual({ read: false, act: false, send: false })
    expect(rules.canRemove).toEqual({ read: false, act: false, send: false })
  })

  it('offers Act only where the Read it needs is there or the giver’s to give, and ticks them together', () => {
    const none = { read: false, act: false, send: false }
    const reader = keyAccessRules({ key: key(), viewerReads: true, keysSend: true })
    const stranger = keyAccessRules({ key: key(), viewerReads: false, keysSend: true })
    expect(keyFlagEditable(reader, none, none, 'act')).toBe(true)
    expect(keyFlagEditable(stranger, none, none, 'act')).toBe(false)
    expect(keyFlagEditable(stranger, { ...none, read: true }, { ...none, read: true }, 'act')).toBe(true)
    // Setting back a change is always possible.
    expect(keyFlagEditable(stranger, none, { ...none, send: true }, 'send')).toBe(true)
    expect(toggleKeyFlag(none, 'act', true)).toEqual({ read: true, act: true, send: false })
    expect(toggleKeyFlag({ read: true, act: true, send: true }, 'read', false)).toEqual({ read: false, act: false, send: true })
  })
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

  it('names each scope, and one a newer server issues by its identifier', () => {
    expect(['read', 'write', 'send', 'admin'].map(scopeLabel)).toEqual(['Read', 'Read and act', 'Read, act and send', 'admin'])
  })

  it('names who a key’s record names: you, a member, someone gone, or the upgrade', () => {
    const name = (id: string) => id === 'usr_b' ? 'Bea' : ''
    expect(keyPerson('usr_a', 'usr_a', name)).toBe('You')
    expect(keyPerson('usr_b', 'usr_a', name)).toBe('Bea')
    expect(keyPerson('usr_c', 'usr_a', name)).toBe('someone no longer in the team')
    expect(keyPerson('migration', 'usr_a', name)).toBe('the upgrade to workspace keys')
    expect(keyPerson(undefined, 'usr_a', name)).toBe('')
  })

  it('says what a key made before keys belonged to workspaces does differently, and nothing for one made now', () => {
    expect(keyOriginNote({})).toBe('')
    expect(keyOriginNote({ carried_over: true, other_workspaces: 2 })).toContain('it also holds mailboxes in 2 other workspaces, gains no mailbox')
    // Among the keys the person created, revoking one carried over ends it
    // everywhere, and that list does not count the other workspaces.
    const mine = keyOriginNote({ carried_over: true }, 'mine')
    expect(mine).toContain('acts only while you allow actions')
    expect(mine).toContain('Revoking it here ends it in every workspace.')
    expect(mine).not.toContain('other workspaces')
    expect(mine).not.toContain('takes this workspace’s mailboxes out of it')
    expect(keyOriginNote({ origin: 'person' }, 'mine')).toBe(keyOriginNote({ origin: 'person' }))
    expect(keyOriginNote({ origin: 'person-all' })).toContain('it was given the mailboxes they read then, and none connected since')
    expect(keyOriginNote({ origin: 'person' })).toContain('never sends')
  })

  it('reads a key the running console can draw, and holds the contract to its scopes and shape', () => {
    expect(isWorkspaceKey({ ...listed(PREFIX), scope: 'admin' })).toBe(true)
    expect(isWorkspaceKey({ ...listed(PREFIX), scope: 'admin' }, true)).toBe(false)
    expect(isWorkspaceKey({ ...listed(PREFIX), scope: 'send' }, true)).toBe(true)
    expect(isWorkspaceKey({ ...listed(PREFIX), prefix: '../x' })).toBe(false)
    expect(isWorkspaceKey({ ...listed(PREFIX), prefix: 'a?b' })).toBe(false)
    expect(isCreatedKey(made(), true)).toBe(true)
    // The mailboxes a key holds are always named, [] for none.
    const { mailboxes: _, ...unnamed } = made()
    expect(isCreatedKey(unnamed)).toBe(false)
    expect(isCreatedKey({ ...made(), key: `${PREFIX}.` })).toBe(false)
    expect(isCreatedKey({ ...made(), key: `${PREFIX}.a b` })).toBe(false)
    // A key's mailboxes are its workspace's.
    expect(isWorkspaceKey(listed(PREFIX, { mailboxes: [held(ONE, { workspace_id: TEAM })] }), true)).toBe(false)
  })
})
