// API keys & MCP as a person presses it, on a page (test/dom.ts): creating a
// key of the workspace shown asks nothing of the server until Create key,
// under the text of what the key authorizes; Read is offered only on a
// mailbox the person reads, Act with a scope that acts, Send with the send
// scope, which is offered only where the server says its keys may send; the
// key is shown once, copied on request, and gone from the page when the
// dialog closes, which only Done or the close button do while the key is
// shown; the command with the key in it goes to the clipboard and never onto
// the page; what a key holds is ticked and saved, a mailbox taken out of it,
// its sends listed; revoking asks first; and the person's own keys, in their
// account, are revoked from there.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { check, click, fill, find, fire, flush, keydown, page, submit, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Account, CreatedKey, KeyMailbox, Workspace, WorkspaceKey } from '../src/api/types'
import KeysPanel from '../src/components/KeysPanel.vue'
import MyKeys from '../src/components/MyKeys.vue'
import { loadAccounts } from '../src/state/accounts'
import { apiKeys, myKeys } from '../src/state/apikeys'
import { signIn, signOut } from '../src/state/session'
import { loadWorkspaces, selectWorkspace } from '../src/state/workspaces'
import { KEY_TERMS_VERSION } from '../src/open/versions'
import { account, ana, failure, json, now, reply, serve, stubPage, type Route } from './support'

const PREFIX = '3f9a0c1d2e4b5a6c'
const SECRET = `${PREFIX}.Zm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXg`
const COMMAND = `claude mcp add --transport http mailie http://localhost:5174/mcp --header "Authorization: Bearer ${SECRET}"`
const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const ONE = 'acc_0000000000000001'
const TWO = 'acc_0000000000000002'
const BEA = 'usr_00000000000000b2'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support: Workspace = { id: TEAM, kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1_790_000_000 }
const reads = { read: true, act: true, send: true, manage: true }

const held = (accountID: string, workspaceID: string, fields: Partial<KeyMailbox> = {}): KeyMailbox => ({ account_id: accountID, workspace_id: workspaceID, read: true, act: false, send: false, granted_by: ana.id, updated_at: now() - 3_600, ...fields })
function listed(prefix: string, fields: Partial<WorkspaceKey> = {}): WorkspaceKey {
  return { prefix, name: `Key ${prefix.slice(0, 4)}`, scope: 'read', workspace_id: PERSONAL, mailboxes: [], created_by: ana.id, created_at: now() - 86_400, expires_at: now() + 89 * 86_400, live: true, terms_version: KEY_TERMS_VERSION, sends: false, ...fields }
}

let keys: WorkspaceKey[] = []
let mine: WorkspaceKey[] = []
/** What GET /v1/me/mcp answers: whether the server serves MCP over HTTP, and whether its keys send. */
let mcp = { http: true, keys_send: true }
let mounted: Mounted | null = null
let clipboard: string[] = []

interface Place { workspaces: Workspace[]; shown: string; mailboxes: Account[]; members?: object[] }
const home: Place = {
  workspaces: [personal], shown: PERSONAL,
  mailboxes: [account({ id: ONE, state: 'active', workspace_id: PERSONAL, access: reads }), account({ id: TWO, email: 'loja@example.test', provider: 'imap', state: 'active', workspace_id: PERSONAL, access: reads })],
}
/** Ana, an admin of Support: she reads one of its mailboxes and manages the other by her role. */
const team: Place = {
  workspaces: [personal, support], shown: TEAM,
  mailboxes: [
    account({ id: ONE, email: 'suporte@support.example', state: 'active', workspace_id: TEAM, access: { ...reads, manage: true } }),
    account({ id: TWO, email: 'vendas@support.example', provider: 'imap', state: 'active', workspace_id: TEAM, access: { read: false, act: false, send: false, manage: true } }),
  ],
  members: [
    { user_id: ana.id, email: ana.email, name: ana.name, role: 'admin', status: 'active', last_owner: false, last_reader_of: [], joined_at: 1_790_000_000 },
    { user_id: BEA, email: 'bea@example.test', name: 'Bea Lima', role: 'owner', status: 'active', last_owner: true, last_reader_of: [], joined_at: 1_790_000_000 },
  ],
}

/** Signed in, with the workspace shown and its mailboxes; route answers the key routes it changes. */
async function signedIn(route: Route, place: Place = home) {
  const keysPath = `/v1/workspaces/${place.shown}/apikeys`
  const fetch = serve(request => {
    const { path, method } = request
    if (path === '/v1/auth/login') return json(reply())
    if (path === '/v1/auth/logout') return new Response(null, { status: 204 })
    if (path === '/v1/workspaces' && method === 'GET') return json(place.workspaces)
    if (path === '/v1/accounts') return json(place.mailboxes)
    if (path === `/v1/workspaces/${TEAM}/members`) return json(place.members ?? [])
    if (path === keysPath && method === 'GET') return json(keys)
    if (path === '/v1/me/apikeys' && method === 'GET') return json(mine)
    if (path === '/v1/me/mcp' && method === 'GET') return json(mcp)
    return route(request)
  })
  await signIn('ana@example.test', 'correct-password')
  await loadWorkspaces()
  selectWorkspace(place.shown)
  await loadAccounts()
  mounted = mount(KeysPanel)
  await vi.waitFor(() => expect(apiKeys.loaded).toBe(true))
  await flush()
  return fetch
}
const sent = (fetch: ReturnType<typeof serve>, method: string, path: string) => fetch.mock.calls
  .filter(([url, init]) => new URL(String(url)).pathname === path && (init?.method ?? 'GET') === method)
  .map(([, init]) => typeof init?.body === 'string' ? JSON.parse(init.body) : undefined)
const posts = (fetch: ReturnType<typeof serve>, workspace = PERSONAL) => sent(fetch, 'POST', `/v1/workspaces/${workspace}/apikeys`)
const deletes = (fetch: ReturnType<typeof serve>) => fetch.mock.calls
  .filter(([, init]) => init?.method === 'DELETE').map(([url]) => new URL(String(url)).pathname)

/** A daemon that makes the key it is asked for. */
const creates = (workspace = PERSONAL): Route => ({ path, method, body }) => {
  if (path !== `/v1/workspaces/${workspace}/apikeys` || method !== 'POST') return failure('not_found', 404)
  const request = body as { name: string; scope: string; ttl_days: number; mailboxes?: KeyMailbox[] }
  const created: CreatedKey = {
    ...listed(PREFIX, { name: request.name, scope: request.scope, workspace_id: workspace, sends: request.scope === 'send', created_at: now(), expires_at: now() + request.ttl_days * 86_400 }),
    mailboxes: (request.mailboxes ?? []).map(item => held(item.account_id, workspace, item)), key: SECRET,
  }
  const { key: _, ...stored } = created
  keys = [stored, ...keys]
  return json(created, 201)
}

const dialog = () => find('dialog')
const createButton = () => find('.section-actions button', 'Create key') ?? find('.empty-card button', 'Create key')
const flag = (accountID: string, name: string) => find(`dialog input[name=${accountID}-${name}]`)

beforeEach(() => {
  stubPage()
  keys = []
  mine = []
  mcp = { http: true, keys_send: true }
  clipboard = []
  vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn(async (text: string) => { clipboard.push(text) }) } })
})
afterEach(async () => {
  mounted?.unmount()
  mounted = null
  await signOut().catch(() => {})
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('creating a key', () => {
  it('asks the server nothing until Create key, under the text of what the key allows, and shows the key once', async () => {
    const fetch = await signedIn(creates())
    await click(createButton())
    const form = find('dialog form')!
    const text = words(dialog()!)
    expect(text).toContain('Whoever holds this key can reach the mailboxes it is given through this server’s API and MCP server, until the key expires or is revoked.')
    expect(text).toContain('The key belongs to your personal workspace: you see it, choose which of your mailboxes it reaches and what it may do in each, and can revoke it.')
    // Not only the person: the operator lists and revokes every key, and the text says so.
    expect(text).not.toContain('only you')
    expect(text).toContain('Whoever runs this server can also list every API key, with the mailboxes it holds, and revoke it.')
    expect(text).toContain('where it is given Read: search the messages by the details in the index')
    // What the MCP server keeps to resume a dropped stream is said, with its bound (internal/mcp DefaultEventStoreAge).
    expect(text).toContain('This server does not store what it fetches for the tool. So that the tool can pick up a dropped connection, the MCP server holds what it sent in memory only, never on disk, for at most five minutes.')
    expect(text).toContain('An AI assistant such as Claude, for example, sends it to Anthropic to answer you.')
    expect(text).toContain('Keys created under this server’s earlier terms for API keys keep working under those terms, and never send email.')
    expect(text).toContain('It cannot send email, connect or remove mailboxes')
    // The open console's text belongs to whoever runs the server: it links to no company policy.
    expect(find('dialog .key-terms a')).toBeNull()
    // The actions line belongs to a scope that acts, the sending line to send; each shows with it.
    expect(text).not.toContain('where it is given Act')
    expect(text).not.toContain('where it is given Send')
    expect(flag(ONE, 'act')).toBeNull()
    await fill(find('dialog input[name=key-name]'), '  Claude Code  ')
    await check(find('dialog input[value=write]'))
    // It acts under these terms: turning actions off in an account does not stop it.
    expect(words(dialog()!)).toContain('where it is given Act: mark messages as read or unread, star them, archive them, and move them to another folder or to the trash; turning actions off in an account does not stop it, taking Act away from it does')
    await check(find('dialog input[value=send]'))
    expect(words(dialog()!)).toContain('Each message is sent only when the tool’s own request confirms it (confirm: true)')
    expect(words(dialog()!)).toContain('It cannot connect or remove mailboxes, create other keys, or change anyone’s access or account.')
    // Act ticks the Read it needs; Send stands alone.
    await check(flag(TWO, 'act'))
    expect(flag(TWO, 'read')!.checked).toBe(true)
    await check(flag(ONE, 'send'))
    await check(find('dialog label.lifetime', '30 days')!.querySelector('input'))
    expect(posts(fetch)).toEqual([])

    await submit(form)
    expect(posts(fetch)).toEqual([{
      name: 'Claude Code', scope: 'send', ttl_days: 30, terms_version: KEY_TERMS_VERSION,
      mailboxes: [{ account_id: ONE, read: false, act: false, send: true }, { account_id: TWO, read: true, act: true, send: false }],
    }])
    expect(words(find('dialog .key-secret')!)).toBe(SECRET)
    expect(find('dialog .key-secret')!.getAttribute('translate')).toBe('no')
    expect(words(dialog()!)).toContain('Mailie shows it only this once and cannot show it again.')
    expect(words(find('dialog .key-facts')!)).toContain('loja@example.test (Read, Act)')
    // The list has the key, without its secret.
    expect(words(find('.key-card')!)).toContain('Claude Code')
    expect(words(find('.key-card')!)).not.toContain(SECRET)
    expect(page.activeElement).toBe(find('dialog button', 'Copy key'))
  })

  it('offers no sending where the server says its keys do not send', async () => {
    mcp = { http: true, keys_send: false }
    const fetch = await signedIn(creates())
    await click(createButton())
    expect(find('dialog input[value=send]')).toBeNull()
    await check(find('dialog input[value=write]'))
    expect(flag(ONE, 'send')).toBeNull()
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(posts(fetch).map(body => body.scope)).toEqual(['write'])
    // The key's card names no sends either.
    expect(words(find('.key-card')!)).toContain('Mailboxes…')
    expect(words(find('.key-card')!)).not.toContain('Mailboxes and sends…')
  })

  it('offers Read in a team only on a mailbox the person reads, says why, and names the team in the terms', async () => {
    const fetch = await signedIn(creates(TEAM), team)
    await click(createButton())
    const text = words(dialog()!)
    expect(text).toContain('The key belongs to Support: its owners and admins see it, choose which of the team’s mailboxes it reaches and what it may do in each, and can revoke it.')
    expect(text).toContain('You, and any owner or admin of Support, can revoke the key here whenever you want, and it stops working at once.')
    expect(text).toContain('It stops working when it expires or is revoked, and when you leave Support or your account on this server is disabled or deleted.')
    expect(flag(ONE, 'read')!.disabled).toBe(false)
    expect(flag(TWO, 'read')!.disabled).toBe(true)
    expect(words(find('dialog .mailbox-flag-row', 'vendas@support.example')!)).toContain('You do not read it')
    // Act needs Read, which is not hers to give there; Send is any owner's or admin's to give.
    await check(find('dialog input[value=send]'))
    expect(flag(TWO, 'act')!.disabled).toBe(true)
    expect(flag(TWO, 'send')!.disabled).toBe(false)
    await check(flag(TWO, 'send'))
    await check(flag(ONE, 'read'))
    await fill(find('dialog input[name=key-name]'), 'Helpdesk')
    await submit(find('dialog form'))
    expect(posts(fetch, TEAM)[0].mailboxes).toEqual([{ account_id: ONE, read: true, act: false, send: false }, { account_id: TWO, read: false, act: false, send: true }])
  })

  it('narrows what each mailbox holds when a narrower scope is chosen', async () => {
    const fetch = await signedIn(creates())
    await click(createButton())
    await check(find('dialog input[value=send]'))
    await check(flag(ONE, 'act'))
    await check(flag(ONE, 'send'))
    await check(find('dialog input[value=read]'))
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(posts(fetch)[0].mailboxes).toEqual([{ account_id: ONE, read: true, act: false, send: false }])
  })

  it('copies the key, and the command with the key only to the clipboard, never onto the page', async () => {
    await signedIn(creates())
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Claude Code')
    await submit(find('dialog form'))
    await click(find('dialog button', 'Copy key'))
    await click(find('dialog button', 'Copy the Claude Code command with this key'))
    expect(clipboard).toEqual([SECRET, COMMAND])
    expect(words()).not.toContain(`Bearer ${SECRET}`)
    expect(words(find('dialog')!)).toContain('Command copied')
    // The command on the page carries the placeholder, never a key.
    expect(words(find('.connect-panel')!)).toContain('--header "Authorization: Bearer <your key>"')
    expect(words(find('.connect-panel')!)).not.toContain(PREFIX)
  })

  it('offers no Claude Code command, and shows no MCP address, on a server that does not serve MCP over HTTP', async () => {
    mcp = { http: false, keys_send: true }
    const fetch = await signedIn(creates())
    expect(fetch.mock.calls.some(([url]) => new URL(String(url)).pathname === '/v1/me/mcp')).toBe(true)
    expect(find('.connect-panel')).toBeNull()
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Claude Code')
    await submit(find('dialog form'))
    expect(words(find('dialog .key-secret')!)).toBe(SECRET)
    expect(find('dialog button', 'Copy the Claude Code command with this key')).toBeNull()
    expect(words(dialog()!)).not.toContain('The command connects Claude Code')
    expect(words()).not.toContain('/mcp')
  })

  it('lets go of the key when the dialog closes, however it closes, and opens on a new form after', async () => {
    await signedIn(creates())
    for (const close of [() => click(find('dialog button', 'Done')), () => click(find('dialog button[aria-label=Close]'))]) {
      await click(createButton())
      await fill(find('dialog input[name=key-name]'), 'Claude Code')
      await submit(find('dialog form'))
      expect(words()).toContain(SECRET)
      await close()
      expect(dialog()).toBeNull()
      expect(words()).not.toContain(SECRET)
      expect(JSON.stringify(apiKeys)).not.toContain(SECRET)
    }
    await click(createButton())
    expect(find('dialog form')).not.toBeNull()
    expect(find('dialog .key-secret')).toBeNull()
  })

  it('keeps the key on screen through Escape or a tap beside the dialog, which close the form as any dialog', async () => {
    await signedIn(creates())
    // The form: Escape closes it.
    await click(createButton())
    expect(keydown({ key: 'Escape' }).defaultPrevented).toBe(false)
    await fire(dialog(), 'cancel')
    expect(dialog()).toBeNull()

    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Claude Code')
    await submit(find('dialog form'))
    // Escape's keydown is held back, so the browser never asks to close; a cancel it sends anyway changes nothing.
    expect(keydown({ key: 'Escape' }).defaultPrevented).toBe(true)
    await fire(dialog(), 'cancel')
    // A tap on the backdrop is a press and a click on the <dialog> itself.
    await fire(dialog(), 'pointerdown')
    await fire(dialog(), 'click')
    expect(dialog()?.open).toBe(true)
    expect(words(find('dialog .key-secret')!)).toBe(SECRET)
    await click(find('dialog button', 'Done'))
    expect(dialog()).toBeNull()
    expect(words()).not.toContain(SECRET)
  })

  it('makes a key that reaches nothing yet when no mailbox is ticked, and says so', async () => {
    const fetch = await signedIn(creates())
    await click(createButton())
    expect(words(dialog()!)).toContain('With no mailbox ticked, the key reaches nothing until one is given to it.')
    await fill(find('dialog input[name=key-name]'), 'Later')
    await submit(find('dialog form'))
    expect(posts(fetch)).toEqual([{ name: 'Later', scope: 'read', ttl_days: 90, terms_version: KEY_TERMS_VERSION }])
    expect(words(find('dialog .key-facts')!)).toContain('None yet')
  })

  it('says why a key was not made in the console’s words, never the server’s', async () => {
    await signedIn(() => failure('not_found', 404))
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(words(find('dialog .alert')!)).toBe('A chosen mailbox is no longer in this workspace. Choose again.')
    expect(words()).not.toContain('hunter2')
    expect(find('dialog form')).not.toBeNull()
  })

  it('offers a reload instead of Create key when the terms changed while the page was open', async () => {
    const fetch = await signedIn(() => failure('conflict', 409))
    await click(createButton())
    await fill(find('dialog input[name=key-name]'), 'Reader')
    await submit(find('dialog form'))
    expect(words(find('dialog .alert')!)).toBe('The terms for API keys changed while this page was open. Reload the page to read the current text.')
    expect(find('dialog button', 'Reload page')).not.toBeNull()
    expect(find('dialog button[type=submit]')).toBeNull()
    // The text shown is not the one the server asks about now, so it no longer stands as the terms.
    expect(find('dialog .key-terms')).toBeNull()
    expect(posts(fetch)).toHaveLength(1)
  })

  it('offers no new key at the workspace’s limit, and says why', async () => {
    keys = Array.from({ length: 20 }, (_, i) => listed(`a${String(i).padStart(15, '0')}`))
    await signedIn(creates())
    expect(createButton()!.disabled).toBe(true)
    expect(words()).toContain('This workspace has 20 active keys, the most it can have. Revoke one to create another.')
  })
})

describe('the list', () => {
  it('names what each key holds on each mailbox, who created it in a team, and what a key from before does differently', async () => {
    const { workspace_id: _, ...carried } = listed('0a0b0c0d0e0f0a0b', { name: 'Old script', workspace_id: TEAM, created_by: BEA, mailboxes: [held(ONE, TEAM)] })
    keys = [
      listed(PREFIX, { name: 'Helpdesk', scope: 'send', sends: true, workspace_id: TEAM, created_by: BEA, mailboxes: [held(ONE, TEAM, { act: true }), held(TWO, TEAM, { read: false, send: true })] }),
      { ...carried, carried_over: true, other_workspaces: 2 },
    ]
    await signedIn(() => failure('not_found', 404), team)
    await vi.waitFor(() => expect(words(find('.key-card', 'Helpdesk')!)).toContain('Bea Lima'))
    const card = words(find('.key-card', 'Helpdesk')!)
    expect(card).toContain('Read, act and send')
    expect(card).toContain('suporte@support.example (Read, Act), vendas@support.example (Send)')
    expect(words(find('.key-card', 'Old script')!)).toContain('it also holds mailboxes in 2 other workspaces, gains no mailbox')
    expect(words()).toContain('Every owner and admin of Support sees these keys and can revoke them.')
  })
})

describe('a key’s mailboxes and sends', () => {
  it('saves what is ticked with every flag, takes a mailbox out when nothing is left, and lists the sends without who or what', async () => {
    keys = [listed(PREFIX, { name: 'Helpdesk', scope: 'send', sends: true, mailboxes: [held(ONE, PERSONAL)] })]
    const send = { account_id: ONE, idempotency_key: 'k1', state: 'unknown', message_id: 'm@example.test', attempts: 1, recipients: 2, sent_copy: 'n/a', created_at: now() - 60, updated_at: now() - 60 }
    const fetch = await signedIn(({ path, method, body }) => {
      if (path === `/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}/sends`) return json([send])
      if (path === `/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}/accounts/${TWO}` && method === 'PUT') return json(held(TWO, PERSONAL, body as Partial<KeyMailbox>))
      if (path === `/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}/accounts/${ONE}` && method === 'DELETE') return new Response(null, { status: 204 })
      return failure('not_found', 404)
    })
    await click(find('.key-card button', 'Mailboxes and sends'))
    await vi.waitFor(() => expect(find('dialog .send-row')).not.toBeNull())
    const sends = words(find('dialog .send-row')!)
    expect(sends).toContain('May have been delivered')
    expect(sends).toContain('suporte@example.test')
    expect(sends).toContain('Recipients: 2')
    expect(sends).not.toContain('m@example.test')

    const two = find('dialog .key-row[data-account=acc_0000000000000002]')!
    await check(two.querySelector('input[value=act]'))
    expect((two.querySelector('input[value=read]') as unknown as HTMLInputElement).checked).toBe(true)
    await click(find('dialog .key-row[data-account=acc_0000000000000002] button', 'Save access'))
    expect(sent(fetch, 'PUT', `/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}/accounts/${TWO}`)).toEqual([{ read: true, act: true, send: false }])

    const one = find('dialog .key-row[data-account=acc_0000000000000001]')!
    await check(one.querySelector('input[value=read]'), false)
    await click(find('dialog .key-row[data-account=acc_0000000000000001] button', 'Take out of the key'))
    expect(deletes(fetch)).toEqual([`/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}/accounts/${ONE}`])
    expect(apiKeys.list[0]!.mailboxes.map(item => item.account_id)).toEqual([TWO])
  })

  it('gives Read in a team only on a mailbox the person reads, and gives a key from before nothing', async () => {
    const { workspace_id: _, ...carried } = listed('0a0b0c0d0e0f0a0b', { name: 'Old script', mailboxes: [held(ONE, TEAM)] })
    keys = [listed(PREFIX, { name: 'Helpdesk', scope: 'write', workspace_id: TEAM, mailboxes: [] }), { ...carried, carried_over: true, other_workspaces: 1 }]
    await signedIn(() => failure('not_found', 404), team)
    await click(find('.key-card[data-key=3f9a0c1d2e4b5a6c] button', 'Mailboxes and sends'))
    const row = (id: string) => find(`dialog .key-row[data-account=${id}]`)!
    expect((row(ONE).querySelector('input[value=read]') as unknown as HTMLInputElement).disabled).toBe(false)
    expect((row(TWO).querySelector('input[value=read]') as unknown as HTMLInputElement).disabled).toBe(true)
    expect(words(dialog()!)).toContain('You can give the key Read only on a mailbox you read yourself.')
    await click(find('dialog button', 'Done'))
    await click(find('.key-card[data-key=0a0b0c0d0e0f0a0b] button', 'Mailboxes and sends'))
    // Only what it holds, and that only to take away.
    expect(page.querySelectorAll('dialog .key-row').map(item => item.getAttribute('data-account'))).toEqual([ONE])
    const read = row(ONE).querySelector('input[value=read]') as unknown as HTMLInputElement
    expect(read.checked).toBe(true)
    expect(read.disabled).toBe(false)
    expect(row(ONE).querySelector('input[value=act]')).toBeNull()
  })
})

describe('revoking a key', () => {
  it('asks first, and Cancel revokes nothing', async () => {
    keys = [listed(PREFIX, { name: 'Claude Code' })]
    const fetch = await signedIn(({ path, method }) => {
      if (method === 'DELETE' && path === `/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}`) {
        keys = [listed(PREFIX, { name: 'Claude Code', revoked_at: now(), live: false })]
        return new Response(null, { status: 204 })
      }
      return failure('not_found', 404)
    })
    await click(find('.key-card button', 'Revoke'))
    expect(words(dialog()!)).toContain('Claude Code stops working at once: a tool using it loses access at its next request. This cannot be undone.')
    expect(deletes(fetch)).toEqual([])
    await click(find('dialog button', 'Cancel'))
    expect(dialog()).toBeNull()
    expect(deletes(fetch)).toEqual([])
    await click(find('.key-card button', 'Revoke'))
    await click(find('dialog button.danger', 'Revoke key'))
    expect(deletes(fetch)).toEqual([`/v1/workspaces/${PERSONAL}/apikeys/${PREFIX}`])
    expect(dialog()).toBeNull()
    await flush()
    expect(words(find('.key-card .status-chip')!)).toBe('Revoked')
    expect(find('.key-card button', 'Revoke')).toBeNull()
    expect(words(find('.success')!)).toBe('Claude Code was revoked. A tool using it can no longer reach these mailboxes.')
  })

  it('says that a key from before only lost this workspace’s mailboxes', async () => {
    const { workspace_id: _, ...carried } = listed('0a0b0c0d0e0f0a0b', { name: 'Old script', mailboxes: [held(ONE, TEAM)] })
    keys = [{ ...carried, carried_over: true, other_workspaces: 1 }]
    const fetch = await signedIn(({ path, method }) => {
      if (method === 'DELETE' && path === `/v1/workspaces/${TEAM}/apikeys/0a0b0c0d0e0f0a0b`) {
        keys = []
        return new Response(null, { status: 204 })
      }
      return failure('not_found', 404)
    }, team)
    await click(find('.key-card button', 'Revoke'))
    expect(words(dialog()!)).toContain('Old script loses every mailbox of this workspace at once, and is revoked if it holds none elsewhere.')
    await click(find('dialog button.danger', 'Revoke key'))
    expect(deletes(fetch)).toEqual([`/v1/workspaces/${TEAM}/apikeys/0a0b0c0d0e0f0a0b`])
    await flush()
    expect(words(find('.success')!)).toBe('Old script no longer holds this workspace’s mailboxes, and was revoked if it held none elsewhere.')
  })
})

describe('the keys a person created, in their account', () => {
  it('are listed with their workspace and revoked after a question, wherever they are', async () => {
    const { workspace_id: _, ...carried } = listed('0c0d0e0f0a0b0c0d', { name: 'Older script', mailboxes: [held(ONE, TEAM), held(TWO, PERSONAL)] })
    mine = [
      listed(PREFIX, { name: 'Helpdesk', workspace_id: TEAM, mailboxes: [held(ONE, TEAM)] }),
      listed('0a0b0c0d0e0f0a0b', { name: 'Old', revoked_at: now() - 60, live: false }),
      { ...carried, carried_over: true },
    ]
    const fetch = await signedIn(({ path, method }) => {
      if (path === `/v1/me/apikeys/${PREFIX}` && method === 'DELETE') {
        mine = [{ ...mine[0]!, revoked_at: now(), live: false }, mine[1]!]
        return new Response(null, { status: 204 })
      }
      return failure('not_found', 404)
    }, team)
    mounted?.unmount()
    mounted = mount(MyKeys)
    await vi.waitFor(() => expect(myKeys.loaded).toBe(true))
    await flush()
    expect(words(find('.my-key', 'Helpdesk')!)).toContain('Support')
    expect(find('.my-key[data-key=0a0b0c0d0e0f0a0b]')!.querySelector('button')).toBeNull()
    // A key from before, here: revoking it ends it everywhere, as the dialog says.
    const older = words(find('.my-key', 'Older script')!)
    expect(older).toContain('Several workspaces (2)')
    expect(older).toContain('Revoking it here ends it in every workspace.')
    expect(older).not.toContain('other workspaces')
    await click(find('.my-key button', 'Revoke'))
    expect(words(dialog()!)).toContain('Helpdesk stops working at once, in every mailbox it holds')
    await click(find('dialog button.danger', 'Revoke key'))
    expect(deletes(fetch)).toEqual([`/v1/me/apikeys/${PREFIX}`])
    await flush()
    expect(words(find('.my-key', 'Helpdesk')!)).toContain('Revoked')
  })

  it('shows nothing to a person who never created a key', async () => {
    await signedIn(() => failure('not_found', 404))
    mounted?.unmount()
    mounted = mount(MyKeys)
    await vi.waitFor(() => expect(myKeys.loaded).toBe(true))
    await flush()
    expect(find('.my-keys')).toBeNull()
  })
})
